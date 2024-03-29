package handlers

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"

	"log"
	"net/http"
	"reservations-service/data"
	error2 "reservations-service/error"
	"reservations-service/repository"
	"reservations-service/services"
	"reservations-service/utils"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"github.com/sony/gobreaker"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

var validateFields = validator.New()

type KeyProduct struct{}

type ReservationsHandler struct {
	logger         *log.Logger
	Repo           *repository.ReservationRepo
	EventRepo      *repository.EventRepo
	serviceAv      services.AvailabilityService
	DB             *mongo.Collection
	Tracer         trace.Tracer
	CircuitBreaker *gobreaker.CircuitBreaker
	logg           *logrus.Logger
}

func NewReservationsHandler(l *log.Logger, srv services.AvailabilityService, r *repository.ReservationRepo, er *repository.EventRepo, db *mongo.Collection, tracer trace.Tracer, logg *logrus.Logger) ReservationsHandler {
	circuitBreaker := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name: "HTTPSRequest",
		OnStateChange: func(name string, from gobreaker.State, to gobreaker.State) {
			fmt.Printf("Circuit Breaker state changed from %s to %s\n", from, to)
		},
	})
	return ReservationsHandler{
		logger:         l,
		serviceAv:      srv,
		Repo:           r,
		EventRepo:      er,
		DB:             db,
		Tracer:         tracer,
		CircuitBreaker: circuitBreaker,
		logg:           logg,
	}
}

