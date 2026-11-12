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

	// WebSocket passthrough. The server sends ws_open when a visitor asks to
	// upgrade; the client dials the local service and answers ws_open_ack
	// (StatusCode 101 on success). Frames then flow both ways as ws_msg, and
	// either side ends the stream with ws_close. RequestID identifies the
	// stream in all of them.
	MsgTypeWSOpen    = "ws_open"
	MsgTypeWSOpenAck = "ws_open_ack"
	MsgTypeWSMsg     = "ws_msg"
	MsgTypeWSClose   = "ws_close"
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

	// WSType is the WebSocket frame type of a ws_msg (1 text, 2 binary).
	WSType int `json:"ws_type,omitempty"`
}

// WebSocketHopHeaders are handshake headers each side of a proxied WebSocket
// generates itself, so they must not be copied from the other leg.
var WebSocketHopHeaders = map[string]bool{
	"Upgrade":                  true,
	"Connection":               true,
	"Sec-Websocket-Key":        true,
	"Sec-Websocket-Version":    true,
	"Sec-Websocket-Extensions": true,
	"Sec-Websocket-Accept":     true,
	"Host":                     true,
	"Content-Length":           true,
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
