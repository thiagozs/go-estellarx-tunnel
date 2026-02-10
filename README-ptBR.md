# EstellarX Tunnel

Exponha servicos HTTP locais para a internet atraves de um tunnel baseado em WebSocket.

[Read in English](README.md)

## Funcionalidades

- **Canal de controle WebSocket** — `gorilla/websocket`
- **Proxy HTTP reverso** — requisicoes encaminhadas para o servico local
- **Roteamento por subdominio** — cada tunnel recebe um subdominio unico (`myapp.tunnel.example.com`)
- **Autenticacao por token** — shared-secret opcional
- **TLS com Let's Encrypt** — certificados automaticos via `autocert`
- **Reconnect automatico** — backoff exponencial no client
- **CLI** — powered by `cobra` (`tunnel expose 8080`)

## Build

```bash
make build
# binarios em bin/
```

## Inicio rapido (local)

### 1. Iniciar o servidor

```bash
./bin/tunnel-server serve -p 8080 -d localhost
```

### 2. Iniciar uma aplicacao web local (exemplo)

```bash
python3 -m http.server 3000
```

### 3. Expor a aplicacao

```bash
./bin/tunnel-client expose 3000 \
  --server ws://localhost:8080/_tunnel/ws \
  --subdomain myapp
```

### 4. Acessar

```bash
# curl
curl -H "Host: myapp.localhost" http://localhost:8080/

# HTTPie
http GET http://localhost:8080/ Host:myapp.localhost
```

## Deploy com dominio proprio (passo a passo)

Este guia cobre todo o processo: VPS, DNS, firewall, systemd e cliente.

### 1. Requisitos

- Um **VPS** (Ubuntu/Debian) com IP publico (ex: `203.0.113.50`)
- Um **dominio** registrado (ex: `estellarx.com`)
- Acesso ao painel DNS do seu provedor (Cloudflare, Route53, GoDaddy, etc.)

### 2. Configurar DNS

No painel do seu provedor de DNS, crie dois registros apontando para o IP da VPS:

| Tipo | Nome       | Valor          | TTL |
| ---- | ---------- | -------------- | --- |
| `A`  | `tunnel`   | `203.0.113.50` | 300 |
| `A`  | `*.tunnel` | `203.0.113.50` | 300 |

Isso faz com que:

- `tunnel.estellarx.com` → IP da VPS (endpoint WebSocket)
- `*.tunnel.estellarx.com` → IP da VPS (subdominios dos tunnels)

Verifique a propagacao:

```bash
dig +short tunnel.estellarx.com
dig +short myapi.tunnel.estellarx.com

# HTTPie (alternativa)
http GET https://dns.google/resolve name==tunnel.estellarx.com type==A
```

Ambos devem retornar `203.0.113.50`.

> **Cloudflare**: desative o proxy (nuvem laranja → cinza) para que o TLS do Let's Encrypt funcione diretamente.

### 3. Preparar a VPS

```bash
# Conectar na VPS
ssh root@203.0.113.50

# Atualizar e instalar dependencias
apt update && apt upgrade -y
apt install -y make golang-go git ufw

# Abrir portas necessarias
ufw allow 22/tcp    # SSH
ufw allow 80/tcp    # HTTP (challenge Let's Encrypt)
ufw allow 443/tcp   # HTTPS (tunnel)
ufw enable
ufw status
```

### 4. Build na VPS

```bash
# Clonar o projeto
git clone https://github.com/thiagozs/go-estellarx-tunnel.git /opt/tunnel
cd /opt/tunnel

# Compilar
make build
```

Ou compile localmente e envie o binario:

```bash
# Na sua maquina (cross-compile para Linux)
make cross-linux
scp bin/tunnel-server-linux-amd64 root@203.0.113.50:/opt/tunnel/bin/tunnel-server
```

### 5. Gerar token de autenticacao

```bash
TOKEN=$(openssl rand -hex 32)
echo "TOKEN: $TOKEN"
# Guarde este token — voce vai usar no client tambem
```

### 6. Criar diretorio de certificados

```bash
mkdir -p /opt/tunnel/certs
```

O Let's Encrypt salva os certificados automaticamente nesta pasta via `autocert`.

### 7. Criar servico systemd

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
  -t "COLE_SEU_TOKEN_AQUI" \
  --tls-domain tunnel.estellarx.com
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
```

> Substitua `COLE_SEU_TOKEN_AQUI` pelo token gerado no passo 5.
> Substitua `tunnel.estellarx.com` pelo seu dominio.

```bash
# Ativar e iniciar
systemctl daemon-reload
systemctl enable tunnel-server
systemctl start tunnel-server

