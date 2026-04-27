package main

import (
  "context"
  "crypto/rand"
  "encoding/base64"
  "encoding/hex"
  "encoding/json"
  "errors"
  "fmt"
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
  ListenAddr             string
  PublicBaseDomain       string
  StatePath              string
  AdminUsers             map[string]struct{}
  RegistrationOpen       bool
  AllowUserAppApproval   bool
  AutoApproveForUsers    bool
  AutoApproveForAdmins   bool
  TrustedProxyOnly       bool
  DisableAuth            bool
  LocalDevUser           string
  LocalDevEmail          string
  MaxBodyBytes           int64
  ReadTimeout            time.Duration
  WriteTimeout           time.Duration
  IdleTimeout            time.Duration
  RequestTimeout         time.Duration
}

func loadConfig() Config {
  return Config{
    ListenAddr:       env("REVERSE_SERVER_LISTEN_ADDR", ":8080"),
    PublicBaseDomain: strings.Trim(strings.ToLower(env("REVERSE_BASE_DOMAIN", "reverse.example.test")), "."),
    StatePath:        env("REVERSE_STATE_PATH", "/var/lib/portflare/state.json"),
    AdminUsers:       parseUserSet(env("REVERSE_ADMIN_USERS", "admin"), ","),
    RegistrationOpen:     envBool("REVERSE_REGISTRATION_OPEN", true),
    AllowUserAppApproval: envBool("REVERSE_ALLOW_USER_APP_APPROVAL", false),
    AutoApproveForUsers:  envBool("REVERSE_AUTO_APPROVE_APPS_FOR_USERS", false),
    AutoApproveForAdmins: envBool("REVERSE_AUTO_APPROVE_APPS_FOR_ADMINS", false),
    TrustedProxyOnly:     envBool("REVERSE_TRUST_AUTH_HEADERS", true),
    DisableAuth:      envBool("REVERSE_DISABLE_AUTH", false),
    LocalDevUser:     env("REVERSE_LOCAL_DEV_USER", "localdev"),
    LocalDevEmail:    env("REVERSE_LOCAL_DEV_EMAIL", "localdev@example.test"),
    MaxBodyBytes:     envInt64("REVERSE_MAX_BODY_BYTES", 8<<20),
    ReadTimeout:      envDuration("REVERSE_READ_TIMEOUT", 15*time.Second),
    WriteTimeout:     envDuration("REVERSE_WRITE_TIMEOUT", 30*time.Second),
    IdleTimeout:      envDuration("REVERSE_IDLE_TIMEOUT", 120*time.Second),
    RequestTimeout:   envDuration("REVERSE_REQUEST_TIMEOUT", 60*time.Second),
  }
}

type State struct {
  RegistrationOpen     bool              `json:"registration_open"`
  AllowUserAppApproval bool              `json:"allow_user_app_approval"`
  AutoApproveForUsers  bool              `json:"auto_approve_for_users"`
  AutoApproveForAdmins bool              `json:"auto_approve_for_admins"`
  Users                map[string]*User  `json:"users"`
  Apps                 map[string]*App   `json:"apps"`
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
  ID          string    `json:"id"`
  UserName    string    `json:"user_name"`
  AppName     string    `json:"app_name"`
  PublicPort  int       `json:"public_port,omitempty"`
  Approved    bool      `json:"approved"`
  Connected   bool      `json:"connected"`
  LastSeenAt  time.Time `json:"last_seen_at"`
  CreatedAt   time.Time `json:"created_at"`
  UpdatedAt   time.Time `json:"updated_at"`
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

type Server struct {
  cfg           Config
  logger        *slog.Logger
  upgrader      websocket.Upgrader
  templates     *template.Template

  stateMu       sync.RWMutex
  state         State

  clientsMu     sync.RWMutex
  clients       map[string]*TunnelClient

  pendingMu     sync.Mutex
  pending       map[string]*pendingResponse

  listenersMu   sync.Mutex
  listeners     map[int]net.Listener

  uiSubsMu      sync.Mutex
  uiSubscribers map[chan struct{}]struct{}
}

const (
  minPublicUserLabelLen = 3
  maxPublicUserLabelLen = 32
)

func main() {
  if len(os.Args) > 1 {
    switch os.Args[1] {
    case "version", "--version", "-version", "-v":
      fmt.Println(buildinfo.Summary("reverse-server"))
      return
    case "help", "--help", "-h":
      fmt.Println("usage:")
      fmt.Println("  reverse-server")
      fmt.Println("  reverse-server version")
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
    logger.Info("reverse server listening", "addr", cfg.ListenAddr, "base_domain", cfg.PublicBaseDomain, "version", version, "commit", commit, "build_date", buildDate)
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
      s.state = State{RegistrationOpen: s.cfg.RegistrationOpen, AllowUserAppApproval: s.cfg.AllowUserAppApproval, AutoApproveForUsers: s.cfg.AutoApproveForUsers, AutoApproveForAdmins: s.cfg.AutoApproveForAdmins, Users: map[string]*User{}, Apps: map[string]*App{}}
      return s.saveStateLocked()
    }
    return fmt.Errorf("read state: %w", err)
  }

  var st State
  if err := json.Unmarshal(raw, &st); err != nil {
    return fmt.Errorf("decode state: %w", err)
  }
  if st.Users == nil {
    st.Users = map[string]*User{}
  }
  if st.Apps == nil {
    st.Apps = map[string]*App{}
  }
  changed := false
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
  mux.HandleFunc("/connect", s.handleConnect)
  mux.HandleFunc("/ws/ui", s.handleUIWebSocket)
  mux.HandleFunc("/admin", s.handleAdminPage)
  mux.HandleFunc("/api/admin/state", s.handleAdminState)
  mux.HandleFunc("/admin/toggle-registration", s.handleToggleRegistration)
  mux.HandleFunc("/admin/toggle-setting", s.handleToggleSetting)
  mux.HandleFunc("/api/admin/approve", s.handleApproveApp)
  mux.HandleFunc("/me", s.handleUserPage)
  mux.HandleFunc("/api/me/state", s.handleUserState)
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

  if user, app, redirectHost, ok := s.matchAppHost(host); ok {
    if redirectHost != "" {
      http.Redirect(w, r, rewriteRequestURLHost(r, redirectHost), http.StatusTemporaryRedirect)
      return
    }
    s.proxyToApp(w, r, user, app)
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
    "service": "portflare",
    "base_domain": s.cfg.PublicBaseDomain,
    "admin_url_example": fmt.Sprintf("https://admin.%s", s.cfg.PublicBaseDomain),
    "user_url_example": fmt.Sprintf("https://<user-label>.%s", s.cfg.PublicBaseDomain),
    "app_url_example": fmt.Sprintf("https://<app>-<user-label>.%s", s.cfg.PublicBaseDomain),
    "local_path_example": "/r/<user>/<app>",
  })
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
    ID:         id,
    UserName:   userName,
    AppName:    appName,
    PublicPort: publicPort,
    Approved:   shouldAutoApprove,
    Connected:  true,
    LastSeenAt: now,
    CreatedAt:  now,
    UpdatedAt:  now,
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
  for _, u := range s.state.Users {
    cp := *u
    users = append(users, &cp)
  }
  for _, a := range s.state.Apps {
    cp := *a
    publicLabel := cp.UserName
    if user, ok := s.state.Users[cp.UserName]; ok && user.PublicUserLabel != "" {
      publicLabel = user.PublicUserLabel
    }
    apps = append(apps, map[string]any{
      "user_name": cp.UserName,
      "app_name": cp.AppName,
      "approved": cp.Approved,
      "connected": cp.Connected,
      "public_port": cp.PublicPort,
      "public_url": fmt.Sprintf("https://%s-%s.%s", cp.AppName, publicLabel, s.cfg.PublicBaseDomain),
    })
  }
  registrationOpen := s.state.RegistrationOpen
  allowUserAppApproval := s.state.AllowUserAppApproval
  autoApproveForUsers := s.state.AutoApproveForUsers
  autoApproveForAdmins := s.state.AutoApproveForAdmins
  s.stateMu.RUnlock()

  sort.Slice(users, func(i, j int) bool { return users[i].UserName < users[j].UserName })
  sort.Slice(apps, func(i, j int) bool { return fmt.Sprint(apps[i]["user_name"], "/", apps[i]["app_name"]) < fmt.Sprint(apps[j]["user_name"], "/", apps[j]["app_name"]) })
  return map[string]any{
    "identity": map[string]any{"user_name": identity.UserName},
    "registration_open": registrationOpen,
    "allow_user_app_approval": allowUserAppApproval,
    "auto_approve_for_users": autoApproveForUsers,
    "auto_approve_for_admins": autoApproveForAdmins,
    "users": users,
    "apps": apps,
    "base_domain": s.cfg.PublicBaseDomain,
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
    "Identity":         data["identity"].(map[string]any),
    "RegistrationOpen":      data["registration_open"],
    "AllowUserAppApproval":  data["allow_user_app_approval"],
    "AutoApproveForUsers":   data["auto_approve_for_users"],
    "AutoApproveForAdmins":  data["auto_approve_for_admins"],
    "Users":                 data["users"],
    "Apps":                  data["apps"],
    "BaseDomain":            data["base_domain"],
  })
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
  var value bool
  switch setting {
  case "allow_user_app_approval":
    s.state.AllowUserAppApproval = !s.state.AllowUserAppApproval
    value = s.state.AllowUserAppApproval
  case "auto_approve_for_users":
    s.state.AutoApproveForUsers = !s.state.AutoApproveForUsers
    value = s.state.AutoApproveForUsers
  case "auto_approve_for_admins":
    s.state.AutoApproveForAdmins = !s.state.AutoApproveForAdmins
    value = s.state.AutoApproveForAdmins
  default:
    s.stateMu.Unlock()
    writeError(w, http.StatusBadRequest, "unknown setting")
    return
  }
  err := s.saveStateLocked()
  s.stateMu.Unlock()
  if err != nil {
    writeError(w, http.StatusInternalServerError, err.Error())
    return
  }
  s.notifyUISubscribers()
  if wantsJSON(r) {
    writeJSON(w, http.StatusOK, map[string]any{"setting": setting, "value": value})
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
  apps := make([]map[string]any, 0)
  for _, a := range s.state.Apps {
    if a.UserName != user.UserName {
      continue
    }
    cp := *a
    canApprove := identity.IsAdmin || (allowUserAppApproval && !cp.Approved)
    apps = append(apps, map[string]any{
      "app_name": cp.AppName,
      "approved": cp.Approved,
      "connected": cp.Connected,
      "public_port": cp.PublicPort,
      "public_url": fmt.Sprintf("https://%s-%s.%s", cp.AppName, user.PublicUserLabel, s.cfg.PublicBaseDomain),
      "status": func() string { if cp.Approved { return "approved" }; return "pending admin approval" }(),
      "can_approve": canApprove,
      "user_name": user.UserName,
    })
  }
  s.stateMu.RUnlock()
  sort.Slice(apps, func(i, j int) bool { return fmt.Sprint(apps[i]["app_name"]) < fmt.Sprint(apps[j]["app_name"]) })

  return map[string]any{
    "identity": map[string]any{"user_name": identity.UserName, "is_admin": identity.IsAdmin},
    "user": user,
    "apps": apps,
    "allow_user_app_approval": allowUserAppApproval,
    "base_domain": s.cfg.PublicBaseDomain,
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

func (s *Server) proxyToApp(w http.ResponseWriter, r *http.Request, userName, appName string) {
  s.stateMu.RLock()
  app, ok := s.state.Apps[appKey(userName, appName)]
  s.stateMu.RUnlock()
  if !ok || !app.Approved {
    writeError(w, http.StatusNotFound, "app is not available")
    return
  }

  s.clientsMu.RLock()
  client := s.clients[userName]
  s.clientsMu.RUnlock()
  if client == nil {
    writeError(w, http.StatusBadGateway, "client is offline")
    return
  }
  if _, ok := client.apps[appName]; !ok {
    writeError(w, http.StatusBadGateway, "app is not connected")
    return
  }

  body, err := io.ReadAll(io.LimitReader(r.Body, s.cfg.MaxBodyBytes))
  if err != nil {
    writeError(w, http.StatusBadRequest, err.Error())
    return
  }

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
    writeError(w, http.StatusBadGateway, err.Error())
    return
  }

  ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
  defer cancel()

  select {
  case resp := <-pending.ch:
    if resp.Error != "" {
      writeError(w, http.StatusBadGateway, resp.Error)
      return
    }
    for k, values := range resp.Headers {
      if strings.EqualFold(k, "content-length") || strings.EqualFold(k, "connection") || strings.EqualFold(k, "transfer-encoding") {
        continue
      }
      for _, v := range values {
        w.Header().Add(k, v)
      }
    }
    status := resp.StatusCode
    if status == 0 {
      status = http.StatusOK
    }
    w.WriteHeader(status)
    payload, err := base64.StdEncoding.DecodeString(resp.BodyBase64)
    if err != nil {
      writeError(w, http.StatusBadGateway, "invalid upstream response")
      return
    }
    _, _ = w.Write(payload)
  case <-ctx.Done():
    s.pendingMu.Lock()
    delete(s.pending, requestID)
    s.pendingMu.Unlock()
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

func appHostLabel(app, publicUserLabel string) string { return slug(app) + "-" + userLabel(publicUserLabel) }

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
      <tr><th>User</th><th>App</th><th>Approved</th><th>Connected</th><th>Public</th><th>Port</th><th>Status</th></tr>
      <tbody id="apps-body">
      {{range .Apps}}
      <tr>
        <td>{{index . "user_name"}}</td>
        <td>{{index . "app_name"}}</td>
        <td>{{index . "approved"}}</td>
        <td>{{index . "connected"}}</td>
        <td><code>{{index . "public_url"}}</code></td>
        <td>{{with index . "public_port"}}{{.}}{{else}}-{{end}}</td>
        <td>{{if index . "approved"}}approved{{else}}pending{{end}}</td>
      </tr>
      {{else}}
      <tr><td colspan="7">No applications registered.</td></tr>
      {{end}}
      </tbody>
    </table>
    <p class="muted" id="live-status">Live updates: connecting…</p>
    <script>
      (() => {
        const status = document.getElementById('live-status');
        const usersBody = document.getElementById('users-body');
        const appsBody = document.getElementById('apps-body');
        const registrationOpen = document.getElementById('registration-open');
        const allowUserAppApproval = document.getElementById('allow-user-app-approval');
        const autoApproveForUsers = document.getElementById('auto-approve-for-users');
        const autoApproveForAdmins = document.getElementById('auto-approve-for-admins');
        const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
        const ws = new WebSocket(proto + '//' + window.location.host + '/ws/ui');
        const esc = (value) => String(value ?? '').replace(/[&<>"']/g, (ch) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
        const render = async () => {
          const res = await fetch('/api/admin/state', {headers: {'accept': 'application/json'}});
          if (!res.ok) return;
          const data = await res.json();
          registrationOpen.textContent = String(data.registration_open);
          allowUserAppApproval.textContent = String(data.allow_user_app_approval);
          autoApproveForUsers.textContent = String(data.auto_approve_for_users);
          autoApproveForAdmins.textContent = String(data.auto_approve_for_admins);
          usersBody.innerHTML = data.users.length ? data.users.map((u) => '<tr><td><a href="/me">' + esc(u.user_name) + '</a></td><td>' + esc(u.email) + '</td><td>' + esc(u.created_at) + '</td></tr>').join('') : '<tr><td colspan="3">No users yet.</td></tr>';
          appsBody.innerHTML = data.apps.length ? data.apps.map((a) => '<tr><td>' + esc(a.user_name) + '</td><td>' + esc(a.app_name) + '</td><td>' + esc(a.approved) + '</td><td>' + esc(a.connected) + '</td><td><code>' + esc(a.public_url) + '</code></td><td>' + (a.public_port || '-') + '</td><td>' + (a.approved ? 'approved' : 'pending') + '</td></tr>').join('') : '<tr><td colspan="7">No applications registered.</td></tr>';
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
      <tr><th>App</th><th>Approved</th><th>Connected</th><th>Subdomain</th><th>Port</th><th>Status</th><th>Action</th></tr>
      <tbody id="user-apps-body">
      {{range .Apps}}
      <tr>
        <td>{{index . "app_name"}}</td>
        <td>{{index . "approved"}}</td>
        <td>{{index . "connected"}}</td>
        <td><code>{{index . "public_url"}}</code></td>
        <td>{{with index . "public_port"}}{{.}}{{else}}-{{end}}</td>
        <td>{{index . "status"}}</td>
        <td>{{if index . "can_approve"}}<form method="post" action="/api/me/approve"><input type="hidden" name="user" value="{{index . "user_name"}}"><input type="hidden" name="app" value="{{index . "app_name"}}"><button type="submit">Approve</button></form>{{else}}-{{end}}</td>
      </tr>
      {{else}}
      <tr><td colspan="7">No applications registered yet.</td></tr>
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
          appsBody.innerHTML = data.apps.length ? data.apps.map((a) => '<tr><td>' + esc(a.app_name) + '</td><td>' + esc(a.approved) + '</td><td>' + esc(a.connected) + '</td><td><code>' + esc(a.public_url) + '</code></td><td>' + (a.public_port || '-') + '</td><td>' + esc(a.status) + '</td><td>' + (a.can_approve ? '<form method="post" action="/api/me/approve"><input type="hidden" name="user" value="' + esc(a.user_name) + '"><input type="hidden" name="app" value="' + esc(a.app_name) + '"><button type="submit">Approve</button></form>' : '-') + '</td></tr>').join('') : '<tr><td colspan="7">No applications registered yet.</td></tr>';
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
