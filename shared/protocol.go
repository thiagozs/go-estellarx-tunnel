package shared

import (
	"encoding/json"
	"fmt"
)

// Message types exchanged over the WebSocket control channel.
const (
	MsgTypeRegister    = "register"
	MsgTypeRegisterAck = "register_ack"
	MsgTypeData        = "data"
	MsgTypeDataAck     = "data_ack"
	MsgTypeError       = "error"
	MsgTypePing        = "ping"
	MsgTypePong        = "pong"
)

// Message is the envelope exchanged between server and client.
type Message struct {
	Type      string `json:"type"`
	TunnelID  string `json:"tunnel_id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Token     string `json:"token,omitempty"`
	Subdomain string `json:"subdomain,omitempty"`
	Data      []byte `json:"data,omitempty"`
	Error     string `json:"error,omitempty"`

	// HTTP metadata
	HTTPMethod string            `json:"http_method,omitempty"`
	HTTPPath   string            `json:"http_path,omitempty"`
	HTTPHeader map[string]string `json:"http_header,omitempty"`
	StatusCode int               `json:"status_code,omitempty"`
}

// Encode serialises a Message to JSON.
func (m *Message) Encode() ([]byte, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encode message: %w", err)
	}
	return b, nil
}

// DecodeMessage deserialises JSON bytes into a Message.
func DecodeMessage(b []byte) (*Message, error) {
	var m Message
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode message: %w", err)
	}
	return &m, nil
}
