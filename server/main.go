package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/thiagozs/go-estellarx-tunnel/shared"
	"golang.org/x/crypto/acme/autocert"
)

// tunnel represents a connected client tunnel.
type tunnel struct {
	subdomain string
	conn      *websocket.Conn
	mu        sync.Mutex
	pending   map[string]chan *shared.Message // requestID -> response chan
}

// registry maps subdomain -> tunnel.
type registry struct {
	mu      sync.RWMutex
	tunnels map[string]*tunnel
}

func newRegistry() *registry {
	return &registry{tunnels: make(map[string]*tunnel)}
}

func (r *registry) register(sub string, t *tunnel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tunnels[sub] = t
}

func (r *registry) unregister(sub string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tunnels, sub)
}

func (r *registry) get(sub string) *tunnel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tunnels[sub]
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	var (
		port      int
		domain    string
		token     string
		tlsDomain string
	)

	rootCmd := &cobra.Command{
		Use:   "tunnel-server",
		Short: "EstellarX Tunnel Server — expose local services to the internet",
	}

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the tunnel server",
		Run: func(cmd *cobra.Command, args []string) {
			runServer(port, domain, token, tlsDomain)
		},
	}

	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "HTTP listen port")
	serveCmd.Flags().StringVarP(&domain, "domain", "d", "localhost", "Base domain for subdomains (e.g. tunnel.example.com)")
	serveCmd.Flags().StringVarP(&token, "token", "t", "", "Auth token clients must provide (empty = no auth)")
	serveCmd.Flags().StringVar(&tlsDomain, "tls-domain", "", "Enable TLS via Let's Encrypt for this domain")

	rootCmd.AddCommand(serveCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runServer(port int, domain, token, tlsDomain string) {
	reg := newRegistry()

	mux := http.NewServeMux()

	// WebSocket control endpoint — clients connect here to register tunnels.
	mux.HandleFunc("/_tunnel/ws", func(w http.ResponseWriter, r *http.Request) {
		handleControl(w, r, reg, token)
	})

	// Everything else is proxied to the matching tunnel client.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handleProxy(w, r, reg, domain)
	})

	addr := fmt.Sprintf(":%d", port)

	if tlsDomain != "" {
		m := &autocert.Manager{
			Cache:  autocert.DirCache("certs"),
			Prompt: autocert.AcceptTOS,
			HostPolicy: func(_ context.Context, host string) error {
				if host == tlsDomain || strings.HasSuffix(host, "."+tlsDomain) {
					return nil
				}
				return fmt.Errorf("acme/autocert: host %q not allowed", host)
			},
		}
		srv := &http.Server{
			Addr:      ":443",
			Handler:   mux,
			TLSConfig: m.TLSConfig(),
		}
		srv.TLSConfig.NextProtos = append([]string{"h2", "http/1.1"}, srv.TLSConfig.NextProtos...)
		// Redirect HTTP → HTTPS
		go func() {
			log.Printf("HTTP redirect on :80")
			log.Fatal(http.ListenAndServe(":80", m.HTTPHandler(nil)))
		}()
		log.Printf("Tunnel server (TLS) on :443 domain=%s", tlsDomain)
		log.Fatal(srv.ListenAndServeTLS("", ""))
	} else {
		log.Printf("Tunnel server on %s domain=%s", addr, domain)
		log.Fatal(http.ListenAndServe(addr, mux))
	}
}

