package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMemoryTrafficStoreBucketsAndFilters(t *testing.T) {
	store := newMemoryTrafficStore(10 * time.Second)
	base := time.Date(2026, 4, 28, 12, 0, 5, 0, time.UTC)

	store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "web", StatusCode: http.StatusOK, BytesIn: 10, BytesOut: 100, Duration: 25 * time.Millisecond, At: base})
	store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "web", StatusCode: http.StatusBadGateway, BytesIn: 5, BytesOut: 0, Duration: 10 * time.Millisecond, Failed: true, At: base.Add(3 * time.Second)})
	store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "api", StatusCode: http.StatusCreated, BytesIn: 7, BytesOut: 70, Duration: 15 * time.Millisecond, At: base.Add(12 * time.Second)})
	store.RecordTraffic(TrafficRecord{UserName: "", AppName: "ignored", At: base})
	store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "", At: base})

	buckets, err := store.QueryTraffic(TrafficQuery{UserName: "alice", AppName: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected one web bucket, got %d: %#v", len(buckets), buckets)
	}
	bucket := buckets[0]
	if !bucket.StartAt.Equal(base.Truncate(10*time.Second)) || !bucket.EndAt.Equal(base.Truncate(10*time.Second).Add(10*time.Second)) {
		t.Fatalf("unexpected bucket interval: %s - %s", bucket.StartAt, bucket.EndAt)
	}
	if bucket.RequestsTotal != 2 || bucket.RequestsSucceeded != 1 || bucket.RequestsFailed != 1 {
		t.Fatalf("unexpected counters: %#v", bucket)
	}
	if bucket.BytesIn != 15 || bucket.BytesOut != 100 || bucket.DurationTotalMs != 35 || bucket.LastStatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected aggregate values: %#v", bucket)
	}
	if !bucket.LastRequestAt.Equal(base.Add(3 * time.Second)) {
		t.Fatalf("unexpected last request time: %s", bucket.LastRequestAt)
	}

	all, err := store.QueryTraffic(TrafficQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected two non-empty buckets, got %d: %#v", len(all), all)
	}
}

func TestMemoryTrafficStoreTimeFilters(t *testing.T) {
	store := newMemoryTrafficStore(10 * time.Second)
	base := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "web", At: base.Add(1 * time.Second)})
	store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "web", At: base.Add(21 * time.Second)})

	buckets, err := store.QueryTraffic(TrafficQuery{Since: base.Add(15 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || !buckets[0].StartAt.Equal(base.Add(20*time.Second)) {
		t.Fatalf("unexpected since-filtered buckets: %#v", buckets)
	}

	buckets, err = store.QueryTraffic(TrafficQuery{Until: base.Add(15 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 || !buckets[0].StartAt.Equal(base) {
		t.Fatalf("unexpected until-filtered buckets: %#v", buckets)
	}
}

func TestParseTrafficQuery(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/admin/traffic?user=Alice_Smith&app=Web.App&since=2026-04-28T12:00:00Z&until=2026-04-28T12:01:00Z", nil)
	query, err := parseTrafficQuery(req)
	if err != nil {
		t.Fatal(err)
	}
	if query.UserName != "alice-smith" || query.AppName != "web-app" {
		t.Fatalf("unexpected slugs: %#v", query)
	}
	if query.Since.IsZero() || query.Until.IsZero() {
		t.Fatalf("expected timestamps to be parsed: %#v", query)
	}

	req = httptest.NewRequest("GET", "/api/admin/traffic?since=not-a-time", nil)
	if _, err := parseTrafficQuery(req); err == nil || !strings.Contains(err.Error(), "invalid since") {
		t.Fatalf("expected invalid since error, got %v", err)
	}
}

func TestTrafficEndpointsPermissionsAndFiltering(t *testing.T) {
	store := newMemoryTrafficStore(10 * time.Second)
	store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "web", StatusCode: http.StatusOK, At: time.Now().UTC()})
	store.RecordTraffic(TrafficRecord{UserName: "bob", AppName: "api", StatusCode: http.StatusOK, At: time.Now().UTC()})

	srv := &Server{
		cfg:     Config{DisableAuth: true, LocalDevUser: "alice", LocalDevEmail: "alice@example.test"},
		traffic: store,
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me/traffic", nil)
	srv.handleUserTraffic(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected user traffic status: %d body=%s", rr.Code, rr.Body.String())
	}
	var userPayload struct {
		Buckets []TrafficBucket `json:"buckets"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &userPayload); err != nil {
		t.Fatal(err)
	}
	if len(userPayload.Buckets) != 1 || userPayload.Buckets[0].UserName != "alice" {
		t.Fatalf("user endpoint should only return alice buckets: %#v", userPayload.Buckets)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/admin/traffic?user=bob", nil)
	srv.handleAdminTraffic(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected admin traffic status: %d body=%s", rr.Code, rr.Body.String())
	}
	var adminPayload struct {
		Buckets []TrafficBucket `json:"buckets"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &adminPayload); err != nil {
		t.Fatal(err)
	}
	if len(adminPayload.Buckets) != 1 || adminPayload.Buckets[0].UserName != "bob" {
		t.Fatalf("admin endpoint should return filtered bob bucket: %#v", adminPayload.Buckets)
	}
}

func TestRecordTrafficNoopsWithoutStore(t *testing.T) {
	srv := &Server{}
	srv.recordTraffic("alice", "web", http.StatusOK, 1, 2, time.Millisecond, false)
}

func TestMemoryTrafficStoreDefaultIntervalAndOrdering(t *testing.T) {
	store := newMemoryTrafficStore(0)
	base := time.Date(2026, 4, 28, 12, 0, 35, 0, time.UTC)

	store.RecordTraffic(TrafficRecord{UserName: "bob", AppName: "z", At: base})
	store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "web", At: base.Add(-30 * time.Second)})
	store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "api", At: base})

	buckets, err := store.QueryTraffic(TrafficQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 3 {
		t.Fatalf("expected three buckets, got %d: %#v", len(buckets), buckets)
	}
	if !buckets[0].StartAt.Equal(base.Add(-30*time.Second).Truncate(30*time.Second)) || buckets[0].UserName != "alice" || buckets[0].AppName != "web" {
		t.Fatalf("unexpected first bucket/order: %#v", buckets)
	}
	if !buckets[1].StartAt.Equal(base.Truncate(30*time.Second)) || buckets[1].UserName != "alice" || buckets[1].AppName != "api" {
		t.Fatalf("unexpected second bucket/order: %#v", buckets)
	}
	if !buckets[2].StartAt.Equal(base.Truncate(30*time.Second)) || buckets[2].UserName != "bob" || buckets[2].AppName != "z" {
		t.Fatalf("unexpected third bucket/order: %#v", buckets)
	}
}

func TestMemoryTrafficStoreConcurrentWrites(t *testing.T) {
	store := newMemoryTrafficStore(10 * time.Second)
	base := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	const workers = 8
	const perWorker = 50

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				store.RecordTraffic(TrafficRecord{UserName: "alice", AppName: "web", BytesIn: 1, BytesOut: 2, At: base})
			}
		}()
	}
	wg.Wait()

	buckets, err := store.QueryTraffic(TrafficQuery{UserName: "alice", AppName: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 1 {
		t.Fatalf("expected one bucket, got %d: %#v", len(buckets), buckets)
	}
	want := uint64(workers * perWorker)
	if buckets[0].RequestsTotal != want || buckets[0].BytesIn != want || buckets[0].BytesOut != want*2 {
		t.Fatalf("unexpected concurrent totals: %#v", buckets[0])
	}
}

type captureTrafficStore struct {
	mu      sync.Mutex
	query   TrafficQuery
	buckets []TrafficBucket
	err     error
	records []TrafficRecord
}

func (s *captureTrafficStore) RecordTraffic(record TrafficRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
}

func (s *captureTrafficStore) QueryTraffic(query TrafficQuery) ([]TrafficBucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.query = query
	if s.err != nil {
		return nil, s.err
	}
	return append([]TrafficBucket(nil), s.buckets...), nil
}

func (s *captureTrafficStore) lastRecord(t *testing.T) TrafficRecord {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		t.Fatal("expected traffic record")
	}
	return s.records[len(s.records)-1]
}

func TestUserTrafficEndpointOverridesUserQuery(t *testing.T) {
	store := &captureTrafficStore{}
	srv := &Server{cfg: Config{DisableAuth: true, LocalDevUser: "alice", LocalDevEmail: "alice@example.test"}, traffic: store}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me/traffic?user=bob&app=web", nil)
	srv.handleUserTraffic(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}
	if store.query.UserName != "alice" || store.query.AppName != "web" {
		t.Fatalf("expected authenticated user scoped query, got %#v", store.query)
	}
}

func TestAdminTrafficEndpointRejectsNonAdmin(t *testing.T) {
	srv := &Server{cfg: Config{AdminUsers: map[string]struct{}{}}, traffic: &captureTrafficStore{}}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/admin/traffic", nil)
	req.Header.Set("X-Auth-Request-User", "alice")
	req.Header.Set("X-Auth-Request-Email", "alice@example.test")
	srv.handleAdminTraffic(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestTrafficEndpointsReturnBadRequestForInvalidQuery(t *testing.T) {
	srv := &Server{cfg: Config{DisableAuth: true, LocalDevUser: "alice", LocalDevEmail: "alice@example.test"}, traffic: &captureTrafficStore{}}

	for _, path := range []string{"/api/me/traffic?until=bad", "/api/admin/traffic?since=bad"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		if strings.HasPrefix(path, "/api/me/") {
			srv.handleUserTraffic(rr, req)
		} else {
			srv.handleAdminTraffic(rr, req)
		}
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s: expected bad request, got %d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestTrafficEndpointsReturnStoreErrors(t *testing.T) {
	srv := &Server{cfg: Config{DisableAuth: true, LocalDevUser: "alice", LocalDevEmail: "alice@example.test"}, traffic: &captureTrafficStore{err: errors.New("store failed")}}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/me/traffic", nil)
	srv.handleUserTraffic(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected store error status, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestProxyToAppRecordsAppUnavailable(t *testing.T) {
	store := &captureTrafficStore{}
	srv := &Server{state: State{Apps: map[string]*App{}}, traffic: store}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	srv.proxyToApp(rr, req, "alice", "web")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected not found, got %d body=%s", rr.Code, rr.Body.String())
	}
	record := store.lastRecord(t)
	if record.UserName != "alice" || record.AppName != "web" || record.StatusCode != http.StatusNotFound || !record.Failed {
		t.Fatalf("unexpected traffic record: %#v", record)
	}
}

func TestProxyToAppRecordsClientOffline(t *testing.T) {
	store := &captureTrafficStore{}
	srv := &Server{
		state:   State{Apps: map[string]*App{"alice/web": {UserName: "alice", AppName: "web", Approved: true}}},
		clients: map[string]*TunnelClient{},
		traffic: store,
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	srv.proxyToApp(rr, req, "alice", "web")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected bad gateway, got %d body=%s", rr.Code, rr.Body.String())
	}
	record := store.lastRecord(t)
	if record.StatusCode != http.StatusBadGateway || !record.Failed || record.BytesIn != 0 || record.BytesOut != 0 {
		t.Fatalf("unexpected traffic record: %#v", record)
	}
}

func TestProxyToAppRecordsAppNotConnected(t *testing.T) {
	store := &captureTrafficStore{}
	srv := &Server{
		state:   State{Apps: map[string]*App{"alice/web": {UserName: "alice", AppName: "web", Approved: true}}},
		clients: map[string]*TunnelClient{"alice": {apps: map[string]*ConnectedApp{}}},
		traffic: store,
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	srv.proxyToApp(rr, req, "alice", "web")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected bad gateway, got %d body=%s", rr.Code, rr.Body.String())
	}
	record := store.lastRecord(t)
	if record.StatusCode != http.StatusBadGateway || !record.Failed {
		t.Fatalf("unexpected traffic record: %#v", record)
	}
}

func TestProxyToAppRecordsSuccessfulUpstreamResponse(t *testing.T) {
	store := &captureTrafficStore{}
	srv, cleanup := newProxyTestServer(t, store, func(req TunnelRequest) TunnelResponse {
		if req.AppName != "web" || req.Method != http.MethodPost {
			t.Fatalf("unexpected tunnel request: %#v", req)
		}
		return TunnelResponse{RequestID: req.RequestID, StatusCode: http.StatusCreated, Headers: http.Header{"X-Upstream": []string{"ok"}}, BodyBase64: base64.StdEncoding.EncodeToString([]byte("created"))}
	})
	defer cleanup()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/submit", strings.NewReader("hello"))
	srv.proxyToApp(rr, req, "alice", "web")
	if rr.Code != http.StatusCreated || rr.Body.String() != "created" || rr.Header().Get("X-Upstream") != "ok" {
		t.Fatalf("unexpected response: status=%d headers=%v body=%q", rr.Code, rr.Header(), rr.Body.String())
	}
	record := store.lastRecord(t)
	if record.StatusCode != http.StatusCreated || record.Failed || record.BytesIn != 5 || record.BytesOut != 7 {
		t.Fatalf("unexpected traffic record: %#v", record)
	}
}

func TestProxyToAppRecordsUpstreamError(t *testing.T) {
	store := &captureTrafficStore{}
	srv, cleanup := newProxyTestServer(t, store, func(req TunnelRequest) TunnelResponse {
		return TunnelResponse{RequestID: req.RequestID, Error: "upstream failed"}
	})
	defer cleanup()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/submit", strings.NewReader("hello"))
	srv.proxyToApp(rr, req, "alice", "web")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected bad gateway, got %d body=%s", rr.Code, rr.Body.String())
	}
	record := store.lastRecord(t)
	if record.StatusCode != http.StatusBadGateway || !record.Failed || record.BytesIn != 5 || record.BytesOut != 0 {
		t.Fatalf("unexpected traffic record: %#v", record)
	}
}

func TestProxyToAppRecordsInvalidUpstreamBody(t *testing.T) {
	store := &captureTrafficStore{}
	srv, cleanup := newProxyTestServer(t, store, func(req TunnelRequest) TunnelResponse {
		return TunnelResponse{RequestID: req.RequestID, StatusCode: http.StatusOK, BodyBase64: "not base64"}
	})
	defer cleanup()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.proxyToApp(rr, req, "alice", "web")
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected bad gateway before upstream headers are written, got %d body=%s", rr.Code, rr.Body.String())
	}
	record := store.lastRecord(t)
	if record.StatusCode != http.StatusBadGateway || !record.Failed {
		t.Fatalf("unexpected traffic record: %#v", record)
	}
}

func TestPrepareProxiedResponseInjectsEligibleHTMLGet(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://web-alice.example.test/page", nil)
	upstreamBody := "<!doctype html><html><body><main>Hello</main></body></html>"
	resp := TunnelResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":   []string{"text/html; charset=utf-8"},
			"Content-Length": []string{strconv.Itoa(len(upstreamBody))},
			"ETag":           []string{`"upstream"`},
			"Last-Modified":  []string{"Wed, 21 Oct 2015 07:28:00 GMT"},
			"X-Upstream":     []string{"ok"},
		},
		BodyBase64: base64.StdEncoding.EncodeToString([]byte(upstreamBody)),
	}

	prepared, err := prepareProxiedResponse(req, resp)
	if err != nil {
		t.Fatal(err)
	}
	body := string(prepared.body)
	if prepared.decision != responseDecorationHTMLInject {
		t.Fatalf("expected html injection decision, got %s", prepared.decision)
	}
	if !strings.Contains(body, "Served by Portflare") || strings.Index(body, "Served by Portflare") > strings.Index(strings.ToLower(body), "</body>") {
		t.Fatalf("expected served-by markup before closing body, got %q", body)
	}
	if prepared.headers.Get("X-Upstream") != "ok" || prepared.headers.Get("X-Portflare-Served-By") != "Portflare" {
		t.Fatalf("expected upstream and attribution headers, got %v", prepared.headers)
	}
	if got := prepared.headers.Get("Content-Length"); got != strconv.Itoa(len(prepared.body)) {
		t.Fatalf("expected recalculated content length %d, got %q", len(prepared.body), got)
	}
	if prepared.headers.Get("ETag") != "" || prepared.headers.Get("Last-Modified") != "" {
		t.Fatalf("expected stale validators removed, got %v", prepared.headers)
	}
}

func TestPrepareProxiedResponseHeaderOnlyDecisionMatrix(t *testing.T) {
	htmlBody := "<html><body>Hello</body></html>"
	tests := []struct {
		name       string
		method     string
		status     int
		headers    http.Header
		wantBody   string
		wantNoBody bool
	}{
		{
			name:     "post html",
			method:   http.MethodPost,
			status:   http.StatusOK,
			headers:  http.Header{"Content-Type": []string{"text/html"}},
			wantBody: htmlBody,
		},
		{
			name:       "head html",
			method:     http.MethodHead,
			status:     http.StatusOK,
			headers:    http.Header{"Content-Type": []string{"text/html"}},
			wantNoBody: true,
		},
		{
			name:     "redirect",
			method:   http.MethodGet,
			status:   http.StatusFound,
			headers:  http.Header{"Content-Type": []string{"text/html"}, "Location": []string{"/next"}},
			wantBody: htmlBody,
		},
		{
			name:       "no content",
			method:     http.MethodGet,
			status:     http.StatusNoContent,
			headers:    http.Header{"Content-Type": []string{"text/html"}},
			wantNoBody: true,
		},
		{
			name:       "not modified",
			method:     http.MethodGet,
			status:     http.StatusNotModified,
			headers:    http.Header{"Content-Type": []string{"text/html"}},
			wantNoBody: true,
		},
		{
			name:     "json",
			method:   http.MethodGet,
			status:   http.StatusOK,
			headers:  http.Header{"Content-Type": []string{"application/json"}},
			wantBody: htmlBody,
		},
		{
			name:     "attachment",
			method:   http.MethodGet,
			status:   http.StatusOK,
			headers:  http.Header{"Content-Type": []string{"text/html"}, "Content-Disposition": []string{`attachment; filename="index.html"`}},
			wantBody: htmlBody,
		},
		{
			name:     "encoded html",
			method:   http.MethodGet,
			status:   http.StatusOK,
			headers:  http.Header{"Content-Type": []string{"text/html"}, "Content-Encoding": []string{"gzip"}},
			wantBody: htmlBody,
		},
		{
			name:     "missing content type",
			method:   http.MethodGet,
			status:   http.StatusOK,
			headers:  http.Header{},
			wantBody: htmlBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "https://web-alice.example.test/page", nil)
			resp := TunnelResponse{
				StatusCode: tt.status,
				Headers:    tt.headers,
				BodyBase64: base64.StdEncoding.EncodeToString([]byte(htmlBody)),
			}
			prepared, err := prepareProxiedResponse(req, resp)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.decision != responseDecorationHeaderOnly {
				t.Fatalf("expected header-only decision, got %s", prepared.decision)
			}
			if prepared.headers.Get("X-Portflare-Served-By") != "Portflare" {
				t.Fatalf("expected attribution header, got %v", prepared.headers)
			}
			if strings.Contains(string(prepared.body), "Served by Portflare") {
				t.Fatalf("did not expect visible injection in %q body %q", tt.name, string(prepared.body))
			}
			if tt.wantNoBody {
				if len(prepared.body) != 0 {
					t.Fatalf("expected no response body, got %q", string(prepared.body))
				}
				if prepared.headers.Get("Content-Length") != "" {
					t.Fatalf("expected no content length for bodyless response, got %v", prepared.headers)
				}
				return
			}
			if string(prepared.body) != tt.wantBody {
				t.Fatalf("expected body %q, got %q", tt.wantBody, string(prepared.body))
			}
		})
	}
}

func TestPrepareProxiedResponseSkipsUpgrades(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://web-alice.example.test/socket", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp := TunnelResponse{
		StatusCode: http.StatusSwitchingProtocols,
		Headers:    http.Header{"Connection": []string{"Upgrade"}, "Upgrade": []string{"websocket"}},
		BodyBase64: base64.StdEncoding.EncodeToString(nil),
	}

	prepared, err := prepareProxiedResponse(req, resp)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.decision != responseDecorationSkip {
		t.Fatalf("expected skip decision, got %s", prepared.decision)
	}
	if prepared.headers.Get("X-Portflare-Served-By") != "" {
		t.Fatalf("did not expect attribution headers for upgrade, got %v", prepared.headers)
	}
}

func TestProxyToAppWritesInjectedResponseAfterClassification(t *testing.T) {
	store := &captureTrafficStore{}
	srv, cleanup := newProxyTestServer(t, store, func(req TunnelRequest) TunnelResponse {
		body := "<html><body><h1>Hello</h1></body></html>"
		return TunnelResponse{
			RequestID:  req.RequestID,
			StatusCode: http.StatusOK,
			Headers: http.Header{
				"Content-Type":   []string{"text/html"},
				"Content-Length": []string{strconv.Itoa(len(body))},
				"ETag":           []string{`"old"`},
			},
			BodyBase64: base64.StdEncoding.EncodeToString([]byte(body)),
		}
	})
	defer cleanup()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.proxyToApp(rr, req, "alice", "web")
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "Served by Portflare") {
		t.Fatalf("expected injected served-by markup, got %q", rr.Body.String())
	}
	if rr.Header().Get("ETag") != "" {
		t.Fatalf("expected stale etag removed, got %v", rr.Header())
	}
	if got := rr.Header().Get("Content-Length"); got != strconv.Itoa(rr.Body.Len()) {
		t.Fatalf("expected recalculated content length %d, got %q", rr.Body.Len(), got)
	}
	record := store.lastRecord(t)
	if record.StatusCode != http.StatusOK || record.Failed || record.BytesOut != int64(rr.Body.Len()) {
		t.Fatalf("unexpected traffic record: %#v", record)
	}
}

func newProxyTestServer(t *testing.T, store TrafficStore, respond func(TunnelRequest) TunnelResponse) (*Server, func()) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	ready := make(chan *websocket.Conn, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		ready <- conn
	}))

	url := "ws" + strings.TrimPrefix(wsServer.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		wsServer.Close()
		t.Fatalf("dial websocket: %v", err)
	}
	serverConn := <-ready

	srv := &Server{
		cfg: Config{MaxBodyBytes: 1024, RequestTimeout: time.Second},
		state: State{Apps: map[string]*App{
			"alice/web": {UserName: "alice", AppName: "web", Approved: true},
		}},
		clients: map[string]*TunnelClient{
			"alice": {conn: clientConn, apps: map[string]*ConnectedApp{"web": {appName: "web"}}},
		},
		pending: map[string]*pendingResponse{},
		traffic: store,
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		var req TunnelRequest
		if err := serverConn.ReadJSON(&req); err != nil {
			t.Errorf("read tunnel request: %v", err)
			return
		}
		resp := respond(req)
		srv.pendingMu.Lock()
		pending := srv.pending[req.RequestID]
		srv.pendingMu.Unlock()
		if pending == nil {
			t.Errorf("missing pending request %q", req.RequestID)
			return
		}
		pending.ch <- resp
	}()

	cleanup := func() {
		_ = clientConn.Close()
		wsServer.Close()
		<-done
	}
	return srv, cleanup
}
