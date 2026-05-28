package main

import (
	"bytes"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newAbuseReportTestServer(t *testing.T) *Server {
	t.Helper()
	tpls, err := template.New("pages").Parse(dashboardTemplates)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		cfg:       Config{PublicBaseDomain: "reverse.example.test", StatePath: filepath.Join(t.TempDir(), "state.json")},
		logger:    slog.Default(),
		templates: tpls,
		state: State{
			RegistrationOpen: true,
			Users: map[string]*User{
				"alice": {UserName: "alice", PublicUserLabel: "alicesmith", Email: "alice@example.test", APIKey: "pf_secret"},
			},
			Apps: map[string]*App{
				"alice/web": {UserName: "alice", AppName: "web", Approved: true},
			},
			AbuseReports: map[string]*AbuseReport{},
		},
	}
}

func TestReportAbuseFormPrefillsReportedURLAndContext(t *testing.T) {
	srv := newAbuseReportTestServer(t)
	reportedURL := "https://web-alicesmith.reverse.example.test/bad?next=%2Flogin"
	req := httptest.NewRequest(http.MethodGet, "https://reverse.example.test/report-abuse?url="+url.QueryEscape(reportedURL)+"&context="+url.QueryEscape("served-by banner"), nil)
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("expected html content type, got %v", rr.Header())
	}
	body := rr.Body.String()
	for _, want := range []string{
		`action="/api/report-abuse"`,
		`name="reported_url"`,
		`value="https://web-alicesmith.reverse.example.test/bad?next=%2Flogin"`,
		`name="context"`,
		"served-by banner",
		"Portflare routes traffic for independently operated apps",
		"Portflare stores only the report details needed to investigate abuse",
		"Routine report records should be retained only for the operator's abuse-response window",
		"Do not submit passwords, API keys, private tokens, or other secrets",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected form body to contain %q, got %s", want, body)
		}
	}
}

