package connection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aelpxy/pulse/apps"
	"github.com/aelpxy/pulse/auth"
	"github.com/aelpxy/pulse/channel"
	"github.com/aelpxy/pulse/presence"
	"github.com/aelpxy/pulse/protocol"
	"github.com/charmbracelet/log"
	"github.com/gorilla/websocket"
)

var (
	ErrMaxConnectionsReached = errors.New("maximum connections reached")
	ErrServerShutdown        = errors.New("server is shutting down")
)

type Manager struct {
	connections     map[string]*Connection
	connectionsMux  sync.RWMutex
	channelManager  *channel.Manager
	presenceManager *presence.Manager
	appsManager     *apps.Manager
	authService     *auth.Service
	authServices    map[string]*auth.Service
	authServicesMux sync.RWMutex
	activityTimeout time.Duration

	maxConnections     int
	currentConnections int64
	connectionSem      chan struct{}

	ctx      context.Context
	cancel   context.CancelFunc
	shutdown int32
	wg       sync.WaitGroup
}

// NewManager creates a new connection manager with connection limits
func NewManager(channelManager *channel.Manager, authService *auth.Service, maxConnections int) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	m := &Manager{
		connections:     make(map[string]*Connection),
		channelManager:  channelManager,
		presenceManager: presence.NewManager(),
		authService:     authService,
		authServices:    make(map[string]*auth.Service),
		activityTimeout: 120 * time.Second,
		maxConnections:  maxConnections,
		connectionSem:   make(chan struct{}, maxConnections),
		ctx:             ctx,
		cancel:          cancel,
	}

	// start background goroutine to close inactive connections
	go m.cleanupInactiveConnections()

	return m
}

// cleanupInactiveConnections periodically checks and closes connections
// that have been inactive for too long (24 hours per Pusher protocol spec)
func (m *Manager) cleanupInactiveConnections() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	const maxInactivity = 24 * time.Hour

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.connectionsMux.RLock()
			var toClose []*Connection
			now := time.Now()

			for _, conn := range m.connections {
				if now.Sub(conn.lastActivity) > maxInactivity {
					toClose = append(toClose, conn)
				}
			}
			m.connectionsMux.RUnlock()

			for _, conn := range toClose {
				log.Debug("closing inactive connection", "id", conn.ID, "last_activity", conn.lastActivity)
				// send error before closing (code 4202: closed after inactivity)
				code := protocol.ErrorClosedAfterInactivityTimeout
				errMsg, err := protocol.NewError("Connection closed due to inactivity", &code)
				if err == nil {
					conn.SendMessage(errMsg)
				}
				conn.Close()
			}
		}
	}
}

func (m *Manager) SetAuthServices(services map[string]*auth.Service) {
	m.authServicesMux.Lock()
	defer m.authServicesMux.Unlock()
	m.authServices = services
}

func (m *Manager) SetAppsManager(appsManager *apps.Manager) {
	m.appsManager = appsManager
}

func (m *Manager) getAuthService(appKey string) *auth.Service {
	if appKey != "" {
		m.authServicesMux.RLock()
		svc, exists := m.authServices[appKey]
		m.authServicesMux.RUnlock()
		if exists {
			return svc
		}
	}

	return m.authService
}

func (m *Manager) RegisterWithApp(ws *websocket.Conn, appKey string) (*Connection, error) {
	if atomic.LoadInt32(&m.shutdown) == 1 {
		return nil, ErrServerShutdown
	}

	select {
	case m.connectionSem <- struct{}{}:
	default:
		return nil, ErrMaxConnectionsReached
	}

	socketID, err := generateSocketID()
	if err != nil {
		<-m.connectionSem
		return nil, err
	}

	conn := NewConnection(socketID, ws, m, m.activityTimeout)
	conn.AppKey = appKey

	// set rate limits from app config
	if m.appsManager != nil {
		if app, exists := m.appsManager.GetApp(appKey); exists {
			conn.SetRateLimit(app.GetMaxEventRate(), app.GetMaxEventBurst())
		}
	}

	m.connectionsMux.Lock()
	m.connections[socketID] = conn
	atomic.AddInt64(&m.currentConnections, 1)
	currentConns := atomic.LoadInt64(&m.currentConnections)
	m.connectionsMux.Unlock()

	log.Debug("connection registered", "id", socketID, "app", appKey, "active_connections", currentConns)

	m.wg.Add(1)

	connEstablished, err := protocol.NewConnectionEstablished(socketID, int(m.activityTimeout.Seconds()))
	if err != nil {
		m.Unregister(conn)
		return nil, err
	}

	if err := conn.SendMessage(connEstablished); err != nil {
		m.Unregister(conn)
		return nil, err
	}

	return conn, nil
}

func (m *Manager) Register(ws *websocket.Conn) (*Connection, error) {
	return m.RegisterWithApp(ws, "")
}

func (m *Manager) Unregister(conn *Connection) {
	m.connectionsMux.Lock()
	_, exists := m.connections[conn.ID]
	if exists {
		delete(m.connections, conn.ID)
		atomic.AddInt64(&m.currentConnections, -1)
		log.Debug("connection unregistered", "id", conn.ID, "active_connections", atomic.LoadInt64(&m.currentConnections))
	}
	m.connectionsMux.Unlock()

	if !exists {
		return
	}

	for _, channelName := range conn.GetChannels() {
		if protocol.IsPresenceChannel(channelName) {
			member := m.presenceManager.RemoveMember(channelName, conn.ID)
			if member != nil {
				memberRemovedMsg, err := protocol.NewMemberRemoved(channelName, map[string]any{
					"user_id": member.UserID,
				})
				if err == nil {
					m.BroadcastToChannel(channelName, memberRemovedMsg, "")
				}
			}
		}
		m.channelManager.Unsubscribe(channelName, conn.ID)
	}

	conn.Close()

	select {
	case <-m.connectionSem:
	default:
	}

	m.wg.Done()
}

func (m *Manager) GetConnection(id string) (*Connection, bool) {
	m.connectionsMux.RLock()
	defer m.connectionsMux.RUnlock()
	conn, exists := m.connections[id]
	return conn, exists
}

func (m *Manager) SubscribeConnection(conn *Connection, subData *protocol.SubscribeData) {
	channelName := subData.Channel

	if protocol.IsPrivateChannel(channelName) || protocol.IsPresenceChannel(channelName) || protocol.IsEncryptedChannel(channelName) {
		if subData.Auth == nil {
			code := protocol.ErrorConnectionIsUnauthorized
			conn.sendError("Authentication required for private/presence channels", &code)
			return
		}

		authSvc := m.getAuthService(conn.AppKey)
		if authSvc == nil {
			code := protocol.ErrorConnectionIsUnauthorized
			conn.sendError("No auth service configured", &code)
			return
		}
		if !authSvc.ValidateAuth(*subData.Auth, conn.ID, channelName, subData.ChannelData) {
			code := protocol.CloseInvalidSignature
			conn.sendError("Invalid authentication signature", &code)
			return
		}
	}

	if err := m.channelManager.Subscribe(channelName, conn.ID); err != nil {
		conn.sendError(fmt.Sprintf("Failed to subscribe: %v", err), nil)
		return
	}

	conn.Subscribe(channelName)

	if protocol.IsPresenceChannel(channelName) {
		var member *presence.Member
		if subData.ChannelData != nil && *subData.ChannelData != "" {
			var err error
			member, err = presence.ParseChannelData(*subData.ChannelData)
			if err != nil {
				code := protocol.ErrorConnectionIsUnauthorized
				conn.sendError("Invalid channel_data for presence channel", &code)
				return
			}
		} else {
			member = &presence.Member{
				UserID:   conn.ID,
				UserInfo: nil,
			}
		}

		m.presenceManager.AddMember(channelName, conn.ID, member)

		memberAddedMsg, err := protocol.NewMemberAdded(channelName, member)
		if err == nil {
			m.BroadcastToChannel(channelName, memberAddedMsg, conn.ID)
		}

		presenceData := m.presenceManager.GetPresenceData(channelName)
		successMsg, err := protocol.NewSubscriptionSucceededWithPresence(channelName, presenceData)
		if err != nil {
			return
		}
		conn.SendMessage(successMsg)
	} else {
		successMsg, err := protocol.NewSubscriptionSucceeded(channelName)
		if err != nil {
			return
		}
		conn.SendMessage(successMsg)
	}
}

func (m *Manager) UnsubscribeConnection(conn *Connection, channelName string) {
	if protocol.IsPresenceChannel(channelName) {
		member := m.presenceManager.RemoveMember(channelName, conn.ID)
		if member != nil {
			memberRemovedMsg, err := protocol.NewMemberRemoved(channelName, map[string]interface{}{
				"user_id": member.UserID,
			})
			if err == nil {
				m.BroadcastToChannel(channelName, memberRemovedMsg, "")
			}
		}
	}

	m.channelManager.Unsubscribe(channelName, conn.ID)
	conn.Unsubscribe(channelName)
}

func (m *Manager) BroadcastToChannel(channelName string, msg *protocol.Message, excludeConnID string) {
	connIDs := m.channelManager.GetSubscribers(channelName)

	m.connectionsMux.RLock()
	defer m.connectionsMux.RUnlock()

	for _, connID := range connIDs {
		if connID == excludeConnID {
			continue
		}

		if conn, exists := m.connections[connID]; exists {
			conn.SendMessage(msg)
		}
	}
}

func (m *Manager) PublishToChannel(channelName string, event string, data any) error {
	msg, err := protocol.NewMessage(event, &channelName, data)
	if err != nil {
		return err
	}

	m.BroadcastToChannel(channelName, msg, "")
	return nil
}

func (m *Manager) Shutdown(timeout time.Duration) error {
	atomic.StoreInt32(&m.shutdown, 1)

	m.cancel()

	m.connectionsMux.RLock()
	conns := make([]*Connection, 0, len(m.connections))
	for _, conn := range m.connections {
		conns = append(conns, conn)
	}
	m.connectionsMux.RUnlock()

	for _, conn := range conns {
		conn.Close()
	}

	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("shutdown timed out after %v", timeout)
	}
}

func (m *Manager) GetStats() map[string]any {
	return map[string]any{
		"current_connections": atomic.LoadInt64(&m.currentConnections),
		"max_connections":     m.maxConnections,
		"total_channels":      m.channelManager.GetChannelCount(),
	}
}

func (m *Manager) GetConnectionCount() int64 {
	return atomic.LoadInt64(&m.currentConnections)
}

func (m *Manager) IsShuttingDown() bool {
	return atomic.LoadInt32(&m.shutdown) == 1
}

func generateSocketID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	socketID := hex.EncodeToString(b[:4]) + "." + hex.EncodeToString(b[4:])
	return socketID, nil
}
