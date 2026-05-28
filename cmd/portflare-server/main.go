package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"log/slog"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"

	protocoltypes "github.com/portflare/protocol/types"
	protocolvalidation "github.com/portflare/protocol/validation"

	"github.com/portflare/server/internal/buildinfo"
)

type Config struct {
	ListenAddr                    string
	PublicBaseDomain              string
	StatePath                     string
	AdminUsers                    map[string]struct{}
	RegistrationOpen              bool
	AllowUserAppApproval          bool
	AutoApproveForUsers           bool
	AutoApproveForAdmins          bool
	ServedByEnabled               bool
	ServedByMode                  string
	ServedByHTMLInjectionEnabled  bool
	ReportAbuseEnabled            bool
	ReportAbuseChallengeMode      string
	ServedByAppDisableAllowed     bool
	ServedByEmergencyForceVisible bool
	TrustedProxyOnly              bool
	DisableAuth                   bool
	LocalDevUser                  string
	LocalDevEmail                 string
	MaxBodyBytes                  int64
	ReadTimeout                   time.Duration
	WriteTimeout                  time.Duration
	IdleTimeout                   time.Duration
	RequestTimeout                time.Duration
	TrafficStatsInterval          time.Duration
}

func loadConfig() Config {
	return Config{
		ListenAddr:                    env("PORTFLARE_SERVER_LISTEN_ADDR", ":8080"),
		PublicBaseDomain:              strings.Trim(strings.ToLower(env("PORTFLARE_BASE_DOMAIN", "reverse.example.test")), "."),
		StatePath:                     env("PORTFLARE_STATE_PATH", "/var/lib/portflare/state.json"),
		AdminUsers:                    parseUserSet(env("PORTFLARE_ADMIN_USERS", "admin"), ","),
		RegistrationOpen:              envBool("PORTFLARE_REGISTRATION_OPEN", true),
		AllowUserAppApproval:          envBool("PORTFLARE_ALLOW_USER_APP_APPROVAL", false),
		AutoApproveForUsers:           envBool("PORTFLARE_AUTO_APPROVE_APPS_FOR_USERS", false),
		AutoApproveForAdmins:          envBool("PORTFLARE_AUTO_APPROVE_APPS_FOR_ADMINS", false),
		ServedByEnabled:               envBool("PORTFLARE_SERVED_BY_ENABLED", true),
		ServedByMode:                  envServedByMode("PORTFLARE_SERVED_BY_MODE", servedByModeVisibleAndHeaders),
		ServedByHTMLInjectionEnabled:  envBool("PORTFLARE_SERVED_BY_HTML_INJECTION_ENABLED", true),
		ReportAbuseEnabled:            envBool("PORTFLARE_REPORT_ABUSE_ENABLED", true),
		ReportAbuseChallengeMode:      envAbuseReportChallengeMode("PORTFLARE_REPORT_ABUSE_CHALLENGE_MODE", abuseReportChallengeOff),
		ServedByAppDisableAllowed:     envBool("PORTFLARE_SERVED_BY_APP_DISABLE_ALLOWED", false),
		ServedByEmergencyForceVisible: envBool("PORTFLARE_SERVED_BY_EMERGENCY_FORCE_VISIBLE", false),
		TrustedProxyOnly:              envBool("PORTFLARE_TRUST_AUTH_HEADERS", true),
		DisableAuth:                   envBool("PORTFLARE_DISABLE_AUTH", false),
		LocalDevUser:                  env("PORTFLARE_LOCAL_DEV_USER", "localdev"),
		LocalDevEmail:                 env("PORTFLARE_LOCAL_DEV_EMAIL", "localdev@example.test"),
		MaxBodyBytes:                  envInt64("PORTFLARE_MAX_BODY_BYTES", 8<<20),
		ReadTimeout:                   envDuration("PORTFLARE_READ_TIMEOUT", 15*time.Second),
		WriteTimeout:                  envDuration("PORTFLARE_WRITE_TIMEOUT", 30*time.Second),
		IdleTimeout:                   envDuration("PORTFLARE_IDLE_TIMEOUT", 120*time.Second),
		RequestTimeout:                envDuration("PORTFLARE_REQUEST_TIMEOUT", 60*time.Second),
		TrafficStatsInterval:          envDuration("PORTFLARE_TRAFFIC_STATS_INTERVAL", 30*time.Second),
	}
}

type State struct {
	RegistrationOpen              bool                    `json:"registration_open"`
	AllowUserAppApproval          bool                    `json:"allow_user_app_approval"`
	AutoApproveForUsers           bool                    `json:"auto_approve_for_users"`
	AutoApproveForAdmins          bool                    `json:"auto_approve_for_admins"`
	ServedByEnabled               bool                    `json:"served_by_enabled"`
	ServedByMode                  string                  `json:"served_by_mode"`
	ServedByHTMLInjectionEnabled  bool                    `json:"served_by_html_injection_enabled"`
	ReportAbuseEnabled            bool                    `json:"report_abuse_enabled"`
	ServedByAppDisableAllowed     bool                    `json:"served_by_app_disable_allowed"`
	ServedByEmergencyForceVisible bool                    `json:"served_by_emergency_force_visible"`
	Users                         map[string]*User        `json:"users"`
	Apps                          map[string]*App         `json:"apps"`
	AbuseReports                  map[string]*AbuseReport `json:"abuse_reports,omitempty"`
	AuditEvents                   []AuditEvent            `json:"audit_events,omitempty"`
}

type User struct {
	UserName          string    `json:"user_name"`
	PublicUserLabel   string    `json:"public_user_label"`
	PublicUserAliases []string  `json:"public_user_aliases,omitempty"`
	Email             string    `json:"email"`
	APIKey            string    `json:"api_key"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type App struct {
	ID                        string    `json:"id"`
	UserName                  string    `json:"user_name"`
	AppName                   string    `json:"app_name"`
	PublicPort                int       `json:"public_port,omitempty"`
	Approved                  bool      `json:"approved"`
	Connected                 bool      `json:"connected"`
	ServedByOverride          string    `json:"served_by_override"`
	ServedByOverrideReason    string    `json:"served_by_override_reason,omitempty"`
	ServedByOverrideUpdatedBy string    `json:"served_by_override_updated_by,omitempty"`
	ServedByOverrideUpdatedAt time.Time `json:"served_by_override_updated_at,omitempty"`
	LastSeenAt                time.Time `json:"last_seen_at"`
	CreatedAt                 time.Time `json:"created_at"`
	UpdatedAt                 time.Time `json:"updated_at"`
}

type AuditEvent struct {
	ID             string    `json:"id"`
	Action         string    `json:"action"`
	ActorUserName  string    `json:"actor_user_name"`
	TargetUserName string    `json:"target_user_name,omitempty"`
	TargetAppName  string    `json:"target_app_name,omitempty"`
	OldValue       string    `json:"old_value,omitempty"`
	NewValue       string    `json:"new_value,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

type AbuseReport struct {
	ID                  string            `json:"id"`
	ReportedURL         string            `json:"reported_url"`
	ReportedHost        string            `json:"reported_host,omitempty"`
	ReportedPath        string            `json:"reported_path,omitempty"`
	ReportedUserName    string            `json:"reported_user_name,omitempty"`
	ReportedUserLabel   string            `json:"reported_user_label,omitempty"`
	ReportedAppName     string            `json:"reported_app_name,omitempty"`
	Category            string            `json:"category"`
	Description         string            `json:"description"`
	Context             string            `json:"context,omitempty"`
	ReporterContact     string            `json:"reporter_contact,omitempty"`
	ReporterContactHash string            `json:"reporter_contact_hash,omitempty"`
	ReporterIP          string            `json:"reporter_ip"`
	ReporterUserAgent   string            `json:"reporter_user_agent,omitempty"`
	ReporterCount       int               `json:"reporter_count,omitempty"`
	CategoryCounts      map[string]int    `json:"category_counts,omitempty"`
	Status              string            `json:"status"`
	StatusUpdatedBy     string            `json:"status_updated_by,omitempty"`
	StatusUpdatedAt     time.Time         `json:"status_updated_at,omitempty"`
	InternalNotes       []AbuseReportNote `json:"internal_notes,omitempty"`
	CreatedAt           time.Time         `json:"created_at"`
	UpdatedAt           time.Time         `json:"updated_at"`
}

type AbuseReportNote struct {
	ID            string    `json:"id"`
	Body          string    `json:"body"`
	ActorUserName string    `json:"actor_user_name"`
	CreatedAt     time.Time `json:"created_at"`
}

type authIdentity struct {
	UserName        string
	PublicUserLabel string
	Email           string
	IsAdmin         bool
}

type TunnelRequest = protocoltypes.TunnelRequest

type TunnelResponse = protocoltypes.TunnelResponse

type TunnelClient struct {
	userName string
	email    string
	conn     *websocket.Conn
	sendMu   sync.Mutex
	apps     map[string]*ConnectedApp
}

type ConnectedApp struct {
	appName    string
	publicPort int
}

type pendingResponse struct {
	ch chan TunnelResponse
}

type RegistrationRequest struct {
	UserName string `json:"user_name"`
	Email    string `json:"email"`
}

type RegistrationResponse struct {
	UserName        string `json:"user_name"`
	PublicUserLabel string `json:"public_user_label"`
	Email           string `json:"email,omitempty"`
	APIKey          string `json:"api_key"`
}

type TrafficRecord struct {
	UserName   string
	AppName    string
	StatusCode int
	BytesIn    int64
	BytesOut   int64
	Duration   time.Duration
	Failed     bool
	At         time.Time
}

type TrafficBucket struct {
	StartAt           time.Time `json:"start_at"`
	EndAt             time.Time `json:"end_at"`
	UserName          string    `json:"user_name"`
	AppName           string    `json:"app_name"`
	RequestsTotal     uint64    `json:"requests_total"`
	RequestsSucceeded uint64    `json:"requests_succeeded"`
	RequestsFailed    uint64    `json:"requests_failed"`
	BytesIn           uint64    `json:"bytes_in"`
	BytesOut          uint64    `json:"bytes_out"`
	DurationTotalMs   uint64    `json:"duration_total_ms"`
	LastRequestAt     time.Time `json:"last_request_at"`
	LastStatusCode    int       `json:"last_status_code,omitempty"`
}

type TrafficQuery struct {
	UserName string
	AppName  string
	Since    time.Time
	Until    time.Time
}

type TrafficStore interface {
	RecordTraffic(record TrafficRecord)
	QueryTraffic(query TrafficQuery) ([]TrafficBucket, error)
}

type memoryTrafficStore struct {
	mu       sync.RWMutex
	interval time.Duration
	buckets  map[string]*TrafficBucket
}

func newMemoryTrafficStore(interval time.Duration) *memoryTrafficStore {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &memoryTrafficStore{interval: interval, buckets: map[string]*TrafficBucket{}}
}

func (s *memoryTrafficStore) RecordTraffic(record TrafficRecord) {
	if record.UserName == "" || record.AppName == "" {
		return
	}
	if record.At.IsZero() {
		record.At = time.Now().UTC()
	}
	start := record.At.UTC().Truncate(s.interval)
	key := fmt.Sprintf("%s|%s|%d", record.UserName, record.AppName, start.UnixNano())

	s.mu.Lock()
	bucket := s.buckets[key]
	if bucket == nil {
		bucket = &TrafficBucket{StartAt: start, EndAt: start.Add(s.interval), UserName: record.UserName, AppName: record.AppName}
		s.buckets[key] = bucket
	}
	bucket.RequestsTotal++
	if record.Failed {
		bucket.RequestsFailed++
	} else {
		bucket.RequestsSucceeded++
	}
	if record.BytesIn > 0 {
		bucket.BytesIn += uint64(record.BytesIn)
	}
	if record.BytesOut > 0 {
		bucket.BytesOut += uint64(record.BytesOut)
	}
	if record.Duration > 0 {
		bucket.DurationTotalMs += uint64(record.Duration.Milliseconds())
	}
	bucket.LastRequestAt = record.At.UTC()
	bucket.LastStatusCode = record.StatusCode
	s.mu.Unlock()
}

func (s *memoryTrafficStore) QueryTraffic(query TrafficQuery) ([]TrafficBucket, error) {
	s.mu.RLock()
	out := make([]TrafficBucket, 0, len(s.buckets))
	for _, bucket := range s.buckets {
		if query.UserName != "" && bucket.UserName != query.UserName {
			continue
		}
		if query.AppName != "" && bucket.AppName != query.AppName {
			continue
		}
		if !query.Since.IsZero() && bucket.EndAt.Before(query.Since) {
			continue
		}
		if !query.Until.IsZero() && bucket.StartAt.After(query.Until) {
			continue
		}
		if bucket.RequestsTotal == 0 {
			continue
		}
		out = append(out, *bucket)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartAt.Equal(out[j].StartAt) {
			if out[i].UserName == out[j].UserName {
				return out[i].AppName < out[j].AppName
			}
			return out[i].UserName < out[j].UserName
		}
		return out[i].StartAt.Before(out[j].StartAt)
	})
	return out, nil
}

type Server struct {
	cfg       Config
	logger    *slog.Logger
	upgrader  websocket.Upgrader
	templates *template.Template

	stateMu sync.RWMutex
	state   State

	clientsMu sync.RWMutex
	clients   map[string]*TunnelClient

	pendingMu sync.Mutex
	pending   map[string]*pendingResponse

	listenersMu sync.Mutex
	listeners   map[int]net.Listener

	uiSubsMu      sync.Mutex
	uiSubscribers map[chan struct{}]struct{}

	traffic      TrafficStore
	abuseLimiter *abuseReportLimiter
}

const (
	minPublicUserLabelLen = 3
	maxPublicUserLabelLen = 32
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-version", "-v":
			fmt.Println(buildinfo.Summary("portflare-server"))
			return
		case "help", "--help", "-h":
			fmt.Println("usage:")
			fmt.Println("  portflare-server")
			fmt.Println("  portflare-server version")
			return
		}
	}

	cfg := loadConfig()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	srv, err := newServer(cfg, logger)
	if err != nil {
		logger.Error("failed to initialize server", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      srv.routes(),
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
		IdleTimeout:  cfg.IdleTimeout,
	}

	go func() {
		version, commit, buildDate := buildinfo.Effective()
		logger.Info("portflare server listening", "addr", cfg.ListenAddr, "base_domain", cfg.PublicBaseDomain, "version", version, "commit", commit, "build_date", buildDate)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			os.Exit(1)
		}
	}()

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	srv.closeDynamicListeners()
}

func newServer(cfg Config, logger *slog.Logger) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.StatePath), 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	s := &Server{
		cfg:    cfg,
		logger: logger,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients:       map[string]*TunnelClient{},
		pending:       map[string]*pendingResponse{},
		listeners:     map[int]net.Listener{},
		uiSubscribers: map[chan struct{}]struct{}{},
		traffic:       newMemoryTrafficStore(cfg.TrafficStatsInterval),
		abuseLimiter:  newAbuseReportLimiter(abuseReportLimitPerWindow, abuseReportThrottleWindow),
	}

	if err := s.loadState(); err != nil {
		return nil, err
	}

	tpls, err := template.New("pages").Parse(dashboardTemplates)
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.templates = tpls

	return s, nil
}

func (s *Server) loadState() error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	raw, err := os.ReadFile(s.cfg.StatePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			settings := defaultServedBySettingsFromConfig(s.cfg)
			s.state = State{
				RegistrationOpen:              s.cfg.RegistrationOpen,
				AllowUserAppApproval:          s.cfg.AllowUserAppApproval,
				AutoApproveForUsers:           s.cfg.AutoApproveForUsers,
				AutoApproveForAdmins:          s.cfg.AutoApproveForAdmins,
				ServedByEnabled:               settings.Enabled,
				ServedByMode:                  settings.Mode,
				ServedByHTMLInjectionEnabled:  settings.HTMLInjectionEnabled,
				ReportAbuseEnabled:            settings.ReportAbuseEnabled,
				ServedByAppDisableAllowed:     settings.AppDisableAllowed,
				ServedByEmergencyForceVisible: settings.EmergencyForceVisible,
				Users:                         map[string]*User{},
				Apps:                          map[string]*App{},
				AbuseReports:                  map[string]*AbuseReport{},
			}
			return s.saveStateLocked()
		}
		return fmt.Errorf("read state: %w", err)
	}

	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return fmt.Errorf("decode state: %w", err)
	}
	var rawState map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawState); err != nil {
		return fmt.Errorf("decode state metadata: %w", err)
	}
	if st.Users == nil {
		st.Users = map[string]*User{}
	}
	if st.Apps == nil {
		st.Apps = map[string]*App{}
	}
	if st.AbuseReports == nil {
		st.AbuseReports = map[string]*AbuseReport{}
	}
	changed := false
	defaultSettings := defaultServedBySettingsFromConfig(s.cfg)
	if _, ok := rawState["served_by_enabled"]; !ok {
		st.ServedByEnabled = defaultSettings.Enabled
		changed = true
	}
	if _, ok := rawState["served_by_mode"]; !ok {
		st.ServedByMode = defaultSettings.Mode
		changed = true
	} else if mode, ok := normalizeServedByMode(st.ServedByMode); ok {
		if mode != st.ServedByMode {
			st.ServedByMode = mode
			changed = true
		}
	} else {
		st.ServedByMode = defaultSettings.Mode
		changed = true
	}
	if _, ok := rawState["served_by_html_injection_enabled"]; !ok {
		st.ServedByHTMLInjectionEnabled = defaultSettings.HTMLInjectionEnabled
		changed = true
	}
	if _, ok := rawState["report_abuse_enabled"]; !ok {
		st.ReportAbuseEnabled = defaultSettings.ReportAbuseEnabled
		changed = true
	}
	if _, ok := rawState["served_by_app_disable_allowed"]; !ok {
		st.ServedByAppDisableAllowed = defaultSettings.AppDisableAllowed
		changed = true
	}
	if _, ok := rawState["served_by_emergency_force_visible"]; !ok {
		st.ServedByEmergencyForceVisible = defaultSettings.EmergencyForceVisible
		changed = true
	}
	seenLabels := map[string]string{}
	for key, user := range st.Users {
		if user.PublicUserLabel == "" {
			user.PublicUserLabel = userLabel(user.UserName)
			changed = true
		}
		user.PublicUserAliases = uniqueUserLabels(user.PublicUserAliases)
		if other, ok := seenLabels[user.PublicUserLabel]; ok && other != key {
			return fmt.Errorf("duplicate public user label %q in state for users %q and %q", user.PublicUserLabel, other, key)
		}
		seenLabels[user.PublicUserLabel] = key
		for _, alias := range user.PublicUserAliases {
			if alias == user.PublicUserLabel {
				changed = true
				continue
			}
			if other, ok := seenLabels[alias]; ok && other != key {
				return fmt.Errorf("duplicate public user alias %q in state for users %q and %q", alias, other, key)
			}
			seenLabels[alias] = key
		}
	}
	for _, app := range st.Apps {
		override, ok := normalizeServedByAppOverride(app.ServedByOverride)
		if !ok {
			override = servedByAppOverrideInherit
		}
		if app.ServedByOverride != override {
			app.ServedByOverride = override
			changed = true
		}
		if override != servedByAppOverrideHeadersOnly && override != servedByAppOverrideDisabled {
			if app.ServedByOverrideReason != "" {
				app.ServedByOverrideReason = ""
				changed = true
			}
		}
	}
	for _, report := range st.AbuseReports {
		if report == nil {
			continue
		}
		if report.ReporterCount <= 0 {
			report.ReporterCount = 1
			changed = true
		}
		if report.CategoryCounts == nil {
			report.CategoryCounts = map[string]int{}
			changed = true
		}
		if report.Category != "" && report.CategoryCounts[report.Category] == 0 {
			report.CategoryCounts[report.Category] = report.ReporterCount
			changed = true
		}
		if report.ReporterContactHash == "" {
			if hash, ok := reporterEmailHash(report.ReporterContact); ok {
				report.ReporterContactHash = hash
				changed = true
			}
		}
	}
	s.state = st
	if changed {
		return s.saveStateLocked()
	}
	return nil
}

func (s *Server) saveStateLocked() error {
	tmp := s.cfg.StatePath + ".tmp"
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write state tmp: %w", err)
	}
	if err := os.Rename(tmp, s.cfg.StatePath); err != nil {
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("/readyz", handleReadyz("portflare-server"))
	mux.HandleFunc(learnMorePath, s.handleLearnMorePage)
	mux.HandleFunc(reportPath, s.handleReportAbuseForm)
	mux.HandleFunc("/api/report-abuse", s.handleReportAbuseAPI)
	mux.HandleFunc("/api/register", s.handleRegister)
	mux.HandleFunc("/connect", s.handleConnect)
	mux.HandleFunc("/ws/ui", s.handleUIWebSocket)
	mux.HandleFunc("/admin/abuse-reports/", s.handleAdminAbuseReportPage)
	mux.HandleFunc("/admin", s.handleAdminPage)
	mux.HandleFunc("/api/admin/abuse-reports/", s.handleAdminAbuseReport)
	mux.HandleFunc("/api/admin/abuse-reports", s.handleAdminAbuseReports)
	mux.HandleFunc("/api/admin/state", s.handleAdminState)
	mux.HandleFunc("/api/admin/traffic", s.handleAdminTraffic)
	mux.HandleFunc("/admin/toggle-registration", s.handleToggleRegistration)
	mux.HandleFunc("/admin/toggle-setting", s.handleToggleSetting)
	mux.HandleFunc("/api/admin/app-served-by-override", s.handleAdminAppServedByOverride)
	mux.HandleFunc("/api/admin/approve", s.handleApproveApp)
	mux.HandleFunc("/me", s.handleUserPage)
	mux.HandleFunc("/api/me/state", s.handleUserState)
	mux.HandleFunc("/api/me/traffic", s.handleUserTraffic)
	mux.HandleFunc("/api/me/approve", s.handleApproveApp)
	mux.HandleFunc("/api/me/rotate-key", s.handleRotateKey)
	mux.HandleFunc("/api/me/public-user-label", s.handleUpdatePublicUserLabel)
	mux.HandleFunc("/", s.handleHostAware)
	return withLogging(s.logger, mux)
}

func (s *Server) handleUIWebSocket(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("ui websocket upgrade failed", "error", err)
		return
	}
	defer conn.Close()

	sub := make(chan struct{}, 1)
	s.uiSubsMu.Lock()
	s.uiSubscribers[sub] = struct{}{}
	s.uiSubsMu.Unlock()
	defer func() {
		s.uiSubsMu.Lock()
		delete(s.uiSubscribers, sub)
		s.uiSubsMu.Unlock()
	}()

	_ = conn.WriteJSON(map[string]any{"type": "hello", "user": identity.UserName})
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sub:
			if err := conn.WriteJSON(map[string]any{"type": "refresh"}); err != nil {
				return
			}
		case <-ticker.C:
			if err := conn.WriteJSON(map[string]any{"type": "ping"}); err != nil {
				return
			}
		}
	}
}

func (s *Server) notifyUISubscribers() {
	s.uiSubsMu.Lock()
	defer s.uiSubsMu.Unlock()
	for ch := range s.uiSubscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (s *Server) handleHostAware(w http.ResponseWriter, r *http.Request) {
	host := canonicalHost(r.Host)
	if host == "" {
		http.NotFound(w, r)
		return
	}

	if host == canonicalHost("admin."+s.cfg.PublicBaseDomain) {
		s.handleAdminPage(w, r)
		return
	}

	if user, app, redirectHost, ok := s.matchAppHost(host); ok {
		if redirectHost != "" {
			http.Redirect(w, r, rewriteRequestURLHost(r, redirectHost), http.StatusTemporaryRedirect)
			return
		}
		s.proxyToApp(w, r, user, app)
		return
	}

	if userLabelHost, ok := s.matchUserHost(host); ok {
		identity, ok := s.requireIdentity(w, r)
		if !ok {
			return
		}
		matchedUser, found, canonical := s.findUserByAnyPublicLabel(userLabelHost)
		if !found {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		if identity.PublicUserLabel != matchedUser.PublicUserLabel && !identity.IsAdmin {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		if !canonical {
			http.Redirect(w, r, rewriteRequestURLHost(r, matchedUser.PublicUserLabel+"."+s.cfg.PublicBaseDomain), http.StatusTemporaryRedirect)
			return
		}
		s.renderUserPage(w, r, identity, matchedUser.UserName)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin") {
		s.handleAdminPage(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/me") {
		s.handleUserPage(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/r/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/r/"), "/")
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		s.proxyToApp(w, r, slug(parts[0]), slug(parts[1]))
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service":            "portflare",
		"base_domain":        s.cfg.PublicBaseDomain,
		"admin_url_example":  fmt.Sprintf("https://admin.%s", s.cfg.PublicBaseDomain),
		"user_url_example":   fmt.Sprintf("https://<user-label>.%s", s.cfg.PublicBaseDomain),
		"app_url_example":    fmt.Sprintf("https://<app>-<user-label>.%s", s.cfg.PublicBaseDomain),
		"local_path_example": "/r/<user>/<app>",
	})
}

func (s *Server) handleLearnMorePage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	settings := s.currentServedBySettings()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.templates.ExecuteTemplate(w, "learn_more", map[string]any{
		"BaseDomain":         s.cfg.PublicBaseDomain,
		"ReportAbuseEnabled": settings.ReportAbuseEnabled,
		"ReportAbuseURL":     reportPath,
	})
}

func (s *Server) handleReportAbuseForm(w http.ResponseWriter, r *http.Request) {
	if !s.currentServedBySettings().ReportAbuseEnabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.templates.ExecuteTemplate(w, "report_abuse", map[string]any{
		"ReportedURL":   strings.TrimSpace(r.URL.Query().Get("url")),
		"Context":       strings.TrimSpace(r.URL.Query().Get("context")),
		"Categories":    abuseReportCategories(),
		"FormStartedAt": strconv.FormatInt(time.Now().UTC().Unix(), 10),
	})
}

type abuseReportInput struct {
	ReportedURL      string `json:"reported_url"`
	Category         string `json:"category"`
	Description      string `json:"description"`
	ReporterContact  string `json:"reporter_contact"`
	Context          string `json:"context"`
	Website          string `json:"website"`
	FormStartedAt    string `json:"form_started_at"`
	ChallengeToken   string `json:"challenge_token"`
	ProofOfWorkNonce string `json:"pow_nonce"`
}

type resolvedReportedURL struct {
	URL       string
	Host      string
	Path      string
	UserName  string
	UserLabel string
	AppName   string
}

func (s *Server) handleReportAbuseAPI(w http.ResponseWriter, r *http.Request) {
	if !s.currentServedBySettings().ReportAbuseEnabled {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	input, status, err := parseAbuseReportInput(r)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if strings.TrimSpace(input.Website) != "" {
		writeError(w, http.StatusBadRequest, "invalid report")
		return
	}

	resolved, err := s.validateAbuseReportInput(input, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.validateAbuseReportChallenge(input, resolved); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	reporterIP := requestClientIP(r)
	if reporterIP == "" {
		reporterIP = "unknown"
	}
	limiter := s.ensureAbuseLimiter()
	if !limiter.AllowAll(abuseReportRateLimitKeys(reporterIP, resolved, input.ReporterContact)) {
		writeError(w, http.StatusTooManyRequests, "too many reports; try again later")
		return
	}

	now := time.Now().UTC()
	reporterContact := strings.TrimSpace(input.ReporterContact)
	reporterContactHash, _ := reporterEmailHash(reporterContact)
	category := strings.TrimSpace(input.Category)
	report := &AbuseReport{
		ReportedURL:         resolved.URL,
		ReportedHost:        resolved.Host,
		ReportedPath:        resolved.Path,
		ReportedUserName:    resolved.UserName,
		ReportedUserLabel:   resolved.UserLabel,
		ReportedAppName:     resolved.AppName,
		Category:            category,
		Description:         strings.TrimSpace(input.Description),
		Context:             strings.TrimSpace(input.Context),
		ReporterContact:     reporterContact,
		ReporterContactHash: reporterContactHash,
		ReporterIP:          reporterIP,
		ReporterUserAgent:   strings.TrimSpace(r.UserAgent()),
		ReporterCount:       1,
		CategoryCounts:      map[string]int{category: 1},
		Status:              abuseReportStatusNew,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	s.stateMu.Lock()
	if s.state.AbuseReports == nil {
		s.state.AbuseReports = map[string]*AbuseReport{}
	}
	if existing := s.findDuplicateAbuseReportLocked(report); existing != nil {
		coalesceDuplicateAbuseReport(existing, report, now)
		report = existing
	} else {
		report.ID = s.newAbuseReportIDLocked()
		s.state.AbuseReports[report.ID] = report
	}
	err = s.saveStateLocked()
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save report")
		return
	}
	s.notifyUISubscribers()

	writeJSON(w, http.StatusCreated, map[string]string{"case_id": report.ID})
}

func parseAbuseReportInput(r *http.Request) (abuseReportInput, int, error) {
	var input abuseReportInput
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAbuseReportBodyBytes+1))
	if err != nil {
		return input, http.StatusBadRequest, errors.New("invalid report request")
	}
	if int64(len(raw)) > maxAbuseReportBodyBytes {
		return input, http.StatusRequestEntityTooLarge, errors.New("report body is too large")
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if contentType == "application/json" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if err := dec.Decode(&input); err != nil {
			return input, http.StatusBadRequest, errors.New("invalid report request")
		}
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			return input, http.StatusBadRequest, errors.New("invalid report request")
		}
		return input, http.StatusOK, nil
	}

	r.Body = io.NopCloser(bytes.NewReader(raw))
	if err := r.ParseForm(); err != nil {
		return input, http.StatusBadRequest, errors.New("invalid report request")
	}
	input = abuseReportInput{
		ReportedURL:      r.Form.Get("reported_url"),
		Category:         r.Form.Get("category"),
		Description:      r.Form.Get("description"),
		ReporterContact:  r.Form.Get("reporter_contact"),
		Context:          r.Form.Get("context"),
		Website:          r.Form.Get("website"),
		FormStartedAt:    r.Form.Get("form_started_at"),
		ChallengeToken:   r.Form.Get("challenge_token"),
		ProofOfWorkNonce: r.Form.Get("pow_nonce"),
	}
	return input, http.StatusOK, nil
}

func (s *Server) validateAbuseReportInput(input abuseReportInput, r *http.Request) (resolvedReportedURL, error) {
	reportedURL := strings.TrimSpace(input.ReportedURL)
	category := strings.TrimSpace(input.Category)
	description := strings.TrimSpace(input.Description)
	contact := strings.TrimSpace(input.ReporterContact)
	contextValue := strings.TrimSpace(input.Context)

	if reportedURL == "" {
		return resolvedReportedURL{}, errors.New("reported_url is required")
	}
	if len(reportedURL) > maxAbuseReportURLLen {
		return resolvedReportedURL{}, fmt.Errorf("reported_url must be at most %d bytes", maxAbuseReportURLLen)
	}
	if !isAllowedAbuseReportCategory(category) {
		return resolvedReportedURL{}, errors.New("category is invalid")
	}
	if description == "" {
		return resolvedReportedURL{}, errors.New("description is required")
	}
	if len(description) > maxAbuseReportDescriptionLen {
		return resolvedReportedURL{}, fmt.Errorf("description must be at most %d bytes", maxAbuseReportDescriptionLen)
	}
	if len(contact) > maxAbuseReportContactLen {
		return resolvedReportedURL{}, fmt.Errorf("reporter_contact must be at most %d bytes", maxAbuseReportContactLen)
	}
	if strings.ContainsAny(contact, "\r\n") {
		return resolvedReportedURL{}, errors.New("reporter_contact is invalid")
	}
	if len(contextValue) > maxAbuseReportContextLen {
		return resolvedReportedURL{}, fmt.Errorf("context must be at most %d bytes", maxAbuseReportContextLen)
	}
	if err := validateAbuseReportSubmitTiming(input.FormStartedAt); err != nil {
		return resolvedReportedURL{}, err
	}

	resolved, err := s.resolveReportedURL(reportedURL, r)
	if err != nil {
		return resolvedReportedURL{}, err
	}
	return resolved, nil
}

func abuseReportCategories() []map[string]string {
	return []map[string]string{
		{"Value": "phishing", "Label": "Phishing or credential theft"},
		{"Value": "malware", "Label": "Malware or harmful downloads"},
		{"Value": "child_safety", "Label": "Child safety or CSAM"},
		{"Value": "harassment", "Label": "Harassment or threats"},
		{"Value": "spam", "Label": "Spam or scams"},
		{"Value": "copyright", "Label": "Copyright or IP concern"},
		{"Value": "privacy", "Label": "Privacy leak"},
		{"Value": "other", "Label": "Other abuse"},
	}
}

func isAllowedAbuseReportCategory(category string) bool {
	for _, item := range abuseReportCategories() {
		if item["Value"] == category {
			return true
		}
	}
	return false
}

func (s *Server) resolveReportedURL(raw string, r *http.Request) (resolvedReportedURL, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "/") {
		parsed, err := neturl.ParseRequestURI(raw)
		if err != nil || parsed.Path == "" {
			return resolvedReportedURL{}, errors.New("reported_url is invalid")
		}
		resolved, err := s.resolveReportedRoute(parsed.Path)
		if err != nil {
			return resolvedReportedURL{}, err
		}
		absolute := s.publicServiceBaseURL(r) + parsed.RequestURI()
		resolved.URL = absolute
		resolved.Host = canonicalHost(s.cfg.PublicBaseDomain)
		return resolved, nil
	}

	parsed, err := neturl.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return resolvedReportedURL{}, errors.New("reported_url is invalid")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return resolvedReportedURL{}, errors.New("reported_url must be http or https")
	}
	host := canonicalHost(parsed.Host)
	if !s.isPortflareHost(host) {
		return resolvedReportedURL{}, errors.New("reported_url must be a Portflare URL")
	}
	parsed.Host = host
	parsed.Fragment = ""

	resolved := resolvedReportedURL{URL: parsed.String(), Host: host, Path: parsed.EscapedPath()}
	if resolved.Path == "" {
		resolved.Path = "/"
	}
	if route, err := s.resolveReportedRoute(parsed.Path); err == nil {
		route.URL = resolved.URL
		route.Host = host
		return route, nil
	}
	if userName, appName, _, ok := s.matchAppHost(host); ok {
		resolved.UserName = userName
		resolved.AppName = appName
		s.stateMu.RLock()
		if user := s.state.Users[userName]; user != nil {
			resolved.UserLabel = user.PublicUserLabel
		}
		s.stateMu.RUnlock()
	}
	return resolved, nil
}

func (s *Server) isPortflareHost(host string) bool {
	base := canonicalHost(s.cfg.PublicBaseDomain)
	return host == base || strings.HasSuffix(host, "."+base)
}

func (s *Server) resolveReportedRoute(path string) (resolvedReportedURL, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/r/"), "/")
	if !strings.HasPrefix(path, "/r/") || len(parts) < 2 || slug(parts[0]) == "" || slug(parts[1]) == "" {
		return resolvedReportedURL{}, errors.New("reported_url must be under /r/<user>/<app> or the Portflare base domain")
	}
	userName := slug(parts[0])
	appName := slug(parts[1])
	resolved := resolvedReportedURL{Path: path, UserName: userName, AppName: appName}
	s.stateMu.RLock()
	if user := s.state.Users[userName]; user != nil {
		resolved.UserLabel = user.PublicUserLabel
	}
	s.stateMu.RUnlock()
	return resolved, nil
}

