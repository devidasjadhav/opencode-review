package apperr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	gogithub "github.com/google/go-github/v67/github"
)

func TestPermanentKindAndMessage(t *testing.T) {
	inner := errors.New("disk full")
	ae := Permanent("write", inner)
	if ae.Kind != KindPermanent {
		t.Errorf("Kind = %v, want KindPermanent", ae.Kind)
	}
	if ae.Error() != "write: disk full" {
		t.Errorf("Error() = %q, want %q", ae.Error(), "write: disk full")
	}
	if !errors.Is(ae, inner) {
		t.Error("errors.Is should unwrap to inner")
	}
}

func TestTransientKindAndMessage(t *testing.T) {
	inner := errors.New("timeout")
	ae := Transient("http", inner)
	if ae.Kind != KindTransient {
		t.Errorf("Kind = %v, want KindTransient", ae.Kind)
	}
	if ae.Error() != "http: timeout" {
		t.Errorf("Error() = %q", ae.Error())
	}
}

func TestWarningKindAndMessage(t *testing.T) {
	inner := errors.New("skipped")
	ae := Warning("", inner)
	if ae.Kind != KindWarning {
		t.Errorf("Kind = %v, want KindWarning", ae.Kind)
	}
	// No context — message is just the inner error.
	if ae.Error() != "skipped" {
		t.Errorf("Error() = %q, want %q", ae.Error(), "skipped")
	}
}

func TestIsTransient(t *testing.T) {
	if IsTransient(Transient("x", errors.New("e"))) != true {
		t.Error("IsTransient(Transient(...)) should be true")
	}
	if IsTransient(Permanent("x", errors.New("e"))) != false {
		t.Error("IsTransient(Permanent(...)) should be false")
	}
	if IsTransient(errors.New("plain")) != false {
		t.Error("IsTransient(plain error) should be false")
	}
	if IsTransient(nil) != false {
		t.Error("IsTransient(nil) should be false")
	}
}

func TestIsWarning(t *testing.T) {
	if IsWarning(Warning("x", errors.New("e"))) != true {
		t.Error("IsWarning(Warning(...)) should be true")
	}
	if IsWarning(Permanent("x", errors.New("e"))) != false {
		t.Error("IsWarning(Permanent(...)) should be false")
	}
}

func TestClassifyGitHubNilReturnsNil(t *testing.T) {
	if ClassifyGitHub("ctx", nil) != nil {
		t.Error("ClassifyGitHub(nil) should return nil")
	}
}

func TestClassifyGitHubRateLimitIsTransient(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusTooManyRequests}
	ghErr := &gogithub.ErrorResponse{Response: resp, Message: "rate limited"}
	ae := ClassifyGitHub("api", ghErr)
	if ae == nil {
		t.Fatal("expected non-nil AppError")
	}
	if ae.Kind != KindTransient {
		t.Errorf("429 should be Transient, got Kind=%v", ae.Kind)
	}
}

func TestClassifyGitHubServiceUnavailableIsTransient(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusServiceUnavailable}
	ghErr := &gogithub.ErrorResponse{Response: resp}
	ae := ClassifyGitHub("api", ghErr)
	if ae.Kind != KindTransient {
		t.Errorf("503 should be Transient, got Kind=%v", ae.Kind)
	}
}

func TestClassifyGitHubNotFoundIsPermanent(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusNotFound}
	ghErr := &gogithub.ErrorResponse{Response: resp}
	ae := ClassifyGitHub("api", ghErr)
	if ae.Kind != KindPermanent {
		t.Errorf("404 should be Permanent, got Kind=%v", ae.Kind)
	}
}

func TestClassifyGitHubUnauthorizedIsPermanent(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusUnauthorized}
	ghErr := &gogithub.ErrorResponse{Response: resp}
	ae := ClassifyGitHub("api", ghErr)
	if ae.Kind != KindPermanent {
		t.Errorf("401 should be Permanent, got Kind=%v", ae.Kind)
	}
}

func TestClassifyGitHubNonGitHubErrorIsPermanent(t *testing.T) {
	ae := ClassifyGitHub("net", fmt.Errorf("connection refused"))
	if ae.Kind != KindPermanent {
		t.Errorf("generic error should be Permanent, got Kind=%v", ae.Kind)
	}
}

func TestAppErrorUnwrap(t *testing.T) {
	sentinel := errors.New("sentinel")
	wrapped := Permanent("ctx", fmt.Errorf("outer: %w", sentinel))
	if !errors.Is(wrapped, sentinel) {
		t.Error("errors.Is should traverse wrapped chain")
	}
}
