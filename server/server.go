package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aelpxy/pulse/apps"
	"github.com/aelpxy/pulse/auth"
	"github.com/aelpxy/pulse/channel"
	"github.com/aelpxy/pulse/connection"
	"github.com/charmbracelet/log"
	"github.com/gorilla/websocket"
)

type Server struct {
	appsManager    *apps.Manager
	channelManager *channel.Manager
	connectionMgr  *connection.Manager
	authServices   map[string]*auth.Service
	authMux        sync.RWMutex
	upgrader       websocket.Upgrader
	appPathRegex   *regexp.Regexp
}

type Config struct {
	AppsConfigFile string
	MaxConnections int
}

func New(config Config) (*Server, *apps.ServerConfig, error) {
	if config.MaxConnections == 0 {
		config.MaxConnections = 100000
	}

	appsMgr := apps.NewManager()
	serverConfig, err := appsMgr.LoadFromFile(config.AppsConfigFile)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load apps config: %w", err)
	}

	log.Info("loaded apps", "count", appsMgr.GetAppCount(), "file", config.AppsConfigFile)

	channelMgr := channel.NewManager()

	connMgr := connection.NewManager(channelMgr, nil, config.MaxConnections)

	authServices := make(map[string]*auth.Service)
	for _, app := range appsMgr.GetAllApps() {
		authServices[app.Key] = auth.NewService(app.Key, app.Secret)
		log.Info("app registered", "name", app.Name, "key", app.Key, "max_connections", app.MaxConnections)
	}

	connMgr.SetAuthServices(authServices)
	connMgr.SetAppsManager(appsMgr)

	upgrader := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	return &Server{
		appsManager:    appsMgr,
		channelManager: channelMgr,
		connectionMgr:  connMgr,
		authServices:   authServices,
		upgrader:       upgrader,
		appPathRegex:   regexp.MustCompile(`^/app/([^/]+)$`),
	}, serverConfig, nil
}

func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic in websocket handler", "panic", r)
		}
	}()

	matches := s.appPathRegex.FindStringSubmatch(r.URL.Path)
	if len(matches) != 2 {
		http.Error(w, "Invalid WebSocket path", http.StatusBadRequest)
		return
	}

	appKey := matches[1]

	app, exists := s.appsManager.GetApp(appKey)
	if !exists {
		log.Warn("unknown app key", "key", appKey)
		http.Error(w, "Unknown app", http.StatusNotFound)
		return
	}

	if !app.Enabled {
		log.Warn("disabled app", "key", appKey)
		http.Error(w, "App disabled", http.StatusForbidden)
		return
	}

	origin := r.Header.Get("Origin")
	allowedOrigins := app.GetAllowedOrigins()
	originAllowed := false
	for _, allowed := range allowedOrigins {
		if origin == allowed || allowed == "*" {
			originAllowed = true
			break
		}
	}
	if !originAllowed && origin != "" {
		log.Warn("origin not allowed", "app", appKey, "origin", origin)
		http.Error(w, "Origin not allowed", http.StatusForbidden)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error("failed to upgrade connection", "error", err)
		return
	}

	ws.SetReadLimit(app.GetMaxMessageSize())

	conn, err := s.connectionMgr.RegisterWithApp(ws, appKey)
	if err != nil {
		if err == connection.ErrMaxConnectionsReached {
			log.Warn("max connections reached", "app", appKey)
			ws.WriteMessage(1, []byte(`{"event":"pusher:error","data":{"message":"Over capacity","code":4100}}`))
		} else {
			log.Error("failed to register connection", "error", err)
		}
		ws.Close()
		return
	}

	log.Debug("new connection", "id", conn.ID, "app", appKey)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in write pump", "connection", conn.ID, "panic", r)
				s.connectionMgr.Unregister(conn)
			}
		}()
		conn.WritePump()
	}()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in read pump", "connection", conn.ID, "panic", r)
				s.connectionMgr.Unregister(conn)
			}
		}()
		conn.ReadPump()
	}()
}

