package main

import (
  "net/http/httptest"
  "strings"
  "testing"
)

func TestIsValidAPIKey(t *testing.T) {
  if !isValidAPIKey("pf_123") {
    t.Fatal("expected pf_ prefix to be accepted")
  }
  if isValidAPIKey("abc123") {
    t.Fatal("expected missing pf_ prefix to be rejected")
  }
}

func TestMatchHosts(t *testing.T) {
  srv := &Server{cfg: Config{PublicBaseDomain: "reverse.example.test"}}

  srv.state = State{
    Users: map[string]*User{"alice-smith": {UserName: "alice-smith", PublicUserLabel: "alicesmith"}},
    Apps:  map[string]*App{"alice-smith/web": {UserName: "alice-smith", AppName: "web"}},
  }

  user, app, redirectHost, ok := srv.matchAppHost("web-alicesmith.reverse.example.test")
  if !ok || user != "alice-smith" || app != "web" || redirectHost != "" {
    t.Fatalf("unexpected app host match: ok=%v user=%q app=%q redirect=%q", ok, user, app, redirectHost)
  }

  label, ok := srv.matchUserHost("alicesmith.reverse.example.test")
  if !ok || label != "alicesmith" {
    t.Fatalf("unexpected user host match: ok=%v label=%q", ok, label)
  }

  if _, ok := srv.matchUserHost("admin.reverse.example.test"); ok {
    t.Fatal("admin subdomain should not be treated as a user host")
  }
}

func TestSlug(t *testing.T) {
  got := slug("My App_3000")
  if got != "my-app-3000" {
    t.Fatalf("unexpected slug: %q", got)
  }
}

func TestUserLabel(t *testing.T) {
  got := userLabel("Alice-Smith.dev")
  if got != "alicesmithdev" {
    t.Fatalf("unexpected user label: %q", got)
  }
}

func TestEnsureUserRejectsPublicLabelCollision(t *testing.T) {
  srv := &Server{state: State{RegistrationOpen: true, Users: map[string]*User{
    "alice-smith": {UserName: "alice-smith", PublicUserLabel: "alicesmith"},
  }}}

  _, err := srv.ensureUser(authIdentity{UserName: "alice_smith", PublicUserLabel: "alicesmith"})
  if err == nil || !strings.Contains(err.Error(), "choose a new slug") {
    t.Fatalf("expected collision error, got %v", err)
  }
}

func TestFindUserByPublicLabel(t *testing.T) {
  srv := &Server{state: State{Users: map[string]*User{
    "alice-smith": {UserName: "alice-smith", PublicUserLabel: "alicesmith", PublicUserAliases: []string{"asmith"}},
  }}}

  user, ok := srv.findUserByPublicLabel("alice-smith")
  if !ok || user.UserName != "alice-smith" {
    t.Fatalf("unexpected user lookup result: ok=%v user=%#v", ok, user)
  }

  user, found, canonical := srv.findUserByAnyPublicLabel("asmith")
  if !found || canonical || user.UserName != "alice-smith" {
    t.Fatalf("unexpected alias lookup result: found=%v canonical=%v user=%#v", found, canonical, user)
  }
}

func TestValidatePublicUserLabel(t *testing.T) {
  if _, err := validatePublicUserLabel("ab"); err == nil {
    t.Fatal("expected min length validation error")
  }
  if _, err := validatePublicUserLabel("admin"); err == nil {
    t.Fatal("expected reserved label validation error")
  }
  if got, err := validatePublicUserLabel("Alice-123"); err != nil || got != "alice123" {
    t.Fatalf("unexpected validation result: got=%q err=%v", got, err)
  }
}

func TestRequireIdentityDisableAuth(t *testing.T) {
  srv := &Server{cfg: Config{DisableAuth: true, LocalDevUser: "local-dev", LocalDevEmail: "local@example.test"}}
  rr := httptest.NewRecorder()
  req := httptest.NewRequest("GET", "/", nil)

  id, ok := srv.requireIdentity(rr, req)
  if !ok {
    t.Fatal("expected disable-auth identity to succeed")
  }
  if id.UserName != "local-dev" || id.PublicUserLabel != "localdev" || !id.IsAdmin {
    t.Fatalf("unexpected identity: %#v", id)
  }
}

func TestMatchAppHostRedirectsAliasUserLabel(t *testing.T) {
  srv := &Server{cfg: Config{PublicBaseDomain: "reverse.example.test"}, state: State{
    Users: map[string]*User{
      "alice-smith": {UserName: "alice-smith", PublicUserLabel: "alicesmith", PublicUserAliases: []string{"asmith"}},
    },
    Apps: map[string]*App{
      "alice-smith/web": {UserName: "alice-smith", AppName: "web"},
    },
  }}

  user, app, redirectHost, ok := srv.matchAppHost("web-asmith.reverse.example.test")
  if !ok || user != "alice-smith" || app != "web" || redirectHost != "web-alicesmith.reverse.example.test" {
    t.Fatalf("unexpected alias app host result: ok=%v user=%q app=%q redirect=%q", ok, user, app, redirectHost)
  }
}