func TestReportAbuseJSONPersistsReportAndReturnsSafeCaseID(t *testing.T) {
	srv := newAbuseReportTestServer(t)
	reqBody := map[string]string{
		"reported_url":     "https://web-alicesmith.reverse.example.test/bad",
		"category":         "phishing",
		"description":      "This page is collecting credentials.",
		"reporter_contact": "reporter@example.test",
		"context":          "served-by banner",
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "reporter-browser/1.0")
	req.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	caseID := resp["case_id"]
	if !strings.HasPrefix(caseID, "abr_") {
		t.Fatalf("expected safe case id, got %#v", resp)
	}
	for _, leaked := range []string{"alice", "web", "phishing", "credentials"} {
		if strings.Contains(rr.Body.String(), leaked) {
			t.Fatalf("response leaked investigation detail %q in %s", leaked, rr.Body.String())
		}
	}

	srv.stateMu.RLock()
	report := srv.state.AbuseReports[caseID]
	srv.stateMu.RUnlock()
	if report == nil {
		t.Fatalf("expected report %q to be persisted in state: %#v", caseID, srv.state.AbuseReports)
	}
	if report.ReportedURL != "https://web-alicesmith.reverse.example.test/bad" ||
		report.ReportedHost != "web-alicesmith.reverse.example.test" ||
		report.ReportedPath != "/bad" ||
		report.ReportedUserName != "alice" ||
		report.ReportedUserLabel != "alicesmith" ||
		report.ReportedAppName != "web" ||
		report.Category != "phishing" ||
		report.Description != "This page is collecting credentials." ||
		report.ReporterContact != "reporter@example.test" ||
		report.ReporterIP != "203.0.113.7" ||
		report.ReporterUserAgent != "reporter-browser/1.0" ||
		report.Context != "served-by banner" ||
		report.Status != "new" ||
		report.CreatedAt.IsZero() ||
		report.UpdatedAt.IsZero() {
		t.Fatalf("unexpected report: %#v", report)
	}
	if report.UpdatedAt.Before(report.CreatedAt) {
		t.Fatalf("updated_at should not be before created_at: %#v", report)
	}

	reloaded, err := newServer(Config{PublicBaseDomain: "reverse.example.test", StatePath: srv.cfg.StatePath}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.state.AbuseReports[caseID] == nil {
		t.Fatalf("expected report %q after reload, got %#v", caseID, reloaded.state.AbuseReports)
	}
}

func TestReportAbuseRecordDoesNotStoreUnnecessaryRequestData(t *testing.T) {
	srv := newAbuseReportTestServer(t)
	raw, err := json.Marshal(map[string]string{
		"reported_url": "https://web-alicesmith.reverse.example.test/bad",
		"category":     "phishing",
		"description":  "Credential collection.",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse?utm=tracking", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "session=secret-cookie")
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header.Set("Referer", "https://external.example.test/private")
	req.Header.Set("Accept-Language", "en-US")
	req.Header.Set("X-Forwarded-For", "203.0.113.8")
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	report := srv.state.AbuseReports[resp["case_id"]]
	rawReport, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	lowerReport := strings.ToLower(string(rawReport))
	for _, forbidden := range []string{
		"secret-cookie",
		"secret-token",
		"authorization",
		"referer",
		"accept-language",
		"external.example.test/private",
		"utm=tracking",
	} {
		if strings.Contains(lowerReport, strings.ToLower(forbidden)) {
			t.Fatalf("report stored unnecessary request data %q: %s", forbidden, rawReport)
		}
	}
}

func TestReportAbuseFormPostAcceptsLocalRoute(t *testing.T) {
	srv := newAbuseReportTestServer(t)
	form := url.Values{
		"reported_url":     []string{"/r/alice/web/path?x=1"},
		"category":         []string{"spam"},
		"description":      []string{"The route is posting unsolicited content."},
		"reporter_contact": []string{"ops@example.test"},
	}
	req := httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "198.51.100.44:1234"
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	report := srv.state.AbuseReports[resp["case_id"]]
	if report == nil {
		t.Fatalf("expected report to be stored: %#v", srv.state.AbuseReports)
	}
	if report.ReportedURL != "https://reverse.example.test/r/alice/web/path?x=1" ||
		report.ReportedHost != "reverse.example.test" ||
		report.ReportedPath != "/r/alice/web/path" ||
		report.ReportedUserName != "alice" ||
		report.ReportedAppName != "web" ||
		report.ReporterIP != "198.51.100.44" {
		t.Fatalf("unexpected local route report: %#v", report)
	}
}

func TestReportAbuseRejectsInvalidAndOverlongFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		body map[string]string
	}{
		{
			name: "missing url",
			body: map[string]string{"category": "phishing", "description": "bad"},
		},
		{
			name: "external url",
			body: map[string]string{"reported_url": "https://evil.example.test/bad", "category": "phishing", "description": "bad"},
		},
		{
			name: "unsupported scheme",
			body: map[string]string{"reported_url": "ftp://reverse.example.test/bad", "category": "phishing", "description": "bad"},
		},
		{
			name: "relative non route",
			body: map[string]string{"reported_url": "/admin", "category": "phishing", "description": "bad"},
		},
		{
			name: "unknown category",
			body: map[string]string{"reported_url": "https://reverse.example.test/r/alice/web", "category": "billing", "description": "bad"},
		},
		{
			name: "overlong description",
			body: map[string]string{"reported_url": "https://reverse.example.test/r/alice/web", "category": "phishing", "description": strings.Repeat("x", maxAbuseReportDescriptionLen+1)},
		},
		{
			name: "overlong contact",
			body: map[string]string{"reported_url": "https://reverse.example.test/r/alice/web", "category": "phishing", "description": "bad", "reporter_contact": strings.Repeat("x", maxAbuseReportContactLen+1)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newAbuseReportTestServer(t)
			raw, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse", bytes.NewReader(raw))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			srv.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("expected bad request, got %d body=%s", rr.Code, rr.Body.String())
			}
			if len(srv.state.AbuseReports) != 0 {
				t.Fatalf("invalid report should not be stored: %#v", srv.state.AbuseReports)
			}
		})
	}
}

func TestReportAbuseRejectsOversizedBody(t *testing.T) {
	srv := newAbuseReportTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse", strings.NewReader(strings.Repeat("x", maxAbuseReportBodyBytes+1)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected payload too large, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(srv.state.AbuseReports) != 0 {
		t.Fatalf("oversized report should not be stored: %#v", srv.state.AbuseReports)
	}
}

func TestReportAbuseThrottlesAcrossRateLimitDimensions(t *testing.T) {
	srv := newAbuseReportTestServer(t)
	for i := 0; i < abuseReportLimitPerWindow; i++ {
		req := validAbuseReportRequest(t, "https://web-alicesmith.reverse.example.test/bad")
		rr := httptest.NewRecorder()
		srv.routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("attempt %d: expected created, got %d body=%s", i+1, rr.Code, rr.Body.String())
		}
	}

	req := validAbuseReportRequest(t, "https://web-alicesmith.reverse.example.test/bad")
	rr := httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("expected throttle response, got %d body=%s", rr.Code, rr.Body.String())
	}

	req = validAbuseReportRequest(t, "https://web-alicesmith.reverse.example.test/other")
	rr = httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("same reporter IP should be throttled across URLs, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestReportAbuseRateLimitKeysIncludeIPURLHostEmailHashAndRoute(t *testing.T) {
	keys := abuseReportRateLimitKeys("203.0.113.9", resolvedReportedURL{
		URL:      "https://web-alicesmith.reverse.example.test/bad",
		Host:     "web-alicesmith.reverse.example.test",
		UserName: "alice",
		AppName:  "web",
	}, "Reporter@Example.Test")

	for _, want := range []string{
		"ip:203.0.113.9",
		"url:https://web-alicesmith.reverse.example.test/bad",
		"host:web-alicesmith.reverse.example.test",
		"route:alice/web",
	} {
		if !containsString(keys, want) {
			t.Fatalf("expected rate limit keys to contain %q, got %#v", want, keys)
		}
	}
	emailKeys := 0
	for _, key := range keys {
		if strings.HasPrefix(key, "email:") {
			emailKeys++
			if strings.Contains(strings.ToLower(key), "reporter@example.test") {
				t.Fatalf("email rate limit key exposed raw address: %#v", keys)
			}
		}
	}
	if emailKeys != 1 {
		t.Fatalf("expected exactly one hashed email key, got %#v", keys)
	}
}

