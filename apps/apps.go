package apps

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

type App struct {
	ID                 string   `json:"id"`
	Key                string   `json:"key"`
	Secret             string   `json:"secret"`
	Name               string   `json:"name"`
	Enabled            bool     `json:"enabled"`
	MaxConnections     int      `json:"max_connections"`
	MaxChannelsPerConn int      `json:"max_channels_per_connection"`
	AllowedOrigins     []string `json:"allowed_origins"`
	MaxMessageSize     int64    `json:"max_message_size"`
	MaxBatchEvents     int      `json:"max_batch_events"`
	EnableClientEvents *bool    `json:"enable_client_events"`
	MaxEventRate       int      `json:"max_event_rate"`
	MaxEventBurst      int      `json:"max_event_burst"`
}

type ServerConfig struct {
	Debug    bool   `json:"debug"`
	Port     string `json:"port"`
	Hostname string `json:"hostname"`
	Region   string `json:"region"`
}

type Config struct {
	Server ServerConfig `json:"server"`
	Apps   []App        `json:"apps"`
}

type Manager struct {
	apps         map[string]*App // key -> app
	serverConfig ServerConfig
	mu           sync.RWMutex
}

func NewManager() *Manager {
	return &Manager{
		apps: make(map[string]*App),
	}
}

func (m *Manager) LoadFromFile(filename string) (*ServerConfig, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read apps config: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse apps config: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.serverConfig = config.Server

	m.apps = make(map[string]*App)

	for i := range config.Apps {
		app := &config.Apps[i]
		if app.Enabled {
			m.apps[app.Key] = app
		}
	}

	return &config.Server, nil
}

func (m *Manager) GetServerConfig() ServerConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.serverConfig
}

func (m *Manager) GetApp(key string) (*App, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	app, exists := m.apps[key]
	return app, exists
}

func (m *Manager) GetAllApps() []*App {
	m.mu.RLock()
	defer m.mu.RUnlock()

	apps := make([]*App, 0, len(m.apps))
	for _, app := range m.apps {
		apps = append(apps, app)
	}
	return apps
}

func (m *Manager) ValidateApp(key, secret string) (*App, error) {
	app, exists := m.GetApp(key)
	if !exists {
		return nil, fmt.Errorf("app not found: %s", key)
	}

	if !app.Enabled {
		return nil, fmt.Errorf("app disabled: %s", key)
	}

	if app.Secret != secret {
		return nil, fmt.Errorf("invalid app secret")
	}

	return app, nil
}

func (m *Manager) AddApp(app *App) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apps[app.Key] = app
}

func (m *Manager) RemoveApp(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.apps, key)
}

func (m *Manager) GetAppCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.apps)
}

// 8KB default
func (a *App) GetMaxMessageSize() int64 {
	if a.MaxMessageSize <= 0 {
		return 8192
	}

	return a.MaxMessageSize
}

func (a *App) GetAllowedOrigins() []string {
	if len(a.AllowedOrigins) == 0 {
		return []string{"*"}
	}
	return a.AllowedOrigins
}

func (a *App) GetMaxBatchEvents() int {
	if a.MaxBatchEvents <= 0 {
		return 100
	}
	return a.MaxBatchEvents
}

func (a *App) IsClientEventsEnabled() bool {
	if a.EnableClientEvents == nil {
		return false
	}
	return *a.EnableClientEvents
}

func (a *App) GetMaxEventRate() int {
	if a.MaxEventRate <= 0 {
		return 10
	}
	return a.MaxEventRate
}

func (a *App) GetMaxEventBurst() int {
	if a.MaxEventBurst <= 0 {
		return 20
	}
	return a.MaxEventBurst
}
