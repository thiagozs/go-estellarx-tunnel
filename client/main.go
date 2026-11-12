package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/thiagozs/go-estellarx-tunnel/shared"
)

func main() {
	var (
		serverURL string
		token     string
		subdomain string
	)

	rootCmd := &cobra.Command{
		Use:   "tunnel",
		Short: "EstellarX Tunnel — expose local services to the internet",
	}

	exposeCmd := &cobra.Command{
		Use:   "expose [port]",
		Short: "Expose a local HTTP port through the tunnel",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			localAddr := fmt.Sprintf("http://localhost:%s", args[0])
			run(serverURL, token, subdomain, localAddr)
		},
	}

	exposeCmd.Flags().StringVarP(&serverURL, "server", "s", "ws://localhost:8080/_tunnel/ws", "Tunnel server WebSocket URL")
	exposeCmd.Flags().StringVarP(&token, "token", "t", "", "Auth token")
	exposeCmd.Flags().StringVarP(&subdomain, "subdomain", "d", "", "Requested subdomain (auto-generated if empty)")

	rootCmd.AddCommand(exposeCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(serverURL, token, subdomain, localAddr string) {
	var attempt int
	for {
		err := connect(serverURL, token, subdomain, localAddr)
		if err != nil {
			attempt++
			delay := backoff(attempt)
			log.Printf("disconnected: %v — reconnecting in %v (attempt %d)", err, delay, attempt)
			time.Sleep(delay)
		}
	}
}

// wsConn serializes writes: gorilla/websocket allows only one concurrent
// writer, and responses are sent from one goroutine per request.
type wsConn struct {
	*websocket.Conn
	mu sync.Mutex
}

func (c *wsConn) WriteMessage(messageType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.WriteMessage(messageType, data)
}

func connect(serverURL, token, subdomain, localAddr string) error {
	log.Printf("connecting to %s ...", serverURL)
	raw, _, err := websocket.DefaultDialer.Dial(serverURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	conn := &wsConn{Conn: raw}
	defer conn.Close()
	sess := &session{conn: conn, localAddr: localAddr, streams: make(map[string]*localStream)}
	defer sess.closeAll()

	// Send register message.
	reg := &shared.Message{
		Type:      shared.MsgTypeRegister,
		Token:     token,
		Subdomain: subdomain,
	}
	regRaw, _ := reg.Encode()
	if err := conn.WriteMessage(websocket.TextMessage, regRaw); err != nil {
		return fmt.Errorf("send register: %w", err)
	}

	// Read ack.
	_, ackRaw, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read ack: %w", err)
	}
	ack, err := shared.DecodeMessage(ackRaw)
	if err != nil {
		return fmt.Errorf("decode ack: %w", err)
	}
	if ack.Type == shared.MsgTypeError {
		return fmt.Errorf("server error: %s", ack.Error)
	}
	if ack.Type != shared.MsgTypeRegisterAck {
		return fmt.Errorf("unexpected message type: %s", ack.Type)
	}

	log.Printf("tunnel ready: subdomain=%s", ack.Subdomain)

	// Read loop — receive proxied requests and forward to local server.
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		msg, err := shared.DecodeMessage(raw)
		if err != nil {
			log.Printf("decode error: %v", err)
			continue
		}

		switch msg.Type {
		case shared.MsgTypeData:
			go handleRequest(conn, msg, localAddr)
		case shared.MsgTypeWSOpen:
			go sess.openStream(msg)
		case shared.MsgTypeWSMsg, shared.MsgTypeWSClose:
			sess.dispatch(msg)
		case shared.MsgTypePing:
			pong := &shared.Message{Type: shared.MsgTypePong}
			b, _ := pong.Encode()
			if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
				return fmt.Errorf("send pong: %w", err)
			}
		}
	}
}

// localHTTP talks to the exposed service. Redirects are passed back to the
// visitor untouched: following them here would drop their Set-Cookie.
var localHTTP = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func handleRequest(conn *wsConn, msg *shared.Message, localAddr string) {
	url := localAddr + msg.HTTPPath
	method := msg.HTTPMethod
	if method == "" {
		method = http.MethodGet
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(msg.Data))
	if err != nil {
		sendResponse(conn, msg.RequestID, http.StatusBadGateway, nil, []byte("failed to build request"))
		return
	}
	for k, v := range msg.HTTPHeader {
		req.Header.Set(k, v)
	}

	resp, err := localHTTP.Do(req)
	if err != nil {
		sendResponse(conn, msg.RequestID, http.StatusBadGateway, nil, []byte(fmt.Sprintf("local request failed: %v", err)))
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	headers := make(map[string]string)
	for k, v := range resp.Header {
		headers[k] = strings.Join(v, ", ")
	}

	sendResponse(conn, msg.RequestID, resp.StatusCode, headers, body)
}

func sendResponse(conn *wsConn, reqID string, status int, headers map[string]string, body []byte) {
	msg := &shared.Message{
		Type:       shared.MsgTypeDataAck,
		RequestID:  reqID,
		StatusCode: status,
		HTTPHeader: headers,
		Data:       body,
	}
	raw, _ := msg.Encode()
	if err := conn.WriteMessage(websocket.TextMessage, raw); err != nil {
		log.Printf("send response %s: %v", reqID, err)
	}
}

// backoff returns an exponential delay capped at 30s.
func backoff(attempt int) time.Duration {
	d := min(time.Duration(math.Pow(2, float64(attempt)))*time.Second, 30*time.Second)
	return d
}

// session holds the WebSocket streams proxied over one control connection.
type session struct {
	conn      *wsConn
	localAddr string

	mu      sync.Mutex
	streams map[string]*localStream
}

// localStream is one WebSocket to the local service.
type localStream struct {
	out          chan *shared.Message // frames from the visitor, written by one goroutine
	kill         chan struct{}
	once         sync.Once
	remoteClosed atomic.Bool // the visitor side closed: don't echo ws_close back
}

func (ls *localStream) abort() { ls.once.Do(func() { close(ls.kill) }) }

func (s *session) send(m *shared.Message) error {
	raw, err := m.Encode()
	if err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, raw)
}

var localWSDialer = &websocket.Dialer{HandshakeTimeout: 10 * time.Second}

func (s *session) openStream(msg *shared.Message) {
	id := msg.RequestID
	ls := &localStream{out: make(chan *shared.Message, 1024), kill: make(chan struct{})}
	s.mu.Lock()
	s.streams[id] = ls
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.streams, id)
		s.mu.Unlock()
	}()

	header := http.Header{}
	for k, v := range msg.HTTPHeader {
		if !shared.WebSocketHopHeaders[http.CanonicalHeaderKey(k)] {
			header.Set(k, v)
		}
	}
	url := "ws" + strings.TrimPrefix(s.localAddr, "http") + msg.HTTPPath

	local, resp, err := localWSDialer.Dial(url, header)
	if err != nil {
		ack := &shared.Message{Type: shared.MsgTypeWSOpenAck, RequestID: id, StatusCode: http.StatusBadGateway,
			Data: []byte(fmt.Sprintf("local websocket failed: %v", err))}
		if resp != nil {
			ack.StatusCode = resp.StatusCode
			ack.Data, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			ack.HTTPHeader = map[string]string{"Content-Type": resp.Header.Get("Content-Type")}
			resp.Body.Close()
		}
		_ = s.send(ack)
		return
	}
	defer local.Close()

	ack := &shared.Message{Type: shared.MsgTypeWSOpenAck, RequestID: id, StatusCode: http.StatusSwitchingProtocols,
		HTTPHeader: map[string]string{"Sec-Websocket-Protocol": resp.Header.Get("Sec-Websocket-Protocol")}}
	if err := s.send(ack); err != nil {
		return
	}

	// Visitor → local. This goroutine is the only writer on local.
	go func() {
		defer ls.abort()
		for {
			select {
			case m := <-ls.out:
				if m.Type == shared.MsgTypeWSClose {
					ls.remoteClosed.Store(true)
					_ = local.WriteControl(websocket.CloseMessage,
						websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
					return
				}
				typ := m.WSType
				if typ == 0 {
					typ = websocket.TextMessage
				}
				if local.WriteMessage(typ, m.Data) != nil {
					return
				}
			case <-ls.kill:
				return
			}
		}
	}()

	// Local → visitor.
	go func() {
		defer ls.abort()
		for {
			typ, data, err := local.ReadMessage()
			if err != nil {
				return
			}
			if s.send(&shared.Message{Type: shared.MsgTypeWSMsg, RequestID: id, WSType: typ, Data: data}) != nil {
				return
			}
		}
	}()

	<-ls.kill
	if !ls.remoteClosed.Load() {
		_ = s.send(&shared.Message{Type: shared.MsgTypeWSClose, RequestID: id})
	}
}

// dispatch hands a visitor frame to its stream without blocking the read loop.
func (s *session) dispatch(msg *shared.Message) {
	s.mu.Lock()
	ls, ok := s.streams[msg.RequestID]
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ls.out <- msg:
	default:
		log.Printf("stream %s overflow, closing", msg.RequestID)
		ls.abort()
	}
}

// closeAll ends every stream when the control connection drops.
func (s *session) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ls := range s.streams {
		ls.remoteClosed.Store(true)
		ls.abort()
	}
}