func validateAbuseReportSubmitTiming(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	startedUnix, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return errors.New("invalid report")
	}
	startedAt := time.Unix(startedUnix, 0).UTC()
	now := time.Now().UTC()
	if startedAt.After(now.Add(time.Minute)) {
		return errors.New("invalid report")
	}
	if now.Sub(startedAt) < minAbuseReportSubmitDelay {
		return errors.New("invalid report")
	}
	return nil
}

func (s *Server) validateAbuseReportChallenge(input abuseReportInput, resolved resolvedReportedURL) error {
	mode, ok := normalizeAbuseReportChallengeMode(s.cfg.ReportAbuseChallengeMode)
	if !ok {
		mode = abuseReportChallengeOff
	}
	switch mode {
	case abuseReportChallengeOff:
		return nil
	case abuseReportChallengeCaptcha:
		if strings.TrimSpace(input.ChallengeToken) == "" {
			return errors.New("report verification is required")
		}
		return nil
	case abuseReportChallengeProofOfWork:
		if !validAbuseReportProofOfWork(resolved.URL, input.ProofOfWorkNonce) {
			return errors.New("report verification is required")
		}
		return nil
	default:
		return nil
	}
}

func validAbuseReportProofOfWork(reportedURL, nonce string) bool {
	nonce = strings.TrimSpace(nonce)
	if nonce == "" || len(nonce) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(reportedURL + "|" + nonce))
	return strings.HasPrefix(hex.EncodeToString(sum[:]), abuseReportProofOfWorkPrefix)
}

func abuseReportRateLimitKeys(reporterIP string, resolved resolvedReportedURL, reporterContact string) []string {
	keys := make([]string, 0, 5)
	if reporterIP = strings.TrimSpace(reporterIP); reporterIP != "" {
		keys = appendUniqueString(keys, "ip:"+reporterIP)
	}
	if resolved.URL != "" {
		keys = appendUniqueString(keys, "url:"+resolved.URL)
	}
	if resolved.Host != "" {
		keys = appendUniqueString(keys, "host:"+resolved.Host)
	}
	if resolved.UserName != "" && resolved.AppName != "" {
		keys = appendUniqueString(keys, "route:"+resolved.UserName+"/"+resolved.AppName)
	}
	if hash, ok := reporterEmailHash(reporterContact); ok {
		keys = appendUniqueString(keys, "email:"+hash)
	}
	return keys
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func reporterEmailHash(contact string) (string, bool) {
	email := strings.ToLower(strings.TrimSpace(contact))
	if email == "" || !strings.Contains(email, "@") || strings.ContainsAny(email, "\r\n") {
		return "", false
	}
	sum := sha256.Sum256([]byte(email))
	return hex.EncodeToString(sum[:]), true
}

func (s *Server) findDuplicateAbuseReportLocked(report *AbuseReport) *AbuseReport {
	var duplicate *AbuseReport
	for _, existing := range s.state.AbuseReports {
		if !isDuplicateAbuseReport(existing, report) {
			continue
		}
		if duplicate == nil || existing.CreatedAt.Before(duplicate.CreatedAt) || (existing.CreatedAt.Equal(duplicate.CreatedAt) && existing.ID < duplicate.ID) {
			duplicate = existing
		}
	}
	return duplicate
}

func isDuplicateAbuseReport(existing, incoming *AbuseReport) bool {
	if existing == nil || incoming == nil || !abuseReportCanAcceptDuplicate(existing) {
		return false
	}
	if existing.ReportedURL != "" && incoming.ReportedURL != "" {
		return existing.ReportedURL == incoming.ReportedURL
	}
	if existing.ReportedUserName != "" && existing.ReportedAppName != "" && incoming.ReportedUserName != "" && incoming.ReportedAppName != "" {
		return existing.ReportedUserName == incoming.ReportedUserName &&
			existing.ReportedAppName == incoming.ReportedAppName &&
			existing.ReportedPath == incoming.ReportedPath
	}
	return false
}

func abuseReportCanAcceptDuplicate(report *AbuseReport) bool {
	switch abuseReportStatusOrDefault(report.Status) {
	case abuseReportStatusRejected, abuseReportStatusActionedMitigated, abuseReportStatusClosed:
		return false
	default:
		return true
	}
}

func coalesceDuplicateAbuseReport(existing, incoming *AbuseReport, now time.Time) {
	if existing.ReporterCount <= 0 {
		existing.ReporterCount = 1
	}
	existing.ReporterCount++
	if existing.CategoryCounts == nil {
		existing.CategoryCounts = map[string]int{}
	}
	if existing.Category != "" && existing.CategoryCounts[existing.Category] == 0 {
		existing.CategoryCounts[existing.Category] = 1
	}
	if incoming.Category != "" {
		existing.CategoryCounts[incoming.Category]++
	}
	if existing.ReporterContactHash == "" && incoming.ReporterContactHash != "" {
		existing.ReporterContactHash = incoming.ReporterContactHash
	}
	existing.UpdatedAt = now
}

func requestClientIP(r *http.Request) string {
	for _, raw := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
		if ip := strings.TrimSpace(raw); ip != "" {
			return ip
		}
	}
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
		return ip
	}
	host := strings.TrimSpace(r.RemoteAddr)
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		return parsedHost
	}
	return host
}

func (s *Server) ensureAbuseLimiter() *abuseReportLimiter {
	if s.abuseLimiter != nil {
		return s.abuseLimiter
	}
	s.abuseLimiter = newAbuseReportLimiter(abuseReportLimitPerWindow, abuseReportThrottleWindow)
	return s.abuseLimiter
}

func (s *Server) newAbuseReportIDLocked() string {
	for {
		id := "abr_" + randomToken(8)
		if _, exists := s.state.AbuseReports[id]; !exists {
			return id
		}
	}
}

type abuseReportLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string][]time.Time
	now     func() time.Time
}

func newAbuseReportLimiter(limit int, window time.Duration) *abuseReportLimiter {
	if limit <= 0 {
		limit = abuseReportLimitPerWindow
	}
	if window <= 0 {
		window = abuseReportThrottleWindow
	}
	return &abuseReportLimiter{
		limit:   limit,
		window:  window,
		entries: map[string][]time.Time{},
		now:     func() time.Time { return time.Now().UTC() },
	}
}

func (l *abuseReportLimiter) Allow(key string) bool {
	return l.AllowAll([]string{key})
}

func (l *abuseReportLimiter) AllowAll(keys []string) bool {
	if l == nil {
		return true
	}
	unique := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		unique = appendUniqueString(unique, key)
	}
	if len(unique) == 0 {
		return true
	}
	now := l.now()
	cutoff := now.Add(-l.window)

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range unique {
		recent := l.entries[key][:0]
		for _, at := range l.entries[key] {
			if at.After(cutoff) {
				recent = append(recent, at)
			}
		}
		l.entries[key] = recent
		if len(recent) >= l.limit {
			return false
		}
	}
	for _, key := range unique {
		l.entries[key] = append(l.entries[key], now)
	}
	return true
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req RegistrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid registration request")
		return
	}

	userName := slug(req.UserName)
	if userName == "" {
		writeError(w, http.StatusBadRequest, "user_name is required")
		return
	}
	publicUserLabel := userLabel(userName)
	if _, err := validateNormalizedPublicUserLabel(publicUserLabel); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	email := strings.TrimSpace(strings.ToLower(req.Email))
	now := time.Now().UTC()

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.state.Users == nil {
		s.state.Users = map[string]*User{}
	}
	if !s.state.RegistrationOpen {
		writeError(w, http.StatusForbidden, "registration is closed")
		return
	}
	if _, ok := s.state.Users[userName]; ok {
		writeError(w, http.StatusConflict, "user already exists")
		return
	}
	for _, existing := range s.state.Users {
		if existing.UserName == userName {
			continue
		}
		if existing.PublicUserLabel == publicUserLabel || containsUserLabel(existing.PublicUserAliases, publicUserLabel) {
			writeError(w, http.StatusConflict, "public user label is already in use")
			return
		}
	}

	user := &User{UserName: userName, PublicUserLabel: publicUserLabel, Email: email, APIKey: newAPIKey(), CreatedAt: now, UpdatedAt: now}
	s.state.Users[userName] = user
	if err := s.saveStateLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyUISubscribers()
	writeJSON(w, http.StatusCreated, RegistrationResponse{UserName: user.UserName, PublicUserLabel: user.PublicUserLabel, Email: user.Email, APIKey: user.APIKey})
}

func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		key = bearerToken(r.Header.Get("Authorization"))
	}
	if key == "" {
		writeError(w, http.StatusUnauthorized, "missing key")
		return
	}

	user, ok := s.findUserByKey(key)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid key")
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("upgrade failed", "error", err)
		return
	}

	client := &TunnelClient{userName: user.UserName, email: user.Email, conn: conn, apps: map[string]*ConnectedApp{}}
	s.clientsMu.Lock()
	s.clients[user.UserName] = client
	s.clientsMu.Unlock()
	s.logger.Info("client connected", "user", user.UserName)

	s.send(client, TunnelResponse{Type: "hello", UserName: user.UserName, Message: "connected"})

	defer func() {
		conn.Close()
		s.disconnectUser(user.UserName)
	}()

	for {
		var msg TunnelResponse
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || strings.Contains(err.Error(), "close") {
				return
			}
			s.logger.Error("read message failed", "user", user.UserName, "error", err)
			return
		}

		switch msg.Type {
		case protocoltypes.MessageTypeRegister:
			appName := slug(msg.AppName)
			if appName == "" {
				_ = s.send(client, TunnelResponse{Type: protocoltypes.MessageTypeError, Error: "app_name is required"})
				continue
			}
			app, err := s.upsertApp(user.UserName, appName, msg.PublicPort)
			if err != nil {
				_ = s.send(client, TunnelResponse{Type: protocoltypes.MessageTypeRegisterAck, AppName: appName, Error: err.Error()})
				continue
			}
			client.apps[appName] = &ConnectedApp{appName: appName, publicPort: app.PublicPort}
			if app.PublicPort > 0 && app.Approved {
				if err := s.ensurePortListener(app.PublicPort, app.UserName, app.AppName); err != nil {
					s.logger.Error("dynamic listener failed", "port", app.PublicPort, "error", err)
				}
			}
			_ = s.send(client, TunnelResponse{Type: protocoltypes.MessageTypeRegisterAck, AppName: appName, PublicPort: app.PublicPort, Approved: app.Approved})
			s.notifyUISubscribers()
		case "response":
			s.pendingMu.Lock()
			pending := s.pending[msg.RequestID]
			if pending != nil {
				delete(s.pending, msg.RequestID)
			}
			s.pendingMu.Unlock()
			if pending != nil {
				pending.ch <- msg
			}
		}
	}
}

func (s *Server) disconnectUser(userName string) {
	s.clientsMu.Lock()
	delete(s.clients, userName)
	s.clientsMu.Unlock()

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	for _, app := range s.state.Apps {
		if app.UserName == userName {
			app.Connected = false
			app.UpdatedAt = time.Now().UTC()
		}
	}
	_ = s.saveStateLocked()
	s.notifyUISubscribers()
}

func (s *Server) upsertApp(userName, appName string, publicPort int) (*App, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	id := appKey(userName, appName)
	now := time.Now().UTC()
	user := s.state.Users[userName]
	userIsAdmin := user != nil && isAdmin(user.UserName, user.Email, s.cfg.AdminUsers)
	shouldAutoApprove := (userIsAdmin && s.state.AutoApproveForAdmins) || (!userIsAdmin && s.state.AutoApproveForUsers)

	existing, ok := s.state.Apps[id]
	if ok {
		if override, ok := normalizeServedByAppOverride(existing.ServedByOverride); ok {
			existing.ServedByOverride = override
		} else {
			existing.ServedByOverride = servedByAppOverrideInherit
		}
		if publicPort > 0 {
			existing.PublicPort = publicPort
		}
		existing.Connected = true
		existing.LastSeenAt = now
		existing.UpdatedAt = now
		if shouldAutoApprove {
			existing.Approved = true
		}
		if err := s.saveStateLocked(); err != nil {
			return nil, err
		}
		s.notifyUISubscribers()
		return existing, nil
	}

	app := &App{
		ID:               id,
		UserName:         userName,
		AppName:          appName,
		PublicPort:       publicPort,
		Approved:         shouldAutoApprove,
		Connected:        true,
		ServedByOverride: servedByAppOverrideInherit,
		LastSeenAt:       now,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	s.state.Apps[id] = app
	if err := s.saveStateLocked(); err != nil {
		return nil, err
	}
	s.notifyUISubscribers()
	return app, nil
}

func (s *Server) adminViewData(identity authIdentity) map[string]any {
	s.stateMu.RLock()
	users := make([]*User, 0, len(s.state.Users))
	apps := make([]map[string]any, 0, len(s.state.Apps))
	abuseReports := make([]map[string]any, 0, len(s.state.AbuseReports))
	for _, u := range s.state.Users {
		cp := *u
		users = append(users, &cp)
	}
	registrationOpen := s.state.RegistrationOpen
	allowUserAppApproval := s.state.AllowUserAppApproval
	autoApproveForUsers := s.state.AutoApproveForUsers
	autoApproveForAdmins := s.state.AutoApproveForAdmins
	servedByEnabled := s.state.ServedByEnabled
	servedByMode := s.state.ServedByMode
	servedByHTMLInjectionEnabled := s.state.ServedByHTMLInjectionEnabled
	reportAbuseEnabled := s.state.ReportAbuseEnabled
	servedByAppDisableAllowed := s.state.ServedByAppDisableAllowed
	servedByEmergencyForceVisible := s.state.ServedByEmergencyForceVisible
	globalSettings := servedBySettingsFromState(defaultServedBySettingsFromConfig(s.cfg), s.state)
	for _, a := range s.state.Apps {
		cp := *a
		publicLabel := cp.UserName
		if user, ok := s.state.Users[cp.UserName]; ok && user.PublicUserLabel != "" {
			publicLabel = user.PublicUserLabel
		}
		override, ok := normalizeServedByAppOverride(cp.ServedByOverride)
		if !ok {
			override = servedByAppOverrideInherit
		}
		effective := effectiveServedBySettings(globalSettings, override)
		apps = append(apps, map[string]any{
			"user_name":                     cp.UserName,
			"app_name":                      cp.AppName,
			"approved":                      cp.Approved,
			"connected":                     cp.Connected,
			"public_port":                   cp.PublicPort,
			"public_url":                    fmt.Sprintf("https://%s-%s.%s", cp.AppName, publicLabel, s.cfg.PublicBaseDomain),
			"served_by_override":            override,
			"served_by_override_reason":     cp.ServedByOverrideReason,
			"effective_served_by_policy":    servedByPolicyName(effective),
			"served_by_override_updated_by": cp.ServedByOverrideUpdatedBy,
		})
	}
	for _, report := range s.state.AbuseReports {
		abuseReports = append(abuseReports, s.abuseReportSummaryLocked(report))
	}
	s.stateMu.RUnlock()
	if mode, ok := normalizeServedByMode(servedByMode); ok {
		servedByMode = mode
	} else {
		servedByMode = defaultServedBySettingsFromConfig(s.cfg).Mode
	}
	servedByWarnings := servedBySettingWarnings(servedBySettings{
		Enabled:               servedByEnabled,
		Mode:                  servedByMode,
		HTMLInjectionEnabled:  servedByHTMLInjectionEnabled,
		ReportAbuseEnabled:    reportAbuseEnabled,
		AppDisableAllowed:     servedByAppDisableAllowed,
		EmergencyForceVisible: servedByEmergencyForceVisible,
	})

	sort.Slice(users, func(i, j int) bool { return users[i].UserName < users[j].UserName })
	sort.Slice(apps, func(i, j int) bool {
		return fmt.Sprint(apps[i]["user_name"], "/", apps[i]["app_name"]) < fmt.Sprint(apps[j]["user_name"], "/", apps[j]["app_name"])
	})
	sort.Slice(abuseReports, func(i, j int) bool {
		left, _ := abuseReports[i]["created_at"].(time.Time)
		right, _ := abuseReports[j]["created_at"].(time.Time)
		if left.Equal(right) {
			return fmt.Sprint(abuseReports[i]["id"]) < fmt.Sprint(abuseReports[j]["id"])
		}
		return left.After(right)
	})
	return map[string]any{
		"identity":                          map[string]any{"user_name": identity.UserName},
		"registration_open":                 registrationOpen,
		"allow_user_app_approval":           allowUserAppApproval,
		"auto_approve_for_users":            autoApproveForUsers,
		"auto_approve_for_admins":           autoApproveForAdmins,
		"served_by_enabled":                 servedByEnabled,
		"served_by_mode":                    servedByMode,
		"served_by_html_injection_enabled":  servedByHTMLInjectionEnabled,
		"report_abuse_enabled":              reportAbuseEnabled,
		"served_by_app_disable_allowed":     servedByAppDisableAllowed,
		"served_by_emergency_force_visible": servedByEmergencyForceVisible,
		"served_by_warnings":                servedByWarnings,
		"served_by_app_override_options":    servedByAppOverrideOptions(),
		"abuse_report_status_options":       abuseReportStatusOptions(),
		"abuse_report_category_options":     abuseReportCategories(),
		"users":                             users,
		"apps":                              apps,
		"abuse_reports":                     abuseReports,
		"base_domain":                       s.cfg.PublicBaseDomain,
	}
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if !identity.IsAdmin {
		writeError(w, http.StatusForbidden, "admin access required")
		return
	}

	data := s.adminViewData(identity)
	_ = s.templates.ExecuteTemplate(w, "admin", map[string]any{
		"Identity":                      data["identity"].(map[string]any),
		"RegistrationOpen":              data["registration_open"],
		"AllowUserAppApproval":          data["allow_user_app_approval"],
		"AutoApproveForUsers":           data["auto_approve_for_users"],
		"AutoApproveForAdmins":          data["auto_approve_for_admins"],
		"ServedByEnabled":               data["served_by_enabled"],
		"ServedByMode":                  data["served_by_mode"],
		"ServedByHTMLInjectionEnabled":  data["served_by_html_injection_enabled"],
		"ReportAbuseEnabled":            data["report_abuse_enabled"],
		"ServedByAppDisableAllowed":     data["served_by_app_disable_allowed"],
		"ServedByEmergencyForceVisible": data["served_by_emergency_force_visible"],
		"ServedByWarnings":              data["served_by_warnings"],
		"ServedByAppOverrideOptions":    data["served_by_app_override_options"],
		"AbuseReportStatusOptions":      data["abuse_report_status_options"],
		"AbuseReportCategoryOptions":    data["abuse_report_category_options"],
		"Users":                         data["users"],
		"Apps":                          data["apps"],
		"AbuseReports":                  data["abuse_reports"],
		"BaseDomain":                    data["base_domain"],
	})
}

func (s *Server) handleAdminTraffic(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if !identity.IsAdmin {
		writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	query, err := parseTrafficQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	buckets, err := s.traffic.QueryTraffic(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets})
}

func (s *Server) handleUserTraffic(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	query, err := parseTrafficQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	query.UserName = identity.UserName
	buckets, err := s.traffic.QueryTraffic(query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets})
}

