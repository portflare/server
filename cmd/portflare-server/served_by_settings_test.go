package main

import (
	"encoding/base64"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigServedByDefaultsAndEnvOverrides(t *testing.T) {
	for _, key := range []string{
		"PORTFLARE_SERVED_BY_ENABLED",
		"PORTFLARE_SERVED_BY_MODE",
		"PORTFLARE_SERVED_BY_HTML_INJECTION_ENABLED",
		"PORTFLARE_REPORT_ABUSE_ENABLED",
	} {
		t.Setenv(key, "")
	}

	cfg := loadConfig()
	if !cfg.ServedByEnabled || cfg.ServedByMode != servedByModeVisibleAndHeaders || !cfg.ServedByHTMLInjectionEnabled || !cfg.ReportAbuseEnabled {
		t.Fatalf("unexpected served-by defaults: %#v", cfg)
	}

	t.Setenv("PORTFLARE_SERVED_BY_ENABLED", "false")
	t.Setenv("PORTFLARE_SERVED_BY_MODE", servedByModeHeadersOnly)
	t.Setenv("PORTFLARE_SERVED_BY_HTML_INJECTION_ENABLED", "false")
	t.Setenv("PORTFLARE_REPORT_ABUSE_ENABLED", "false")

	cfg = loadConfig()
	if cfg.ServedByEnabled || cfg.ServedByMode != servedByModeHeadersOnly || cfg.ServedByHTMLInjectionEnabled || cfg.ReportAbuseEnabled {
		t.Fatalf("unexpected served-by env overrides: %#v", cfg)
	}
}

func TestLoadStateAppliesServedByDefaultsToNewAndLegacyState(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	cfg := Config{
		PublicBaseDomain:             "reverse.example.test",
		StatePath:                    statePath,
		ServedByEnabled:              true,
		ServedByMode:                 servedByModeVisibleAndHeaders,
		ServedByHTMLInjectionEnabled: true,
		ReportAbuseEnabled:           true,
		TrafficStatsInterval:         30,
	}

	srv, err := newServer(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !srv.state.ServedByEnabled || srv.state.ServedByMode != servedByModeVisibleAndHeaders || !srv.state.ServedByHTMLInjectionEnabled || !srv.state.ReportAbuseEnabled {
		t.Fatalf("new state did not receive served-by defaults: %#v", srv.state)
	}

	legacyPath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(legacyPath, []byte(`{"registration_open":true,"users":{},"apps":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := newServer(Config{
		PublicBaseDomain:             "reverse.example.test",
		StatePath:                    legacyPath,
		ServedByEnabled:              true,
		ServedByMode:                 servedByModeVisibleAndHeaders,
		ServedByHTMLInjectionEnabled: true,
		ReportAbuseEnabled:           true,
		TrafficStatsInterval:         30,
	}, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.state.ServedByEnabled || legacy.state.ServedByMode != servedByModeVisibleAndHeaders || !legacy.state.ServedByHTMLInjectionEnabled || !legacy.state.ReportAbuseEnabled {
		t.Fatalf("legacy state did not receive served-by defaults: %#v", legacy.state)
	}
}

func TestAdminStateDisplaysAndUpdatesServedBySettings(t *testing.T) {
	srv := newServedByAdminTestServer(t)

	rr := httptest.NewRecorder()
	req := adminRequest(http.MethodGet, "/api/admin/state", "")
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected admin state status: %d body=%s", rr.Code, rr.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["served_by_enabled"] != true ||
		state["served_by_mode"] != servedByModeVisibleAndHeaders ||
		state["served_by_html_injection_enabled"] != true ||
		state["report_abuse_enabled"] != true {
		t.Fatalf("admin state omitted served-by settings: %#v", state)
	}

	rr = httptest.NewRecorder()
	req = adminRequest(http.MethodPost, "/admin/toggle-setting", url.Values{
		"setting": []string{"served_by_enabled"},
		"value":   []string{"false"},
	}.Encode())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected update status: %d body=%s", rr.Code, rr.Body.String())
	}
	var update struct {
		Setting  string   `json:"setting"`
		Value    bool     `json:"value"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &update); err != nil {
		t.Fatal(err)
	}
	if update.Setting != "served_by_enabled" || update.Value || len(update.Warnings) == 0 {
		t.Fatalf("expected served-by disable warning, got %#v", update)
	}

	reloaded, err := newServer(srv.cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.state.ServedByEnabled {
		t.Fatalf("expected served-by setting to persist false: %#v", reloaded.state)
	}

	rr = httptest.NewRecorder()
	req = adminRequest(http.MethodPost, "/admin/toggle-setting", url.Values{
		"setting": []string{"served_by_mode"},
		"value":   []string{"invalid-mode"},
	}.Encode())
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected invalid mode to be rejected, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminServedBySettingsRequireAdmin(t *testing.T) {
	srv := newServedByAdminTestServer(t)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/toggle-setting", strings.NewReader(url.Values{
		"setting": []string{"report_abuse_enabled"},
		"value":   []string{"false"},
	}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Auth-Request-User", "alice")
	req.Header.Set("X-Auth-Request-Email", "alice@example.test")
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden for non-admin settings update, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestAdminPageShowsServedBySettingsAndWarnings(t *testing.T) {
	srv := newServedByAdminTestServer(t)
	srv.stateMu.Lock()
	srv.state.ServedByEnabled = false
	srv.state.ReportAbuseEnabled = false
	srv.stateMu.Unlock()

	rr := httptest.NewRecorder()
	req := adminRequest(http.MethodGet, "/admin", "")
	srv.routes().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected admin page status: %d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Served-by and report abuse settings",
		`id="served-by-enabled">false`,
		`id="served-by-mode">visible_and_headers`,
		`id="served-by-html-injection-enabled">true`,
		`id="report-abuse-enabled">false`,
		"Warning",
		"public disclosure",
		"abuse intake",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected admin page to contain %q, got %s", want, body)
		}
	}
}

func TestReportAbuseCanBeDisabledByGlobalSetting(t *testing.T) {
	srv := newServedByAdminTestServer(t)
	srv.stateMu.Lock()
	srv.state.ReportAbuseEnabled = false
	srv.stateMu.Unlock()

	for _, tt := range []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/report-abuse"},
		{method: http.MethodPost, path: "/api/report-abuse", body: `{"reported_url":"https://reverse.example.test/r/alice/web","category":"phishing","description":"bad"}`},
	} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(tt.method, "https://reverse.example.test"+tt.path, strings.NewReader(tt.body))
		req.Header.Set("Content-Type", "application/json")
		srv.routes().ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s: expected disabled report abuse to return not found, got %d body=%s", tt.method, tt.path, rr.Code, rr.Body.String())
		}
	}
}

func TestProxyDecorationHonorsServedByStateSettings(t *testing.T) {
	t.Run("headers only omits html injection without restart", func(t *testing.T) {
		store := &captureTrafficStore{}
		srv, cleanup := newProxyTestServer(t, store, func(req TunnelRequest) TunnelResponse {
			body := "<html><body><h1>Hello</h1></body></html>"
			return TunnelResponse{
				RequestID:  req.RequestID,
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/html"}},
				BodyBase64: base64.StdEncoding.EncodeToString([]byte(body)),
			}
		})
		defer cleanup()
		srv.stateMu.Lock()
		srv.state.Users["alice"] = &User{UserName: "alice", PublicUserLabel: "alicesmith", Email: "alice@example.test"}
		srv.state.ServedByEnabled = true
		srv.state.ServedByMode = servedByModeHeadersOnly
		srv.state.ServedByHTMLInjectionEnabled = false
		srv.state.ReportAbuseEnabled = true
		srv.stateMu.Unlock()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "https://web-alicesmith.reverse.example.test/", nil)
		srv.proxyToApp(rr, req, "alice", "web")
		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "Served by Portflare") {
			t.Fatalf("headers-only mode should not inject visible markup: %q", rr.Body.String())
		}
		assertPortflareFallbackHeaders(t, rr.Header(), req.URL.String(), "web", "alicesmith")
	})

	t.Run("disabled omits decoration and report link", func(t *testing.T) {
		store := &captureTrafficStore{}
		srv, cleanup := newProxyTestServer(t, store, func(req TunnelRequest) TunnelResponse {
			body := "<html><body><h1>Hello</h1></body></html>"
			return TunnelResponse{
				RequestID:  req.RequestID,
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"text/html"}},
				BodyBase64: base64.StdEncoding.EncodeToString([]byte(body)),
			}
		})
		defer cleanup()
		srv.stateMu.Lock()
		srv.state.Users["alice"] = &User{UserName: "alice", PublicUserLabel: "alicesmith", Email: "alice@example.test"}
		srv.state.ServedByEnabled = false
		srv.state.ServedByMode = servedByModeVisibleAndHeaders
		srv.state.ServedByHTMLInjectionEnabled = true
		srv.state.ReportAbuseEnabled = false
		srv.stateMu.Unlock()

		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "https://web-alicesmith.reverse.example.test/", nil)
		srv.proxyToApp(rr, req, "alice", "web")
		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), "Served by Portflare") || rr.Header().Get(servedByHeaderName) != "" || rr.Header().Get(reportAbuseHeaderName) != "" {
			t.Fatalf("disabled served-by should omit decoration, headers=%v body=%q", rr.Header(), rr.Body.String())
		}
	})
}

func newServedByAdminTestServer(t *testing.T) *Server {
	t.Helper()
	tpls, err := template.New("pages").Parse(dashboardTemplates)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		cfg: Config{
			PublicBaseDomain:             "reverse.example.test",
			StatePath:                    filepath.Join(t.TempDir(), "state.json"),
			AdminUsers:                   map[string]struct{}{"admin": {}},
			ServedByEnabled:              true,
			ServedByMode:                 servedByModeVisibleAndHeaders,
			ServedByHTMLInjectionEnabled: true,
			ReportAbuseEnabled:           true,
		},
		logger:    slog.Default(),
		templates: tpls,
		state: State{
			RegistrationOpen:             true,
			ServedByEnabled:              true,
			ServedByMode:                 servedByModeVisibleAndHeaders,
			ServedByHTMLInjectionEnabled: true,
			ReportAbuseEnabled:           true,
			Users: map[string]*User{
				"alice": {UserName: "alice", PublicUserLabel: "alicesmith", Email: "alice@example.test", APIKey: "pf_secret"},
			},
			Apps: map[string]*App{
				"alice/web": {UserName: "alice", AppName: "web", Approved: true},
			},
			AbuseReports: map[string]*AbuseReport{},
		},
		clients:      map[string]*TunnelClient{},
		pending:      map[string]*pendingResponse{},
		traffic:      newMemoryTrafficStore(30),
		abuseLimiter: newAbuseReportLimiter(abuseReportLimitPerWindow, abuseReportThrottleWindow),
	}
}

func adminRequest(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Auth-Request-User", "admin")
	req.Header.Set("X-Auth-Request-Email", "admin@example.test")
	return req
}