// handleControl upgrades to WebSocket and manages the tunnel lifecycle.
func handleControl(w http.ResponseWriter, r *http.Request,
	reg *registry, authToken string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	defer conn.Close()

	// Read the first message — must be "register".
	_, raw, err := conn.ReadMessage()
	if err != nil {
		log.Printf("ws read register: %v", err)
		return
	}
	msg, err := shared.DecodeMessage(raw)
	if err != nil || msg.Type != shared.MsgTypeRegister {
		sendError(conn, "expected register message")
		return
	}

	// Auth check.
	if authToken != "" {
		if subtle.ConstantTimeCompare([]byte(msg.Token), []byte(authToken)) != 1 {
			sendError(conn, "invalid token")
			return
		}
	}

	sub := msg.Subdomain
	if sub == "" {
		sub = fmt.Sprintf("t%d", time.Now().UnixNano()%100000)
	}

	t := &tunnel{
		subdomain: sub,
		conn:      conn,
		pending:   make(map[string]chan *shared.Message),
	}
	reg.register(sub, t)
	defer reg.unregister(sub)

	// Send ack.
	ack := &shared.Message{Type: shared.MsgTypeRegisterAck, Subdomain: sub}
	b, _ := ack.Encode()
	conn.WriteMessage(websocket.TextMessage, b)
	log.Printf("tunnel registered: %s", sub)

	// Read loop — receives responses from the client.
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			log.Printf("tunnel %s disconnected: %v", sub, err)
			return
		}
		resp, err := shared.DecodeMessage(raw)
		if err != nil {
			continue
		}
		switch resp.Type {
		case shared.MsgTypeDataAck:
			t.mu.Lock()
			ch, ok := t.pending[resp.RequestID]
			t.mu.Unlock()
			if ok {
				ch <- resp
			}
		case shared.MsgTypePong:
			// keep-alive response, ignore
		}
	}
}

// handleProxy routes incoming HTTP requests to the correct tunnel client.
func handleProxy(w http.ResponseWriter, r *http.Request,
	reg *registry, baseDomain string) {
	sub := extractSubdomain(r.Host, baseDomain)
	if sub == "" {
		http.Error(w, "no tunnel subdomain found", http.StatusBadGateway)
		return
	}

	t := reg.get(sub)
	if t == nil {
		http.Error(w, fmt.Sprintf("tunnel %q not found", sub), http.StatusBadGateway)
		return
	}

	// Build a request ID.
	reqID := fmt.Sprintf("%d", time.Now().UnixNano())

	// Read body.
	body, _ := io.ReadAll(r.Body)

	headers := make(map[string]string)
	for k, v := range r.Header {
		headers[k] = strings.Join(v, ", ")
	}

	msg := &shared.Message{
		Type:       shared.MsgTypeData,
		RequestID:  reqID,
		HTTPMethod: r.Method,
		HTTPPath:   r.RequestURI,
		HTTPHeader: headers,
		Data:       body,
	}

	// Create response channel.
	ch := make(chan *shared.Message, 1)
	t.mu.Lock()
	t.pending[reqID] = ch
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.pending, reqID)
		t.mu.Unlock()
	}()

	// Send to client.
	raw, _ := msg.Encode()
	t.mu.Lock()
	err := t.conn.WriteMessage(websocket.TextMessage, raw)
	t.mu.Unlock()
	if err != nil {
		http.Error(w, "tunnel write failed", http.StatusBadGateway)
		return
	}

	// Wait for response with timeout.
	select {
	case resp := <-ch:
		for k, v := range resp.HTTPHeader {
			w.Header().Set(k, v)
		}
		status := resp.StatusCode
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		w.Write(resp.Data)
	case <-time.After(30 * time.Second):
		http.Error(w, "tunnel timeout", http.StatusGatewayTimeout)
	}
}

// extractSubdomain returns the subdomain prefix from the Host header.
// e.g. "myapp.tunnel.example.com" with baseDomain "tunnel.example.com" → "myapp"
func extractSubdomain(host, baseDomain string) string {
	// Strip port.
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}
	// For localhost testing, fall back to the first label.
	if baseDomain == "localhost" {
		parts := strings.SplitN(host, ".", 2)
		if len(parts) == 2 {
			return parts[0]
		}
		return ""
	}
	if !strings.HasSuffix(host, "."+baseDomain) {
		return ""
	}
	return strings.TrimSuffix(host, "."+baseDomain)
}

func sendError(conn *websocket.Conn, errMsg string) {
	m := &shared.Message{Type: shared.MsgTypeError, Error: errMsg}
	b, _ := m.Encode()
	conn.WriteMessage(websocket.TextMessage, b)
}
