package connection

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aelpxy/pulse/protocol"
	"github.com/charmbracelet/log"
	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"
)

var bufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

type Connection struct {
	ID              string
	AppKey          string
	ws              *websocket.Conn
	send            chan []byte
	manager         *Manager
	channels        map[string]bool
	channelsMux     sync.RWMutex
	lastActivity    time.Time
	activityTimeout time.Duration
	closing         chan struct{}
	closeMux        sync.Mutex
	isClosed        bool
	closeOnce       sync.Once
	rateLimiter     *rate.Limiter
}

func NewConnection(id string, ws *websocket.Conn, manager *Manager, activityTimeout time.Duration) *Connection {
	return &Connection{
		ID:              id,
		ws:              ws,
		send:            make(chan []byte, 512),
		manager:         manager,
		channels:        make(map[string]bool),
		lastActivity:    time.Now(),
		activityTimeout: activityTimeout,
		closing:         make(chan struct{}),
		isClosed:        false,
		rateLimiter:     rate.NewLimiter(10, 20), // default, updated per app config
	}
}

func (c *Connection) SetRateLimit(eventsPerSecond int, burst int) {
	c.rateLimiter = rate.NewLimiter(rate.Limit(eventsPerSecond), burst)
}

func (c *Connection) ReadPump() {
	defer func() {
		c.closeWebSocket()
		c.manager.Unregister(c)
	}()

	c.ws.SetReadDeadline(time.Now().Add(c.activityTimeout))

	c.ws.SetPongHandler(func(string) error {
		c.lastActivity = time.Now()
		c.ws.SetReadDeadline(time.Now().Add(c.activityTimeout))
		return nil
	})

	for {
		_, message, err := c.ws.ReadMessage()
		if err != nil {
			if log.GetLevel() == log.DebugLevel {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Debug("websocket error", "connection", c.ID, "error", err)
				}
			}
			break
		}

		c.lastActivity = time.Now()

		c.handleMessage(message)
	}
}

func (c *Connection) WritePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		c.closeWebSocket()
		c.manager.Unregister(c)
	}()

	const writeDeadline = 10 * time.Second
	deadline := time.Now().Add(writeDeadline)

	for {
		select {
		case message, ok := <-c.send:
			deadline = time.Now().Add(writeDeadline)
			c.ws.SetWriteDeadline(deadline)

			if !ok {
				c.drainMessages()
				c.ws.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.ws.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		batchLoop:
			for range 10 {
				select {
				case message, ok := <-c.send:
					if !ok {
						c.ws.WriteMessage(websocket.CloseMessage, []byte{})
						return
					}
					if err := c.ws.WriteMessage(websocket.TextMessage, message); err != nil {
						return
					}
				default:
					break batchLoop
				}
			}

		case <-ticker.C:
			c.ws.SetWriteDeadline(time.Now().Add(writeDeadline))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-c.closing:
			ticker.Stop()
			c.drainMessages()
			c.ws.WriteMessage(websocket.CloseMessage, []byte{})
			return
		}
	}
}

func (c *Connection) drainMessages() {
	deadline := time.Now().Add(5 * time.Second)
	c.ws.SetWriteDeadline(deadline)

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				return
			}

			c.ws.WriteMessage(websocket.TextMessage, message)
		default:
			return
		}

		if time.Now().After(deadline) {
			return
		}
	}
}

func (c *Connection) SendMessage(msg *protocol.Message) error {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	encoder := json.NewEncoder(buf)
	if err := encoder.Encode(msg); err != nil {
		return err
	}

	data := make([]byte, buf.Len())
	copy(data, buf.Bytes())

	if log.GetLevel() == log.DebugLevel {
		channelName := ""
		if msg.Channel != nil {
			channelName = *msg.Channel
		}
		log.Debug("sending message", "connection", c.ID, "event", msg.Event, "channel", channelName)
	}

	select {
	case c.send <- data:
		return nil
	default:
		return fmt.Errorf("send buffer full")
	}
}

func (c *Connection) Subscribe(channel string) {
	c.channelsMux.Lock()
	c.channels[channel] = true
	c.channelsMux.Unlock()

	if log.GetLevel() == log.DebugLevel {
		log.Debug("subscribed to channel", "connection", c.ID, "channel", channel)
	}
}

func (c *Connection) Unsubscribe(channel string) {
	c.channelsMux.Lock()
	delete(c.channels, channel)
	c.channelsMux.Unlock()

	if log.GetLevel() == log.DebugLevel {
		log.Debug("unsubscribed from channel", "connection", c.ID, "channel", channel)
	}
}

func (c *Connection) IsSubscribed(channel string) bool {
	c.channelsMux.RLock()
	defer c.channelsMux.RUnlock()
	return c.channels[channel]
}

func (c *Connection) GetChannels() []string {
	c.channelsMux.RLock()
	defer c.channelsMux.RUnlock()

	channels := make([]string, 0, len(c.channels))
	for channel := range c.channels {
		channels = append(channels, channel)
	}
	return channels
}

func (c *Connection) handleMessage(data []byte) {
	var msg protocol.Message
	if err := json.Unmarshal(data, &msg); err != nil {
		if log.GetLevel() == log.DebugLevel {
			log.Debug("failed to parse message", "connection", c.ID, "error", err)
		}
		c.sendError("Invalid JSON", nil)
		return
	}

	switch msg.Event {
	case protocol.EventPing:
		c.handlePing()
	case protocol.EventSubscribe:
		c.handleSubscribe(&msg)
	case protocol.EventUnsubscribe:
		c.handleUnsubscribe(&msg)
	default:
		c.handleClientEvent(&msg)
	}
}

func (c *Connection) handlePing() {
	pong, err := protocol.NewPong()
	if err != nil {
		return
	}
	c.SendMessage(pong)
}

func (c *Connection) handleSubscribe(msg *protocol.Message) {
	if msg.Data == "" {
		msg.Data = "{}"
	}

	subData, err := protocol.ParseSubscribeData(msg.Data)
	if err != nil {
		c.sendError("Invalid subscription data", nil)
		return
	}

	c.manager.SubscribeConnection(c, subData)
}

func (c *Connection) handleUnsubscribe(msg *protocol.Message) {
	subData, err := protocol.ParseSubscribeData(msg.Data)
	if err != nil {
		c.sendError("Invalid subscription data", nil)
		return
	}

	c.manager.UnsubscribeConnection(c, subData.Channel)
}

func (c *Connection) handleClientEvent(msg *protocol.Message) {
	if len(msg.Event) < 7 || msg.Event[:7] != "client-" {
		return
	}

	// rate limit client events (error 4301 per Pusher protocol)
	if !c.rateLimiter.Allow() {
		code := protocol.ErrorClientEventRateLimitReached
		c.sendError("Rate limit exceeded for client events", &code)
		return
	}

	if c.manager.appsManager != nil {
		app, exists := c.manager.appsManager.GetApp(c.AppKey)
		if exists && !app.IsClientEventsEnabled() {
			code := protocol.ErrorConnectionIsUnauthorized
			c.sendError("Client events are not enabled for this app", &code)
			return
		}
	}

	if msg.Channel == nil {
		code := protocol.ErrorConnectionIsUnauthorized
		c.sendError("Client events require a channel", &code)
		return
	}

	channelName := *msg.Channel

	// channel must be private or presence (encrypted channels do NOT support client events per spec)
	if !protocol.IsPrivateChannel(channelName) && !protocol.IsPresenceChannel(channelName) {
		code := protocol.ErrorConnectionIsUnauthorized
		c.sendError("Client events are only supported on private and presence channels", &code)
		return
	}

	// encrypted channels explicitly do not support client events
	if protocol.IsEncryptedChannel(channelName) {
		code := protocol.ErrorConnectionIsUnauthorized
		c.sendError("Client events are not supported on encrypted channels", &code)
		return
	}

	if !c.IsSubscribed(channelName) {
		return
	}

	c.manager.BroadcastToChannel(channelName, msg, c.ID)
}

func (c *Connection) sendError(message string, code *int) {
	errMsg, err := protocol.NewError(message, code)
	if err != nil {
		return
	}
	c.SendMessage(errMsg)
}

func (c *Connection) closeWebSocket() {
	c.closeOnce.Do(func() {
		c.ws.Close()
	})
}

func (c *Connection) Close() {
	c.closeMux.Lock()
	defer c.closeMux.Unlock()

	if c.isClosed {
		return
	}
	c.isClosed = true

	close(c.closing)
}