func TestReportAbuseCoalescesDuplicateReportsAndPreservesSignals(t *testing.T) {
	srv := newAbuseReportTestServer(t)
	first := submitAbuseReport(t, srv, map[string]string{
		"reported_url":     "https://web-alicesmith.reverse.example.test/bad",
		"category":         "phishing",
		"description":      "Credential collection.",
		"reporter_contact": "one@example.test",
	}, "203.0.113.10")
	second := submitAbuseReport(t, srv, map[string]string{
		"reported_url":     "https://web-alicesmith.reverse.example.test/bad",
		"category":         "malware",
		"description":      "The same URL is now serving a suspicious download.",
		"reporter_contact": "two@example.test",
	}, "198.51.100.22")

	if second["case_id"] != first["case_id"] {
		t.Fatalf("expected duplicate report to return original case id, first=%#v second=%#v", first, second)
	}
	srv.stateMu.RLock()
	defer srv.stateMu.RUnlock()
	if len(srv.state.AbuseReports) != 1 {
		t.Fatalf("expected duplicate report to be coalesced into one stored case, got %#v", srv.state.AbuseReports)
	}
	report := srv.state.AbuseReports[first["case_id"]]
	if report.ReporterCount != 2 {
		t.Fatalf("expected reporter count to preserve duplicate signal, got %#v", report)
	}
	if report.CategoryCounts["phishing"] != 1 || report.CategoryCounts["malware"] != 1 {
		t.Fatalf("expected category counts to preserve duplicate categories, got %#v", report.CategoryCounts)
	}
	if report.UpdatedAt.Before(report.CreatedAt) {
		t.Fatalf("expected duplicate to update report timestamp, got %#v", report)
	}
}

func TestReportAbuseFormIncludesAndEnforcesSubmitTiming(t *testing.T) {
	srv := newAbuseReportTestServer(t)
	formReq := httptest.NewRequest(http.MethodGet, "https://reverse.example.test/report-abuse", nil)
	formRR := httptest.NewRecorder()
	srv.routes().ServeHTTP(formRR, formReq)
	if formRR.Code != http.StatusOK {
		t.Fatalf("unexpected form status: %d body=%s", formRR.Code, formRR.Body.String())
	}
	if !strings.Contains(formRR.Body.String(), `name="form_started_at"`) {
		t.Fatalf("expected form to include submit timing field, got %s", formRR.Body.String())
	}

	fast := validAbuseReportForm()
	fast.Set("form_started_at", strconv.FormatInt(time.Now().Unix(), 10))
	req := httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse", strings.NewReader(fast.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected fast form submit to be rejected, got %d body=%s", rr.Code, rr.Body.String())
	}

	older := validAbuseReportForm()
	older.Set("form_started_at", strconv.FormatInt(time.Now().Add(-minAbuseReportSubmitDelay-time.Second).Unix(), 10))
	req = httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse", strings.NewReader(older.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr = httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected old enough form submit to be accepted, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestReportAbuseChallengeHookCanBeRequiredByConfig(t *testing.T) {
	srv := newAbuseReportTestServer(t)
	srv.cfg.ReportAbuseChallengeMode = abuseReportChallengeCaptcha

	req := validAbuseReportRequest(t, "https://web-alicesmith.reverse.example.test/bad")
	rr := httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected missing challenge token to be rejected, got %d body=%s", rr.Code, rr.Body.String())
	}

	resp := submitAbuseReport(t, srv, map[string]string{
		"reported_url":    "https://web-alicesmith.reverse.example.test/bad",
		"category":        "phishing",
		"description":     "bad",
		"challenge_token": "captcha-ok",
	}, "203.0.113.11")
	if !strings.HasPrefix(resp["case_id"], "abr_") {
		t.Fatalf("expected challenged report to be accepted, got %#v", resp)
	}
}

func validAbuseReportRequest(t *testing.T, reportedURL string) *http.Request {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"reported_url": reportedURL,
		"category":     "phishing",
		"description":  "bad",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	return req
}

func validAbuseReportForm() url.Values {
	return url.Values{
		"reported_url": []string{"https://web-alicesmith.reverse.example.test/bad"},
		"category":     []string{"phishing"},
		"description":  []string{"bad"},
	}
}

func submitAbuseReport(t *testing.T, srv *Server, body map[string]string, ip string) map[string]string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	rr := httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected report creation, got %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
