package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
