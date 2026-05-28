package main

import (
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServedByOperatorGuideCoversConfigurationAndCompatibility(t *testing.T) {
	doc := readServerDoc(t, "docs/served-by-operator-guide.md")

	assertDocContains(t, doc, []string{
		"visible_and_headers",
		"headers_only",
		"disabled",
		"Default public deployment recommendation",
		"PORTFLARE_SERVED_BY_ENABLED=true",
		"PORTFLARE_SERVED_BY_MODE=visible_and_headers",
		"PORTFLARE_SERVED_BY_HTML_INJECTION_ENABLED=true",
		"PORTFLARE_REPORT_ABUSE_ENABLED=true",
		"PORTFLARE_SERVED_BY_APP_DISABLE_ALLOWED=false",
		"PORTFLARE_SERVED_BY_EMERGENCY_FORCE_VISIBLE=false",
		"/admin",
		"/api/admin/state",
		"per-app overrides",
		"force_visible",
		"HTML rewriting",
		"Content-Encoding",
		"Content-Security-Policy",
		"streaming",
		"single-page apps",
		"binary responses",
		"Owner preview",
		"curl -i",
		"headers-only fallback",
		"Compressed responses",
		"downloads",
		"layout issues",
		"Opt-out request",
		"informational",
	})
}

func TestAbuseRunbookCoversOperatorDutiesAndEscalation(t *testing.T) {
	doc := readServerDoc(t, "docs/abuse-response-runbook.md")

	assertDocContains(t, doc, []string{
		"Severity matrix",
		"Disable the route",
		"Owner notification",
		"Evidence preservation",
		"Legal escalation",
		"LEGAL CONTACT PLACEHOLDER",
		"operator is responsible for monitoring and responding to reports",
	})
}

func TestPublicServedByCopyIsNeutralAndSelfHostedOperatorResponsible(t *testing.T) {
	tpls, err := template.New("pages").Parse(dashboardTemplates)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{
		cfg:       Config{PublicBaseDomain: "reverse.example.test"},
		logger:    slog.Default(),
		templates: tpls,
		state: State{
			Users: map[string]*User{},
			Apps:  map[string]*App{},
		},
	}

	pages := []struct {
		name   string
		target string
	}{
		{name: "learn more", target: "https://reverse.example.test/about-portflare"},
		{name: "report abuse", target: "https://reverse.example.test/report-abuse?url=https%3A%2F%2Freverse.example.test%2Fr%2Falice%2Fweb"},
	}
	for _, page := range pages {
		t.Run(page.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, page.target, nil)
			rr := httptest.NewRecorder()
			srv.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			assertDocContains(t, body, []string{
				"Served by Portflare",
				"self-hosted operator",
				"responsible for monitoring and responding to reports",
			})
			for _, forbidden := range []string{"Protected by Portflare", "Verified by Portflare"} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("public copy must not use %q: %s", forbidden, body)
				}
			}
		})
	}
}

func readServerDoc(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func assertDocContains(t *testing.T, body string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Fatalf("expected content to contain %q", want)
		}
	}
}
