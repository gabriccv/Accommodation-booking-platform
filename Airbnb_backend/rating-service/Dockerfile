FROM golang:alpine as build_container
WORKDIR /app
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY . .
RUN go build -o server

FROM alpine
COPY --from=build_container /app/server /usr/bin
COPY rating-service.crt /app/
COPY rating-service.key /app/
#RUN mkdir logs
#COPY ./logs/log.log /app
EXPOSE 8087
ENTRYPOINT ["server", "https"]