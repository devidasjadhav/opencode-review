package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	gogithub "github.com/google/go-github/v67/github"
)

// Kind classifies how an error should be handled.
type Kind int

const (
	// KindPermanent indicates a logic or configuration error — fail fast.
	KindPermanent Kind = iota
	// KindTransient indicates a network or rate-limit error — safe to retry.
	KindTransient
	// KindWarning indicates a non-fatal error — log and continue.
	KindWarning
)

// AppError wraps an underlying error with a kind and human context.
type AppError struct {
	Kind    Kind
	Context string
	Err     error
}

func (e *AppError) Error() string {
	if e.Context != "" {
		return fmt.Sprintf("%s: %v", e.Context, e.Err)
	}
	return e.Err.Error()
}

func (e *AppError) Unwrap() error { return e.Err }

// Permanent wraps err as a permanent (non-retryable) error.
func Permanent(context string, err error) *AppError {
	return &AppError{Kind: KindPermanent, Context: context, Err: err}
}

// Transient wraps err as a transient (retryable) error.
func Transient(context string, err error) *AppError {
	return &AppError{Kind: KindTransient, Context: context, Err: err}
}

// Warning wraps err as a warning (non-fatal, continue).
func Warning(context string, err error) *AppError {
	return &AppError{Kind: KindWarning, Context: context, Err: err}
}

// IsTransient reports whether err is classified as transient.
func IsTransient(err error) bool {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae.Kind == KindTransient
	}
	return false
}

// IsWarning reports whether err is classified as a non-fatal warning.
func IsWarning(err error) bool {
	var ae *AppError
	if errors.As(err, &ae) {
		return ae.Kind == KindWarning
	}
	return false
}

// ClassifyOpencode classifies errors from the opencode SDK/server.
// Connection and availability errors are transient; everything else is permanent.
func ClassifyOpencode(err error) *AppError {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "unavailable") ||
		strings.Contains(msg, "eof") {
		return Transient("opencode", err)
	}
	return Permanent("opencode", err)
}

// ClassifyGitHub inspects a GitHub API error and returns an AppError with the
// appropriate Kind: rate-limit and server errors are transient; everything else
// is permanent.
func ClassifyGitHub(context string, err error) *AppError {
	if err == nil {
		return nil
	}
	var ghErr *gogithub.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case http.StatusTooManyRequests, http.StatusServiceUnavailable,
			http.StatusBadGateway, http.StatusGatewayTimeout:
			return Transient(context, err)
		}
	}
	return Permanent(context, err)
}
