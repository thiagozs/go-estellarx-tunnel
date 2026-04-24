MODULE   := github.com/thiagozs/go-estellarx-tunnel
BIN_DIR  := bin

# Certificate management defaults (override on CLI: make run-server-tls CERT_TTL=48h)
CERT_CACHE_DIR        ?= certs
CERT_TTL              ?= 168h
CERT_CLEANUP_INTERVAL ?= 6h

# DNS provider credentials (override on CLI or export as env vars before running)
#   make run-server-namecom TLS_DOMAIN=tunnel.example.com TOKEN=secret \
#                           NAMECOM_USER=myuser NAMECOM_TOKEN=mytoken
#   make run-server-cloudflare TLS_DOMAIN=tunnel.example.com TOKEN=secret \
#                              CF_API_TOKEN=cf_token_here
NAMECOM_USER  ?=
NAMECOM_TOKEN ?=
CF_API_TOKEN  ?=

.PHONY: all build server client clean fmt vet tidy \
        build-linux build-mac build-windows build-rpi build-all \
        run-server run-server-tls run-server-namecom run-server-cloudflare run-client \
        example-namecom example-cloudflare help

all: build

## help: Show this help message
help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "Build targets:"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /' | column -t -s ':'
	@echo ""
	@echo "Variables (override on CLI):"
	@echo "  TLS_DOMAIN              Domain for TLS certificate"
	@echo "  TOKEN                   Shared secret token"
	@echo "  NAMECOM_USER            name.com username"
	@echo "  NAMECOM_TOKEN           name.com API token"
	@echo "  CF_API_TOKEN            Cloudflare API token"
	@echo "  CERT_CACHE_DIR          Certificate cache directory (default: certs)"
	@echo "  CERT_TTL                Certificate TTL (default: 168h)"
	@echo "  CERT_CLEANUP_INTERVAL   Cert cleanup interval (default: 6h)"

## build: Build server and client for the current platform
build: server client

server:
	go build -o $(BIN_DIR)/tunnel-server ./server

client:
	go build -o $(BIN_DIR)/tunnel-client ./client

## build-linux: Build for Linux amd64
build-linux:
	GOOS=linux GOARCH=amd64 go build -o $(BIN_DIR)/tunnel-server-linux-amd64 ./server
	GOOS=linux GOARCH=amd64 go build -o $(BIN_DIR)/tunnel-client-linux-amd64 ./client

## build-mac: Build for macOS (Intel and Apple Silicon)
build-mac:
	GOOS=darwin GOARCH=amd64 go build -o $(BIN_DIR)/tunnel-server-darwin-amd64 ./server
	GOOS=darwin GOARCH=amd64 go build -o $(BIN_DIR)/tunnel-client-darwin-amd64 ./client
	GOOS=darwin GOARCH=arm64 go build -o $(BIN_DIR)/tunnel-server-darwin-arm64 ./server
	GOOS=darwin GOARCH=arm64 go build -o $(BIN_DIR)/tunnel-client-darwin-arm64 ./client

## build-windows: Build for Windows amd64
build-windows:
	GOOS=windows GOARCH=amd64 go build -o $(BIN_DIR)/tunnel-server-windows-amd64.exe ./server
	GOOS=windows GOARCH=amd64 go build -o $(BIN_DIR)/tunnel-client-windows-amd64.exe ./client

## build-rpi: Build for Raspberry Pi (arm64 and arm32/armv7)
build-rpi:
	GOOS=linux GOARCH=arm64 go build -o $(BIN_DIR)/tunnel-server-linux-arm64 ./server
	GOOS=linux GOARCH=arm64 go build -o $(BIN_DIR)/tunnel-client-linux-arm64 ./client
	GOOS=linux GOARCH=arm GOARM=7 go build -o $(BIN_DIR)/tunnel-server-linux-armv7 ./server
	GOOS=linux GOARCH=arm GOARM=7 go build -o $(BIN_DIR)/tunnel-client-linux-armv7 ./client

## build-all: Build for all platforms (linux, mac, windows, raspberry pi)
build-all: build-linux build-mac build-windows build-rpi

## clean: Remove build artifacts
clean:
	rm -rf $(BIN_DIR)

## fmt: Format Go source files
fmt:
	go fmt ./...

## vet: Run go vet
vet:
	go vet ./...

## tidy: Tidy go modules
tidy:
	go mod tidy

# Run server locally without TLS (development)
run-server:
	go run ./server serve -p 8080 -d localhost

# Run server with TLS + certificate management (production-like)
# Requires: TLS_DOMAIN and TOKEN env vars (or override on CLI)
#   make run-server-tls TLS_DOMAIN=tunnel.example.com TOKEN=mysecret
run-server-tls:
	go run ./server serve \
	  -d $(TLS_DOMAIN) \
	  -t "$(TOKEN)" \
	  --tls-domain $(TLS_DOMAIN) \
	  --cert-cache-dir $(CERT_CACHE_DIR) \
	  --cert-ttl $(CERT_TTL) \
	  --cert-cleanup-interval $(CERT_CLEANUP_INTERVAL)

run-client:
	go run ./client expose 3000 -s ws://localhost:8080/_tunnel/ws -d myapp

# Run server with wildcard TLS via name.com DNS-01
# Requires: TLS_DOMAIN, TOKEN, NAMECOM_USER, NAMECOM_TOKEN
run-server-namecom:
	go run ./server serve \
	  -d $(TLS_DOMAIN) \
	  -t "$(TOKEN)" \
	  --tls-domain $(TLS_DOMAIN) \
	  --cert-cache-dir $(CERT_CACHE_DIR) \
	  --dns-namecom-user $(NAMECOM_USER) \
	  --dns-namecom-token $(NAMECOM_TOKEN)

# Run server with wildcard TLS via Cloudflare DNS-01
# Requires: TLS_DOMAIN, TOKEN, CF_API_TOKEN
run-server-cloudflare:
	go run ./server serve \
	  -d $(TLS_DOMAIN) \
	  -t "$(TOKEN)" \
	  --tls-domain $(TLS_DOMAIN) \
	  --cert-cache-dir $(CERT_CACHE_DIR) \
	  --dns-cf-token $(CF_API_TOKEN)

# Smoke-test DNS credentials (creates and deletes a test TXT record)
# Requires: NAMECOM_USER, NAMECOM_TOKEN
example-namecom:
	NAMECOM_USER=$(NAMECOM_USER) NAMECOM_TOKEN=$(NAMECOM_TOKEN) \
	  go run ./examples/dns-namecom

# Requires: CF_API_TOKEN
example-cloudflare:
	CF_API_TOKEN=$(CF_API_TOKEN) go run ./examples/dns-cloudflare