# Verificar status
systemctl status tunnel-server

# Ver logs
journalctl -u tunnel-server -f
```

O servidor agora escuta em:

- `:80` → redirect HTTP para HTTPS + challenge ACME
- `:443` → tunnel com TLS automatico

### 8. Verificar o servidor

```bash
# curl
curl -I https://tunnel.estellarx.com/

# HTTPie
http --headers GET https://tunnel.estellarx.com/
```

Deve retornar `502 Bad Gateway` com "no tunnel subdomain found" — isso e correto, significa que o server esta rodando.

### 9. Conectar o client (na sua maquina local)

Compile o client localmente:

```bash
make client
```

Suba a sua API local (exemplo: porta 3000):

```bash
go run ./cmd/api    # ou qualquer app local
```

Conecte o tunnel:

```bash
./bin/tunnel-client expose 3000 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "COLE_SEU_TOKEN_AQUI" \
  --subdomain api
```

### 10. Acessar a API de qualquer lugar

```bash
# curl
curl https://api.tunnel.estellarx.com/
curl -X POST https://api.tunnel.estellarx.com/users \
  -H "Content-Type: application/json" \
  -d '{"name": "teste"}'

# HTTPie
http GET https://api.tunnel.estellarx.com/
http POST https://api.tunnel.estellarx.com/users name=teste
```

### 11. Multiplos tunnels

Voce pode expor varios servicos simultaneamente com subdominios diferentes:

```bash
# Terminal 1 — API backend na porta 3000
./bin/tunnel-client expose 3000 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "SEU_TOKEN" \
  --subdomain api

# Terminal 2 — Frontend na porta 5173
./bin/tunnel-client expose 5173 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "SEU_TOKEN" \
  --subdomain app

# Terminal 3 — Admin na porta 8081
./bin/tunnel-client expose 8081 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "SEU_TOKEN" \
  --subdomain admin
```

Acesso:

- `https://api.tunnel.estellarx.com` → localhost:3000
- `https://app.tunnel.estellarx.com` → localhost:5173
- `https://admin.tunnel.estellarx.com` → localhost:8081

### Troubleshooting

| Problema              | Solucao                                                                                 |
| --------------------- | --------------------------------------------------------------------------------------- |
| `connection refused`  | Verifique se as portas 80/443 estao abertas (`ufw status`)                              |
| `certificate error`   | Aguarde 1-2 min para o Let's Encrypt emitir. Logs: `journalctl -u tunnel-server -f`    |
| `invalid token`       | Token do client deve ser identico ao do server                                          |
| `no tunnel subdomain` | O DNS `*.tunnel.seudominio` deve apontar para a VPS                                     |
| `tunnel not found`    | O client precisa estar conectado com aquele subdomain                                   |
| Client desconecta     | Reconnect automatico com backoff exponencial (2s, 4s, 8s... max 30s)                    |

## Flags da CLI

### Server (`tunnel-server serve`)

| Flag           | Short | Default     | Descricao                                  |
| -------------- | ----- | ----------- | ------------------------------------------ |
| `--port`       | `-p`  | `8080`      | Porta HTTP                                 |
| `--domain`     | `-d`  | `localhost` | Dominio base para roteamento por subdomino |
| `--token`      | `-t`  | _(vazio)_   | Token de autenticacao (vazio = sem auth)   |
| `--tls-domain` |       | _(vazio)_   | Ativar TLS via Let's Encrypt               |

### Client (`tunnel-client expose <port>`)

| Flag              | Default                          | Descricao              |
| ----------------- | -------------------------------- | ---------------------- |
| `-s, --server`    | `ws://localhost:8080/_tunnel/ws` | URL WebSocket do server |
| `-t, --token`     | _(vazio)_                        | Token de autenticacao  |
| `-d, --subdomain` | _(auto)_                         | Subdominio desejado    |

## Arquitetura

```
Internet → [Server :443] → WebSocket → [Client] → localhost:PORT
              ↑ roteamento por subdominio
              ↑ TLS (autocert)
              ↑ autenticacao por token
```

## Licença

Este projeto é distribuído sob a Licença MIT. Consulte o arquivo [LICENSE](LICENSE) para obter detalhes.

## Autor

2026, Thiago Zilli Sarmento :heart:
