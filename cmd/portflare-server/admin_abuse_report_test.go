package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newAdminAbuseReportTestServer(t *testing.T) *Server {
	t.Helper()
	srv := newAbuseReportTestServer(t)
	srv.cfg.AdminUsers = map[string]struct{}{"admin": {}}
	now := time.Date(2026, 5, 28, 12, 0, 0, 0, time.UTC)
	srv.state.Users["bob"] = &User{UserName: "bob", PublicUserLabel: "bob", Email: "bob@example.test", APIKey: "pf_bob", CreatedAt: now.Add(-5 * time.Hour)}
	srv.state.Apps["bob/blog"] = &App{UserName: "bob", AppName: "blog", Approved: false, Connected: false, CreatedAt: now.Add(-4 * time.Hour)}
	srv.state.AbuseReports = map[string]*AbuseReport{
		"abr_new": {
			ID:                "abr_new",
			ReportedURL:       "https://web-alicesmith.reverse.example.test/bad",
			ReportedHost:      "web-alicesmith.reverse.example.test",
			ReportedPath:      "/bad",
			ReportedUserName:  "alice",
			ReportedUserLabel: "alicesmith",
			ReportedAppName:   "web",
			Category:          "phishing",
			Description:       "Credential collection.",
			ReporterContact:   "reporter@example.test",
			ReporterIP:        "203.0.113.10",
			ReporterUserAgent: "browser/1.0",
			Status:            "new",
			CreatedAt:         now.Add(-2 * time.Hour),
			UpdatedAt:         now.Add(-2 * time.Hour),
		},
		"abr_prior": {
			ID:                "abr_prior",
			ReportedURL:       "https://web-alicesmith.reverse.example.test/login",
			ReportedUserName:  "alice",
			ReportedUserLabel: "alicesmith",
			ReportedAppName:   "web",
			Category:          "spam",
			Description:       "Earlier related report.",
			Status:            "closed",
			CreatedAt:         now.Add(-3 * time.Hour),
			UpdatedAt:         now.Add(-3 * time.Hour),
		},
		"abr_other": {
			ID:               "abr_other",
			ReportedURL:      "https://blog-bob.reverse.example.test/malware",
			ReportedHost:     "blog-bob.reverse.example.test",
			ReportedPath:     "/malware",
			ReportedUserName: "bob",
			ReportedAppName:  "blog",
			Category:         "malware",
			Description:      "Suspicious download.",
			Status:           "escalated_legal",
			CreatedAt:        now.Add(-1 * time.Hour),
			UpdatedAt:        now.Add(-1 * time.Hour),
		},
	}
	return srv
}

func adminJSONRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, "https://reverse.example.test"+target, strings.NewReader(body))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Auth-Request-User", "admin")
	req.Header.Set("X-Auth-Request-Email", "admin@example.test")
	return req
}

func TestAdminAbuseReportAPIsRequireAdminIdentity(t *testing.T) {
	srv := newAdminAbuseReportTestServer(t)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "list", method: http.MethodGet, path: "/api/admin/abuse-reports"},
		{name: "detail", method: http.MethodGet, path: "/api/admin/abuse-reports/abr_new"},
		{name: "status", method: http.MethodPost, path: "/api/admin/abuse-reports/abr_new/status", body: `{"status":"triaged_reviewing"}`},
		{name: "note", method: http.MethodPost, path: "/api/admin/abuse-reports/abr_new/notes", body: `{"body":"reviewed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, "https://reverse.example.test"+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			srv.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("expected unauthenticated request to be rejected, got %d body=%s", rr.Code, rr.Body.String())
			}

			req = httptest.NewRequest(tc.method, "https://reverse.example.test"+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Auth-Request-User", "alice")
			req.Header.Set("X-Auth-Request-Email", "alice@example.test")
			rr = httptest.NewRecorder()
			srv.routes().ServeHTTP(rr, req)
			if rr.Code != http.StatusForbidden {
				t.Fatalf("expected non-admin request to be forbidden, got %d body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestAdminAbuseReportListFiltersByStatusCategoryAppUserAndReportedURL(t *testing.T) {
	srv := newAdminAbuseReportTestServer(t)
	req := adminJSONRequest(http.MethodGet, "/api/admin/abuse-reports?status=new&category=phishing&user=alice&app=web&reported_url=/bad", "")
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Reports       []map[string]any `json:"reports"`
		StatusOptions []map[string]any `json:"status_options"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Reports) != 1 || body.Reports[0]["id"] != "abr_new" {
		t.Fatalf("expected only abr_new, got %#v", body.Reports)
	}
	if len(body.StatusOptions) != 8 {
		t.Fatalf("expected all abuse report statuses, got %#v", body.StatusOptions)
	}
}

func TestAdminAbuseReportDetailIncludesReporterMetadataAppStatusAndRelatedReports(t *testing.T) {
	srv := newAdminAbuseReportTestServer(t)
	req := adminJSONRequest(http.MethodGet, "/api/admin/abuse-reports/abr_new", "")
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Report         map[string]any   `json:"report"`
		ReportedUser   map[string]any   `json:"reported_user"`
		CurrentApp     map[string]any   `json:"current_app"`
		RelatedReports []map[string]any `json:"related_reports"`
		ActionLinks    map[string]any   `json:"action_links"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Report["reporter_contact"] != "reporter@example.test" ||
		body.Report["reporter_ip"] != "203.0.113.10" ||
		body.Report["reporter_user_agent"] != "browser/1.0" {
		t.Fatalf("expected reporter metadata in detail, got %#v", body.Report)
	}
	if body.ReportedUser["user_name"] != "alice" || body.ReportedUser["public_user_label"] != "alicesmith" || body.ReportedUser["email"] != "alice@example.test" {
		t.Fatalf("expected resolved user context, got %#v", body.ReportedUser)
	}
	if body.CurrentApp["status"] != "approved" || body.CurrentApp["approved"] != true || body.CurrentApp["connected"] != false {
		t.Fatalf("expected current app status, got %#v", body.CurrentApp)
	}
	if len(body.RelatedReports) != 1 || body.RelatedReports[0]["id"] != "abr_prior" {
		t.Fatalf("expected prior related report, got %#v", body.RelatedReports)
	}
	if body.ActionLinks["approve_app"] == "" || body.ActionLinks["app_public_url"] == "" {
		t.Fatalf("expected app action links, got %#v", body.ActionLinks)
	}
}

func TestAdminAbuseReportUIListsReportsAndShowsCaseWorkflow(t *testing.T) {
	srv := newAdminAbuseReportTestServer(t)
	req := adminJSONRequest(http.MethodGet, "/admin", "")
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected admin page status: %d body=%s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{"Abuse reports", "abr_new", `name="reported_url"`, "Triaged / reviewing"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("expected admin report queue to contain %q, got %s", want, rr.Body.String())
		}
	}

	req = adminJSONRequest(http.MethodGet, "/admin/abuse-reports/abr_new", "")
	rr = httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected detail page status: %d body=%s", rr.Code, rr.Body.String())
	}
	for _, want := range []string{
		"Reporter metadata",
		"Resolved target",
		`action="/api/admin/abuse-reports/abr_new/status"`,
		`action="/api/admin/abuse-reports/abr_new/notes"`,
		"Prior related reports",
		"abr_prior",
	} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Fatalf("expected admin detail workflow to contain %q, got %s", want, rr.Body.String())
		}
	}
}

func TestAdminAbuseReportStatusTransitionsAndNotes(t *testing.T) {
	srv := newAdminAbuseReportTestServer(t)
	raw, err := json.Marshal(map[string]string{"status": "reviewing", "note": "Started triage."})
	if err != nil {
		t.Fatal(err)
	}
	req := adminJSONRequest(http.MethodPost, "/api/admin/abuse-reports/abr_new/status", string(raw))
	rr := httptest.NewRecorder()

	srv.routes().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status update response: %d body=%s", rr.Code, rr.Body.String())
	}
	report := srv.state.AbuseReports["abr_new"]
	if report.Status != "triaged_reviewing" {
		t.Fatalf("unexpected report status after update: %#v", report)
	}
	detail := adminJSONRequest(http.MethodGet, "/api/admin/abuse-reports/abr_new", "")
	detailRR := httptest.NewRecorder()
	srv.routes().ServeHTTP(detailRR, detail)
	var detailBody struct {
		Report map[string]any `json:"report"`
	}
	if err := json.Unmarshal(detailRR.Body.Bytes(), &detailBody); err != nil {
		t.Fatal(err)
	}
	if detailBody.Report["status_updated_by"] != "admin" || detailBody.Report["status_updated_at"] == "" {
		t.Fatalf("expected status metadata in detail, got %#v", detailBody.Report)
	}
	notes, ok := detailBody.Report["internal_notes"].([]any)
	if !ok || len(notes) != 1 || notes[0].(map[string]any)["body"] != "Started triage." || notes[0].(map[string]any)["actor_user_name"] != "admin" {
		t.Fatalf("expected status note in detail, got %#v", detailBody.Report["internal_notes"])
	}

	req = adminJSONRequest(http.MethodPost, "/api/admin/abuse-reports/abr_new/notes", `{"body":"Checked public route."}`)
	rr = httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected note response: %d body=%s", rr.Code, rr.Body.String())
	}
	detailRR = httptest.NewRecorder()
	srv.routes().ServeHTTP(detailRR, detail)
	if err := json.Unmarshal(detailRR.Body.Bytes(), &detailBody); err != nil {
		t.Fatal(err)
	}
	notes, ok = detailBody.Report["internal_notes"].([]any)
	if !ok || len(notes) != 2 || notes[1].(map[string]any)["body"] != "Checked public route." {
		t.Fatalf("expected second internal note, got %#v", detailBody.Report["internal_notes"])
	}

	req = adminJSONRequest(http.MethodPost, "/api/admin/abuse-reports/abr_new/status", `{"status":"not_a_status"}`)
	rr = httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid status to be rejected, got %d body=%s", rr.Code, rr.Body.String())
	}
	if report.Status != "triaged_reviewing" {
		t.Fatalf("invalid status changed report: %#v", report)
	}

	req = adminJSONRequest(http.MethodPost, "/api/admin/abuse-reports/abr_new/status", `{"status":"legal"}`)
	rr = httptest.NewRecorder()
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected legal alias to be accepted, got %d body=%s", rr.Code, rr.Body.String())
	}
	if report.Status != "escalated_legal" {
		t.Fatalf("expected legal alias to normalize, got %#v", report)
	}
}
