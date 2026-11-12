package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
	"github.com/thiagozs/go-estellarx-tunnel/providers"
	"github.com/thiagozs/go-estellarx-tunnel/providers/cloudflare"
	"github.com/thiagozs/go-estellarx-tunnel/providers/namecom"
	"github.com/thiagozs/go-estellarx-tunnel/shared"
	"golang.org/x/crypto/acme/autocert"
)

// tunnel represents a connected client tunnel.
type tunnel struct {
	subdomain string
	conn      *websocket.Conn
	mu        sync.Mutex
	pending   map[string]chan *shared.Message // requestID -> response chan
	streams   map[string]*stream              // requestID -> proxied WebSocket
	done      chan struct{}                   // closed when the client disconnects
}

// send writes one message to the client; writes are serialized by t.mu.
func (t *tunnel) send(m *shared.Message) error {
	raw, err := m.Encode()
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.conn.WriteMessage(websocket.TextMessage, raw)
}

// stream is one visitor WebSocket proxied through the tunnel.
type stream struct {
	ch   chan *shared.Message // ws_open_ack, ws_msg and ws_close from the client
	kill chan struct{}
	once sync.Once
}

func (s *stream) abort() { s.once.Do(func() { close(s.kill) }) }

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
	// If a tunnel is already registered for this subdomain, close it so the
	// displaced client is notified and the connection is not left orphaned.
	if existing, ok := r.tunnels[sub]; ok {
		log.Printf("tunnel %s: displacing existing connection", sub)
		_ = existing.conn.Close()
	}
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

var (
	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	// reqCounter generates unique, collision-free request IDs under concurrent load.
	reqCounter atomic.Uint64
)

