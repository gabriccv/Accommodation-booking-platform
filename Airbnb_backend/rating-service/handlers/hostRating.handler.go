package handlers

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"net/http"
	"rating-service/domain"
	"rating-service/services"
	"time"
)

type HostRatingHandler struct {
	hostRatingService services.HostRatingService
	DB                *mongo.Collection
}

func NewHostRatingHandler(hostRatingService services.HostRatingService, db *mongo.Collection) HostRatingHandler {
	return HostRatingHandler{hostRatingService, db}
}

func (s *HostRatingHandler) RateHost(c *gin.Context) {
	hostID := c.Param("hostId")

	token := c.GetHeader("Authorization")
	currentUser, err := s.getCurrentUserFromAuthService(token)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to obtain current user information"})
		return
	}

	hostUser, err := s.getUserByIDFromAuthService(hostID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to obtain reservation information"})
		return
	}

	if !s.canRateHost(token, hostID) {
		c.JSON(http.StatusBadRequest, gin.H{"message": "You cannot rate this host. You don't have reservations from him"})
		return
	}

	var requestBody struct {
		Rating int `json:"rating"`
	}

	if err := c.BindJSON(&requestBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON request"})
		return
	}
	currentDateTime := primitive.NewDateTimeFromTime(time.Now())
	id := primitive.NewObjectID()

	newRateHost := &domain.RateHost{
		ID:          id,
		Host:        hostUser,
		Guest:       currentUser,
		DateAndTime: currentDateTime,
		Rating:      domain.Rating(requestBody.Rating),
	}

	err = s.hostRatingService.SaveRating(newRateHost)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to save rating"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"message": "Rating successfully saved", "rating": newRateHost})
}

func (s *HostRatingHandler) canRateHost(token string, hostID string) bool {
	urlCheckReservations := "https://res-server:8082/api/reservations/getAll"

	timeout := 2000 * time.Second
	ctxRest, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	respRes, errRes := s.HTTPSPerformAuthorizationRequestWithContext(ctxRest, token, urlCheckReservations)
	if errRes != nil {
		fmt.Println(errRes)
		return false
	}

	defer respRes.Body.Close()

	if respRes.StatusCode != http.StatusOK {
		fmt.Println("Failed to fetch user reservations. Status Code:", respRes.StatusCode)
		return false
	}

	decoder := json.NewDecoder(respRes.Body)
	var reservations []domain.ReservationByGuest
	if err := decoder.Decode(&reservations); err != nil {
		fmt.Println(err)
		return false
	}

	if len(reservations) == 0 {
		return false
	}

	for _, reservation := range reservations {
		fmt.Println("AccommodationHostID:", reservation.AccommodationHostId)
		fmt.Println("HostID:", hostID)
		if reservation.AccommodationHostId == hostID {
			return true
		}
	}

	return false
}

func (s *HostRatingHandler) DeleteRating(c *gin.Context) {
	hostID := c.Param("hostId")

	token := c.GetHeader("Authorization")
	currentUser, err := s.getCurrentUserFromAuthService(token)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to obtain current user information"})
		return
	}
	guestID := currentUser.ID.Hex()

	err = s.hostRatingService.DeleteRating(hostID, guestID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Rating successfully deleted"})
}

func (s *HostRatingHandler) GetAllRatings(c *gin.Context) {
	ratings, averageRating, err := s.hostRatingService.GetAllRatings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	response := gin.H{
		"ratings":       ratings,
		"averageRating": averageRating,
	}

	c.JSON(http.StatusOK, response)
}

func (s *HostRatingHandler) getUserByIDFromAuthService(userID string) (*domain.User, error) {
	url := "https://auth-server:8080/api/users/getById/" + userID

	timeout := 2000 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := s.HTTPSPerformAuthorizationRequestWithContext(ctx, "", url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("User not found")
	}

	var userResponse domain.UserResponse
	if err := json.NewDecoder(resp.Body).Decode(&userResponse); err != nil {
		return nil, err
	}

	user := domain.ConvertToDomainUser(userResponse)

	return &user, nil
}

func (s *HostRatingHandler) getCurrentUserFromAuthService(token string) (*domain.User, error) {
	url := "https://auth-server:8080/api/users/currentUser"

	timeout := 2000 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	resp, err := s.HTTPSPerformAuthorizationRequestWithContext(ctx, token, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("Unauthorized")
	}

	var userResponse domain.UserResponse
	if err := json.NewDecoder(resp.Body).Decode(&userResponse); err != nil {
		return nil, err
	}

	user := domain.ConvertToDomainUser(userResponse)

	return &user, nil
}

func (s *HostRatingHandler) HTTPSPerformAuthorizationRequestWithContext(ctx context.Context, token string, url string) (*http.Response, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", token)

	client := &http.Client{Transport: tr}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}

	return resp, nil
}
