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
		case shared.MsgTypePing:
			pong := &shared.Message{Type: shared.MsgTypePong}
			b, _ := pong.Encode()
			if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
				return fmt.Errorf("send pong: %w", err)
			}
		}
	}
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

	resp, err := http.DefaultClient.Do(req)
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