func parseTrafficQuery(r *http.Request) (TrafficQuery, error) {
	q := r.URL.Query()
	query := TrafficQuery{
		UserName: slug(q.Get("user")),
		AppName:  slug(q.Get("app")),
	}
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return query, fmt.Errorf("invalid since timestamp: %w", err)
		}
		query.Since = t
	}
	if raw := strings.TrimSpace(q.Get("until")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return query, fmt.Errorf("invalid until timestamp: %w", err)
		}
		query.Until = t
	}
	return query, nil
}

func (s *Server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if !identity.IsAdmin {
		writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	writeJSON(w, http.StatusOK, s.adminViewData(identity))
}

type abuseReportFilters struct {
	Status      string `json:"status,omitempty"`
	Category    string `json:"category,omitempty"`
	UserQuery   string `json:"user,omitempty"`
	AppName     string `json:"app,omitempty"`
	ReportedURL string `json:"reported_url,omitempty"`
}

type abuseReportStatusUpdateInput struct {
	Status string `json:"status"`
	Note   string `json:"note"`
}

type abuseReportNoteInput struct {
	Body string `json:"body"`
}

func (s *Server) requireAdminIdentity(w http.ResponseWriter, r *http.Request) (authIdentity, bool) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return authIdentity{}, false
	}
	if !identity.IsAdmin {
		writeError(w, http.StatusForbidden, "admin access required")
		return authIdentity{}, false
	}
	return identity, true
}

func (s *Server) handleAdminAbuseReports(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/admin/abuse-reports" {
		http.NotFound(w, r)
		return
	}
	_, ok := s.requireAdminIdentity(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	filters, err := parseAbuseReportFilters(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reports":          s.filteredAbuseReportSummaries(filters),
		"filters":          filters,
		"status_options":   abuseReportStatusOptions(),
		"category_options": abuseReportCategories(),
	})
}

func (s *Server) handleAdminAbuseReport(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireAdminIdentity(w, r)
	if !ok {
		return
	}
	tail := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/admin/abuse-reports/"), "/")
	if tail == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(tail, "/")
	reportID := parts[0]
	if len(parts) == 1 {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		detail, found := s.adminAbuseReportDetail(reportID)
		if !found {
			writeError(w, http.StatusNotFound, "report not found")
			return
		}
		writeJSON(w, http.StatusOK, detail)
		return
	}
	if len(parts) != 2 || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "status":
		s.handleAdminAbuseReportStatus(w, r, identity, reportID)
	case "notes":
		s.handleAdminAbuseReportNote(w, r, identity, reportID)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleAdminAbuseReportPage(w http.ResponseWriter, r *http.Request) {
	_, ok := s.requireAdminIdentity(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	reportID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/admin/abuse-reports/"), "/")
	if reportID == "" || strings.Contains(reportID, "/") {
		http.NotFound(w, r)
		return
	}
	detail, found := s.adminAbuseReportDetail(reportID)
	if !found {
		writeError(w, http.StatusNotFound, "report not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.templates.ExecuteTemplate(w, "admin_abuse_report", map[string]any{
		"Report":         detail["report"],
		"ReportedUser":   detail["reported_user"],
		"CurrentApp":     detail["current_app"],
		"RelatedReports": detail["related_reports"],
		"ActionLinks":    detail["action_links"],
		"StatusOptions":  abuseReportStatusOptions(),
	})
}

func (s *Server) handleAdminAbuseReportStatus(w http.ResponseWriter, r *http.Request, identity authIdentity, reportID string) {
	input, err := parseAbuseReportStatusUpdateInput(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status, ok := normalizeAbuseReportStatus(input.Status)
	if !ok {
		writeError(w, http.StatusBadRequest, "status is invalid")
		return
	}
	note := strings.TrimSpace(input.Note)
	if len(note) > maxAbuseReportNoteLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("note must be at most %d bytes", maxAbuseReportNoteLen))
		return
	}

	s.stateMu.Lock()
	report, found := s.state.AbuseReports[reportID]
	if !found {
		s.stateMu.Unlock()
		writeError(w, http.StatusNotFound, "report not found")
		return
	}
	now := time.Now().UTC()
	report.Status = status
	report.StatusUpdatedBy = identity.UserName
	report.StatusUpdatedAt = now
	report.UpdatedAt = now
	if note != "" {
		report.InternalNotes = append(report.InternalNotes, AbuseReportNote{
			ID:            "abn_" + randomToken(8),
			Body:          note,
			ActorUserName: identity.UserName,
			CreatedAt:     now,
		})
	}
	err = s.saveStateLocked()
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save report")
		return
	}
	s.notifyUISubscribers()
	if wantsJSON(r) {
		detail, _ := s.adminAbuseReportDetail(reportID)
		writeJSON(w, http.StatusOK, detail)
		return
	}
	http.Redirect(w, r, "/admin/abuse-reports/"+neturl.PathEscape(reportID), http.StatusSeeOther)
}

func (s *Server) handleAdminAbuseReportNote(w http.ResponseWriter, r *http.Request, identity authIdentity, reportID string) {
	input, err := parseAbuseReportNoteInput(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	body := strings.TrimSpace(input.Body)
	if body == "" {
		writeError(w, http.StatusBadRequest, "note body is required")
		return
	}
	if len(body) > maxAbuseReportNoteLen {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("note must be at most %d bytes", maxAbuseReportNoteLen))
		return
	}

	s.stateMu.Lock()
	report, found := s.state.AbuseReports[reportID]
	if !found {
		s.stateMu.Unlock()
		writeError(w, http.StatusNotFound, "report not found")
		return
	}
	now := time.Now().UTC()
	report.InternalNotes = append(report.InternalNotes, AbuseReportNote{
		ID:            "abn_" + randomToken(8),
		Body:          body,
		ActorUserName: identity.UserName,
		CreatedAt:     now,
	})
	report.UpdatedAt = now
	err = s.saveStateLocked()
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not save report")
		return
	}
	s.notifyUISubscribers()
	if wantsJSON(r) {
		detail, _ := s.adminAbuseReportDetail(reportID)
		writeJSON(w, http.StatusOK, detail)
		return
	}
	http.Redirect(w, r, "/admin/abuse-reports/"+neturl.PathEscape(reportID), http.StatusSeeOther)
}

func parseAbuseReportFilters(r *http.Request) (abuseReportFilters, error) {
	q := r.URL.Query()
	filters := abuseReportFilters{
		Category:    strings.TrimSpace(q.Get("category")),
		UserQuery:   strings.TrimSpace(q.Get("user")),
		AppName:     slug(q.Get("app")),
		ReportedURL: strings.ToLower(strings.TrimSpace(q.Get("reported_url"))),
	}
	if raw := strings.TrimSpace(q.Get("status")); raw != "" {
		status, ok := normalizeAbuseReportStatus(raw)
		if !ok {
			return filters, errors.New("status is invalid")
		}
		filters.Status = status
	}
	if filters.Category != "" && !isAllowedAbuseReportCategory(filters.Category) {
		return filters, errors.New("category is invalid")
	}
	return filters, nil
}

func parseAbuseReportStatusUpdateInput(r *http.Request) (abuseReportStatusUpdateInput, error) {
	var input abuseReportStatusUpdateInput
	if strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return input, errors.New("invalid status update request")
		}
		return input, nil
	}
	if err := r.ParseForm(); err != nil {
		return input, errors.New("invalid status update request")
	}
	input.Status = r.Form.Get("status")
	input.Note = r.Form.Get("note")
	return input, nil
}

func parseAbuseReportNoteInput(r *http.Request) (abuseReportNoteInput, error) {
	var input abuseReportNoteInput
	if strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			return input, errors.New("invalid note request")
		}
		return input, nil
	}
	if err := r.ParseForm(); err != nil {
		return input, errors.New("invalid note request")
	}
	input.Body = r.Form.Get("body")
	if input.Body == "" {
		input.Body = r.Form.Get("note")
	}
	return input, nil
}

func (s *Server) filteredAbuseReportSummaries(filters abuseReportFilters) []map[string]any {
	s.stateMu.RLock()
	reports := make([]*AbuseReport, 0, len(s.state.AbuseReports))
	for _, report := range s.state.AbuseReports {
		if !abuseReportMatchesFilters(report, filters) {
			continue
		}
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool {
		if reports[i].CreatedAt.Equal(reports[j].CreatedAt) {
			return reports[i].ID < reports[j].ID
		}
		return reports[i].CreatedAt.After(reports[j].CreatedAt)
	})
	out := make([]map[string]any, 0, len(reports))
	for _, report := range reports {
		out = append(out, s.abuseReportSummaryLocked(report))
	}
	s.stateMu.RUnlock()
	return out
}

func abuseReportMatchesFilters(report *AbuseReport, filters abuseReportFilters) bool {
	if report == nil {
		return false
	}
	if filters.Status != "" && abuseReportStatusOrDefault(report.Status) != filters.Status {
		return false
	}
	if filters.Category != "" && report.Category != filters.Category {
		return false
	}
	if filters.UserQuery != "" {
		userQuery := slug(filters.UserQuery)
		labelQuery := userLabel(filters.UserQuery)
		if report.ReportedUserName != userQuery && report.ReportedUserLabel != labelQuery {
			return false
		}
	}
	if filters.AppName != "" && report.ReportedAppName != filters.AppName {
		return false
	}
	if filters.ReportedURL != "" && !strings.Contains(strings.ToLower(report.ReportedURL), filters.ReportedURL) {
		return false
	}
	return true
}

func (s *Server) adminAbuseReportDetail(reportID string) (map[string]any, bool) {
	s.stateMu.RLock()
	report, found := s.state.AbuseReports[reportID]
	if !found {
		s.stateMu.RUnlock()
		return nil, false
	}
	reportCopy := *report
	reportCopy.Status = abuseReportStatusOrDefault(reportCopy.Status)
	reportCopy.InternalNotes = append([]AbuseReportNote(nil), report.InternalNotes...)

	var reportedUser map[string]any
	if user := s.state.Users[report.ReportedUserName]; user != nil {
		reportedUser = map[string]any{
			"user_name":         user.UserName,
			"public_user_label": user.PublicUserLabel,
			"email":             user.Email,
			"created_at":        user.CreatedAt,
			"updated_at":        user.UpdatedAt,
		}
	}

	var currentApp map[string]any
	var app *App
	if report.ReportedUserName != "" && report.ReportedAppName != "" {
		app = s.state.Apps[appKey(report.ReportedUserName, report.ReportedAppName)]
		if app != nil {
			currentApp = s.currentAppReportContextLocked(app)
		}
	}

	related := make([]map[string]any, 0)
	for _, candidate := range s.state.AbuseReports {
		if isPriorRelatedAbuseReport(report, candidate) {
			related = append(related, s.abuseReportSummaryLocked(candidate))
		}
	}
	sort.Slice(related, func(i, j int) bool {
		left, _ := related[i]["created_at"].(time.Time)
		right, _ := related[j]["created_at"].(time.Time)
		if left.Equal(right) {
			return fmt.Sprint(related[i]["id"]) < fmt.Sprint(related[j]["id"])
		}
		return left.After(right)
	})

	actionLinks := s.abuseReportActionLinksLocked(&reportCopy, app)
	s.stateMu.RUnlock()

	return map[string]any{
		"report":          &reportCopy,
		"reported_user":   reportedUser,
		"current_app":     currentApp,
		"related_reports": related,
		"action_links":    actionLinks,
		"status_options":  abuseReportStatusOptions(),
	}, true
}

func (s *Server) abuseReportSummaryLocked(report *AbuseReport) map[string]any {
	summary := map[string]any{
		"id":                  report.ID,
		"reported_url":        report.ReportedURL,
		"reported_host":       report.ReportedHost,
		"reported_path":       report.ReportedPath,
		"reported_user_name":  report.ReportedUserName,
		"reported_user_label": report.ReportedUserLabel,
		"reported_app_name":   report.ReportedAppName,
		"category":            report.Category,
		"category_counts":     report.CategoryCounts,
		"reporter_count":      abuseReportReporterCount(report),
		"status":              abuseReportStatusOrDefault(report.Status),
		"created_at":          report.CreatedAt,
		"updated_at":          report.UpdatedAt,
	}
	if app := s.state.Apps[appKey(report.ReportedUserName, report.ReportedAppName)]; app != nil {
		summary["current_app_status"] = appReportStatus(app)
	}
	return summary
}

func abuseReportReporterCount(report *AbuseReport) int {
	if report == nil || report.ReporterCount <= 0 {
		return 1
	}
	return report.ReporterCount
}

func (s *Server) currentAppReportContextLocked(app *App) map[string]any {
	publicLabel := app.UserName
	if user := s.state.Users[app.UserName]; user != nil && user.PublicUserLabel != "" {
		publicLabel = user.PublicUserLabel
	}
	return map[string]any{
		"user_name":   app.UserName,
		"app_name":    app.AppName,
		"approved":    app.Approved,
		"connected":   app.Connected,
		"public_port": app.PublicPort,
		"public_url":  fmt.Sprintf("https://%s-%s.%s", app.AppName, publicLabel, s.cfg.PublicBaseDomain),
		"status":      appReportStatus(app),
		"created_at":  app.CreatedAt,
		"updated_at":  app.UpdatedAt,
	}
}

func (s *Server) abuseReportActionLinksLocked(report *AbuseReport, app *App) map[string]any {
	links := map[string]any{
		"admin_queue": "/admin#abuse-reports",
		"detail_html": "/admin/abuse-reports/" + neturl.PathEscape(report.ID),
	}
	if report.ReportedUserName != "" {
		links["user_filter"] = "/api/admin/abuse-reports?user=" + neturl.QueryEscape(report.ReportedUserName)
		if user := s.state.Users[report.ReportedUserName]; user != nil && user.PublicUserLabel != "" {
			links["user_public_url"] = fmt.Sprintf("https://%s.%s", user.PublicUserLabel, s.cfg.PublicBaseDomain)
		}
	}
	if report.ReportedAppName != "" {
		links["app_filter"] = "/api/admin/abuse-reports?user=" + neturl.QueryEscape(report.ReportedUserName) + "&app=" + neturl.QueryEscape(report.ReportedAppName)
	}
	if app != nil {
		links["approve_app"] = "/api/admin/approve"
		if currentApp := s.currentAppReportContextLocked(app); currentApp["public_url"] != "" {
			links["app_public_url"] = currentApp["public_url"]
		}
	}
	return links
}

func isPriorRelatedAbuseReport(current, candidate *AbuseReport) bool {
	if current == nil || candidate == nil || current.ID == candidate.ID {
		return false
	}
	if !current.CreatedAt.IsZero() && !candidate.CreatedAt.Before(current.CreatedAt) {
		return false
	}
	if current.ReportedUserName != "" && current.ReportedAppName != "" {
		return candidate.ReportedUserName == current.ReportedUserName && candidate.ReportedAppName == current.ReportedAppName
	}
	return current.ReportedURL != "" && candidate.ReportedURL == current.ReportedURL
}

func appReportStatus(app *App) string {
	if app == nil {
		return "unknown"
	}
	if app.Approved {
		return "approved"
	}
	return "pending"
}

func abuseReportStatusOptions() []map[string]string {
	return []map[string]string{
		{"Value": abuseReportStatusNew, "Label": "New"},
		{"Value": abuseReportStatusTriagedReviewing, "Label": "Triaged / reviewing"},
		{"Value": abuseReportStatusNeedsMoreInfo, "Label": "Needs more info"},
		{"Value": abuseReportStatusActionedMitigated, "Label": "Actioned / mitigated"},
		{"Value": abuseReportStatusRejected, "Label": "Rejected"},
		{"Value": abuseReportStatusDuplicate, "Label": "Duplicate"},
		{"Value": abuseReportStatusEscalatedLegal, "Label": "Escalated / legal"},
		{"Value": abuseReportStatusClosed, "Label": "Closed"},
	}
}

func abuseReportStatusOrDefault(raw string) string {
	if status, ok := normalizeAbuseReportStatus(raw); ok {
		return status
	}
	return abuseReportStatusNew
}

func normalizeAbuseReportStatus(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case abuseReportStatusNew:
		return abuseReportStatusNew, true
	case "triaged", "reviewing", "triaged/reviewing", abuseReportStatusTriagedReviewing:
		return abuseReportStatusTriagedReviewing, true
	case abuseReportStatusNeedsMoreInfo:
		return abuseReportStatusNeedsMoreInfo, true
	case "actioned", "mitigated", "actioned/mitigated", abuseReportStatusActionedMitigated:
		return abuseReportStatusActionedMitigated, true
	case abuseReportStatusRejected:
		return abuseReportStatusRejected, true
	case abuseReportStatusDuplicate:
		return abuseReportStatusDuplicate, true
	case "escalated", "legal", "escalated/legal", abuseReportStatusEscalatedLegal:
		return abuseReportStatusEscalatedLegal, true
	case abuseReportStatusClosed:
		return abuseReportStatusClosed, true
	default:
		return "", false
	}
}

func (s *Server) handleToggleSetting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if !identity.IsAdmin {
		writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	setting := strings.TrimSpace(r.Form.Get("setting"))

	s.stateMu.Lock()
	var value any
	switch setting {
	case "allow_user_app_approval":
		parsed, err := settingBoolFormValue(r, s.state.AllowUserAppApproval)
		if err != nil {
			s.stateMu.Unlock()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.state.AllowUserAppApproval = parsed
		value = s.state.AllowUserAppApproval
	case "auto_approve_for_users":
		parsed, err := settingBoolFormValue(r, s.state.AutoApproveForUsers)
		if err != nil {
			s.stateMu.Unlock()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.state.AutoApproveForUsers = parsed
		value = s.state.AutoApproveForUsers
	case "auto_approve_for_admins":
		parsed, err := settingBoolFormValue(r, s.state.AutoApproveForAdmins)
		if err != nil {
			s.stateMu.Unlock()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.state.AutoApproveForAdmins = parsed
		value = s.state.AutoApproveForAdmins
	case "served_by_enabled":
		parsed, err := settingBoolFormValue(r, s.state.ServedByEnabled)
		if err != nil {
			s.stateMu.Unlock()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.state.ServedByEnabled = parsed
		value = s.state.ServedByEnabled
	case "served_by_html_injection_enabled":
		parsed, err := settingBoolFormValue(r, s.state.ServedByHTMLInjectionEnabled)
		if err != nil {
			s.stateMu.Unlock()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.state.ServedByHTMLInjectionEnabled = parsed
		value = s.state.ServedByHTMLInjectionEnabled
	case "report_abuse_enabled":
		parsed, err := settingBoolFormValue(r, s.state.ReportAbuseEnabled)
		if err != nil {
			s.stateMu.Unlock()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.state.ReportAbuseEnabled = parsed
		value = s.state.ReportAbuseEnabled
	case "served_by_app_disable_allowed":
		parsed, err := settingBoolFormValue(r, s.state.ServedByAppDisableAllowed)
		if err != nil {
			s.stateMu.Unlock()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.state.ServedByAppDisableAllowed = parsed
		value = s.state.ServedByAppDisableAllowed
	case "served_by_emergency_force_visible":
		parsed, err := settingBoolFormValue(r, s.state.ServedByEmergencyForceVisible)
		if err != nil {
			s.stateMu.Unlock()
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		s.state.ServedByEmergencyForceVisible = parsed
		value = s.state.ServedByEmergencyForceVisible
	case "served_by_mode":
		mode, ok := normalizeServedByMode(r.Form.Get("value"))
		if !ok {
			s.stateMu.Unlock()
			writeError(w, http.StatusBadRequest, "served_by_mode must be visible_and_headers or headers_only")
			return
		}
		s.state.ServedByMode = mode
		value = mode
	default:
		s.stateMu.Unlock()
		writeError(w, http.StatusBadRequest, "unknown setting")
		return
	}
	mode, ok := normalizeServedByMode(s.state.ServedByMode)
	if !ok {
		mode = defaultServedBySettingsFromConfig(s.cfg).Mode
	}
	warnings := servedBySettingWarnings(servedBySettings{
		Enabled:               s.state.ServedByEnabled,
		Mode:                  mode,
		HTMLInjectionEnabled:  s.state.ServedByHTMLInjectionEnabled,
		ReportAbuseEnabled:    s.state.ReportAbuseEnabled,
		AppDisableAllowed:     s.state.ServedByAppDisableAllowed,
		EmergencyForceVisible: s.state.ServedByEmergencyForceVisible,
	})
	err := s.saveStateLocked()
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyUISubscribers()
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"setting": setting, "value": value, "warnings": warnings})
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleAdminAppServedByOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if !identity.IsAdmin {
		writeError(w, http.StatusForbidden, "admin access required")
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	userName := slug(r.Form.Get("user"))
	appName := slug(r.Form.Get("app"))
	if userName == "" || appName == "" {
		writeError(w, http.StatusBadRequest, "user and app are required")
		return
	}
	override, ok := normalizeServedByAppOverride(r.Form.Get("override"))
	if !ok {
		writeError(w, http.StatusBadRequest, "served-by override must be inherit, force_visible, headers_only, or disabled")
		return
	}
	reason := strings.TrimSpace(r.Form.Get("reason"))
	if servedByOverrideWeakensDisclosure(override) && reason == "" {
		writeError(w, http.StatusBadRequest, "reason is required when weakening served-by disclosure")
		return
	}

	s.stateMu.Lock()
	app, ok := s.state.Apps[appKey(userName, appName)]
	if !ok {
		s.stateMu.Unlock()
		writeError(w, http.StatusNotFound, "app not found")
		return
	}
	if override == servedByAppOverrideDisabled && !s.state.ServedByAppDisableAllowed {
		s.stateMu.Unlock()
		writeError(w, http.StatusForbidden, "per-app served-by disable is not globally allowed")
		return
	}

	oldOverride, ok := normalizeServedByAppOverride(app.ServedByOverride)
	if !ok {
		oldOverride = servedByAppOverrideInherit
	}
	now := time.Now().UTC()
	app.ServedByOverride = override
	if servedByOverrideWeakensDisclosure(override) {
		app.ServedByOverrideReason = reason
	} else {
		app.ServedByOverrideReason = ""
	}
	storedReason := app.ServedByOverrideReason
	app.ServedByOverrideUpdatedBy = identity.UserName
	app.ServedByOverrideUpdatedAt = now
	app.UpdatedAt = now

	if s.state.AuditEvents == nil {
		s.state.AuditEvents = []AuditEvent{}
	}
	s.state.AuditEvents = append(s.state.AuditEvents, AuditEvent{
		ID:             "aud_" + randomToken(8),
		Action:         auditActionAppServedByOverrideUpdated,
		ActorUserName:  identity.UserName,
		TargetUserName: userName,
		TargetAppName:  appName,
		OldValue:       oldOverride,
		NewValue:       override,
		Reason:         storedReason,
		CreatedAt:      now,
	})

	effective := effectiveServedBySettings(servedBySettingsFromState(defaultServedBySettingsFromConfig(s.cfg), s.state), override)
	err := s.saveStateLocked()
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyUISubscribers()
	if s.logger != nil {
		s.logger.Info("app served-by override updated", "actor", identity.UserName, "user", userName, "app", appName, "old_override", oldOverride, "new_override", override)
	}

	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"user":                       userName,
			"app":                        appName,
			"served_by_override":         override,
			"served_by_override_reason":  storedReason,
			"effective_served_by_policy": servedByPolicyName(effective),
			"audit_prepared":             true,
		})
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleToggleRegistration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if !identity.IsAdmin {
		writeError(w, http.StatusForbidden, "admin access required")
		return
	}

	s.stateMu.Lock()
	s.state.RegistrationOpen = !s.state.RegistrationOpen
	err := s.saveStateLocked()
	open := s.state.RegistrationOpen
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyUISubscribers()

	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"registration_open": open})
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) handleUserPage(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	s.renderUserPage(w, r, identity, identity.UserName)
}

func (s *Server) userViewData(identity authIdentity, userName string) (map[string]any, error) {
	user, err := s.ensureUser(identity)
	if err != nil {
		return nil, err
	}
	if userName != user.UserName && !identity.IsAdmin {
		return nil, errors.New("forbidden")
	}
	if identity.IsAdmin && userName != identity.UserName {
		s.stateMu.RLock()
		other, found := s.state.Users[userName]
		s.stateMu.RUnlock()
		if found {
			user = other
		}
	}

	s.stateMu.RLock()
	allowUserAppApproval := s.state.AllowUserAppApproval
	globalSettings := servedBySettingsFromState(defaultServedBySettingsFromConfig(s.cfg), s.state)
	apps := make([]map[string]any, 0)
	for _, a := range s.state.Apps {
		if a.UserName != user.UserName {
			continue
		}
		cp := *a
		canApprove := identity.IsAdmin || (allowUserAppApproval && !cp.Approved)
		override, ok := normalizeServedByAppOverride(cp.ServedByOverride)
		if !ok {
			override = servedByAppOverrideInherit
		}
		effective := effectiveServedBySettings(globalSettings, override)
		apps = append(apps, map[string]any{
			"app_name":                   cp.AppName,
			"approved":                   cp.Approved,
			"connected":                  cp.Connected,
			"public_port":                cp.PublicPort,
			"public_url":                 fmt.Sprintf("https://%s-%s.%s", cp.AppName, user.PublicUserLabel, s.cfg.PublicBaseDomain),
			"served_by_override":         override,
			"effective_served_by_policy": servedByPolicyName(effective),
			"status": func() string {
				if cp.Approved {
					return "approved"
				}
				return "pending admin approval"
			}(),
			"can_approve": canApprove,
			"user_name":   user.UserName,
		})
	}
	s.stateMu.RUnlock()
	sort.Slice(apps, func(i, j int) bool { return fmt.Sprint(apps[i]["app_name"]) < fmt.Sprint(apps[j]["app_name"]) })

	return map[string]any{
		"identity":                map[string]any{"user_name": identity.UserName, "is_admin": identity.IsAdmin},
		"user":                    user,
		"apps":                    apps,
		"allow_user_app_approval": allowUserAppApproval,
		"base_domain":             s.cfg.PublicBaseDomain,
	}, nil
}

func (s *Server) renderUserPage(w http.ResponseWriter, r *http.Request, identity authIdentity, userName string) {
	data, err := s.userViewData(identity, userName)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	_ = s.templates.ExecuteTemplate(w, "user", map[string]any{
		"Identity":   data["identity"],
		"User":       data["user"],
		"Apps":       data["apps"],
		"BaseDomain": data["base_domain"],
		"Error":      r.URL.Query().Get("error"),
	})
}

func (s *Server) handleUserState(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	data, err := s.userViewData(identity, identity.UserName)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	user, err := s.ensureUser(identity)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}

	s.stateMu.Lock()
	user.APIKey = newAPIKey()
	user.UpdatedAt = time.Now().UTC()
	err = s.saveStateLocked()
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyUISubscribers()

	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"api_key": user.APIKey})
		return
	}
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

func (s *Server) handleUpdatePublicUserLabel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	user, err := s.ensureUser(identity)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	normalized, err := validatePublicUserLabel(r.Form.Get("public_user_label"))
	if err != nil {
		if wantsJSON(r) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		http.Redirect(w, r, "/me?error="+neturl.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	s.stateMu.Lock()
	for _, existing := range s.state.Users {
		if existing.UserName == user.UserName {
			continue
		}
		if existing.PublicUserLabel == normalized || containsUserLabel(existing.PublicUserAliases, normalized) {
			s.stateMu.Unlock()
			msg := fmt.Sprintf("public user label %q is already taken; choose a new slug", normalized)
			if wantsJSON(r) {
				writeError(w, http.StatusConflict, msg)
				return
			}
			http.Redirect(w, r, "/me?error="+neturl.QueryEscape(msg), http.StatusSeeOther)
			return
		}
	}
	if user.PublicUserLabel != normalized {
		user.PublicUserAliases = append(user.PublicUserAliases, user.PublicUserLabel)
		user.PublicUserAliases = uniqueUserLabels(user.PublicUserAliases)
		user.PublicUserLabel = normalized
	}
	user.UpdatedAt = time.Now().UTC()
	err = s.saveStateLocked()
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyUISubscribers()

	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"public_user_label": normalized, "public_user_aliases": user.PublicUserAliases})
		return
	}
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

func (s *Server) handleApproveApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, ok := s.requireIdentity(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	userName := slug(r.Form.Get("user"))
	appName := slug(r.Form.Get("app"))
	if userName == "" || appName == "" {
		writeError(w, http.StatusBadRequest, "user and app are required")
		return
	}
	if !identity.IsAdmin {
		s.stateMu.RLock()
		allow := s.state.AllowUserAppApproval
		s.stateMu.RUnlock()
		if !allow || identity.UserName != userName {
			writeError(w, http.StatusForbidden, "approval not allowed")
			return
		}
	}

	s.stateMu.Lock()
	app, ok := s.state.Apps[appKey(userName, appName)]
	if !ok {
		s.stateMu.Unlock()
		writeError(w, http.StatusNotFound, "app not found")
		return
	}
	app.Approved = true
	app.UpdatedAt = time.Now().UTC()
	err := s.saveStateLocked()
	port := app.PublicPort
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.notifyUISubscribers()
	if port > 0 {
		if err := s.ensurePortListener(port, userName, appName); err != nil {
			s.logger.Error("failed to create dynamic listener", "error", err, "port", port)
		}
	}

	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"approved": true})
		return
	}
	if identity.IsAdmin {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

func (s *Server) recordTraffic(userName, appName string, statusCode int, bytesIn, bytesOut int64, duration time.Duration, failed bool) {
	if s.traffic == nil {
		return
	}
	s.traffic.RecordTraffic(TrafficRecord{
		UserName:   userName,
		AppName:    appName,
		StatusCode: statusCode,
		BytesIn:    bytesIn,
		BytesOut:   bytesOut,
		Duration:   duration,
		Failed:     failed,
		At:         time.Now().UTC(),
	})
}

type responseDecorationDecision string

const (
	responseDecorationHTMLInject responseDecorationDecision = "html_inject"
	responseDecorationHeaderOnly responseDecorationDecision = "header_only"
	responseDecorationSkip       responseDecorationDecision = "skip"

	servedByModeVisibleAndHeaders = "visible_and_headers"
	servedByModeHeadersOnly       = "headers_only"
	servedByPolicyDisabled        = "disabled"

	servedByAppOverrideInherit      = "inherit"
	servedByAppOverrideForceVisible = "force_visible"
	servedByAppOverrideHeadersOnly  = "headers_only"
	servedByAppOverrideDisabled     = "disabled"

	auditActionAppServedByOverrideUpdated = "app_served_by_override_updated"

	learnMorePath = "/about-portflare"
	reportPath    = "/report-abuse"

	servedByHeaderName    = "X-Portflare-Served-By"
	servedByHeaderValue   = "Portflare"
	learnMoreHeaderName   = "X-Portflare-Learn-More"
	reportAbuseHeaderName = "X-Portflare-Report-Abuse"
	appHeaderName         = "X-Portflare-App"
	userHeaderName        = "X-Portflare-User"

	maxAbuseReportBodyBytes      = 32 << 10
	maxAbuseReportURLLen         = 2048
	maxAbuseReportDescriptionLen = 4000
	maxAbuseReportContactLen     = 320
	maxAbuseReportContextLen     = 1000
	maxAbuseReportNoteLen        = 2000
	abuseReportLimitPerWindow    = 5
	abuseReportThrottleWindow    = 10 * time.Minute
	minAbuseReportSubmitDelay    = 2 * time.Second

	abuseReportChallengeOff         = "off"
	abuseReportChallengeCaptcha     = "captcha"
	abuseReportChallengeProofOfWork = "proof_of_work"
	abuseReportProofOfWorkPrefix    = "0000"

	abuseReportStatusNew               = "new"
	abuseReportStatusTriagedReviewing  = "triaged_reviewing"
	abuseReportStatusNeedsMoreInfo     = "needs_more_info"
	abuseReportStatusActionedMitigated = "actioned_mitigated"
	abuseReportStatusRejected          = "rejected"
	abuseReportStatusDuplicate         = "duplicate"
	abuseReportStatusEscalatedLegal    = "escalated_legal"
	abuseReportStatusClosed            = "closed"
)

func (d responseDecorationDecision) String() string {
	return string(d)
}

type servedBySettings struct {
	Enabled               bool
	Mode                  string
	HTMLInjectionEnabled  bool
	ReportAbuseEnabled    bool
	AppDisableAllowed     bool
	EmergencyForceVisible bool
}

func defaultServedBySettingsFromConfig(cfg Config) servedBySettings {
	mode, ok := normalizeServedByMode(cfg.ServedByMode)
	if !ok {
		mode = servedByModeVisibleAndHeaders
	}
	settings := servedBySettings{
		Enabled:               cfg.ServedByEnabled,
		Mode:                  mode,
		HTMLInjectionEnabled:  cfg.ServedByHTMLInjectionEnabled,
		ReportAbuseEnabled:    cfg.ReportAbuseEnabled,
		AppDisableAllowed:     cfg.ServedByAppDisableAllowed,
		EmergencyForceVisible: cfg.ServedByEmergencyForceVisible,
	}
	if strings.TrimSpace(cfg.ServedByMode) == "" {
		settings.Enabled = true
		settings.HTMLInjectionEnabled = true
		settings.ReportAbuseEnabled = true
		settings.AppDisableAllowed = cfg.ServedByAppDisableAllowed
		settings.EmergencyForceVisible = cfg.ServedByEmergencyForceVisible
	}
	return settings
}

func (s *Server) currentServedBySettings() servedBySettings {
	defaults := defaultServedBySettingsFromConfig(s.cfg)
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return servedBySettingsFromState(defaults, s.state)
}

func (s *Server) currentServedBySettingsForApp(userName, appName string) servedBySettings {
	defaults := defaultServedBySettingsFromConfig(s.cfg)
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	global := servedBySettingsFromState(defaults, s.state)
	override := servedByAppOverrideInherit
	if app := s.state.Apps[appKey(userName, appName)]; app != nil {
		if normalized, ok := normalizeServedByAppOverride(app.ServedByOverride); ok {
			override = normalized
		}
	}
	return effectiveServedBySettings(global, override)
}

func servedBySettingsFromState(defaults servedBySettings, st State) servedBySettings {
	if strings.TrimSpace(st.ServedByMode) == "" {
		return defaults
	}
	mode, ok := normalizeServedByMode(st.ServedByMode)
	if !ok {
		mode = defaults.Mode
	}
	return servedBySettings{
		Enabled:               st.ServedByEnabled,
		Mode:                  mode,
		HTMLInjectionEnabled:  st.ServedByHTMLInjectionEnabled,
		ReportAbuseEnabled:    st.ReportAbuseEnabled,
		AppDisableAllowed:     st.ServedByAppDisableAllowed,
		EmergencyForceVisible: st.ServedByEmergencyForceVisible,
	}
}

func effectiveServedBySettings(global servedBySettings, override string) servedBySettings {
	settings := global
	if settings.EmergencyForceVisible {
		settings.Enabled = true
		settings.Mode = servedByModeVisibleAndHeaders
		settings.HTMLInjectionEnabled = true
		settings.ReportAbuseEnabled = true
		return settings
	}

	normalized, ok := normalizeServedByAppOverride(override)
	if !ok || normalized == servedByAppOverrideInherit {
		return settings
	}
	switch normalized {
	case servedByAppOverrideForceVisible:
		settings.Enabled = true
		settings.Mode = servedByModeVisibleAndHeaders
		settings.HTMLInjectionEnabled = true
	case servedByAppOverrideHeadersOnly:
		settings.Enabled = true
		settings.Mode = servedByModeHeadersOnly
		settings.HTMLInjectionEnabled = false
	case servedByAppOverrideDisabled:
		if settings.AppDisableAllowed {
			settings.Enabled = false
			settings.HTMLInjectionEnabled = false
		}
	}
	return settings
}

func servedByPolicyName(settings servedBySettings) string {
	if settings.EmergencyForceVisible {
		return servedByModeVisibleAndHeaders
	}
	if !settings.Enabled {
		return servedByPolicyDisabled
	}
	if settings.Mode == servedByModeHeadersOnly || !settings.HTMLInjectionEnabled {
		return servedByModeHeadersOnly
	}
	return servedByModeVisibleAndHeaders
}

func normalizeServedByMode(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case servedByModeVisibleAndHeaders:
		return servedByModeVisibleAndHeaders, true
	case servedByModeHeadersOnly:
		return servedByModeHeadersOnly, true
	default:
		return "", false
	}
}