func main() {
	var (
		port                int
		domain              string
		token               string
		tlsDomain           string
		certCacheDir        string
		certTTL             time.Duration
		certCleanupInterval time.Duration
		namecomUser         string
		namecomToken        string
		cfToken             string
	)

	rootCmd := &cobra.Command{
		Use:   "tunnel-server",
		Short: "EstellarX Tunnel Server — expose local services to the internet",
	}

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the tunnel server",
		Run: func(cmd *cobra.Command, args []string) {
			dns, err := buildDNSProvider(namecomUser, namecomToken, cfToken)
			if err != nil {
				log.Fatal(err)
			}
			runServer(port, domain, token, tlsDomain, certCacheDir,
				certTTL, certCleanupInterval, dns)
		},
	}

	serveCmd.Flags().IntVarP(&port, "port", "p", 8080, "HTTP listen port")
	serveCmd.Flags().StringVarP(&domain, "domain", "d", "localhost", "Base domain for subdomains (e.g. tunnel.example.com)")
	serveCmd.Flags().StringVarP(&token, "token", "t", "", "Auth token clients must provide (empty = no auth)")
	serveCmd.Flags().StringVar(&tlsDomain, "tls-domain", "", "Enable TLS via Let's Encrypt for this domain")
	serveCmd.Flags().StringVar(&certCacheDir, "cert-cache-dir", "certs", "Directory for TLS certificate cache")
	serveCmd.Flags().DurationVar(&certTTL, "cert-ttl", 7*24*time.Hour, "Remove cert cache entries for subdomains inactive longer than this duration (autocert mode only)")
	serveCmd.Flags().DurationVar(&certCleanupInterval, "cert-cleanup-interval", 6*time.Hour, "How often to run the cert cache cleanup (autocert mode only)")
	// name.com DNS-01
	serveCmd.Flags().StringVar(&namecomUser, "dns-namecom-user", "", "name.com API username (enables wildcard DNS-01 TLS)")
	serveCmd.Flags().StringVar(&namecomToken, "dns-namecom-token", "", "name.com API token (enables wildcard DNS-01 TLS)")
	// Cloudflare DNS-01
	serveCmd.Flags().StringVar(&cfToken, "dns-cf-token", "", "Cloudflare API token with Zone/DNS/Edit permission (enables wildcard DNS-01 TLS)")

	rootCmd.AddCommand(serveCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// buildDNSProvider returns the appropriate DNSProvider based on which flags
// were supplied. Returns nil (no error) when no wildcard DNS flags are set,
// which signals the caller to fall back to per-subdomain HTTP-01 or plain HTTP.
func buildDNSProvider(namecomUser, namecomToken, cfToken string) (providers.DNSProvider, error) {
	switch {
	case cfToken != "":
		return cloudflare.New(cfToken), nil
	case namecomUser != "" && namecomToken != "":
		return namecom.New(namecomUser, namecomToken), nil
	case namecomUser != "" || namecomToken != "":
		return nil, fmt.Errorf("both --dns-namecom-user and --dns-namecom-token are required")
	default:
		return nil, nil // no wildcard DNS provider; use autocert or plain HTTP
	}
}

func runServer(port int, domain, token, tlsDomain, certCacheDir string,
	certTTL, certCleanupInterval time.Duration, dns providers.DNSProvider) {

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

	switch {
	// ── Wildcard mode: DNS-01 via pluggable provider ─────────────────────────
	case tlsDomain != "" && dns != nil:
		wcm := newWildcardCertManager(tlsDomain, certCacheDir, dns)
		if err := wcm.Start(context.Background()); err != nil {
			log.Fatalf("wildcard cert: %v", err)
		}
		srv := &http.Server{
			Addr:      ":443",
			Handler:   mux,
			TLSConfig: wcm.TLSConfig(),
		}
		go func() {
			log.Printf("HTTP redirect on :80")
			log.Fatal(http.ListenAndServe(":80", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://"+r.Host+r.RequestURI, http.StatusMovedPermanently)
			})))
		}()
		log.Printf("Tunnel server (TLS wildcard) on :443 domain=*.%s", tlsDomain)
		log.Fatal(srv.ListenAndServeTLS("", ""))

	// ── Per-subdomain mode: HTTP-01 via autocert ─────────────────────────────
	case tlsDomain != "":
		cm := newCertManager(certCacheDir, tlsDomain, certTTL)
		cm.runCleanup(certCleanupInterval)

		m := &autocert.Manager{
			Cache:  autocert.DirCache(certCacheDir),
			Prompt: autocert.AcceptTOS,
			HostPolicy: func(_ context.Context, host string) error {
				if host == tlsDomain {
					return nil
				}
				if sub, ok := strings.CutSuffix(host, "."+tlsDomain); ok {
					if reg.get(sub) != nil {
						return nil
					}
					return fmt.Errorf("acme/autocert: no active tunnel for %q", host)
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
		go func() {
			log.Printf("HTTP redirect on :80")
			log.Fatal(http.ListenAndServe(":80", m.HTTPHandler(nil)))
		}()
		log.Printf("Tunnel server (TLS per-subdomain) on :443 domain=%s", tlsDomain)
		log.Fatal(srv.ListenAndServeTLS("", ""))

	// ── Plain HTTP ───────────────────────────────────────────────────────────
	default:
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
		streams:   make(map[string]*stream),
		done:      make(chan struct{}),
	}
	reg.register(sub, t)
	defer reg.unregister(sub)
	defer close(t.done)

	// Send ack.
	ack := &shared.Message{Type: shared.MsgTypeRegisterAck, Subdomain: sub}
	b, _ := ack.Encode()
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		log.Printf("tunnel %s: send ack failed: %v", sub, err)
		return
	}
	log.Printf("tunnel registered: %s", sub)

	// Keepalive: send a ping every 30 s so dead connections are detected
	// proactively instead of waiting for the next request to time out.
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for range tick.C {
			ping, _ := (&shared.Message{Type: shared.MsgTypePing}).Encode()
			t.mu.Lock()
			err := t.conn.WriteMessage(websocket.TextMessage, ping)
			t.mu.Unlock()
			if err != nil {
				_ = conn.Close()
				return
			}
		}
	}()

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
		case shared.MsgTypeWSOpenAck, shared.MsgTypeWSMsg, shared.MsgTypeWSClose:
			t.mu.Lock()
			st, ok := t.streams[resp.RequestID]
			t.mu.Unlock()
			if !ok {
				continue
			}
			select {
			case st.ch <- resp:
			default:
				// The visitor is not keeping up; never block the control loop.
				log.Printf("tunnel %s: stream %s overflow, closing", sub, resp.RequestID)
				st.abort()
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
		jsonError(w, "no subdomain provided", http.StatusBadGateway)
		return
	}

	t := reg.get(sub)
	if t == nil {
		jsonError(w, fmt.Sprintf("tunnel %q is not registered or offline", sub), http.StatusNotFound)
		return
	}

	headers := forwardHeaders(r)
	if websocket.IsWebSocketUpgrade(r) {
		proxyWebSocket(w, r, t, headers)
		return
	}

	// Build a request ID — atomic counter guarantees uniqueness under concurrent load.
	reqID := fmt.Sprintf("%d", reqCounter.Add(1))

	// Read body.
	body, _ := io.ReadAll(r.Body)

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
		_, _ = w.Write(resp.Data)
	case <-time.After(30 * time.Second):
		http.Error(w, "tunnel timeout", http.StatusGatewayTimeout)
	}
}

// forwardHeaders copies the visitor's headers and sets X-Forwarded-For,
// -Proto and -Host, replacing whatever the visitor sent in them.
func forwardHeaders(r *http.Request) map[string]string {
	headers := make(map[string]string, len(r.Header)+3)
	for k, v := range r.Header {
		headers[k] = strings.Join(v, ", ")
	}
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	headers["X-Forwarded-For"] = ip
	headers["X-Forwarded-Proto"] = proto
	headers["X-Forwarded-Host"] = r.Host
	return headers
}

// proxyWebSocket asks the client to open the same WebSocket on the local
// service, upgrades the visitor only if that worked, then pumps frames both
// ways until either side closes.
func proxyWebSocket(w http.ResponseWriter, r *http.Request, t *tunnel, headers map[string]string) {
	id := fmt.Sprintf("%d", reqCounter.Add(1))
	for k := range headers {
		if shared.WebSocketHopHeaders[http.CanonicalHeaderKey(k)] {
			delete(headers, k)
		}
	}

	st := &stream{ch: make(chan *shared.Message, 1024), kill: make(chan struct{})}
	t.mu.Lock()
	t.streams[id] = st
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.streams, id)
		t.mu.Unlock()
	}()

	open := &shared.Message{Type: shared.MsgTypeWSOpen, RequestID: id, HTTPPath: r.RequestURI, HTTPHeader: headers}
	if err := t.send(open); err != nil {
		http.Error(w, "tunnel write failed", http.StatusBadGateway)
		return
	}

	var ack *shared.Message
	select {
	case ack = <-st.ch:
	case <-time.After(15 * time.Second):
		_ = t.send(&shared.Message{Type: shared.MsgTypeWSClose, RequestID: id})
		http.Error(w, "tunnel timeout", http.StatusGatewayTimeout)
		return
	case <-st.kill:
		http.Error(w, "tunnel stream aborted", http.StatusBadGateway)
		return
	case <-t.done:
		http.Error(w, "tunnel disconnected", http.StatusBadGateway)
		return
	}
	if ack.Type != shared.MsgTypeWSOpenAck || ack.StatusCode != http.StatusSwitchingProtocols {
		// Local service refused the upgrade: relay its answer (401, 403...).
		status := ack.StatusCode
		if status == 0 || status == http.StatusSwitchingProtocols {
			status = http.StatusBadGateway
		}
		for k, v := range ack.HTTPHeader {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write(ack.Data)
		return
	}

	respHeader := http.Header{}
	if p := ack.HTTPHeader["Sec-Websocket-Protocol"]; p != "" {
		respHeader.Set("Sec-WebSocket-Protocol", p)
	}
	conn, err := upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		_ = t.send(&shared.Message{Type: shared.MsgTypeWSClose, RequestID: id})
		return
	}
	defer conn.Close()

	// Visitor → client.
	go func() {
		defer st.abort()
		for {
			typ, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if t.send(&shared.Message{Type: shared.MsgTypeWSMsg, RequestID: id, WSType: typ, Data: data}) != nil {
				return
			}
		}
	}()

	// Client → visitor. This loop is the only writer on conn.
	closeVisitor := func() {
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	}
	for {
		select {
		case m := <-st.ch:
			switch m.Type {
			case shared.MsgTypeWSMsg:
				typ := m.WSType
				if typ == 0 {
					typ = websocket.TextMessage
				}
				if err := conn.WriteMessage(typ, m.Data); err != nil {
					_ = t.send(&shared.Message{Type: shared.MsgTypeWSClose, RequestID: id})
					return
				}
			case shared.MsgTypeWSClose:
				closeVisitor()
				return
			}
		case <-st.kill:
			_ = t.send(&shared.Message{Type: shared.MsgTypeWSClose, RequestID: id})
			closeVisitor()
			return
		case <-t.done:
			closeVisitor()
			return
		}
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
	_ = conn.WriteMessage(websocket.TextMessage, b)
}

// jsonError writes a JSON error response to the HTTP client.
func jsonError(w http.ResponseWriter, message string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	message = strings.ReplaceAll(message, "\"", "'")
	_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":%q}}`, code, message)
}
