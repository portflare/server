package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func newRegistrationTestServer(t *testing.T, registrationOpen bool) *Server {
	t.Helper()
	return &Server{
		cfg:    Config{StatePath: filepath.Join(t.TempDir(), "state.json")},
		logger: slog.Default(),
		state:  State{RegistrationOpen: registrationOpen, Users: map[string]*User{}, Apps: map[string]*App{}},
	}
}

func TestHandleRegisterCreatesUserAndAPIKey(t *testing.T) {
	srv := newRegistrationTestServer(t, true)
	body := bytes.NewBufferString(`{"user_name":"Alice Smith","email":"Alice@Example.Test"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/register", body)
	rr := httptest.NewRecorder()

	srv.handleRegister(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}

	var resp RegistrationResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.UserName != "alice-smith" || resp.PublicUserLabel != "alicesmith" || resp.Email != "alice@example.test" || !strings.HasPrefix(resp.APIKey, "pf_") {
		t.Fatalf("unexpected registration response: %#v", resp)
	}
	user := srv.state.Users["alice-smith"]
	if user == nil || user.APIKey != resp.APIKey {
		t.Fatalf("expected user to be persisted in state: %#v", srv.state.Users)
	}
}

func TestHandleRegisterRejectsClosedRegistration(t *testing.T) {
	srv := newRegistrationTestServer(t, false)
	req := httptest.NewRequest(http.MethodPost, "/api/register", bytes.NewBufferString(`{"user_name":"alice"}`))
	rr := httptest.NewRecorder()

	srv.handleRegister(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleRegisterRejectsDuplicateUserWithoutLeakingKey(t *testing.T) {
	srv := newRegistrationTestServer(t, true)
	srv.state.Users["alice"] = &User{UserName: "alice", PublicUserLabel: "alice", APIKey: "pf_secret"}
	req := httptest.NewRequest(http.MethodPost, "/api/register", bytes.NewBufferString(`{"user_name":"alice"}`))
	rr := httptest.NewRecorder()

	srv.handleRegister(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected conflict, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "pf_secret") {
		t.Fatalf("duplicate response leaked existing key: %s", rr.Body.String())
	}
}

func TestHandleRegisterRejectsPublicLabelCollision(t *testing.T) {
	srv := newRegistrationTestServer(t, true)
	srv.state.Users["alice-smith"] = &User{UserName: "alice-smith", PublicUserLabel: "alicesmith"}
	req := httptest.NewRequest(http.MethodPost, "/api/register", bytes.NewBufferString(`{"user_name":"alice_smith"}`))
	rr := httptest.NewRecorder()

	srv.handleRegister(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected conflict, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleRegisterRejectsInvalidRequests(t *testing.T) {
	srv := newRegistrationTestServer(t, true)
	for _, tc := range []struct {
		name string
		body string
		code int
	}{
		{name: "bad json", body: `{`, code: http.StatusBadRequest},
		{name: "missing user", body: `{"email":"alice@example.test"}`, code: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/register", bytes.NewBufferString(tc.body))
			rr := httptest.NewRecorder()
			srv.handleRegister(rr, req)
			if rr.Code != tc.code {
				t.Fatalf("expected %d, got %d body=%s", tc.code, rr.Code, rr.Body.String())
			}
		})
	}

	req := httptest.NewRequest(http.MethodGet, "/api/register", nil)
	rr := httptest.NewRecorder()
	srv.handleRegister(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected method not allowed, got %d body=%s", rr.Code, rr.Body.String())
	}
}
