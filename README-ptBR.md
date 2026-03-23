# EstellarX Tunnel

Exponha servicos HTTP locais para a internet atraves de um tunnel baseado em WebSocket.

[Read in English](README.md)

## Funcionalidades

- **Canal de controle WebSocket** — `gorilla/websocket`
- **Proxy HTTP reverso** — requisicoes encaminhadas para o servico local
- **Roteamento por subdominio** — cada tunnel recebe um subdominio unico (`myapp.tunnel.example.com`)
- **Autenticacao por token** — shared-secret opcional
- **TLS com Let's Encrypt** — certificados automaticos via `autocert`
- **TLS wildcard via DNS-01** — certificado unico `*.dominio` usando provedores DNS plugaveis (name.com, Cloudflare)
- **Provedores DNS plugaveis** — implemente `DNSProvider` para suportar qualquer registrador
- **Protecao contra rate limit** — certificados so sao emitidos para subdominios com tunnel ativo; scanners e bots nao consomem cota
- **Limpeza do cache de certificados** — entradas inativas sao removidas em ciclos configuravéis, evitando acumulo indefinido
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
- Acesso ao painel DNS do seu provedor

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
```

Ambos devem retornar `203.0.113.50`.

> **Cloudflare proxy**: no modo wildcard DNS-01, desative a nuvem laranja (proxy) para que o Let's Encrypt valide diretamente. No modo HTTP-01 por subdominio o proxy tambem deve estar desativado.

### 3. Preparar a VPS

```bash
ssh root@203.0.113.50

apt update && apt upgrade -y
apt install -y make golang-go git ufw

ufw allow 22/tcp    # SSH
ufw allow 80/tcp    # HTTP (challenge Let's Encrypt / redirect)
ufw allow 443/tcp   # HTTPS (tunnel)
ufw enable
ufw status
```

### 4. Build na VPS

```bash
git clone https://github.com/thiagozs/go-estellarx-tunnel.git /opt/tunnel
cd /opt/tunnel
make build
```

Ou compile localmente e envie o binario:

```bash
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

### 7. Criar servico systemd

Escolha o modo TLS de acordo com o seu provedor DNS.

#### Opcao A — TLS wildcard via name.com (DNS-01)

Um unico certificado `*.tunnel.estellarx.com` cobre todos os subdominios automaticamente.
Sem emissao por subdominio, sem preocupacao com rate limit.

```bash
cat > /opt/tunnel/.env << 'EOF'
TUNNEL_TOKEN=COLE_SEU_TOKEN_AQUI
NAMECOM_TOKEN=COLE_SEU_TOKEN_NAMECOM_AQUI
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
  --dns-namecom-user SEU_USUARIO_NAMECOM \
  --dns-namecom-token "${NAMECOM_TOKEN}"
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

> Requer acesso a API habilitado no name.com: Conta → Configuracoes → API

#### Opcao B — TLS wildcard via Cloudflare (DNS-01)

```bash
cat > /opt/tunnel/.env << 'EOF'
TUNNEL_TOKEN=COLE_SEU_TOKEN_AQUI
CF_API_TOKEN=COLE_SEU_TOKEN_CLOUDFLARE_AQUI
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

> Requer um API Token do Cloudflare com permissao **Zone / DNS / Edit**.
> Crie em: Cloudflare Dashboard → Perfil → API Tokens

#### Opcao C — TLS por subdominio via HTTP-01 (autocert)

Emite um certificado por subdominio conectado. Sujeito ao rate limit do Let's Encrypt (50 certs / 7 dias).

```ini
ExecStart=/opt/tunnel/bin/tunnel-server serve \
  -d tunnel.estellarx.com \
  -t "SEU_TOKEN" \
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

O servidor escuta em:

- `:80` → redirect HTTP para HTTPS + challenge ACME (modo HTTP-01)
- `:443` → tunnel com TLS automatico

### 8. Verificar o servidor

```bash
curl -I https://tunnel.estellarx.com/
```

Deve retornar `502 Bad Gateway` — correto, significa que o servidor esta rodando e aguardando clients.

### 9. Conectar o client

```bash
make client   # compilar client localmente

./bin/tunnel-client expose 3000 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "SEU_TOKEN" \
  --subdomain api
```

### 10. Acessar o servico de qualquer lugar

```bash
curl https://api.tunnel.estellarx.com/
```

### 11. Multiplos tunnels

```bash
# Terminal 1 — API na porta 3000
./bin/tunnel-client expose 3000 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "SEU_TOKEN" --subdomain api

# Terminal 2 — Frontend na porta 5173
./bin/tunnel-client expose 5173 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "SEU_TOKEN" --subdomain app

# Terminal 3 — Admin na porta 8081
./bin/tunnel-client expose 8081 \
  --server wss://tunnel.estellarx.com/_tunnel/ws \
  --token "SEU_TOKEN" --subdomain admin
```

- `https://api.tunnel.estellarx.com` → localhost:3000
- `https://app.tunnel.estellarx.com` → localhost:5173
- `https://admin.tunnel.estellarx.com` → localhost:8081

## Provedores DNS

O modo wildcard DNS-01 e suportado por uma interface `DNSProvider` plugavel. Provedores inclusos:

| Provedor   | Flag                                         | Documentacao                                     |
| ---------- | -------------------------------------------- | ------------------------------------------------ |
| name.com   | `--dns-namecom-user` + `--dns-namecom-token` | name.com/account/settings/api                    |
| Cloudflare | `--dns-cf-token`                             | Cloudflare Dashboard → Perfil → API Tokens       |

### Adicionando um provedor customizado

Implemente dois metodos e passe o provider ao servidor:

```go
type DNSProvider interface {
    SetTXTRecord(ctx context.Context, domain, value string) (recordID string, err error)
    DeleteTXTRecord(ctx context.Context, domain, recordID string) error
}
```

Veja [providers/namecom/](providers/namecom/) e [providers/cloudflare/](providers/cloudflare/) como referencia, e [examples/](examples/) para exemplos executaveis.

## Flags da CLI

### Server (`tunnel-server serve`)

| Flag                      | Short | Default     | Descricao                                                         |
| ------------------------- | ----- | ----------- | ----------------------------------------------------------------- |
| `--port`                  | `-p`  | `8080`      | Porta HTTP (apenas modo plain HTTP)                               |
| `--domain`                | `-d`  | `localhost` | Dominio base para roteamento por subdominio                       |
| `--token`                 | `-t`  | _(vazio)_   | Token de autenticacao (vazio = sem auth)                          |
| `--tls-domain`            |       | _(vazio)_   | Ativar TLS via Let's Encrypt para este dominio                    |
| `--cert-cache-dir`        |       | `certs`     | Diretorio do cache de certificados TLS                            |
| `--cert-ttl`              |       | `168h`      | Remove entradas de subdominios inativos (apenas HTTP-01)          |
| `--cert-cleanup-interval` |       | `6h`        | Frequencia da limpeza do cache de certificados (apenas HTTP-01)   |
| `--dns-namecom-user`      |       | _(vazio)_   | Usuario da API name.com — ativa TLS wildcard DNS-01               |
| `--dns-namecom-token`     |       | _(vazio)_   | Token da API name.com                                             |
| `--dns-cf-token`          |       | _(vazio)_   | Token da API Cloudflare — ativa TLS wildcard DNS-01               |

> **Selecao do modo TLS:** se uma flag de provedor DNS for definida junto com `--tls-domain`, o modo wildcard DNS-01 e usado. Se apenas `--tls-domain` for definido, o modo HTTP-01 por subdominio (autocert) e usado. Sem `--tls-domain`, o servidor roda em HTTP simples.
> **Rate limits do Let's Encrypt (modo HTTP-01):** maximo de 50 certificados por dominio registrado a cada 7 dias. O guarda da HostPolicy impede que scanners consumam cota. Use `--cert-ttl` e `--cert-cleanup-interval` para controlar o crescimento do cache. O modo wildcard DNS-01 emite um unico certificado e nao esta sujeito a limites por subdominio.

### Client (`tunnel-client expose <port>`)

| Flag              | Default                          | Descricao               |
| ----------------- | -------------------------------- | ----------------------- |
| `-s, --server`    | `ws://localhost:8080/_tunnel/ws` | URL WebSocket do server |
| `-t, --token`     | _(vazio)_                        | Token de autenticacao   |
| `-d, --subdomain` | _(auto)_                         | Subdominio desejado     |

## Arquitetura

```text
Internet → [Server :443] → WebSocket → [Client] → localhost:PORT
              ↑ roteamento por subdominio
              ↑ TLS — wildcard DNS-01 (name.com / Cloudflare)
              ↑      ou HTTP-01 por subdominio (autocert)
              ↑ autenticacao por token
```

```text
providers/
  dns.go              ← interface DNSProvider
  namecom/            ← implementacao name.com
  cloudflare/         ← implementacao Cloudflare

examples/
  dns-namecom/        ← smoke-test de credenciais name.com
  dns-cloudflare/     ← smoke-test de credenciais Cloudflare
```

## Troubleshooting

| Problema | Solucao |
| -------- | ------- |
| `connection refused` | Verifique se as portas 80/443 estao abertas (`ufw status`) |
| `certificate error` | Aguarde 1-2 min para o Let's Encrypt emitir. Logs: `journalctl -u tunnel-server -f` |
| `invalid token` | Token do client deve ser identico ao do server |
| `no tunnel subdomain` | O DNS `*.tunnel.seudominio` deve apontar para a VPS |
| `tunnel not found` | O client precisa estar conectado com aquele subdominio |
| `no active tunnel for "x.y.com"` | Esperado — emissao de cert bloqueada quando nenhum client esta conectado (protecao de rate limit) |
| `too many certificates` (LE 429) | Limite atingido. Mude para modo wildcard DNS-01 para evitar limites por subdominio |
| `Permission Denied` (DNS-01) | Credenciais invalidas ou acesso a API nao habilitado no provedor DNS |
| Client desconecta | Reconnect automatico com backoff exponencial (2s, 4s, 8s... max 30s) |
| Erros de TLS handshake nos logs | Normalmente scanners de internet sondando a porta — inofensivo, conexoes sao rejeitadas na camada TLS |

## Licenca

Este projeto e distribuido sob a Licenca MIT. Consulte o arquivo [LICENSE](LICENSE) para obter detalhes.

## Autor

2026, Thiago Zilli Sarmento :heart:
