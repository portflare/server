package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyToAppLogsAndCountsDecorationDecisionWithoutLeakingContent(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	store := &captureTrafficStore{}
	srv, cleanup := newProxyTestServer(t, store, func(req TunnelRequest) TunnelResponse {
		body := "<html><body><h1>top-secret-body</h1></body></html>"
		return TunnelResponse{
			RequestID:  req.RequestID,
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			BodyBase64: base64.StdEncoding.EncodeToString([]byte(body)),
		}
	})
	defer cleanup()
	srv.logger = logger
	srv.state.Users["alice"] = &User{UserName: "alice", PublicUserLabel: "alicesmith", Email: "alice@example.test"}

	req := httptest.NewRequest(http.MethodGet, "https://web-alicesmith.reverse.example.test/page?api_key=secret-query&next=/private", nil)
	rr := httptest.NewRecorder()

	srv.proxyToApp(rr, req, "alice", "web")
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}

	output := logs.String()
	for _, want := range []string{
		`"msg":"served_by_decoration"`,
		`"app_public_id":"web"`,
		`"user_public_id":"alicesmith"`,
		`"decision":"html_inject"`,
		`"reason":"eligible_html"`,
		`"content_type":"text/html"`,
		`"status":200`,
		`"size_bucket":`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected decoration log to contain %q, got %s", want, output)
		}
	}
	for _, leaked := range []string{"top-secret-body", "secret-query", "api_key", "alice@example.test"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("decoration log leaked %q in %s", leaked, output)
		}
	}

	snapshot := srv.observabilitySnapshot()
	if snapshot.DecorationInjected != 1 || snapshot.DecorationHeaderOnly != 0 || len(snapshot.DecorationSkippedByReason) != 0 {
		t.Fatalf("unexpected decoration counters: %#v", snapshot)
	}
}

func TestObservabilityCountersCoverHeaderOnlySkipAndReportCategories(t *testing.T) {
	srv := &Server{}

	srv.recordDecorationObservation(decorationObservation{Decision: responseDecorationHeaderOnly, Reason: "content_type"})
	srv.recordDecorationObservation(decorationObservation{Decision: responseDecorationSkip, Reason: "disabled"})
	srv.recordReportSubmissionObservation(reportSubmissionObservation{Category: "phishing"})
	srv.recordReportSubmissionObservation(reportSubmissionObservation{Category: "phishing"})
	srv.recordReportSubmissionObservation(reportSubmissionObservation{Category: "malware"})

	snapshot := srv.observabilitySnapshot()
	if snapshot.DecorationHeaderOnly != 1 {
		t.Fatalf("expected one header-only decoration, got %#v", snapshot)
	}
	if got := snapshot.DecorationSkippedByReason["disabled"]; got != 1 {
		t.Fatalf("expected disabled skip counter, got %#v", snapshot.DecorationSkippedByReason)
	}
	if snapshot.ReportsSubmittedByCategory["phishing"] != 2 || snapshot.ReportsSubmittedByCategory["malware"] != 1 {
		t.Fatalf("unexpected report category counters: %#v", snapshot.ReportsSubmittedByCategory)
	}
}

func TestAdminStateIncludesObservabilityCounters(t *testing.T) {
	srv := newServedByAdminTestServer(t)
	srv.recordDecorationObservation(decorationObservation{Decision: responseDecorationHTMLInject, Reason: decorationReasonEligibleHTML})
	srv.recordDecorationObservation(decorationObservation{Decision: responseDecorationSkip, Reason: decorationReasonDisabled})
	srv.recordReportSubmissionObservation(reportSubmissionObservation{Category: "phishing"})

	req := adminRequest(http.MethodGet, "/api/admin/state", "")
	rr := httptest.NewRecorder()
	srv.handleAdminState(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}

	var state map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	observability, ok := state["observability"].(map[string]any)
	if !ok {
		t.Fatalf("admin state omitted observability counters: %#v", state)
	}
	if observability["decoration_injected"] != float64(1) || observability["decoration_header_only"] != float64(0) {
		t.Fatalf("unexpected decoration counters in admin state: %#v", observability)
	}
	skipped := observability["decoration_skipped_by_reason"].(map[string]any)
	reports := observability["reports_submitted_by_category"].(map[string]any)
	if skipped[decorationReasonDisabled] != float64(1) || reports["phishing"] != float64(1) {
		t.Fatalf("unexpected nested observability counters: skipped=%#v reports=%#v", skipped, reports)
	}
}

func TestReportAbuseSubmissionLogsAndCountsSafeMetadata(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	srv := newAbuseReportTestServer(t)
	srv.logger = logger
	raw, err := json.Marshal(map[string]string{
		"reported_url":     "https://web-alicesmith.reverse.example.test/bad?token=secret-query",
		"category":         "phishing",
		"description":      "This description has private reporter details.",
		"reporter_contact": "reporter@example.test",
		"context":          "served-by banner",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://reverse.example.test/api/report-abuse?utm=secret-campaign", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("User-Agent", "reporter-browser/1.0 private-token")
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	output := logs.String()
	for _, want := range []string{
		`"msg":"abuse_report_submitted"`,
		`"report_id":"` + resp["case_id"] + `"`,
		`"category":"phishing"`,
		`"app_public_id":"web"`,
		`"user_public_id":"alicesmith"`,
		`"reported_host":"web-alicesmith.reverse.example.test"`,
		`"reported_path":"/bad"`,
		`"reporter_ip_hash":`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("expected report log to contain %q, got %s", want, output)
		}
	}
	for _, leaked := range []string{
		"secret-query",
		"utm=secret-campaign",
		"203.0.113.7",
		"reporter@example.test",
		"private reporter details",
		"private-token",
	} {
		if strings.Contains(output, leaked) {
			t.Fatalf("report log leaked %q in %s", leaked, output)
		}
	}

	snapshot := srv.observabilitySnapshot()
	if snapshot.ReportsSubmittedByCategory["phishing"] != 1 {
		t.Fatalf("expected phishing report counter, got %#v", snapshot.ReportsSubmittedByCategory)
	}
}

func TestRequestLoggingRedactsQueryAndTruncatesLongPath(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := withLogging(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "https://reverse.example.test/r/alice/web/"+strings.Repeat("x", 400)+"/secret-tail?api_key=secret-query", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	output := logs.String()
	if !strings.Contains(output, `"msg":"request"`) {
		t.Fatalf("expected request log, got %s", output)
	}
	for _, leaked := range []string{"secret-query", "api_key", "secret-tail"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("request log leaked %q in %s", leaked, output)
		}
	}
}