func normalizeServedByAppOverride(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", servedByAppOverrideInherit:
		return servedByAppOverrideInherit, true
	case servedByAppOverrideForceVisible:
		return servedByAppOverrideForceVisible, true
	case servedByAppOverrideHeadersOnly:
		return servedByAppOverrideHeadersOnly, true
	case servedByAppOverrideDisabled:
		return servedByAppOverrideDisabled, true
	default:
		return "", false
	}
}

func servedByAppOverrideOptions() []map[string]string {
	return []map[string]string{
		{"Value": servedByAppOverrideInherit, "Label": "Inherit global policy"},
		{"Value": servedByAppOverrideForceVisible, "Label": "Force visible"},
		{"Value": servedByAppOverrideHeadersOnly, "Label": "Headers only"},
		{"Value": servedByAppOverrideDisabled, "Label": "Disabled"},
	}
}

func servedByOverrideWeakensDisclosure(override string) bool {
	normalized, ok := normalizeServedByAppOverride(override)
	return ok && (normalized == servedByAppOverrideHeadersOnly || normalized == servedByAppOverrideDisabled)
}

func envServedByMode(key, fallback string) string {
	if mode, ok := normalizeServedByMode(env(key, fallback)); ok {
		return mode
	}
	return fallback
}

func envAbuseReportChallengeMode(key, fallback string) string {
	if mode, ok := normalizeAbuseReportChallengeMode(env(key, fallback)); ok {
		return mode
	}
	return fallback
}

func normalizeAbuseReportChallengeMode(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", abuseReportChallengeOff:
		return abuseReportChallengeOff, true
	case abuseReportChallengeCaptcha:
		return abuseReportChallengeCaptcha, true
	case "pow", "proof-of-work", abuseReportChallengeProofOfWork:
		return abuseReportChallengeProofOfWork, true
	default:
		return "", false
	}
}

func servedBySettingWarnings(settings servedBySettings) []string {
	warnings := []string{}
	if !settings.Enabled {
		warnings = append(warnings, "Disabling served-by removes public disclosure headers and visible notices from proxied apps.")
	}
	if settings.Enabled && settings.Mode == servedByModeHeadersOnly {
		warnings = append(warnings, "Headers-only served-by mode removes the visible notice from eligible HTML pages.")
	}
	if settings.Enabled && !settings.HTMLInjectionEnabled && settings.Mode == servedByModeVisibleAndHeaders {
		warnings = append(warnings, "Disabling HTML injection removes the visible notice from eligible HTML pages.")
	}
	if !settings.ReportAbuseEnabled {
		warnings = append(warnings, "Disabling report abuse removes public abuse intake links and endpoints.")
	}
	if settings.AppDisableAllowed {
		warnings = append(warnings, "Per-app served-by disable is globally allowed; require documented compatibility reasons for each exception.")
	}
	if settings.EmergencyForceVisible {
		warnings = append(warnings, "Emergency force-visible is active and overrides per-app served-by settings.")
	}
	return warnings
}

type preparedProxiedResponse struct {
	status   int
	headers  http.Header
	body     []byte
	decision responseDecorationDecision
}

type servedByAffordance struct {
	LearnMoreURL   string
	ReportAbuseURL string
}

func prepareProxiedResponse(r *http.Request, resp TunnelResponse, affordance servedByAffordance) (preparedProxiedResponse, error) {
	return prepareProxiedResponseWithSettings(r, resp, affordance, servedBySettings{
		Enabled:              true,
		Mode:                 servedByModeVisibleAndHeaders,
		HTMLInjectionEnabled: true,
		ReportAbuseEnabled:   true,
	})
}

func prepareProxiedResponseWithSettings(r *http.Request, resp TunnelResponse, affordance servedByAffordance, settings servedBySettings) (preparedProxiedResponse, error) {
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	payload, err := base64.StdEncoding.DecodeString(resp.BodyBase64)
	if err != nil {
		return preparedProxiedResponse{}, err
	}

	decision := classifyProxiedResponse(r, status, resp.Headers, payload, settings)
	headers := filteredProxiedResponseHeaders(resp.Headers)
	body := payload
	payloadChanged := false

	if decision == responseDecorationHTMLInject {
		body = injectServedByMarkup(payload, affordance)
		payloadChanged = true
	}
	if !responseCanHaveBody(r.Method, status) {
		body = nil
	}
	if payloadChanged {
		removeStalePayloadHeaders(headers)
	}
	if settings.Enabled && decision != responseDecorationSkip {
		headers.Set(servedByHeaderName, servedByHeaderValue)
	}
	if responseCanHaveBody(r.Method, status) {
		headers.Set("Content-Length", strconv.Itoa(len(body)))
	} else {
		headers.Del("Content-Length")
	}

	return preparedProxiedResponse{
		status:   status,
		headers:  headers,
		body:     body,
		decision: decision,
	}, nil
}

func classifyProxiedResponse(r *http.Request, status int, headers http.Header, payload []byte, settings servedBySettings) responseDecorationDecision {
	if isUpgradeRequest(r) || isUpgradeResponse(status, headers) {
		return responseDecorationSkip
	}
	if !settings.Enabled {
		return responseDecorationSkip
	}
	if !responseCanHaveBody(r.Method, status) {
		return responseDecorationHeaderOnly
	}
	if settings.Mode == servedByModeHeadersOnly || !settings.HTMLInjectionEnabled {
		return responseDecorationHeaderOnly
	}
	if r.Method != http.MethodGet {
		return responseDecorationHeaderOnly
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return responseDecorationHeaderOnly
	}
	if isAttachment(headers.Get("Content-Disposition")) {
		return responseDecorationHeaderOnly
	}
	if hasUnsafeContentEncoding(headers.Get("Content-Encoding")) {
		return responseDecorationHeaderOnly
	}
	if !isHTMLContentType(headers.Get("Content-Type")) {
		return responseDecorationHeaderOnly
	}
	if len(payload) == 0 {
		return responseDecorationHeaderOnly
	}
	return responseDecorationHTMLInject
}

func responseCanHaveBody(method string, status int) bool {
	if method == http.MethodHead {
		return false
	}
	if status >= 100 && status < 200 {
		return false
	}
	return status != http.StatusNoContent && status != http.StatusNotModified
}

func filteredProxiedResponseHeaders(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, values := range src {
		if shouldDropProxiedResponseHeader(k) {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
	return dst
}

func shouldDropProxiedResponseHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "content-length", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func removeStalePayloadHeaders(headers http.Header) {
	headers.Del("Content-MD5")
	headers.Del("Digest")
	headers.Del("ETag")
	headers.Del("Last-Modified")
}

func isUpgradeRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return headerTokenContains(r.Header, "Connection", "upgrade") || r.Header.Get("Upgrade") != ""
}

func isUpgradeResponse(status int, headers http.Header) bool {
	return status == http.StatusSwitchingProtocols || headerTokenContains(headers, "Connection", "upgrade") || headers.Get("Upgrade") != ""
}

func headerTokenContains(headers http.Header, name, token string) bool {
	for _, value := range headers.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func isAttachment(contentDisposition string) bool {
	if contentDisposition == "" {
		return false
	}
	parts := strings.SplitN(contentDisposition, ";", 2)
	return strings.EqualFold(strings.TrimSpace(parts[0]), "attachment")
}

func hasUnsafeContentEncoding(contentEncoding string) bool {
	if contentEncoding == "" {
		return false
	}
	for _, value := range strings.Split(contentEncoding, ",") {
		if !strings.EqualFold(strings.TrimSpace(value), "identity") {
			return true
		}
	}
	return false
}

func isHTMLContentType(contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	return mediaType == "text/html" || mediaType == "application/xhtml+xml"
}

func injectServedByMarkup(payload []byte, affordance servedByAffordance) []byte {
	markup := servedByMarkup(affordance)
	body := string(payload)
	lower := strings.ToLower(body)
	if idx := strings.LastIndex(lower, "</body>"); idx >= 0 {
		return []byte(body[:idx] + markup + body[idx:])
	}
	if idx := strings.LastIndex(lower, "</html>"); idx >= 0 {
		return []byte(body[:idx] + markup + body[idx:])
	}
	return []byte(body + markup)
}

func servedByMarkup(affordance servedByAffordance) string {
	learnMoreURL := html.EscapeString(affordance.LearnMoreURL)
	reportAbuseURL := html.EscapeString(affordance.ReportAbuseURL)
	markup := `<aside data-portflare-served-by="true" class="portflare-served-by" role="complementary" aria-label="Portflare service notice">` +
		`<span>Served by Portflare</span>` +
		`<a class="portflare-served-by__link" href="` + learnMoreURL + `">Learn more</a>`
	if reportAbuseURL != "" {
		markup += `<a class="portflare-served-by__link" href="` + reportAbuseURL + `">Report abuse</a>`
	}
	return markup + `</aside>`
}

func (s *Server) addPortflareFallbackHeaders(headers http.Header, r *http.Request, appName, publicUserLabel string, settings servedBySettings) {
	if isUpgradeRequest(r) || !settings.Enabled {
		return
	}
	affordance := s.servedByAffordance(r, appName, publicUserLabel, settings)

	headers.Set(servedByHeaderName, servedByHeaderValue)
	headers.Set(learnMoreHeaderName, affordance.LearnMoreURL)
	if affordance.ReportAbuseURL != "" {
		headers.Set(reportAbuseHeaderName, affordance.ReportAbuseURL)
	} else {
		headers.Del(reportAbuseHeaderName)
	}
	if appName = slug(appName); appName != "" {
		headers.Set(appHeaderName, appName)
	}
	if isSafePublicUserLabel(publicUserLabel) {
		headers.Set(userHeaderName, publicUserLabel)
	}
	headers.Add("Link", fmt.Sprintf("<%s>; rel=\"learn-more\"", affordance.LearnMoreURL))
	if affordance.ReportAbuseURL != "" {
		headers.Add("Link", fmt.Sprintf("<%s>; rel=\"report-abuse\"", affordance.ReportAbuseURL))
	}
}

func (s *Server) servedByAffordance(r *http.Request, appName, publicUserLabel string, settings servedBySettings) servedByAffordance {
	serviceBaseURL := s.publicServiceBaseURL(r)
	query := neturl.Values{}
	query.Set("url", publicRequestURL(r))
	if contextValue := servedByRouteContext(appName, publicUserLabel); contextValue != "" {
		query.Set("context", contextValue)
	}
	affordance := servedByAffordance{
		LearnMoreURL: serviceBaseURL + learnMorePath,
	}
	if settings.ReportAbuseEnabled {
		affordance.ReportAbuseURL = serviceBaseURL + reportPath + "?" + query.Encode()
	}
	return affordance
}

func servedByRouteContext(appName, publicUserLabel string) string {
	parts := []string{"served-by banner"}
	if appName = slug(appName); appName != "" {
		parts = append(parts, "app="+appName)
	}
	if isSafePublicUserLabel(publicUserLabel) {
		parts = append(parts, "public_user="+publicUserLabel)
	}
	return strings.Join(parts, "; ")
}

func (s *Server) publicServiceBaseURL(r *http.Request) string {
	host := canonicalHost(s.cfg.PublicBaseDomain)
	if host == "" && r != nil {
		host = canonicalHost(r.Host)
	}
	return requestScheme(r) + "://" + host
}

func publicRequestURL(r *http.Request) string {
	if r == nil {
		return ""
	}
	target := *r.URL
	target.Scheme = requestScheme(r)
	if r.Host != "" {
		target.Host = r.Host
	}
	return target.String()
}

func isSafePublicUserLabel(label string) bool {
	if label == "" {
		return false
	}
	return userLabel(label) == label
}

func (s *Server) proxyToApp(w http.ResponseWriter, r *http.Request, userName, appName string) {
	started := time.Now()

	settings := s.currentServedBySettingsForApp(userName, appName)
	s.stateMu.RLock()
	app, ok := s.state.Apps[appKey(userName, appName)]
	publicUserLabel := ""
	if user, userOK := s.state.Users[userName]; userOK {
		publicUserLabel = user.PublicUserLabel
	}
	s.stateMu.RUnlock()
	if !ok || !app.Approved {
		s.recordTraffic(userName, appName, http.StatusNotFound, 0, 0, time.Since(started), true)
		s.addPortflareFallbackHeaders(w.Header(), r, appName, publicUserLabel, settings)
		writeError(w, http.StatusNotFound, "app is not available")
		return
	}

	s.clientsMu.RLock()
	client := s.clients[userName]
	s.clientsMu.RUnlock()
	if client == nil {
		s.recordTraffic(userName, appName, http.StatusBadGateway, 0, 0, time.Since(started), true)
		s.addPortflareFallbackHeaders(w.Header(), r, appName, publicUserLabel, settings)
		writeError(w, http.StatusBadGateway, "client is offline")
		return
	}
	if _, ok := client.apps[appName]; !ok {
		s.recordTraffic(userName, appName, http.StatusBadGateway, 0, 0, time.Since(started), true)
		s.addPortflareFallbackHeaders(w.Header(), r, appName, publicUserLabel, settings)
		writeError(w, http.StatusBadGateway, "app is not connected")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBodyBytes))
	if err != nil {
		s.recordTraffic(userName, appName, http.StatusBadRequest, 0, 0, time.Since(started), true)
		s.addPortflareFallbackHeaders(w.Header(), r, appName, publicUserLabel, settings)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	bytesIn := int64(len(body))

	requestID := randomToken(16)
	pending := &pendingResponse{ch: make(chan TunnelResponse, 1)}
	s.pendingMu.Lock()
	s.pending[requestID] = pending
	s.pendingMu.Unlock()

	targetURL := *r.URL
	targetURL.Scheme = "http"
	targetURL.Host = "local"

	reqMsg := TunnelRequest{
		Type:       protocoltypes.MessageTypeRequest,
		RequestID:  requestID,
		AppName:    appName,
		Method:     r.Method,
		URL:        targetURL.String(),
		Headers:    cloneHeader(r.Header),
		BodyBase64: base64.StdEncoding.EncodeToString(body),
	}

	if err := s.sendRequest(client, reqMsg); err != nil {
		s.pendingMu.Lock()
		delete(s.pending, requestID)
		s.pendingMu.Unlock()
		s.recordTraffic(userName, appName, http.StatusBadGateway, bytesIn, 0, time.Since(started), true)
		s.addPortflareFallbackHeaders(w.Header(), r, appName, publicUserLabel, settings)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()

	select {
	case resp := <-pending.ch:
		if resp.Error != "" {
			s.recordTraffic(userName, appName, http.StatusBadGateway, bytesIn, 0, time.Since(started), true)
			s.addPortflareFallbackHeaders(w.Header(), r, appName, publicUserLabel, settings)
			writeError(w, http.StatusBadGateway, resp.Error)
			return
		}
		affordance := s.servedByAffordance(r, appName, publicUserLabel, settings)
		prepared, err := prepareProxiedResponseWithSettings(r, resp, affordance, settings)
		if err != nil {
			s.recordTraffic(userName, appName, http.StatusBadGateway, bytesIn, 0, time.Since(started), true)
			s.addPortflareFallbackHeaders(w.Header(), r, appName, publicUserLabel, settings)
			writeError(w, http.StatusBadGateway, "invalid upstream response")
			return
		}
		if prepared.decision != responseDecorationSkip {
			s.addPortflareFallbackHeaders(prepared.headers, r, appName, publicUserLabel, settings)
		}
		for k, values := range prepared.headers {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(prepared.status)
		written := 0
		if len(prepared.body) > 0 {
			written, _ = w.Write(prepared.body)
		}
		s.recordTraffic(userName, appName, prepared.status, bytesIn, int64(written), time.Since(started), prepared.status >= 500)
	case <-ctx.Done():
		s.pendingMu.Lock()
		delete(s.pending, requestID)
		s.pendingMu.Unlock()
		s.recordTraffic(userName, appName, http.StatusGatewayTimeout, bytesIn, 0, time.Since(started), true)
		s.addPortflareFallbackHeaders(w.Header(), r, appName, publicUserLabel, settings)
		writeError(w, http.StatusGatewayTimeout, "upstream request timed out")
	}
}

func (s *Server) ensurePortListener(port int, userName, appName string) error {
	if port <= 0 {
		return nil
	}
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()
	if _, ok := s.listeners[port]; ok {
		return nil
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	s.listeners[port] = ln
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.proxyToApp(w, r, userName, appName)
	})
	go func() {
		if err := http.Serve(ln, handler); err != nil && !strings.Contains(err.Error(), "closed") {
			s.logger.Error("dynamic port listener failed", "port", port, "error", err)
		}
	}()
	s.logger.Info("dynamic port listener ready", "port", port, "user", userName, "app", appName)
	return nil
}

func (s *Server) closeDynamicListeners() {
	s.listenersMu.Lock()
	defer s.listenersMu.Unlock()
	for port, ln := range s.listeners {
		_ = ln.Close()
		delete(s.listeners, port)
	}
}

func (s *Server) requireIdentity(w http.ResponseWriter, r *http.Request) (authIdentity, bool) {
	if s.cfg.DisableAuth {
		id := authIdentity{
			UserName:        strings.TrimSpace(s.cfg.LocalDevUser),
			PublicUserLabel: userLabel(s.cfg.LocalDevUser),
			Email:           strings.TrimSpace(strings.ToLower(s.cfg.LocalDevEmail)),
			IsAdmin:         true,
		}
		if id.UserName == "" || id.PublicUserLabel == "" {
			writeError(w, http.StatusInternalServerError, "invalid local development identity configuration")
			return authIdentity{}, false
		}
		return id, true
	}

	rawUserName := strings.TrimSpace(r.Header.Get("X-Auth-Request-User"))
	id := authIdentity{
		UserName:        rawUserName,
		PublicUserLabel: userLabel(rawUserName),
		Email:           strings.TrimSpace(strings.ToLower(r.Header.Get("X-Auth-Request-Email"))),
	}
	id.IsAdmin = isAdmin(id.UserName, id.Email, s.cfg.AdminUsers)
	if id.UserName == "" {
		writeError(w, http.StatusUnauthorized, "missing X-Auth-Request-User header")
		return authIdentity{}, false
	}
	if id.PublicUserLabel == "" {
		writeError(w, http.StatusBadRequest, "user label is empty after normalization")
		return authIdentity{}, false
	}
	return id, true
}

func (s *Server) ensureUser(identity authIdentity) (*User, error) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if user, ok := s.state.Users[identity.UserName]; ok {
		changed := false
		if user.PublicUserLabel == "" {
			user.PublicUserLabel = identity.PublicUserLabel
			changed = true
		}
		user.PublicUserAliases = uniqueUserLabels(user.PublicUserAliases)
		if identity.Email != "" && user.Email != identity.Email {
			user.Email = identity.Email
			changed = true
		}
		if changed {
			user.UpdatedAt = time.Now().UTC()
			_ = s.saveStateLocked()
			s.notifyUISubscribers()
		}
		return user, nil
	}
	if !s.state.RegistrationOpen {
		return nil, errors.New("registration is closed")
	}
	if _, err := validateNormalizedPublicUserLabel(identity.PublicUserLabel); err != nil {
		return nil, err
	}
	for _, existing := range s.state.Users {
		if existing.PublicUserLabel == identity.PublicUserLabel || containsUserLabel(existing.PublicUserAliases, identity.PublicUserLabel) {
			return nil, fmt.Errorf("public user label %q is already taken; choose a new slug", identity.PublicUserLabel)
		}
	}

	now := time.Now().UTC()
	user := &User{
		UserName:        identity.UserName,
		PublicUserLabel: identity.PublicUserLabel,
		Email:           identity.Email,
		APIKey:          newAPIKey(),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	s.state.Users[user.UserName] = user
	_ = s.saveStateLocked()
	s.notifyUISubscribers()
	return user, nil
}

func (s *Server) findUserByKey(key string) (*User, bool) {
	if !protocolvalidation.IsValidClientKey(strings.TrimSpace(key)) {
		return nil, false
	}
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	for _, u := range s.state.Users {
		if subtleEqual(u.APIKey, key) {
			return u, true
		}
	}
	return nil, false
}

func (s *Server) findUserByPublicLabel(label string) (*User, bool) {
	user, found, _ := s.findUserByAnyPublicLabel(label)
	return user, found
}

func (s *Server) findUserByAnyPublicLabel(label string) (*User, bool, bool) {
	normalized := userLabel(label)
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	for _, u := range s.state.Users {
		if u.PublicUserLabel == normalized {
			return u, true, true
		}
		if containsUserLabel(u.PublicUserAliases, normalized) {
			return u, true, false
		}
	}
	return nil, false, false
}

func (s *Server) send(client *TunnelClient, msg TunnelResponse) error {
	client.sendMu.Lock()
	defer client.sendMu.Unlock()
	return client.conn.WriteJSON(msg)
}

func (s *Server) sendRequest(client *TunnelClient, msg TunnelRequest) error {
	client.sendMu.Lock()
	defer client.sendMu.Unlock()
	return client.conn.WriteJSON(msg)
}

func (s *Server) matchAppHost(host string) (string, string, string, bool) {
	suffix := "." + s.cfg.PublicBaseDomain
	if !strings.HasSuffix(host, suffix) {
		return "", "", "", false
	}

	label := slug(strings.TrimSuffix(host, suffix))
	if label == "" || strings.Contains(label, ".") || label == "admin" {
		return "", "", "", false
	}

	idx := strings.LastIndex(label, "-")
	if idx <= 0 || idx == len(label)-1 {
		return "", "", "", false
	}
	appPart := slug(label[:idx])
	userPart := userLabel(label[idx+1:])
	if appPart == "" || userPart == "" {
		return "", "", "", false
	}

	matchedUser, found, canonical := s.findUserByAnyPublicLabel(userPart)
	if !found {
		return "", "", "", false
	}

	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	app, ok := s.state.Apps[appKey(matchedUser.UserName, appPart)]
	if !ok {
		return "", "", "", false
	}
	redirectHost := ""
	if !canonical {
		redirectHost = appHostLabel(app.AppName, matchedUser.PublicUserLabel) + "." + s.cfg.PublicBaseDomain
	}
	return app.UserName, app.AppName, redirectHost, true
}

func (s *Server) matchUserHost(host string) (string, bool) {
	suffix := "." + s.cfg.PublicBaseDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	labels := strings.Split(strings.TrimSuffix(host, suffix), ".")
	if len(labels) != 1 {
		return "", false
	}
	label := userLabel(labels[0])
	if label == "admin" || label == "" {
		return "", false
	}
	return label, true
}

func withLogging(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "host", r.Host, "duration", time.Since(started))
	})
}