func (s *ReservationsHandler) CreateReservationForGuest(rw http.ResponseWriter, h *http.Request) {
	ctx, span := s.Tracer.Start(h.Context(), "ReservationsHandler.CreateReservationForGuest")
	defer span.End()

	token := h.Header.Get("Authorization")
	url := "https://auth-server:8080/api/users/currentUser"

	timeout := 2000 * time.Second // Adjust the timeout duration as needed
	ctxx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := s.HTTPSperformAuthorizationRequestWithCircuitBreakerRsv(ctx, token, url)
	if err != nil {

		if errors.Is(err, gobreaker.ErrOpenState) {
			// Circuit is open
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Circuit is open. Authorization server is not available.")
			span.SetStatus(codes.Error, "Circuit is open. Authorization service is not available.")
			error2.ReturnJSONError(rw, "Authorization service is not available.", http.StatusBadRequest)
			return
		}

		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Authorization service is not available")
			span.SetStatus(codes.Error, "Authorization service not available")
			errorMsg := map[string]string{"error": "Authorization service not available.."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusInternalServerError)
			return
		}
		span.SetStatus(codes.Error, "Authorization service not available")
		errorMsg := map[string]string{"error": "Authorization service not available.."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode
	if statusCode != 200 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Unauthorized")
		span.SetStatus(codes.Error, "Unauthorized")
		errorMsg := map[string]string{"error": "Unauthorized."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusUnauthorized)
		return
	}

	// Read the response body
	// Create a JSON decoder for the response body
	decoder := json.NewDecoder(resp.Body)

	// Define a struct to represent the JSON structure
	var response struct {
		LoggedInUser struct {
			ID       string        `json:"id"`
			UserRole data.UserRole `json:"userRole"`
			Username string        `json:"username"`
		} `json:"user"`
		Message string `json:"message"`
	}

	// Decode the JSON response into the struct
	if err := decoder.Decode(&response); err != nil {
		if strings.Contains(err.Error(), "cannot parse") {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Inavelid date format in the response")
			span.SetStatus(codes.Error, "Invalid date format in the response")
			error2.ReturnJSONError(rw, "Invalid date format in the response", http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Error decoding JSON response")
		span.SetStatus(codes.Error, "Error decoding JSON response")
		error2.ReturnJSONError(rw, fmt.Sprintf("Error decoding JSON response: %v", err), http.StatusBadRequest)
		return
	}

	// Access the 'id' from the decoded struct
	guestId := response.LoggedInUser.ID
	//guestId = html.EscapeString(guestId)

	userRole := response.LoggedInUser.UserRole

	if userRole != data.Guest {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Permission denied. Only guests can create reservations")
		span.SetStatus(codes.Error, "Permission denied. Only guests can create reservations")
		errorMsg := map[string]string{"error": "Permission denied. Only guests can create reservations"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusForbidden)
		return
	}

	guestReservation := h.Context().Value(KeyProduct{}).(*data.ReservationByGuestCreate)

	accId := guestReservation.AccommodationId
	urlAccommodationCheck := "https://acc-server:8083/api/accommodations/get/" + accId

	resp, err = s.HTTPSperformAuthorizationRequestWithCircuitBreakerRsv(ctx, token, urlAccommodationCheck)
	if err != nil {

		if errors.Is(err, gobreaker.ErrOpenState) {
			// Circuit is open
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Circuit is open. Authorization service is not available")
			span.SetStatus(codes.Error, "Circuit is open. Authorization service is not available.")
			error2.ReturnJSONError(rw, "Authorization service is not available.", http.StatusBadRequest)
			return
		}

		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Accommodation service is not available")
			span.SetStatus(codes.Error, "Accommodation service is not available")
			errorMsg := map[string]string{"error": "Accommodation service is not available."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Accommodation service is not available")
		span.SetStatus(codes.Error, "Accommodation service is not available")
		errorMsg := map[string]string{"error": "Accommodation service is not available."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	defer resp.Body.Close()

	statusCodeAccommodation := resp.StatusCode
	fmt.Println(statusCodeAccommodation)
	if statusCodeAccommodation != 200 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Accommodation with that id does not exist")
		span.SetStatus(codes.Error, "Accommodation with that id does not exist")
		errorMsg := map[string]string{"error": "Accommodation with that id does not exist."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	// Validate empty fields
	if err := validateFields.Struct(guestReservation); err != nil {
		validationErrors := make(map[string]string)
		for _, err := range err.(validator.ValidationErrors) {
			field := err.Field()
			validationErrors[field] = fmt.Sprintf("Field %s is required", field)
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Field is required")
		span.SetStatus(codes.Error, "Field is required")
		error2.ReturnJSONError(rw, validationErrors, http.StatusBadRequest)
		return
	}

	if guestReservation.CheckInDate.IsZero() || guestReservation.CheckOutDate.IsZero() {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Check-in and check-out dates are required")
		span.SetStatus(codes.Error, "Check-in and check-out dates are required.")
		errorMsg := map[string]string{"error": "Check-in and check-out dates are required."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	//if !utils.IsValidDateFormat(guestReservation.CheckInDate.String()) ||
	//	!utils.IsValidDateFormat(guestReservation.CheckOutDate.String()) {
	//	error2.ReturnJSONError(rw, "Invalid date format. Use '2006-01-02T15:04:05Z'", http.StatusBadRequest)
	//	return
	//}

	if guestReservation.CheckInDate.Before(time.Now()) {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Check-in date mus be in the future")
		span.SetStatus(codes.Error, "Check-in date must be in the future")
		errorMsg := map[string]string{"error": "Check-in date must be in the future."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	if guestReservation.CheckInDate.After(guestReservation.CheckOutDate) {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Check-in date must be before check out date")
		span.SetStatus(codes.Error, "Check-in date must be before check out date")
		errorMsg := map[string]string{"error": "Check-in date must be before check out date."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	if !utils.IsValidInteger(guestReservation.NumberOfGuests) {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Invalid field number_of_guests. Its a whole number")
		span.SetStatus(codes.Error, "Invalid field number_of_guests. It's a whole number.")
		errorMsg := map[string]string{"error": "Invalid field number_of_guests. It's a whole number."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	// Define a struct to represent the JSON structure of user
	var responseAccommodation struct {
		AccommodationName      string `json:"accommodation_name"`
		AccommodationLocation  string `json:"accommodation_location"`
		AccommodationHostId    string `json:"host_id"`
		AccommodationMinGuests int    `json:"accommodation_min_guests"`
		AccommodationMaxGuests int    `json:"accommodation_max_guests"`
	}
	decoder = json.NewDecoder(resp.Body)

	// Decode the JSON response into the struct
	if err := decoder.Decode(&responseAccommodation); err != nil {
		if strings.Contains(err.Error(), "cannot parse") {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Invalid date format")
			span.SetStatus(codes.Error, "Invalid date format.")
			errorMsg := map[string]string{"error": "Invalid date format."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Error decoding JSON response")
		span.SetStatus(codes.Error, "Error decoding JSON response:"+err.Error())
		error2.ReturnJSONError(rw, fmt.Sprintf("Error decoding JSON response: %v", err), http.StatusBadRequest)
		return
	}

	if responseAccommodation.AccommodationMaxGuests < guestReservation.NumberOfGuests {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Too much guests. Double check the capasity of accommodation")
		span.SetStatus(codes.Error, "Too much guests.Double check the capacity of accommodation.")
		errorMsg := map[string]string{"error": "Too much guests.Double check the capacity of accommodation."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	guestRsvPrimitive, err := primitive.ObjectIDFromHex(guestReservation.AccommodationId)
	isAvailable, err := s.serviceAv.IsAvailable(guestRsvPrimitive, guestReservation.CheckInDate, guestReservation.CheckOutDate, ctx)
	//if err != nil {
	//	fmt.Println(err)
	//	fmt.Println("here in error")
	//	errorMsg := map[string]string{"error": "Error checking accommodation availability"}
	//	error2.ReturnJSONError(rw, errorMsg, http.StatusInternalServerError)
	//	return
	//}

	if !isAvailable {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Accommodation is not available fot the specified dates")
		span.SetStatus(codes.Error, "Accommodation is not available for the specified dates")
		errorMsg := map[string]string{"error": "Accommodation is not available for the specified dates"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	errReservation := s.Repo.InsertReservationByGuest(ctx, guestReservation, guestId,
		responseAccommodation.AccommodationName, responseAccommodation.AccommodationLocation, responseAccommodation.AccommodationHostId)
	if errReservation != nil {
		span.SetStatus(codes.Error, "Cannot reserve. Please double check if you already reserved exactly the accommodation and check in date")
		s.logger.Print("Database exception: ", errReservation)
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Cannot reserve. Please double check if you already reserved exactly the accommodation")
		errorMsg := map[string]string{"error": "Cannot reserve. Please double check if you already reserved exactly the accommodation and check in date"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	errBookAccommodation := s.serviceAv.BookAccommodation(guestRsvPrimitive, guestReservation.CheckInDate, guestReservation.CheckOutDate, ctx)
	if errBookAccommodation != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Error booking accommodation")
		span.SetStatus(codes.Error, "Error booking accommodation.")
		errorMsg := map[string]string{"error": "Error booking accommodation."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	fmt.Println(responseAccommodation.AccommodationHostId)
	urlHostCheck := "https://auth-server:8080/api/users/getById/" + responseAccommodation.AccommodationHostId

	resp, err = s.HTTPSperformAuthorizationRequestWithCircuitBreakerRsv(ctx, token, urlHostCheck)
	if err != nil {

		if errors.Is(err, gobreaker.ErrOpenState) {
			// Circuit is open
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Circuit is open. Authorization service is not available")
			span.SetStatus(codes.Error, "Circuit is open. Authorization service is not available.")
			error2.ReturnJSONError(rw, "Authorization service is not available.", http.StatusBadRequest)
			return
		}
		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Authorization service is  not available")
			span.SetStatus(codes.Error, "Authorization service is not available.")
			errorMsg := map[string]string{"error": "Authorization service is not available."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Authhorization service is not available")
		span.SetStatus(codes.Error, "Authorization service is not available.")
		errorMsg := map[string]string{"error": "Authorization service is not available."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	defer resp.Body.Close()

	statusCodeHostCheck := resp.StatusCode
	fmt.Println(statusCodeHostCheck)
	if statusCodeHostCheck != 200 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Host with that id does not exist")
		span.SetStatus(codes.Error, "Host with that id does not exist.")
		errorMsg := map[string]string{"error": "Host with that id does not exist."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	decoder = json.NewDecoder(resp.Body)

	// Define a struct to represent the JSON structure
	var responseHost struct {
		Host struct {
			Email    string `json:"email"`
			Username string `json:"username"`
		} `json:"user"`
	}

	// Decode the JSON response into the struct
	if err := decoder.Decode(&responseHost); err != nil {
		if strings.Contains(err.Error(), "cannot parse") {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Invalid date format")
			span.SetStatus(codes.Error, "Invalid date format.")
			errorMsg := map[string]string{"error": "Invalid date format."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Error decoding JSON response")
		span.SetStatus(codes.Error, "Error decoding JSON response: "+err.Error())
		fmt.Println("User has errored")
		error2.ReturnJSONError(rw, fmt.Sprintf("Error decoding JSON response: %v", err), http.StatusBadRequest)
		return
	}

	notificationPayload := map[string]interface{}{
		"host_id":    responseAccommodation.AccommodationHostId,
		"host_email": responseHost.Host.Email,
		"notification_text": "Dear " + responseHost.Host.Username + "\n you have a new reservation! Your " +
			responseAccommodation.AccommodationName + " has been reserved by " + response.LoggedInUser.Username + ".",
	}

	notificationPayloadJSON, err := json.Marshal(notificationPayload)
	if err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Error creating notification payload")
		span.SetStatus(codes.Error, "Error creating notification payload.")
		errorMsg := map[string]string{"error": "Error creating notification payload."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	notificationURL := "https://notifications-server:8089/api/notifications/create"

	resp, err = s.HTTPSperformAuthorizationRequestWithContextAndBodyAccCircuitBreaker(ctx, token, notificationURL, "POST", notificationPayloadJSON)
	if err != nil {
		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Error creating notification request")
			span.SetStatus(codes.Error, "Error creating notification request.")
			errorMsg := map[string]string{"error": "Error creating notification request."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Notification serviec is not available")
		span.SetStatus(codes.Error, "Notification service is not available.")
		errorMsg := map[string]string{"error": "Notification service is not available."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Error creating notification")
		span.SetStatus(codes.Error, "Error creating notification.")
		errorMsg := map[string]string{"error": "Error creating notification."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	responseJSON, err := json.Marshal(guestReservation)
	if err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Error creating JSON response")
		span.SetStatus(codes.Error, "Error creating JSON response")
		error2.ReturnJSONError(rw, "Error creating JSON response", http.StatusInternalServerError)
		return
	}
	s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Info("Created reservation")
	span.SetStatus(codes.Ok, "Created reservation")
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusCreated)

	event := &data.AccommodationEvent{
		Event:           "Reserved",
		GuestID:         response.LoggedInUser.ID,
		AccommodationID: guestReservation.AccommodationId,
	}

	err = s.EventRepo.InsertEvent(ctx, event)
	if err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CreateReservationForGuest"}).Error("Error storing reservation event")
		span.SetStatus(codes.Error, "Error storing reservation event")
		errorMsg := map[string]string{"error": "Error storing reservation event"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusInternalServerError)
		return
	}

	rw.Write(responseJSON)
}

func (s *ReservationsHandler) GetAllReservations(rw http.ResponseWriter, h *http.Request) {
	ctx, span := s.Tracer.Start(h.Context(), "ReservationsHandler.GetAllReservations")
	defer span.End()

	token := h.Header.Get("Authorization")

	url := "https://auth-server:8080/api/users/currentUser"

	timeout := 2000 * time.Second
	ctxx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := s.HTTPSperformAuthorizationRequestWithCircuitBreakerRsv(ctx, token, url)
	if err != nil {

		if errors.Is(err, gobreaker.ErrOpenState) {
			// Circuit is open
			s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("Circuit is open. Authorization service is not available")
			span.SetStatus(codes.Error, "Circuit is open. Authorization service is not available.")
			error2.ReturnJSONError(rw, "Authorization service is not available.", http.StatusBadRequest)
			return
		}

		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("Authorization service not available")
			span.SetStatus(codes.Error, "Authorization service not available.")
			errorMsg := map[string]string{"error": "Authorization service not available."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("Authorization service not available")
		span.SetStatus(codes.Error, "Authorization service not available.")
		errorMsg := map[string]string{"error": "Authorization service not available."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode
	if statusCode != 200 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("Unauthorized")
		span.SetStatus(codes.Error, "Unauthorized.")
		errorMsg := map[string]string{"error": "Unauthorized."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusUnauthorized)
		return
	}

	decoder := json.NewDecoder(resp.Body)

	var response struct {
		LoggedInUser struct {
			ID       string        `json:"id"`
			UserRole data.UserRole `json:"userRole"`
		} `json:"user"`
		Message string `json:"message"`
	}
	if err := decoder.Decode(&response); err != nil {
		if strings.Contains(err.Error(), "cannot parse") {
			s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("Invalid date format in the response")
			span.SetStatus(codes.Error, "Invalid date format in the response")
			error2.ReturnJSONError(rw, "Invalid date format in the response", http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("Error decoding JSON response")
		span.SetStatus(codes.Error, "Error decoding JSON response"+err.Error())
		error2.ReturnJSONError(rw, fmt.Sprintf("Error decoding JSON response: %v", err), http.StatusBadRequest)
		return
	}
	guestID := response.LoggedInUser.ID
	userRole := response.LoggedInUser.UserRole

	if userRole != data.Guest {
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("Permission denied. Only guests can get reservation")
		span.SetStatus(codes.Error, "Permission denied. Only guests can get reservations")
		errorMsg := map[string]string{"error": "Permission denied. Only guests can get reservations"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusForbidden)
		return
	}

	reservations, err := s.Repo.GetAllReservations(ctx, guestID)
	if err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("Error getting reservations")
		span.SetStatus(codes.Error, "Error getting reservations: "+err.Error())
		s.logger.Print("Error getting reservations: ", err)
		error2.ReturnJSONError(rw, err, http.StatusBadRequest)
		return
	}
	if len(reservations) == 0 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("No reservations found")
		span.SetStatus(codes.Error, "No reservations found")
		rw.WriteHeader(http.StatusNotFound)
		return
	}
	s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Info("Ger all successfully")
	span.SetStatus(codes.Ok, "Get all successful")
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	if err := reservations.ToJSON(rw); err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetAllReservations"}).Error("Error encoding JSON")
		span.SetStatus(codes.Error, "Error encoding JSON:"+err.Error())
		s.logger.Println("Error encoding JSON:", err)
	}
}

func (s *ReservationsHandler) CancelReservation(rw http.ResponseWriter, h *http.Request) {
	ctx, span := s.Tracer.Start(h.Context(), "ReservationsHandler.CancelReservation")
	defer span.End()
	s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Info("ReservationHandler.CancelReservation")


	token := h.Header.Get("Authorization")
	url := "https://auth-server:8080/api/users/currentUser"

	timeout := 2000 * time.Second
	ctxx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := s.HTTPSperformAuthorizationRequestWithCircuitBreakerRsv(ctx, token, url)
	if err != nil {

		if errors.Is(err, gobreaker.ErrOpenState) {
			// Circuit is open
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Circuit is open. Authorization service is not available.")

			span.SetStatus(codes.Error, "Circuit is open. Authorization service is not available.")
			error2.ReturnJSONError(rw, "Authorization service is not available.", http.StatusBadRequest)
			return
		}

		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Authorization service not available. Try again later.")

			span.SetStatus(codes.Error, "Authorization service not available. Try again later")
			errorMsg := map[string]string{"error": "Authorization service not available. Try again later"}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Authorization service not available. Try again later.")

		span.SetStatus(codes.Error, "Authorization service not available. Try again later")
		errorMsg := map[string]string{"error": "Authorization service not available. Try again later"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode
	if statusCode != 200 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Unauthorized")

		span.SetStatus(codes.Error, "Unauthorized.")
		errorMsg := map[string]string{"error": "Unauthorized."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusUnauthorized)
		return
	}

	decoder := json.NewDecoder(resp.Body)

	var response struct {
		LoggedInUser struct {
			ID       string        `json:"id"`
			UserRole data.UserRole `json:"userRole"`
			Username string        `json:"username"`
		} `json:"user"`
		Message string `json:"message"`
	}
	if err := decoder.Decode(&response); err != nil {
		if strings.Contains(err.Error(), "cannot parse") {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Invalid date format in the response. ")

			span.SetStatus(codes.Error, "Invalid date format in the response.")
			error2.ReturnJSONError(rw, "Invalid date format in the response", http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Error decoding JSON response. ")

		span.SetStatus(codes.Error, "Error decoding JSON response:"+err.Error())
		error2.ReturnJSONError(rw, fmt.Sprintf("Error decoding JSON response: %v", err), http.StatusBadRequest)
		return
	}
	guestID := response.LoggedInUser.ID
	userRole := response.LoggedInUser.UserRole

	if userRole != data.Guest {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Permission denied. Only guests can delete reservations. ")

		span.SetStatus(codes.Error, "Permission denied. Only guests can delete reservations")
		errorMsg := map[string]string{"error": "Permission denied. Only guests can delete reservations"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusForbidden)
		return
	}
	vars := mux.Vars(h)
	reservationIDString := vars["id"]

	accommodationID, err := s.Repo.GetReservationAccommodationID(ctx, reservationIDString, guestID)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
		errorMsg := map[string]string{"error": err.Error()}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	checkInDate, err := s.Repo.GetReservationCheckInDate(ctx, reservationIDString, guestID)
	if err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Errorf("Error getting check-in date: %v", err.Error())

		span.SetStatus(codes.Error, "Error getting check-in date: "+err.Error())
		s.logger.Println("Error getting check-in date:", err)
		errorMsg := map[string]string{"error": "Error getting check-in date"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusInternalServerError)
		return
	}
	checkOutDate, err := s.Repo.GetReservationCheckOutDate(ctx, reservationIDString, guestID)
	if err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Errorf("Error getting check-out date")

		span.SetStatus(codes.Error, "Error getting check-ou date: "+err.Error())
		s.logger.Println("Error getting check-out date:", err)
		errorMsg := map[string]string{"error": "Error getting check-out date"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusInternalServerError)
		return
	}
	er := s.SendToDelete(rw, accommodationID, guestID, ctx)
	if er != nil {
		//span.SetStatus(codes.Error, er.Error())
		return
	}
	if err := s.Repo.CancelReservationByID(ctx, guestID, reservationIDString, checkInDate); err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Error canceling reservation.")
		span.SetStatus(codes.Error, "Error canceling reservation:"+err.Error())
		s.logger.Println("Error canceling reservation:", err)
		if strings.Contains(err.Error(), "Cannot cancel reservation, check-in date has already started") {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Cannot cancel reservation, check-in date has already started")

			span.SetStatus(codes.Error, "Cannot cancel reservation, check-in date has already started")
			rw.WriteHeader(http.StatusBadRequest)
			rw.Write([]byte(`{"error":"Cannot cancel reservation, check-in date has already started"}`))
			return
		}
		span.SetStatus(codes.Error, "error"+err.Error())
		errorMsg := map[string]string{"error": err.Error()}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	err1 := s.serviceAv.MakeAccommodationAvailable(accommodationID, checkInDate, checkOutDate, ctx)
	if err1 != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Error making accommodation available")

		span.SetStatus(codes.Error, "Error making accommodation available.")
		errorMsg := map[string]string{"error": "Error making accommodation available."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	urlAccommodationCheck := "https://acc-server:8083/api/accommodations/get/" + accommodationID

	resp, err = s.HTTPSperformAuthorizationRequestWithCircuitBreakerRsv(ctx, token, urlAccommodationCheck)
	if err != nil {

		if errors.Is(err, gobreaker.ErrOpenState) {
			// Circuit is open
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Circuit is open. Authorization service is not available")

			span.SetStatus(codes.Error, "Circuit is open. Authorization service is not available.")
			error2.ReturnJSONError(rw, "Authorization service is not available.", http.StatusBadRequest)
			return
		}

		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Accommodation service is not available. ")

			span.SetStatus(codes.Error, "Accommodation service is not available.")
			errorMsg := map[string]string{"error": "Accommodation service is not available."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Accommodation service is not available. ")

		span.SetStatus(codes.Error, "Accommodation service is not available.")
		errorMsg := map[string]string{"error": "Accommodation service is not available."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	defer resp.Body.Close()

	statusCodeAccommodation := resp.StatusCode
	fmt.Println(statusCodeAccommodation)
	if statusCodeAccommodation != 200 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Accommodation with that is does not exist. ")

		span.SetStatus(codes.Error, "Accommodation with that id does not exist.")
		errorMsg := map[string]string{"error": "Accommodation with that id does not exist."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	var responseAccommodation struct {
		AccommodationName      string `json:"accommodation_name"`
		AccommodationLocation  string `json:"accommodation_location"`
		AccommodationHostId    string `json:"host_id"`
		AccommodationMinGuests int    `json:"accommodation_min_guests"`
		AccommodationMaxGuests int    `json:"accommodation_max_guests"`
	}
	decoder = json.NewDecoder(resp.Body)

	// Decode the JSON response into the struct
	if err := decoder.Decode(&responseAccommodation); err != nil {
		if strings.Contains(err.Error(), "cannot parse") {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Invalid date format.")

			span.SetStatus(codes.Error, "Invalid date format.")
			errorMsg := map[string]string{"error": "Invalid date format."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Error decoding JSON response. ")

		span.SetStatus(codes.Error, "Error decoding JSON response"+err.Error())
		error2.ReturnJSONError(rw, fmt.Sprintf("Error decoding JSON response: %v", err), http.StatusBadRequest)
		return
	}

	urlHostCheck := "https://auth-server:8080/api/users/getById/" + responseAccommodation.AccommodationHostId

	resp, err = s.HTTPSperformAuthorizationRequestWithCircuitBreakerRsv(ctx, token, urlHostCheck)
	if err != nil {
		if errors.Is(err, gobreaker.ErrOpenState) {
			// Circuit is open
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Circuit is open. Authorisation service is not available. ")

			span.SetStatus(codes.Error, "Circuit is open. Authorization service is not available.")
			error2.ReturnJSONError(rw, "Authorization service is not available.", http.StatusBadRequest)
			return
		}

		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Authorization service is not available. ")

			span.SetStatus(codes.Error, "Authorization service is not available.")
			errorMsg := map[string]string{"error": "Authorization service is not available."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Authorization service is not available. ")

		span.SetStatus(codes.Error, "Authorization service is not available.")
		errorMsg := map[string]string{"error": "Authorization service is not available."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	defer resp.Body.Close()

	statusCodeHostCheck := resp.StatusCode
	fmt.Println(statusCodeHostCheck)
	if statusCodeHostCheck != 200 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Host with that id does not exist. ")

		span.SetStatus(codes.Error, "Host with that id does not exist.")
		errorMsg := map[string]string{"error": "Host with that id does not exist."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	decoder = json.NewDecoder(resp.Body)

	// Define a struct to represent the JSON structure
	var responseHost struct {
		Host struct {
			Email    string `json:"email"`
			Username string `json:"username"`
		} `json:"user"`
	}

	// Decode the JSON response into the struct
	if err := decoder.Decode(&responseHost); err != nil {
		if strings.Contains(err.Error(), "cannot parse") {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Invalid date format. ")

			span.SetStatus(codes.Error, "Invalid date format.")
			errorMsg := map[string]string{"error": "Invalid date format."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		fmt.Println("User has errored")
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Error decoding JSON response. ")

		span.SetStatus(codes.Error, "Error decoding JSON response"+err.Error())
		error2.ReturnJSONError(rw, fmt.Sprintf("Error decoding JSON response: %v", err), http.StatusBadRequest)
		return
	}

	notificationPayload := map[string]interface{}{
		"host_id":    responseAccommodation.AccommodationHostId,
		"host_email": responseHost.Host.Email,
		"notification_text": "Dear " + responseHost.Host.Username + "\n your reservation has been cancelled! Your " +
			responseAccommodation.AccommodationName + " has been cancelled by " + response.LoggedInUser.Username + ".",
	}

	notificationPayloadJSON, err := json.Marshal(notificationPayload)
	if err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Error creating notification payload. ")

		span.SetStatus(codes.Error, "Error creating notification payload.")
		errorMsg := map[string]string{"error": "Error creating notification payload."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	notificationURL := "https://notifications-server:8089/api/notifications/create"

	resp, err = s.HTTPSperformAuthorizationRequestWithContextAndBodyAccCircuitBreaker(ctx, token, notificationURL, "POST", notificationPayloadJSON)
	if err != nil {

		if errors.Is(err, gobreaker.ErrOpenState) {
			// Circuit is open
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Circuit is open. Authorization service is not available.")

			span.SetStatus(codes.Error, "Circuit is open. Authorization service is not available.")
			error2.ReturnJSONError(rw, "Authorization service is not available.", http.StatusBadRequest)
			return
		}

		if ctx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Notification service is not available")

			span.SetStatus(codes.Error, "Notification service is not available.")
			errorMsg := map[string]string{"error": "Error creating notification request."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Notification service is not available. ")

		span.SetStatus(codes.Error, "Notification service is not available.")
		errorMsg := map[string]string{"error": "Notification service is not available."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 201 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Error creating notification. ")

		span.SetStatus(codes.Error, "Error creating notification.")
		errorMsg := map[string]string{"error": "Error creating notification."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Info("Canceled reservation")

	span.SetStatus(codes.Ok, "Canceled reservation")

	event := &data.AccommodationEvent{
		Event:           "Cancelled",
		GuestID:         response.LoggedInUser.ID,
		AccommodationID: accommodationID,
	}

	err = s.EventRepo.InsertEvent(ctx, event)
	if err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CancelReservation"}).Error("Error storing reservation event. ")

		span.SetStatus(codes.Error, "Error storing reservation event")
		errorMsg := map[string]string{"error": "Error storing reservation event"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusInternalServerError)
		return
	}

	rw.WriteHeader(http.StatusNoContent)
}

func (s *ReservationsHandler) SendToDelete(rw http.ResponseWriter, accommodationId string, guestId string, ctx context.Context) error {
	ctx, span := s.Tracer.Start(ctx, "ReservationService.SendToDelete")
	defer span.End()
	s.logg.WithFields(logrus.Fields{"path": "reservation/sendToDelete"}).Info("ReservationService.SendToDelete")


	url := "https://rating-server:8087/api/rating/deleteReservation"

	timeout := 2000 * time.Second // Adjust the timeout duration as needed
	ctxx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := s.HTTPSperformAuthorizationRequestWithContexttt(ctx, accommodationId, guestId, url)
	if err != nil {
		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/sendToDelete"}).Error("Rating service not available")

			span.SetStatus(codes.Error, "Rating service not available..")
			errorMsg := map[string]string{"error": "Rating service not available.."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return nil
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/sendToDelete"}).Error("Rating service not available")

		span.SetStatus(codes.Error, "Rating service not available..")
		errorMsg := map[string]string{"error": "Rating service not available.."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return nil
	}

	defer resp.Body.Close()

	return nil
}
func (s *ReservationsHandler) HTTPSperformAuthorizationRequestWithContexttt(ctx context.Context, accommodationId string, guestId string, url string) (*http.Response, error) {
	requestData := map[string]string{
		"accommodationId": accommodationId,
		"guestId":         guestId,
	}

	reqBody, err := json.Marshal(requestData)
	if err != nil {
		return nil, fmt.Errorf("error marshaling JSON: %v", err)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	req, err := http.NewRequest("DELETE", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	// Perform the request with the provided context
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *ReservationsHandler) GetReservationByAccommodationIdAndCheckOut(rw http.ResponseWriter, h *http.Request) {
	ctx, span := s.Tracer.Start(h.Context(), "ReservationsHandler.GetReservationByAccommodationIdAndCheckOut")
	defer span.End()
	s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Info("ReservationHandler.GetReservatuonByAccommodationIdAndCheckOut")


	token := h.Header.Get("Authorization")
	url := "https://auth-server:8080/api/users/currentUser"

	timeout := 2000 * time.Second
	ctxx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := s.HTTPSperformAuthorizationRequestWithCircuitBreakerRsv(ctx, token, url)
	if err != nil {

		if errors.Is(err, gobreaker.ErrOpenState) {
			// Circuit is open
			s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Error("Circuit is open. Authorization service is not available.")

			span.SetStatus(codes.Error, "Circuit is open. Authorization service is not available.")
			error2.ReturnJSONError(rw, "Authorization service is not available.", http.StatusBadRequest)
			return
		}

		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Error("Authorization servie is not available.")

			span.SetStatus(codes.Error, "Authorization service not available.")
			errorMsg := map[string]string{"error": "Authorization service not available."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Error("Authorization servie is not available.")

		span.SetStatus(codes.Error, "Authorization service not available.")
		errorMsg := map[string]string{"error": "Authorization service not available."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode
	if statusCode != 200 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Error("Unauthorized")

		span.SetStatus(codes.Error, "Unauthorized")
		errorMsg := map[string]string{"error": "Unauthorized."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusUnauthorized)
		return
	}

	decoder := json.NewDecoder(resp.Body)

	var response struct {
		LoggedInUser struct {
			ID       string        `json:"id"`
			UserRole data.UserRole `json:"userRole"`
		} `json:"user"`
		Message string `json:"message"`
	}

	if err := decoder.Decode(&response); err != nil {
		if strings.Contains(err.Error(), "cannot parse") {
			s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Error("Ivalid date format in the response. ")

			span.SetStatus(codes.Error, "Invalid date format in the response")
			error2.ReturnJSONError(rw, "Invalid date format in the response", http.StatusBadRequest)
			return
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Error("Error decoding JSON response. ")

		span.SetStatus(codes.Error, "Error decoding JSON response:"+err.Error())
		error2.ReturnJSONError(rw, fmt.Sprintf("Error decoding JSON response: %v", err), http.StatusBadRequest)
		return
	}
	vars := mux.Vars(h)
	accIDString := vars["accId"]
	userRole := response.LoggedInUser.UserRole

	if userRole != data.Host {
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Error("Permission denied. Only hosts can see reservations for their guests")

		span.SetStatus(codes.Error, "Permission denied. Only hosts can see reservations for their guests")
		errorMsg := map[string]string{"error": "Permission denied. Only hosts can see reservations for their guests"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusForbidden)
		return
	}

	counter := s.Repo.GetReservationByAccommodationIDAndCheckOut(ctx, accIDString)
	if counter == -1 {
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Error("Error fetching reservations")

		span.SetStatus(codes.Error, "Error fetching reservations")
		s.logger.Println("Error fetching reservations:", counter)
		error2.ReturnJSONError(rw, counter, http.StatusBadRequest)
		return
	}

	var Number struct {
		NumberRsv int `json:"number"`
	}

	Number.NumberRsv = counter

	responseJSON, err := json.Marshal(Number)
	if err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Error("Error creating JSON response")

		span.SetStatus(codes.Error, "Error creating JSON response:")
		error2.ReturnJSONError(rw, "Error creating JSON response", http.StatusInternalServerError)
		return
	}
	s.logg.WithFields(logrus.Fields{"path": "reservation/GetReservationByAccommodationIdAndCheckOut"}).Info("Get reservation by accommodation ID and check out fate successfully")

	span.SetStatus(codes.Ok, "Get reservation by accommodation ID and check out date successful")
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(responseJSON)

}

func (s *ReservationsHandler) CheckAvailability(rw http.ResponseWriter, h *http.Request) {
	ctx, span := s.Tracer.Start(h.Context(), "ReservationsHandler.CheckAvailability")
	defer span.End()
	s.logg.WithFields(logrus.Fields{"path": "reservation/CheckAvailability"}).Info("ReservationHandler.CheckAvailability")


	// token := h.Header.Get("Authorization")
	// url := "https://auth-server:8080/api/users/currentUser"

	// timeout := 2000 * time.Second
	// ctxx, cancel := context.WithTimeout(context.Background(), timeout)
	// defer cancel()

	// resp, err := s.HTTPSperformAuthorizationRequestWithContext(ctx, token, url)
	// if err != nil {
	// 	if ctxx.Err() == context.DeadlineExceeded {
	// 		span.SetStatus(codes.Error, "Authorization service not available.")
	// 		errorMsg := map[string]string{"error": "Authorization service not available."}
	// 		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
	// 		return
	// 	}
	// 	span.SetStatus(codes.Error, "Authorization service not available.")
	// 	errorMsg := map[string]string{"error": "Authorization service not available."}
	// 	error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
	// 	return
	// }
	// defer resp.Body.Close()

	// statusCode := resp.StatusCode
	// if statusCode != 200 {
	// 	span.SetStatus(codes.Error, "Unauthorized")
	// 	errorMsg := map[string]string{"error": "Unauthorized."}
	// 	error2.ReturnJSONError(rw, errorMsg, http.StatusUnauthorized)
	// 	return
	// }

	// decoder := json.NewDecoder(resp.Body)

	// var response struct {
	// 	LoggedInUser struct {
	// 		ID       string        `json:"id"`
	// 		UserRole data.UserRole `json:"userRole"`
	// 	} `json:"user"`
	// 	Message string `json:"message"`
	// }

	// if err := decoder.Decode(&response); err != nil {
	// 	if strings.Contains(err.Error(), "cannot parse") {
	// 		span.SetStatus(codes.Error, "Invalid date format in the response")
	// 		error2.ReturnJSONError(rw, "Invalid date format in the response", http.StatusBadRequest)
	// 		return
	// 	}

	// 	span.SetStatus(codes.Error, "Error decoding JSON response:"+err.Error())
	// 	error2.ReturnJSONError(rw, fmt.Sprintf("Error decoding JSON response: %v", err), http.StatusBadRequest)
	// 	return
	// }

	// userRole := response.LoggedInUser.UserRole

	// if userRole != data.Guest {
	// 	span.SetStatus(codes.Error, "Permission denied. Only guests can check availability of accommodations.")
	// 	errorMsg := map[string]string{"error": "Permission denied. Only guests can check availability of accommodations."}
	// 	error2.ReturnJSONError(rw, errorMsg, http.StatusForbidden)
	// 	return
	// }

	vars := mux.Vars(h)
	accIDString, ok := vars["accId"]
	if !ok {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CheckAvailability"}).Error("Missing accommodationId in the URL.")

		span.SetStatus(codes.Error, "Missing accommodationId in the URL")
		errorMsg := map[string]string{"error": "Missing accommodationId in the URL"}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	var checkAvailabilityRequest data.CheckAvailability
	if err := json.NewDecoder(h.Body).Decode(&checkAvailabilityRequest); err != nil {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CheckAvailability"}).Error("Invalid request body. Check the request format. ")

		span.SetStatus(codes.Error, "Invalid request body. Check the request format.")
		errorMsg := map[string]string{"error": "Invalid request body. Check the request format."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	accIDConvert, err := primitive.ObjectIDFromHex(accIDString)
	checkAvailabilityRequest.CheckInDate = time.Date(
		checkAvailabilityRequest.CheckInDate.Year(),
		checkAvailabilityRequest.CheckInDate.Month(),
		checkAvailabilityRequest.CheckInDate.Day(),
		0, 0, 0, 0,
		checkAvailabilityRequest.CheckInDate.Location())

	checkAvailabilityRequest.CheckOutDate = time.Date(
		checkAvailabilityRequest.CheckOutDate.Year(),
		checkAvailabilityRequest.CheckOutDate.Month(),
		checkAvailabilityRequest.CheckOutDate.Day(),
		0, 0, 0, 0,
		checkAvailabilityRequest.CheckOutDate.Location())

	isAvailable, err := s.serviceAv.IsAvailable(
		accIDConvert,
		checkAvailabilityRequest.CheckInDate,
		checkAvailabilityRequest.CheckOutDate,
		ctx,
	)

	if err != nil {
		fmt.Println(err)
		s.logg.WithFields(logrus.Fields{"path": "reservation/CheckAvailability"}).Error("Accmmodation is not available for the specified dates, try another ones. ")

		span.SetStatus(codes.Error, "Accommodation is not available for the specified dates, try another ones.")
		errorMsg := map[string]string{"error": "Accommodation is not available for the specified dates, try another ones."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}

	if !isAvailable {
		s.logg.WithFields(logrus.Fields{"path": "reservation/CheckAvailability"}).Error("Accommodation is booked for the specified dates, trry another ones. ")

		span.SetStatus(codes.Error, "Accommodation is booked for the specified dates, try another ones.")
		errorMsg := map[string]string{"error": "Accommodation is booked for the specified dates, try another ones."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return
	}
	s.logg.WithFields(logrus.Fields{"path": "reservation/CheckAvailability"}).Info("Accommodation is available for the specified. ")

	successMsg := map[string]string{"message": "Accommodation is available for the specified dates."}
	span.SetStatus(codes.Ok, "Accommodation is available for the specified dates.")
	responseJSON, err := json.Marshal(successMsg)
	if err != nil {
		span.SetStatus(codes.Error, "Error creating JSON response")
		error2.ReturnJSONError(rw, "Error creating JSON response", http.StatusInternalServerError)
		return
	}
	s.logg.WithFields(logrus.Fields{"path": "reservation/CheckAvailability"}).Info("Check availability successful")

	span.SetStatus(codes.Ok, "Check availability successful")
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(responseJSON)
}

// GetTotalReservations()
func (s *ReservationsHandler) GetTotalReservations(rw http.ResponseWriter, h *http.Request) {

	vars := mux.Vars(h)
	hostId, ok := vars["hostId"]
	if !ok {
		error2.ReturnJSONError(rw, "Missing hostId in the URL", http.StatusBadRequest)
		return
	}
	total, err := s.Repo.GetTotalReservations(hostId)
	if err != nil {
		error2.ReturnJSONError(rw, err, http.StatusBadRequest)
		return
	}

	if total == -1 {
		error2.ReturnJSONError(rw, "Error getting total reservations", http.StatusBadRequest)
		return

	}

	responseJSON, err := json.Marshal(total)
	if err != nil {
		error2.ReturnJSONError(rw, "Error creating JSON response", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(responseJSON)
}

// .GetPercentageOfCancelledReservations
func (s *ReservationsHandler) GetPercentageOfCancelledReservations(rw http.ResponseWriter, h *http.Request) {

	vars := mux.Vars(h)
	hostId, ok := vars["hostId"]
	if !ok {
		error2.ReturnJSONError(rw, "Missing hostId in the URL", http.StatusBadRequest)
		return
	}

	total, err := s.Repo.GetPercentageOfCancelledReservations(hostId)
	if err != nil {
		error2.ReturnJSONError(rw, err, http.StatusBadRequest)
		return
	}

	if total == -1 {
		error2.ReturnJSONError(rw, "Error getting percentage of cancelled reservations", http.StatusBadRequest)
		return
	}

	responseJSON, err := json.Marshal(total)
	if err != nil {
		error2.ReturnJSONError(rw, "Error creating JSON response", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(responseJSON)

}

// GetTotalDurationOfReservations
func (s *ReservationsHandler) GetTotalDurationOfReservations(rw http.ResponseWriter, h *http.Request) {

	vars := mux.Vars(h)
	hostId, ok := vars["hostId"]
	if !ok {
		error2.ReturnJSONError(rw, "Missing hostId in the URL", http.StatusBadRequest)
		return
	}

	total, err := s.Repo.GetTotalDurationOfReservations(hostId)
	if err != nil {
		error2.ReturnJSONError(rw, err, http.StatusBadRequest)
		return
	}

	if total == -1 {
		error2.ReturnJSONError(rw, "Error getting total duration of reservations", http.StatusBadRequest)
		return
	}

	responseJSON, err := json.Marshal(total)
	if err != nil {
		error2.ReturnJSONError(rw, "Error creating JSON response", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	rw.Write(responseJSON)

}

func (s *ReservationsHandler) MiddlewareContentTypeSet(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, h *http.Request) {
		if s.logger != nil {
			s.logger.Println("Method [", h.Method, "] - Hit path :", h.URL.Path)
		}

		rw.Header().Add("Content-Type", "application/json")

		next.ServeHTTP(rw, h)
	})
}

func (s *ReservationsHandler) MiddlewareReservationForGuestDeserialization(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, h *http.Request) {
		patient := &data.ReservationByGuestCreate{}
		err := patient.FromJSON(h.Body)
		if err != nil {
			http.Error(rw, "Unable to decode json", http.StatusBadRequest)
			s.logger.Fatal(err)
			return
		}
		ctx := context.WithValue(h.Context(), KeyProduct{}, patient)
		h = h.WithContext(ctx)
		next.ServeHTTP(rw, h)
	})
}

func (s *ReservationsHandler) HTTPSperformAuthorizationRequestWithCircuitBreakerRsv(ctx context.Context, token string, url string) (*http.Response, error) {
	maxRetries := 3
	retryOperation := func() (interface{}, error) {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", token)
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

		client := &http.Client{Transport: tr}
		resp, err := client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		fmt.Println(resp)
		fmt.Println("resp here")
		return resp, nil // Return the response as the first value
	}

	//retryOpErr := retryOperationWithExponentialBackoff(ctx,3, retryOperation)
	//if (r)
	// Use an anonymous function to convert the result to the expected type
	result, err := s.CircuitBreaker.Execute(func() (interface{}, error) {
		return retryOperationWithExponentialBackoff(ctx, maxRetries, retryOperation)
	})
	if err != nil {
		return nil, err
	}
	fmt.Println("result here")
	fmt.Println(result)
	resp, ok := result.(*http.Response)
	if !ok {
		fmt.Println(ok)
		fmt.Println("OK")
		return nil, errors.New("unexpected response type from Circuit Breaker")
	}
	return resp, nil
}

func (s *ReservationsHandler) HTTPSperformAuthorizationRequestWithContextAndBodyAccCircuitBreaker(
	ctx context.Context, token string, url string, method string, requestBody []byte,
) (*http.Response, error) {
	_, span := s.Tracer.Start(ctx, "ReservationsHandler.HTTPSperformAuthorizationRequestWithContextAndBodyAccCircuitBreaker")
	maxRetries := 3

	// Define a retry operation function
	retryOperation := func() (interface{}, error) {
		// Use the Circuit Breaker to execute the request function
		result, err := s.CircuitBreaker.Execute(func() (interface{}, error) {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

			req, err := http.NewRequest(method, url, bytes.NewBuffer(requestBody))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", token)
			otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
			client := &http.Client{Transport: tr}
			resp, err := client.Do(req.WithContext(ctx))
			if err != nil {
				return nil, err
			}

			return resp, nil
		})

		// If there is an error, propagate it
		if err != nil {
			return nil, err
		}

		// Check the type of the result
		resp, ok := result.(*http.Response)
		if !ok {
			return nil, errors.New("unexpected response type from Circuit Breaker")
		}

		return resp, nil
	}

	// Use the retry mechanism
	result, err := s.CircuitBreaker.Execute(func() (interface{}, error) {
		return retryOperationWithExponentialBackoff(ctx, maxRetries, retryOperation)
	})
	if err != nil {
		return nil, err
	}

	resp, ok := result.(*http.Response)
	if !ok {
		err := errors.New("unexpected response type from retry operation")
		s.logg.Error("Unexpected response type from retry operation. ")
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}

	return resp, nil

}

func (s *ReservationsHandler) performAuthorizationRequestWithContext(ctx context.Context, token string, url string) (*http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	// Perform the request with the provided context
	client := &http.Client{}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func (s *ReservationsHandler) SendToRatingService(reservation *data.ReservationByGuest, ctx context.Context) error {
	ctx, span := s.Tracer.Start(ctx, "ReservationService.SendToRating")
	defer span.End()

	var rw http.ResponseWriter
	url := "https://rating-server:8087/api/rating/createReservation"

	timeout := 2000 * time.Second // Adjust the timeout duration as needed
	ctxx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := s.HTTPSperformAuthorizationRequest(ctx, reservation, url)
	if err != nil {
		if ctxx.Err() == context.DeadlineExceeded {
			s.logg.WithFields(logrus.Fields{"path": "reservation/SendToRatingService"}).Error("Rating service not available..")

			span.SetStatus(codes.Error, "Rating service not available..")
			errorMsg := map[string]string{"error": "Rating service not available.."}
			error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
			return nil
		}
		s.logg.WithFields(logrus.Fields{"path": "reservation/SendToRatingService"}).Error("Rating service not available. ")

		span.SetStatus(codes.Error, "Rating service not available..")
		errorMsg := map[string]string{"error": "Rating service not available.."}
		error2.ReturnJSONError(rw, errorMsg, http.StatusBadRequest)
		return nil
	}

	defer resp.Body.Close()

	return nil
}

func (s *ReservationsHandler) HTTPSperformAuthorizationRequest(ctx context.Context, reservation *data.ReservationByGuest, url string) (*http.Response, error) {
	reqBody, err := json.Marshal(reservation)
	if err != nil {
		return nil, fmt.Errorf("error marshaling reservation JSON: %v", err)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
	// Perform the request with the provided context
	client := &http.Client{Transport: tr}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}

	return resp, nil
}

func ExtractTraceInfoMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
