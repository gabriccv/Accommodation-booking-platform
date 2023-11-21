package services

import (
	"auth-service/domain"
	"github.com/gin-gonic/gin"
)

type AuthService interface {
	Login(*domain.LoginInput) (*domain.User, error)
	Registration(*domain.User) (*domain.UserResponse, error)
	ResendVerificationEmail(ctx *gin.Context)
	SendVerificationEmail(credentials *domain.Credentials) error
	SendPasswordResetToken(credentials *domain.Credentials) error
}