func (s *Server) HandleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	appID := parts[1]

	targetApp, exists := s.appsManager.GetAppByID(appID)
	if !exists {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	// limit request body size (default 10KB, or app's max message size)
	maxBodySize := max(targetApp.GetMaxMessageSize(), 10240)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	s.authMux.RLock()
	authSvc, exists := s.authServices[targetApp.Key]
	s.authMux.RUnlock()

	if !exists {
		http.Error(w, "Authentication service not found", http.StatusInternalServerError)
		return
	}

	if err := authSvc.ValidateHTTPRequest(r.Method, r.URL.Path, r.URL.Query(), body); err != nil {
		log.Warn("authentication failed", "error", err, "app", targetApp.Key, "path", r.URL.Path)
		http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusUnauthorized)
		return
	}

	type TriggerRequest struct {
		Name     string   `json:"name"`
		Channel  string   `json:"channel"`
		Channels []string `json:"channels"`
		Data     string   `json:"data"`
	}

	var trigger TriggerRequest
	if err := json.Unmarshal(body, &trigger); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	channels := trigger.Channels
	if trigger.Channel != "" {
		channels = append(channels, trigger.Channel)
	}

	for _, ch := range channels {
		var data any
		if err := json.Unmarshal([]byte(trigger.Data), &data); err != nil {
			log.Warn("failed to parse event data", "error", err, "channel", ch)
			continue
		}
		s.connectionMgr.PublishToChannel(ch, trigger.Name, data)
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{}`)
}

func (s *Server) HandleBatchEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	appID := parts[1]

	targetApp, exists := s.appsManager.GetAppByID(appID)
	if !exists {
		http.Error(w, "App not found", http.StatusNotFound)
		return
	}

	// limit request body size for batch (10x single message size)
	// min 100KB for batch
	maxBodySize := max(targetApp.GetMaxMessageSize()*10,
		102400)
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	s.authMux.RLock()
	authSvc, exists := s.authServices[targetApp.Key]
	s.authMux.RUnlock()

	if !exists {
		http.Error(w, "Authentication service not found", http.StatusInternalServerError)
		return
	}

	if err := authSvc.ValidateHTTPRequest(r.Method, r.URL.Path, r.URL.Query(), body); err != nil {
		log.Warn("authentication failed", "error", err, "app", targetApp.Key, "path", r.URL.Path)
		http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusUnauthorized)
		return
	}

	type BatchEvent struct {
		Name    string `json:"name"`
		Channel string `json:"channel"`
		Data    string `json:"data"`
		Info    string `json:"info,omitempty"`
	}

	type BatchRequest struct {
		Batch []BatchEvent `json:"batch"`
	}

	var batchReq BatchRequest
	if err := json.Unmarshal(body, &batchReq); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	maxBatch := targetApp.GetMaxBatchEvents()
	if len(batchReq.Batch) > maxBatch {
		http.Error(w, fmt.Sprintf("Batch size exceeds maximum of %d events", maxBatch), http.StatusRequestEntityTooLarge)
		return
	}

	type EventResponse struct {
		SubscriptionCount *int `json:"subscription_count,omitempty"`
		UserCount         *int `json:"user_count,omitempty"`
	}

	responses := make([]EventResponse, len(batchReq.Batch))

	for i, event := range batchReq.Batch {
		var data any
		if err := json.Unmarshal([]byte(event.Data), &data); err != nil {
			log.Warn("failed to parse event data", "error", err, "channel", event.Channel, "event", event.Name)
		}

		s.connectionMgr.PublishToChannel(event.Channel, event.Name, data)

		var resp EventResponse
		if event.Info != "" {
			infoParts := strings.Split(event.Info, ",")
			for _, info := range infoParts {
				info = strings.TrimSpace(info)
				if info == "subscription_count" {
					count := len(s.channelManager.GetSubscribers(event.Channel))
					resp.SubscriptionCount = &count
				} else if info == "user_count" && strings.HasPrefix(event.Channel, "presence-") {
					count := len(s.channelManager.GetSubscribers(event.Channel))
					resp.UserCount = &count
				}
			}
		}
		responses[i] = resp
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"batch": responses,
	})
}

func (s *Server) HandleStats(w http.ResponseWriter, r *http.Request) {
	stats := s.connectionMgr.GetStats()
	stats["apps_loaded"] = s.appsManager.GetAppCount()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

func (s *Server) HandleApps(w http.ResponseWriter, r *http.Request) {
	allApps := s.appsManager.GetAllApps()

	type AppInfo struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}

	appsInfo := make([]AppInfo, 0, len(allApps))
	for _, app := range allApps {
		appsInfo = append(appsInfo, AppInfo{
			ID:  app.ID,
			Key: app.Key,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"apps":  appsInfo,
		"count": len(appsInfo),
	})
}

func (s *Server) Shutdown(timeout time.Duration) error {
	return s.connectionMgr.Shutdown(timeout)
}

func (s *Server) GetAppsManager() *apps.Manager {
	return s.appsManager
}

func (s *Server) ReloadApps(configFile string) error {
	newAppsMgr := apps.NewManager()
	_, err := newAppsMgr.LoadFromFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to reload apps: %w", err)
	}

	// Update auth services
	s.authMux.Lock()
	s.authServices = make(map[string]*auth.Service)
	for _, app := range newAppsMgr.GetAllApps() {
		s.authServices[app.Key] = auth.NewService(app.Key, app.Secret)
	}
	authServicesCopy := make(map[string]*auth.Service)
	for k, v := range s.authServices {
		authServicesCopy[k] = v
	}
	s.authMux.Unlock()

	// Update connection manager's auth services
	s.connectionMgr.SetAuthServices(authServicesCopy)

	// Update apps manager (thread-safe as it's only read after startup)
	s.appsManager = newAppsMgr

	log.Info("reloaded apps", "count", newAppsMgr.GetAppCount())
	return nil
}