func cloneHeader(h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, values := range h {
		cp := make([]string, len(values))
		copy(cp, values)
		out[k] = cp
	}
	return out
}

func handleReadyz(application string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, buildinfo.Ready(application))
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func randomToken(bytesLen int) string {
	buf := make([]byte, bytesLen)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func newAPIKey() string {
	return "pf_" + randomToken(24)
}

func subtleEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func appKey(user, app string) string { return user + "/" + app }

func appHostLabel(app, publicUserLabel string) string {
	return slug(app) + "-" + userLabel(publicUserLabel)
}

func canonicalHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if strings.Contains(host, ":") {
		if parsedHost, _, err := net.SplitHostPort(host); err == nil {
			return parsedHost
		}
	}
	return host
}

func slug(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ""
	}
	var b strings.Builder
	dash := false
	for _, r := range v {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	return out
}

func userLabel(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func validatePublicUserLabel(v string) (string, error) {
	return validateNormalizedPublicUserLabel(userLabel(v))
}

func validateNormalizedPublicUserLabel(v string) (string, error) {
	if v == "" {
		return "", errors.New("public user label must contain at least one lowercase letter or digit")
	}
	if len(v) < minPublicUserLabelLen {
		return "", fmt.Errorf("public user label must be at least %d characters", minPublicUserLabelLen)
	}
	if len(v) > maxPublicUserLabelLen {
		return "", fmt.Errorf("public user label must be at most %d characters", maxPublicUserLabelLen)
	}
	if isReservedPublicUserLabel(v) {
		return "", fmt.Errorf("public user label %q is reserved; choose a new slug", v)
	}
	return v, nil
}

func isReservedPublicUserLabel(v string) bool {
	switch v {
	case "admin", "api", "www", "static", "assets", "me":
		return true
	default:
		return false
	}
}

func containsUserLabel(values []string, want string) bool {
	want = userLabel(want)
	for _, value := range values {
		if userLabel(value) == want {
			return true
		}
	}
	return false
}

func uniqueUserLabels(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		normalized := userLabel(value)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

func parseSet(v, sep string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, item := range strings.Split(v, sep) {
		item = slug(item)
		if item == "" {
			continue
		}
		out[item] = struct{}{}
	}
	return out
}

func parseUserSet(v, sep string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, item := range strings.Split(v, sep) {
		item = userLabel(item)
		if item == "" {
			continue
		}
		out[item] = struct{}{}
	}
	return out
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(key string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fallback
	}
	return parsed
}

func bearerToken(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return strings.TrimSpace(v[7:])
	}
	return ""
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(strings.ToLower(r.Header.Get("Accept")), "application/json")
}

func settingBoolFormValue(r *http.Request, current bool) (bool, error) {
	raw := strings.TrimSpace(r.Form.Get("value"))
	if raw == "" {
		return !current, nil
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("value for %s must be true or false", strings.TrimSpace(r.Form.Get("setting")))
	}
	return parsed, nil
}

func isAdmin(userName, email string, admins map[string]struct{}) bool {
	if _, ok := admins[userLabel(userName)]; ok {
		return true
	}
	email = userLabel(strings.ReplaceAll(email, "@", ""))
	_, ok := admins[email]
	return ok
}

func rewriteRequestURLHost(r *http.Request, host string) string {
	target := *r.URL
	target.Scheme = requestScheme(r)
	target.Host = host
	return target.String()
}

func requestScheme(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		return forwarded
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

const dashboardTemplates = `
{{define "report_abuse"}}
<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Report abuse</title>
    <style>
      body { color: #17202a; font-family: sans-serif; line-height: 1.55; max-width: 760px; margin: 2rem auto; padding: 0 1rem; }
      label { display: block; font-weight: 600; margin-top: 1rem; }
      input, select, textarea { box-sizing: border-box; display: block; font: inherit; margin-top: .3rem; padding: .55rem; width: 100%; }
      textarea { min-height: 9rem; }
      button { margin-top: 1rem; padding: .65rem .85rem; }
      .muted { color: #5f6b76; }
      .hp { left: -10000px; position: absolute; top: auto; }
    </style>
  </head>
  <body>
    <h1>Report abuse</h1>
    <p>Portflare routes traffic for independently operated apps. Reports may be shared with the site operator as needed to investigate abuse.</p>
    <p>Portflare stores only the report details needed to investigate abuse: the reported URL, category, description, optional reporter contact, route context, requester IP address, user agent, and timestamps.</p>
    <p class="muted">Routine report records should be retained only for the operator's abuse-response window, with IP and user-agent metadata truncated or deleted after 180 days unless a legal hold or safety obligation requires longer retention.</p>
    <p class="muted">Do not submit passwords, API keys, private tokens, or other secrets. If there is imminent danger, contact local emergency services.</p>
    <form method="post" action="/api/report-abuse">
      <label>Reported URL
        <input type="text" name="reported_url" value="{{.ReportedURL}}" maxlength="2048" required>
      </label>
      <label>Category
        <select name="category" required>
          {{range .Categories}}<option value="{{index . "Value"}}">{{index . "Label"}}</option>{{end}}
        </select>
      </label>
      <label>Description
        <textarea name="description" maxlength="4000" required>{{.Context}}</textarea>
      </label>
      <label>Reporter contact (optional)
        <input type="text" name="reporter_contact" maxlength="320" autocomplete="email">
      </label>
      <label class="hp">Website
        <input type="text" name="website" tabindex="-1" autocomplete="off">
      </label>
      <input type="hidden" name="context" value="{{.Context}}">
      <input type="hidden" name="form_started_at" value="{{.FormStartedAt}}">
      <button type="submit">Submit report</button>
    </form>
  </body>
</html>
{{end}}

{{define "learn_more"}}
<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>About Portflare</title>
    <style>
      body { color: #17202a; font-family: sans-serif; line-height: 1.55; max-width: 760px; margin: 2rem auto; padding: 0 1rem; }
      a { color: #0b5cab; }
      code { background: #f5f5f5; padding: .15rem .3rem; }
      .cta { display: inline-block; margin-top: .5rem; padding: .65rem .85rem; border: 1px solid #0b5cab; color: #0b5cab; text-decoration: none; }
      .muted { color: #5f6b76; }
    </style>
  </head>
  <body>
    <h1>About Portflare</h1>
    <p>Portflare routes public requests to independently operated apps. App owners connect their own services to Portflare, and approved apps can receive public app URLs such as <code>https://&lt;app&gt;-&lt;user-label&gt;.{{.BaseDomain}}</code> or <code>/r/&lt;user&gt;/&lt;app&gt;</code>.</p>

    <h2>What the served-by notice means</h2>
    <p>A <strong>Served by Portflare</strong> notice means the page reached you through Portflare routing infrastructure. Portflare does not create, review, or endorse the app content, and the app operator remains responsible for what the app serves.</p>

    <h2>Report abuse</h2>
    {{if .ReportAbuseEnabled}}
    <p>If a public Portflare URL appears to host phishing, malware, scams, spam, unauthorized private content, or other abusive material, report the URL and include enough context for review.</p>
    <p><a class="cta" href="{{.ReportAbuseURL}}">Report abuse</a></p>
    {{else}}
    <p>Report abuse intake is disabled by this Portflare administrator.</p>
    {{end}}
    <p class="muted">Do not include passwords, API keys, private tokens, or other secrets in an abuse report.</p>
  </body>
</html>
{{end}}

{{define "admin_abuse_report"}}
<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Abuse report {{.Report.ID}}</title>
    <style>
      body { font-family: sans-serif; max-width: 1000px; margin: 2rem auto; padding: 0 1rem; }
      table { width: 100%; border-collapse: collapse; margin: 1rem 0; }
      th, td { border: 1px solid #ddd; padding: .5rem; text-align: left; vertical-align: top; }
      code { background: #f5f5f5; padding: .15rem .3rem; }
      textarea { box-sizing: border-box; min-height: 7rem; width: 100%; }
      .muted { color: #666; }
    </style>
  </head>
  <body>
    <p><a href="/admin#abuse-reports">Back to admin queue</a></p>
    <h1>Abuse report {{.Report.ID}}</h1>
    <p>Status: <strong>{{.Report.Status}}</strong></p>
    <p>Reported URL: <code>{{.Report.ReportedURL}}</code></p>
    <p>Category: {{.Report.Category}}</p>
    <p>Description: {{.Report.Description}}</p>

    <h2>Reporter metadata</h2>
    <table>
      <tr><th>Contact</th><td>{{.Report.ReporterContact}}</td></tr>
      <tr><th>IP</th><td>{{.Report.ReporterIP}}</td></tr>
      <tr><th>User agent</th><td>{{.Report.ReporterUserAgent}}</td></tr>
      <tr><th>Context</th><td>{{.Report.Context}}</td></tr>
    </table>

    <h2>Resolved target</h2>
    <table>
      <tr><th>User</th><td>{{with .ReportedUser}}{{index . "user_name"}} ({{index . "public_user_label"}}) {{index . "email"}}{{else}}Unknown{{end}}</td></tr>
      <tr><th>App</th><td>{{with .CurrentApp}}{{index . "app_name"}}: <strong>{{index . "status"}}</strong>, connected={{index . "connected"}}{{else}}Unknown{{end}}</td></tr>
      <tr><th>Public URL</th><td>{{with .ActionLinks}}{{with index . "app_public_url"}}<a href="{{.}}">{{.}}</a>{{else}}-{{end}}{{end}}</td></tr>
      <tr><th>Actions</th><td>{{with .ActionLinks}}{{with index . "approve_app"}}<code>{{.}}</code>{{else}}-{{end}}{{end}}</td></tr>
    </table>

    <h2>Update status</h2>
    <form method="post" action="/api/admin/abuse-reports/{{.Report.ID}}/status">
      <select name="status">
        {{range .StatusOptions}}<option value="{{index . "Value"}}" {{if eq $.Report.Status (index . "Value")}}selected{{end}}>{{index . "Label"}}</option>{{end}}
      </select>
      <textarea name="note" placeholder="Optional internal note"></textarea>
      <button type="submit">Update status</button>
    </form>

    <h2>Internal notes</h2>
    {{range .Report.InternalNotes}}
    <p><strong>{{.ActorUserName}}</strong> <span class="muted">{{.CreatedAt}}</span><br>{{.Body}}</p>
    {{else}}
    <p class="muted">No internal notes yet.</p>
    {{end}}
    <form method="post" action="/api/admin/abuse-reports/{{.Report.ID}}/notes">
      <textarea name="body" required></textarea>
      <button type="submit">Add note</button>
    </form>

    <h2>Prior related reports</h2>
    <table>
      <tr><th>Case</th><th>Status</th><th>Category</th><th>Reported URL</th><th>Created</th></tr>
      {{range .RelatedReports}}
      <tr><td><a href="/admin/abuse-reports/{{index . "id"}}">{{index . "id"}}</a></td><td>{{index . "status"}}</td><td>{{index . "category"}}</td><td><code>{{index . "reported_url"}}</code></td><td>{{index . "created_at"}}</td></tr>
      {{else}}
      <tr><td colspan="5">No prior related reports.</td></tr>
      {{end}}
    </table>
  </body>
</html>
{{end}}

{{define "admin"}}
<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Portflare admin</title>
    <style>
      body { font-family: sans-serif; max-width: 1100px; margin: 2rem auto; padding: 0 1rem; }
      table { width: 100%; border-collapse: collapse; margin: 1rem 0; }
      th, td { border: 1px solid #ddd; padding: .5rem; text-align: left; }
      code { background: #f5f5f5; padding: .15rem .3rem; }
      .muted { color: #666; }
    </style>
  </head>
  <body>
    <h1>Portflare admin</h1>
    <p>Signed in as <strong id="identity-user">{{index .Identity "user_name"}}</strong></p>
    <p>Registration open: <strong id="registration-open">{{.RegistrationOpen}}</strong></p>
    <form method="post" action="/admin/toggle-registration"><button type="submit">Toggle registration</button></form>

    <h2>App approval settings</h2>
    <ul>
      <li>Users can approve their own apps: <strong id="allow-user-app-approval">{{.AllowUserAppApproval}}</strong></li>
      <li>Auto-approve apps for users: <strong id="auto-approve-for-users">{{.AutoApproveForUsers}}</strong></li>
      <li>Auto-approve apps for admins: <strong id="auto-approve-for-admins">{{.AutoApproveForAdmins}}</strong></li>
    </ul>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="allow_user_app_approval"><button type="submit">Toggle user self-approval</button></form>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="auto_approve_for_users"><button type="submit">Toggle auto-approve for users</button></form>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="auto_approve_for_admins"><button type="submit">Toggle auto-approve for admins</button></form>

    <h2>Served-by and report abuse settings</h2>
    {{if .ServedByWarnings}}
    <ul>
      {{range .ServedByWarnings}}<li><strong>Warning:</strong> {{.}}</li>{{end}}
    </ul>
    {{end}}
    <ul>
      <li>Served-by enabled: <strong id="served-by-enabled">{{.ServedByEnabled}}</strong></li>
      <li>Served-by mode: <strong id="served-by-mode">{{.ServedByMode}}</strong></li>
      <li>HTML injection enabled: <strong id="served-by-html-injection-enabled">{{.ServedByHTMLInjectionEnabled}}</strong></li>
      <li>Report abuse enabled: <strong id="report-abuse-enabled">{{.ReportAbuseEnabled}}</strong></li>
      <li>Per-app disable allowed: <strong id="served-by-app-disable-allowed">{{.ServedByAppDisableAllowed}}</strong></li>
      <li>Emergency force visible: <strong id="served-by-emergency-force-visible">{{.ServedByEmergencyForceVisible}}</strong></li>
    </ul>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="served_by_enabled"><button type="submit">Toggle served-by</button></form>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="served_by_mode"><input type="hidden" name="value" value="visible_and_headers"><button type="submit">Use visible and headers mode</button></form>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="served_by_mode"><input type="hidden" name="value" value="headers_only"><button type="submit">Use headers-only mode</button></form>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="served_by_html_injection_enabled"><button type="submit">Toggle HTML injection</button></form>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="report_abuse_enabled"><button type="submit">Toggle report abuse</button></form>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="served_by_app_disable_allowed"><button type="submit">Toggle per-app disable allowance</button></form>
    <form method="post" action="/admin/toggle-setting"><input type="hidden" name="setting" value="served_by_emergency_force_visible"><button type="submit">Toggle emergency force visible</button></form>

    <h2 id="abuse-reports">Abuse reports</h2>
    <form method="get" action="/api/admin/abuse-reports">
      <select name="status">
        <option value="">Any status</option>
        {{range .AbuseReportStatusOptions}}<option value="{{index . "Value"}}">{{index . "Label"}}</option>{{end}}
      </select>
      <select name="category">
        <option value="">Any category</option>
        {{range .AbuseReportCategoryOptions}}<option value="{{index . "Value"}}">{{index . "Label"}}</option>{{end}}
      </select>
      <input type="text" name="user" placeholder="User">
      <input type="text" name="app" placeholder="App">
      <input type="text" name="reported_url" placeholder="Reported URL">
      <button type="submit">Filter reports</button>
    </form>
    <table>
      <tr><th>Case</th><th>Status</th><th>Category</th><th>Reported URL</th><th>User</th><th>App</th><th>Created</th></tr>
      <tbody id="abuse-reports-body">
      {{range .AbuseReports}}
      <tr>
        <td><a href="/admin/abuse-reports/{{index . "id"}}">{{index . "id"}}</a></td>
        <td>{{index . "status"}}</td>
        <td>{{index . "category"}}</td>
        <td><code>{{index . "reported_url"}}</code></td>
        <td>{{index . "reported_user_name"}}</td>
        <td>{{index . "reported_app_name"}}</td>
        <td>{{index . "created_at"}}</td>
      </tr>
      {{else}}
      <tr><td colspan="7">No abuse reports submitted.</td></tr>
      {{end}}
      </tbody>
    </table>

    <h2>Users</h2>
    <table>
      <tr><th>User</th><th>Email</th><th>Created</th></tr>
      <tbody id="users-body">
      {{range .Users}}
      <tr><td><a href="/me">{{.UserName}}</a></td><td>{{.Email}}</td><td>{{.CreatedAt}}</td></tr>
      {{else}}
      <tr><td colspan="3">No users yet.</td></tr>
      {{end}}
      </tbody>
    </table>

    <h2>Applications</h2>
    <table>
      <tr><th>User</th><th>App</th><th>Approved</th><th>Connected</th><th>Public</th><th>Port</th><th>Status</th><th>Served-by policy</th><th>Override</th></tr>
      <tbody id="apps-body">
      {{range $app := .Apps}}
      <tr>
        <td>{{index $app "user_name"}}</td>
        <td>{{index $app "app_name"}}</td>
        <td>{{index $app "approved"}}</td>
        <td>{{index $app "connected"}}</td>
        <td><code>{{index $app "public_url"}}</code></td>
        <td>{{with index $app "public_port"}}{{.}}{{else}}-{{end}}</td>
        <td>{{if index $app "approved"}}approved{{else}}pending{{end}}</td>
        <td><strong>{{index $app "effective_served_by_policy"}}</strong><br><span class="muted">override: {{index $app "served_by_override"}}</span>{{with index $app "served_by_override_reason"}}<br><span class="muted">reason: {{.}}</span>{{end}}</td>
        <td>
          <form method="post" action="/api/admin/app-served-by-override">
            <input type="hidden" name="user" value="{{index $app "user_name"}}">
            <input type="hidden" name="app" value="{{index $app "app_name"}}">
            <select name="override">
              {{range $opt := $.ServedByAppOverrideOptions}}<option value="{{index $opt "Value"}}" {{if eq (index $app "served_by_override") (index $opt "Value")}}selected{{end}}>{{index $opt "Label"}}</option>{{end}}
            </select>
            <input type="text" name="reason" value="{{index $app "served_by_override_reason"}}" placeholder="Reason for weakening">
            <button type="submit">Update</button>
          </form>
        </td>
      </tr>
      {{else}}
      <tr><td colspan="9">No applications registered.</td></tr>
      {{end}}
      </tbody>
    </table>
    <p class="muted" id="live-status">Live updates: connecting…</p>
    <script>
      (() => {
        const status = document.getElementById('live-status');
        const abuseReportsBody = document.getElementById('abuse-reports-body');
        const usersBody = document.getElementById('users-body');
        const appsBody = document.getElementById('apps-body');
        const registrationOpen = document.getElementById('registration-open');
        const allowUserAppApproval = document.getElementById('allow-user-app-approval');
        const autoApproveForUsers = document.getElementById('auto-approve-for-users');
        const autoApproveForAdmins = document.getElementById('auto-approve-for-admins');
        const servedByEnabled = document.getElementById('served-by-enabled');
        const servedByMode = document.getElementById('served-by-mode');
        const servedByHTMLInjectionEnabled = document.getElementById('served-by-html-injection-enabled');
        const reportAbuseEnabled = document.getElementById('report-abuse-enabled');
        const servedByAppDisableAllowed = document.getElementById('served-by-app-disable-allowed');
        const servedByEmergencyForceVisible = document.getElementById('served-by-emergency-force-visible');
        const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const ws = new WebSocket(proto + '//' + window.location.host + '/ws/ui');
        const esc = (value) => String(value ?? '').replace(/[&<>"']/g, (ch) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
        const overrideForm = (a, options) => {
          const selected = String(a.served_by_override || 'inherit');
          const opts = (options || []).map((o) => '<option value="' + esc(o.Value) + '"' + (selected === o.Value ? ' selected' : '') + '>' + esc(o.Label) + '</option>').join('');
          return '<form method="post" action="/api/admin/app-served-by-override"><input type="hidden" name="user" value="' + esc(a.user_name) + '"><input type="hidden" name="app" value="' + esc(a.app_name) + '"><select name="override">' + opts + '</select> <input type="text" name="reason" value="' + esc(a.served_by_override_reason || '') + '" placeholder="Reason for weakening"> <button type="submit">Update</button></form>';
        };
        const render = async () => {
          const res = await fetch('/api/admin/state', {headers: {'accept': 'application/json'}});
          if (!res.ok) return;
          const data = await res.json();
          registrationOpen.textContent = String(data.registration_open);
          allowUserAppApproval.textContent = String(data.allow_user_app_approval);
          autoApproveForUsers.textContent = String(data.auto_approve_for_users);
          autoApproveForAdmins.textContent = String(data.auto_approve_for_admins);
          servedByEnabled.textContent = String(data.served_by_enabled);
          servedByMode.textContent = String(data.served_by_mode);
          servedByHTMLInjectionEnabled.textContent = String(data.served_by_html_injection_enabled);
          reportAbuseEnabled.textContent = String(data.report_abuse_enabled);
          servedByAppDisableAllowed.textContent = String(data.served_by_app_disable_allowed);
          servedByEmergencyForceVisible.textContent = String(data.served_by_emergency_force_visible);
          abuseReportsBody.innerHTML = data.abuse_reports.length ? data.abuse_reports.map((r) => '<tr><td><a href="/admin/abuse-reports/' + esc(r.id) + '">' + esc(r.id) + '</a></td><td>' + esc(r.status) + '</td><td>' + esc(r.category) + '</td><td><code>' + esc(r.reported_url) + '</code></td><td>' + esc(r.reported_user_name || '') + '</td><td>' + esc(r.reported_app_name || '') + '</td><td>' + esc(r.created_at) + '</td></tr>').join('') : '<tr><td colspan="7">No abuse reports submitted.</td></tr>';
          usersBody.innerHTML = data.users.length ? data.users.map((u) => '<tr><td><a href="/me">' + esc(u.user_name) + '</a></td><td>' + esc(u.email) + '</td><td>' + esc(u.created_at) + '</td></tr>').join('') : '<tr><td colspan="3">No users yet.</td></tr>';
          appsBody.innerHTML = data.apps.length ? data.apps.map((a) => '<tr><td>' + esc(a.user_name) + '</td><td>' + esc(a.app_name) + '</td><td>' + esc(a.approved) + '</td><td>' + esc(a.connected) + '</td><td><code>' + esc(a.public_url) + '</code></td><td>' + (a.public_port || '-') + '</td><td>' + (a.approved ? 'approved' : 'pending') + '</td><td><strong>' + esc(a.effective_served_by_policy) + '</strong><br><span class="muted">override: ' + esc(a.served_by_override) + '</span>' + (a.served_by_override_reason ? '<br><span class="muted">reason: ' + esc(a.served_by_override_reason) + '</span>' : '') + '</td><td>' + overrideForm(a, data.served_by_app_override_options) + '</td></tr>').join('') : '<tr><td colspan="9">No applications registered.</td></tr>';
          status.textContent = 'Live updates: synced';
        };
        ws.onopen = () => { status.textContent = 'Live updates: connected'; };
        ws.onclose = () => { status.textContent = 'Live updates: disconnected'; };
        ws.onmessage = async (event) => {
          try {
            const message = JSON.parse(event.data);
            if (message.type === 'refresh') {
              status.textContent = 'Live updates: syncing…';
              await render();
            }
          } catch (_) {}
        };
      })();
    </script>
  </body>
</html>
{{end}}

{{define "user"}}
<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Portflare user</title>
    <style>
      body { font-family: sans-serif; max-width: 1100px; margin: 2rem auto; padding: 0 1rem; }
      table { width: 100%; border-collapse: collapse; margin: 1rem 0; }
      th, td { border: 1px solid #ddd; padding: .5rem; text-align: left; }
      code { background: #f5f5f5; padding: .15rem .3rem; }
    </style>
  </head>
  <body>
    <h1>Portflare</h1>
    <p>User: <strong id="user-name">{{.User.UserName}}</strong></p>
    <p>Public user label: <strong id="user-public-label">{{.User.PublicUserLabel}}</strong></p>
    <p>Email: <strong id="user-email">{{.User.Email}}</strong></p>
    <p>Connection key: <code id="user-api-key">{{.User.APIKey}}</code></p>
    <form method="post" action="/api/me/rotate-key"><button type="submit">Rotate key</button></form>

    <h2>Public user label</h2>
    <p>Use only lowercase letters and digits. Dashes and special characters are removed during normalization.</p>
    {{if .Error}}<p style="color:#b00020"><strong>{{.Error}}</strong></p>{{end}}
    <form method="post" action="/api/me/public-user-label">
      <input id="public-user-label-input" type="text" name="public_user_label" value="{{.User.PublicUserLabel}}" pattern="[a-z0-9]+" required>
      <button type="submit">Update public user label</button>
    </form>

    <h2>Routes</h2>
    <p>Authenticated dashboard: <code id="dashboard-url">https://{{.User.PublicUserLabel}}.{{.BaseDomain}}</code></p>

    <h2>Applications</h2>
    <table>
      <tr><th>App</th><th>Approved</th><th>Connected</th><th>Subdomain</th><th>Port</th><th>Status</th><th>Served-by policy</th><th>Action</th></tr>
      <tbody id="user-apps-body">
      {{range .Apps}}
      <tr>
        <td>{{index . "app_name"}}</td>
        <td>{{index . "approved"}}</td>
        <td>{{index . "connected"}}</td>
        <td><code>{{index . "public_url"}}</code></td>
        <td>{{with index . "public_port"}}{{.}}{{else}}-{{end}}</td>
        <td>{{index . "status"}}</td>
        <td>{{index . "effective_served_by_policy"}} <span style="color:#666">(override: {{index . "served_by_override"}})</span></td>
        <td>{{if index . "can_approve"}}<form method="post" action="/api/me/approve"><input type="hidden" name="user" value="{{index . "user_name"}}"><input type="hidden" name="app" value="{{index . "app_name"}}"><button type="submit">Approve</button></form>{{else}}-{{end}}</td>
      </tr>
      {{else}}
      <tr><td colspan="8">No applications registered yet.</td></tr>
      {{end}}
      </tbody>
    </table>
    <p style="color:#666" id="live-status">Live updates: connecting…</p>
    <script>
      (() => {
        const status = document.getElementById('live-status');
        const appsBody = document.getElementById('user-apps-body');
        const userName = document.getElementById('user-name');
        const userPublicLabel = document.getElementById('user-public-label');
        const userEmail = document.getElementById('user-email');
        const userAPIKey = document.getElementById('user-api-key');
        const labelInput = document.getElementById('public-user-label-input');
        const dashboardURL = document.getElementById('dashboard-url');
        const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const ws = new WebSocket(proto + '//' + window.location.host + '/ws/ui');
        const esc = (value) => String(value ?? '').replace(/[&<>"']/g, (ch) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
        const render = async () => {
          const res = await fetch('/api/me/state', {headers: {'accept': 'application/json'}});
          if (!res.ok) return;
          const data = await res.json();
          userName.textContent = data.user.user_name;
          userPublicLabel.textContent = data.user.public_user_label;
          userEmail.textContent = data.user.email;
          userAPIKey.textContent = data.user.api_key;
          labelInput.value = data.user.public_user_label;
          dashboardURL.textContent = 'https://' + data.user.public_user_label + '.' + data.base_domain;
          appsBody.innerHTML = data.apps.length ? data.apps.map((a) => '<tr><td>' + esc(a.app_name) + '</td><td>' + esc(a.approved) + '</td><td>' + esc(a.connected) + '</td><td><code>' + esc(a.public_url) + '</code></td><td>' + (a.public_port || '-') + '</td><td>' + esc(a.status) + '</td><td>' + esc(a.effective_served_by_policy) + ' <span style="color:#666">(override: ' + esc(a.served_by_override) + ')</span></td><td>' + (a.can_approve ? '<form method="post" action="/api/me/approve"><input type="hidden" name="user" value="' + esc(a.user_name) + '"><input type="hidden" name="app" value="' + esc(a.app_name) + '"><button type="submit">Approve</button></form>' : '-') + '</td></tr>').join('') : '<tr><td colspan="8">No applications registered yet.</td></tr>';
          status.textContent = 'Live updates: synced';
        };
        ws.onopen = () => { status.textContent = 'Live updates: connected'; };
        ws.onclose = () => { status.textContent = 'Live updates: disconnected'; };
        ws.onmessage = async (event) => {
          try {
            const message = JSON.parse(event.data);
            if (message.type === 'refresh') {
              status.textContent = 'Live updates: syncing…';
              await render();
            }
          } catch (_) {}
        };
      })();
    </script>
  </body>
</html>
{{end}}
`
