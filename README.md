# EstellarX Tunnel

Expose local HTTP services to the internet through a WebSocket-based tunnel.

[Leia em Portugues](README-ptBR.md)

## Features

- **WebSocket control channel** — `gorilla/websocket`
- **Reverse HTTP proxy** — incoming requests forwarded to the local service
- **Subdomain routing** — each tunnel gets a unique subdomain (`myapp.tunnel.example.com`)
- **Token auth** — optional shared-secret authentication
- **TLS with Let's Encrypt** — automatic certificates via `autocert`
- **Wildcard TLS via DNS-01** — single `*.domain` certificate using pluggable DNS providers (name.com, Cloudflare)
- **Pluggable DNS providers** — implement `DNSProvider` to support any registrar
- **Certificate rate-limit guard** — certificates are only issued for subdomains with an active tunnel; scanners and bots cannot consume quota
- **Certificate cache cleanup** — inactive subdomain certs are removed from the local cache on a configurable schedule, preventing indefinite accumulation
- **Auto-reconnect** — exponential backoff on the client side
- **CLI** — powered by `cobra` (`tunnel expose 8080`)

## Build

```bash
make build
# binaries in bin/
```

## Quick start (local)

### 1. Start the server

```bash
./bin/tunnel-server serve -p 8080 -d localhost
```

### 2. Start a local web app (example)

```bash
python3 -m http.server 3000
```

### 3. Expose it

```bash
./bin/tunnel-client expose 3000 \
  --server ws://localhost:8080/_tunnel/ws \
  --subdomain myapp
```

### 4. Access it

```bash
# curl
curl -H "Host: myapp.localhost" http://localhost:8080/

# HTTPie
http GET http://localhost:8080/ Host:myapp.localhost
```

## Production deployment (custom domain)

This guide covers the full process: VPS, DNS, firewall, systemd, and client.

### 1. Requirements

- A **VPS** (Ubuntu/Debian) with a public IP (e.g. `203.0.113.50`)
- A registered **domain** (e.g. `estellarx.com`)
- Access to your DNS provider panel

### 2. Configure DNS

In your DNS provider panel, create two records pointing to your VPS IP:

| Type | Name       | Value          | TTL |
| ---- | ---------- | -------------- | --- |
| `A`  | `tunnel`   | `203.0.113.50` | 300 |
| `A`  | `*.tunnel` | `203.0.113.50` | 300 |

This maps:

- `tunnel.estellarx.com` → VPS IP (WebSocket endpoint)
- `*.tunnel.estellarx.com` → VPS IP (tunnel subdomains)

Verify propagation:

```bash
dig +short tunnel.estellarx.com
dig +short myapi.tunnel.estellarx.com
```

Both should return `203.0.113.50`.

> **Cloudflare proxy**: if using wildcard DNS-01 mode, disable the orange cloud (proxy) so Let's Encrypt can validate directly. In HTTP-01 per-subdomain mode the proxy must also be disabled.

### 3. Prepare the VPS

```bash
ssh root@203.0.113.50

apt update && apt upgrade -y
apt install -y make golang-go git ufw

ufw allow 22/tcp    # SSH
ufw allow 80/tcp    # HTTP (Let's Encrypt challenge / redirect)
ufw allow 443/tcp   # HTTPS (tunnel)
ufw enable
ufw status
```

### 4. Build on VPS

```bash
git clone https://github.com/thiagozs/go-estellarx-tunnel.git /opt/tunnel
cd /opt/tunnel
make build
```

Or cross-compile locally and upload the binary:

```bash
make cross-linux
scp bin/tunnel-server-linux-amd64 root@203.0.113.50:/opt/tunnel/bin/tunnel-server
```

### 5. Generate auth token

```bash
TOKEN=$(openssl rand -hex 32)
echo "TOKEN: $TOKEN"
# Save this token — you'll need it on the client too
```

### 6. Create certificate directory

```bash
mkdir -p /opt/tunnel/certs
```

### 7. Create systemd service

Choose the TLS mode that matches your DNS provider.

#### Option A — Wildcard TLS via name.com (DNS-01)

A single `*.tunnel.estellarx.com` certificate covers all subdomains automatically.
No per-subdomain certificate issuance, no rate-limit concerns.

```bash
cat > /opt/tunnel/.env << 'EOF'
TUNNEL_TOKEN=PASTE_YOUR_TOKEN_HERE
NAMECOM_TOKEN=PASTE_NAMECOM_TOKEN_HERE
EOF
chmod 600 /opt/tunnel/.env
```

