# EstellarX Tunnel

Expose local HTTP services to the internet through a WebSocket-based tunnel.

[Leia em Portugues](README-ptBR.md)

## Features

- **WebSocket control channel** — `gorilla/websocket`
- **Reverse HTTP proxy** — incoming requests forwarded to the local service
- **Subdomain routing** — each tunnel gets a unique subdomain (`myapp.tunnel.example.com`)
- **Token auth** — optional shared-secret authentication
- **TLS with Let's Encrypt** — automatic certificates via `autocert`
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
- Access to your DNS provider panel (Cloudflare, Route53, GoDaddy, etc.)

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

# HTTPie (alternative)
http GET https://dns.google/resolve name==tunnel.estellarx.com type==A
```

Both should return `203.0.113.50`.

> **Cloudflare**: disable the proxy (orange cloud → grey) so Let's Encrypt TLS works directly.

### 3. Prepare the VPS

```bash
# Connect to VPS
ssh root@203.0.113.50

# Update and install dependencies
apt update && apt upgrade -y
apt install -y make golang-go git ufw

# Open required ports
ufw allow 22/tcp    # SSH
ufw allow 80/tcp    # HTTP (Let's Encrypt challenge)
ufw allow 443/tcp   # HTTPS (tunnel)
ufw enable
ufw status
```

### 4. Build on VPS

```bash
# Clone the project
git clone https://github.com/thiagozs/go-estellarx-tunnel.git /opt/tunnel
cd /opt/tunnel

# Compile
make build
```

Or cross-compile locally and upload the binary:

```bash
# On your machine (cross-compile for Linux)
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

Let's Encrypt saves certificates automatically in this directory via `autocert`.

### 7. Create systemd service

```bash
cat > /etc/systemd/system/tunnel-server.service << 'EOF'
[Unit]
Description=EstellarX Tunnel Server
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/opt/tunnel
ExecStart=/opt/tunnel/bin/tunnel-server serve \
  -d tunnel.estellarx.com \
  -t "PASTE_YOUR_TOKEN_HERE" \
  --tls-domain tunnel.estellarx.com \
  --cert-cache-dir /opt/tunnel/certs \
  --cert-ttl 168h \
  --cert-cleanup-interval 6h
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
```

> Replace `PASTE_YOUR_TOKEN_HERE` with the token generated in step 5.
> Replace `tunnel.estellarx.com` with your domain.

```bash
# Enable and start
systemctl daemon-reload
systemctl enable tunnel-server
systemctl start tunnel-server

# Check status
systemctl status tunnel-server

# View logs
journalctl -u tunnel-server -f
```

The server now listens on:

- `:80` → HTTP to HTTPS redirect + ACME challenge
- `:443` → tunnel with automatic TLS

### 8. Verify the server

```bash
# curl
curl -I https://tunnel.estellarx.com/

# HTTPie
http --headers GET https://tunnel.estellarx.com/
```

Should return `502 Bad Gateway` with "no tunnel subdomain found" — this is correct, it means the server is running.

### 9. Connect the client (on your local machine)

Build the client locally:

```bash
make client
```

Start your local API (example: port 3000):

```bash
go run ./cmd/api    # or any local app
```

Connect the tunnel:

```bash
./bin/tunnel-client expose 3000 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "PASTE_YOUR_TOKEN_HERE" \
  --subdomain api
```

### 10. Access your API from anywhere

```bash
# curl
curl https://api.tunnel.estellarx.com/
curl -X POST https://api.tunnel.estellarx.com/users \
  -H "Content-Type: application/json" \
  -d '{"name": "test"}'

# HTTPie
http GET https://api.tunnel.estellarx.com/
http POST https://api.tunnel.estellarx.com/users name=test
```

### 11. Multiple tunnels

You can expose multiple services simultaneously with different subdomains:

```bash
# Terminal 1 — API backend on port 3000
./bin/tunnel-client expose 3000 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "YOUR_TOKEN" \
  --subdomain api

# Terminal 2 — Frontend on port 5173
./bin/tunnel-client expose 5173 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "YOUR_TOKEN" \
  --subdomain app

# Terminal 3 — Admin on port 8081
./bin/tunnel-client expose 8081 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "YOUR_TOKEN" \
  --subdomain admin
```

Access:

- `https://api.tunnel.estellarx.com` → localhost:3000
- `https://app.tunnel.estellarx.com` → localhost:5173
- `https://admin.tunnel.estellarx.com` → localhost:8081

### Troubleshooting

| Problem | Solution |
| ------- | -------- |
| `connection refused` | Check that ports 80/443 are open (`ufw status`) |
| `certificate error` | Wait 1-2 min for Let's Encrypt to issue. Check logs: `journalctl -u tunnel-server -f` |
| `invalid token` | Client token must match the server token exactly |
| `no tunnel subdomain` | DNS `*.tunnel.yourdomain` must point to the VPS |
| `tunnel not found` | The client must be connected with that subdomain |
| `no active tunnel for "x.y.com"` | Expected — cert issuance is blocked when no client is connected (rate-limit guard) |
| `too many certificates` (LE 429) | Let's Encrypt limit of 50 certs/7 days reached. Wait for the window to expire. Lower `--cert-ttl` to clean up stale cache entries sooner |
| Client disconnects | Auto-reconnect retries with exponential backoff (2s, 4s, 8s… max 30s) |

## CLI flags

### Server (`tunnel-server serve`)

| Flag                       | Short | Default     | Description                                                        |
| -------------------------- | ----- | ----------- | ------------------------------------------------------------------ |
| `--port`                   | `-p`  | `8080`      | HTTP listen port                                                   |
| `--domain`                 | `-d`  | `localhost` | Base domain for subdomain routing                                  |
| `--token`                  | `-t`  | _(empty)_   | Auth token (empty = no auth)                                       |
| `--tls-domain`             |       | _(empty)_   | Enable Let's Encrypt TLS for this domain                           |
| `--cert-cache-dir`         |       | `certs`     | Directory for the TLS certificate cache                            |
| `--cert-ttl`               |       | `168h`      | Remove cache entries for subdomains inactive longer than this      |
| `--cert-cleanup-interval`  |       | `6h`        | How often the cert cache cleanup runs                              |

> **Note on Let's Encrypt rate limits:** Let's Encrypt allows at most **50 certificates per registered domain per 7 days**. The server guards against this in two ways:
>
> 1. **HostPolicy guard** — certificates are only issued when a client tunnel is actively connected for that subdomain. Scanners and bots cannot consume quota.
> 2. **Cache cleanup** — cert files for subdomains inactive longer than `--cert-ttl` are deleted on every `--cert-cleanup-interval` cycle, preventing indefinite accumulation.
>
> Revoking or deleting certificates does **not** reset the rate-limit counter; that counter resets on its own after 7 days.

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
              ↑ TLS (autocert)
              ↑ token auth
```

## License

This project is distributed under the MIT License. See the [LICENSE](LICENSE) file for details.

## Autor

2026, Thiago Zilli Sarmento :heart: