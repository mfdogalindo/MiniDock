package app

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/julieta/minidock/internal/store"
)

func TestSetupUnlockAndLockFlow(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	handler := application.Handler()

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/setup" {
		t.Fatalf("unexpected initial response: %d %s", response.Code, response.Header().Get("Location"))
	}

	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request = httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("setup status = %d", response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected session cookie, got %d", len(cookies))
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Estado del servidor") {
		t.Fatalf("dashboard was not available: %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/lock", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("lock status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/unlock" {
		t.Fatalf("dashboard should be locked: %d %s", response.Code, response.Header().Get("Location"))
	}
}