```ini
# /etc/systemd/system/tunnel-server.service
[Unit]
Description=EstellarX Tunnel Server
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/tunnel
EnvironmentFile=/opt/tunnel/.env
ExecStart=/opt/tunnel/bin/tunnel-server serve \
  -d tunnel.estellarx.com \
  -t "${TUNNEL_TOKEN}" \
  --tls-domain tunnel.estellarx.com \
  --cert-cache-dir /opt/tunnel/certs \
  --dns-namecom-user YOUR_NAMECOM_USERNAME \
  --dns-namecom-token "${NAMECOM_TOKEN}"
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

> Requires API access enabled on name.com: Account → Settings → API

#### Option B — Wildcard TLS via Cloudflare (DNS-01)

```bash
cat > /opt/tunnel/.env << 'EOF'
TUNNEL_TOKEN=PASTE_YOUR_TOKEN_HERE
CF_API_TOKEN=PASTE_CLOUDFLARE_TOKEN_HERE
EOF
chmod 600 /opt/tunnel/.env
```

```ini
# /etc/systemd/system/tunnel-server.service
[Unit]
Description=EstellarX Tunnel Server
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/tunnel
EnvironmentFile=/opt/tunnel/.env
ExecStart=/opt/tunnel/bin/tunnel-server serve \
  -d tunnel.estellarx.com \
  -t "${TUNNEL_TOKEN}" \
  --tls-domain tunnel.estellarx.com \
  --cert-cache-dir /opt/tunnel/certs \
  --dns-cf-token "${CF_API_TOKEN}"
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

> Requires a Cloudflare API Token with **Zone / DNS / Edit** permission.
> Create one at: Cloudflare Dashboard → Profile → API Tokens

#### Option C — Per-subdomain TLS via HTTP-01 (autocert)

Issues one certificate per connected subdomain. Subject to Let's Encrypt rate limits (50 certs / 7 days).

```ini
ExecStart=/opt/tunnel/bin/tunnel-server serve \
  -d tunnel.estellarx.com \
  -t "YOUR_TOKEN" \
  --tls-domain tunnel.estellarx.com \
  --cert-cache-dir /opt/tunnel/certs \
  --cert-ttl 168h \
  --cert-cleanup-interval 6h
```

```bash
systemctl daemon-reload
systemctl enable tunnel-server
systemctl start tunnel-server

systemctl status tunnel-server
journalctl -u tunnel-server -f
```

The server listens on:

- `:80` → HTTP to HTTPS redirect + ACME challenge (HTTP-01 mode)
- `:443` → tunnel with automatic TLS

### 8. Verify the server

```bash
curl -I https://tunnel.estellarx.com/
```

Should return `502 Bad Gateway` — correct, it means the server is running and waiting for clients.

### 9. Connect the client

```bash
make client   # build client locally

./bin/tunnel-client expose 3000 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "YOUR_TOKEN" \
  --subdomain api
```

### 10. Access your service from anywhere

```bash
curl https://api.tunnel.estellarx.com/
```

### 11. Multiple tunnels

```bash
# Terminal 1 — API on port 3000
./bin/tunnel-client expose 3000 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "YOUR_TOKEN" --subdomain api

# Terminal 2 — Frontend on port 5173
./bin/tunnel-client expose 5173 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "YOUR_TOKEN" --subdomain app

# Terminal 3 — Admin on port 8081
./bin/tunnel-client expose 8081 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "YOUR_TOKEN" --subdomain admin
```

- `https://api.tunnel.estellarx.com` → localhost:3000
- `https://app.tunnel.estellarx.com` → localhost:5173
- `https://admin.tunnel.estellarx.com` → localhost:8081

## DNS Providers

The wildcard DNS-01 mode is backed by a pluggable `DNSProvider` interface. Built-in providers:

| Provider   | Flag                                            | Docs                                        |
| ---------- | ----------------------------------------------- | ------------------------------------------- |
| name.com   | `--dns-namecom-user` + `--dns-namecom-token`    | name.com/account/settings/api               |
| Cloudflare | `--dns-cf-token`                                | Cloudflare Dashboard → Profile → API Tokens |

### Adding a custom provider

Implement two methods and pass the provider to the server:

```go
type DNSProvider interface {
    SetTXTRecord(ctx context.Context, domain, value string) (recordID string, err error)
    DeleteTXTRecord(ctx context.Context, domain, recordID string) error
}
```

See [providers/namecom/](providers/namecom/) and [providers/cloudflare/](providers/cloudflare/) for reference implementations, and [examples/](examples/) for runnable usage examples.

## CLI flags

### Server (`tunnel-server serve`)

| Flag                      | Short | Default     | Description                                                       |
| ------------------------- | ----- | ----------- | ----------------------------------------------------------------- |
| `--port`                  | `-p`  | `8080`      | HTTP listen port (plain HTTP mode only)                           |
| `--domain`                | `-d`  | `localhost` | Base domain for subdomain routing                                 |
| `--token`                 | `-t`  | _(empty)_   | Auth token (empty = no auth)                                      |
| `--tls-domain`            |       | _(empty)_   | Enable Let's Encrypt TLS for this domain                          |
| `--cert-cache-dir`        |       | `certs`     | Directory for the TLS certificate cache                           |
| `--cert-ttl`              |       | `168h`      | Remove cache entries for inactive subdomains (HTTP-01 only)       |
| `--cert-cleanup-interval` |       | `6h`        | How often the cert cache cleanup runs (HTTP-01 only)              |
| `--dns-namecom-user`      |       | _(empty)_   | name.com API username — enables wildcard DNS-01 TLS               |
| `--dns-namecom-token`     |       | _(empty)_   | name.com API token                                                |
| `--dns-cf-token`          |       | _(empty)_   | Cloudflare API token — enables wildcard DNS-01 TLS                |

> **TLS mode selection:** if a DNS provider flag is set together with `--tls-domain`, wildcard DNS-01 mode is used. If only `--tls-domain` is set, per-subdomain HTTP-01 (autocert) is used. Without `--tls-domain`, the server runs plain HTTP.
> **Let's Encrypt rate limits (HTTP-01 mode):** max 50 certificates per registered domain per 7 days. The HostPolicy guard prevents scanners from consuming quota. Use `--cert-ttl` and `--cert-cleanup-interval` to control cache growth. Wildcard DNS-01 mode issues a single certificate and is not subject to per-subdomain limits.

### Client (`tunnel-client expose <port>`)

| Flag              | Default                          | Description          |
| ----------------- | -------------------------------- | -------------------- |
| `-s, --server`    | `ws://localhost:8080/_tunnel/ws` | Server WebSocket URL |
| `-t, --token`     | _(empty)_                        | Auth token           |
| `-d, --subdomain` | _(auto)_                         | Requested subdomain  |

## Architecture

```text
Internet → [Server :443] → WebSocket → [Client] → localhost:PORT
              ↑ subdomain routing
              ↑ TLS — wildcard DNS-01 (name.com / Cloudflare)
              ↑      or per-subdomain HTTP-01 (autocert)
              ↑ token auth
```

```text
providers/
  dns.go              ← DNSProvider interface
  namecom/            ← name.com implementation
  cloudflare/         ← Cloudflare implementation

examples/
  dns-namecom/        ← credential smoke-test for name.com
  dns-cloudflare/     ← credential smoke-test for Cloudflare
```

## Troubleshooting

| Problem | Solution |
| ------- | -------- |
| `connection refused` | Check that ports 80/443 are open (`ufw status`) |
| `certificate error` | Wait 1-2 min for Let's Encrypt. Check logs: `journalctl -u tunnel-server -f` |
| `invalid token` | Client token must match the server token exactly |
| `no tunnel subdomain` | DNS `*.tunnel.yourdomain` must point to the VPS |
| `tunnel not found` | The client must be connected with that subdomain |
| `no active tunnel for "x.y.com"` | Expected — cert issuance blocked when no client is connected (rate-limit guard) |
| `too many certificates` (LE 429) | Let's Encrypt limit reached. Switch to wildcard DNS-01 mode to avoid per-subdomain limits |
| `Permission Denied` (DNS-01) | API credentials invalid or API access not enabled on your DNS provider account |
| Client disconnects | Auto-reconnect retries with exponential backoff (2s, 4s, 8s… max 30s) |
| TLS handshake errors in logs | Usually internet scanners probing the port — harmless, connections are rejected at TLS level |

## License

This project is distributed under the MIT License. See the [LICENSE](LICENSE) file for details.

## Author

2026, Thiago Zilli Sarmento :heart:
