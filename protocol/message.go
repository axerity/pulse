package protocol

import "encoding/json"

// this represents a Pusher protocol message
type Message struct {
	Event   string          `json:"event"`
	Channel *string         `json:"channel,omitempty"`
	Data    string          `json:"-"`
	RawData json.RawMessage `json:"data,omitempty"`
}

// check https://github.com/pusher/pusher-socket-protocol/blob/master/protocol.adoc#events
func (m *Message) MarshalJSON() ([]byte, error) {
	type TempMessage struct {
		Event   string  `json:"event"`
		Channel *string `json:"channel,omitempty"`
		Data    string  `json:"data,omitempty"`
	}

	temp := TempMessage{
		Event:   m.Event,
		Channel: m.Channel,
	}

	if m.Data != "" {
		temp.Data = m.Data
	} else if len(m.RawData) > 0 {
		temp.Data = string(m.RawData)
	}

	return json.Marshal(temp)
}

func (m *Message) UnmarshalJSON(data []byte) error {
	// define a temp type to avoid recursion
	type TempMessage struct {
		Event   string          `json:"event"`
		Channel *string         `json:"channel,omitempty"`
		RawData json.RawMessage `json:"data,omitempty"`
	}

	var temp TempMessage
	if err := json.Unmarshal(data, &temp); err != nil {
		return err
	}

	m.Event = temp.Event
	m.Channel = temp.Channel
	m.RawData = temp.RawData

	if len(m.RawData) > 0 {
		var str string
		if err := json.Unmarshal(m.RawData, &str); err == nil {
			m.Data = str
		} else {
			m.Data = string(m.RawData)
		}
	}

	return nil
}

type ConnectionEstablishedData struct {
	SocketID        string `json:"socket_id"`
	ActivityTimeout int    `json:"activity_timeout"`
}

type ErrorData struct {
	Message string `json:"message"`
	Code    *int   `json:"code,omitempty"`
}

type SubscribeData struct {
	Channel     string  `json:"channel"`
	Auth        *string `json:"auth,omitempty"`
	ChannelData *string `json:"channel_data,omitempty"`
}

type SubscriptionSucceededData struct {
	Channel string `json:"channel,omitempty"`
}

func NewMessage(event string, channel *string, data interface{}) (*Message, error) {
	var dataStr string

	if data != nil {
		dataBytes, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		dataStr = string(dataBytes)
	}

	return &Message{
		Event:   event,
		Channel: channel,
		Data:    dataStr,
	}, nil
}

func NewConnectionEstablished(socketID string, activityTimeout int) (*Message, error) {
	data := ConnectionEstablishedData{
		SocketID:        socketID,
		ActivityTimeout: activityTimeout,
	}
	return NewMessage("pusher:connection_established", nil, data)
}

func NewError(message string, code *int) (*Message, error) {
	data := ErrorData{
		Message: message,
		Code:    code,
	}
	return NewMessage("pusher:error", nil, data)
}

func NewPong() (*Message, error) {
	return NewMessage("pusher:pong", nil, struct{}{})
}

func NewSubscriptionSucceeded(channel string) (*Message, error) {
	return NewMessage("pusher_internal:subscription_succeeded", &channel, struct{}{})
}

func NewSubscriptionSucceededWithPresence(channel string, presenceData any) (*Message, error) {
	data := map[string]any{
		"presence": presenceData,
	}

	return NewMessage("pusher_internal:subscription_succeeded", &channel, data)
}

func NewMemberAdded(channel string, memberData any) (*Message, error) {
	return NewMessage("pusher_internal:member_added", &channel, memberData)
}

func NewMemberRemoved(channel string, memberData any) (*Message, error) {
	return NewMessage("pusher_internal:member_removed", &channel, memberData)
}

func ParseSubscribeData(dataStr string) (*SubscribeData, error) {
	var data SubscribeData
	if err := json.Unmarshal([]byte(dataStr), &data); err != nil {
		return nil, err
	}
	return &data, nil
}
