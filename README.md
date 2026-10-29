# 🇧🇷 ptbrr: Torznab pt-BR Reverse Proxy Middleware

[![Go Report Card](https://goreportcard.com/badge/github.com/lucasfariamm/ptbrr)](https://goreportcard.com/report/github.com/lucasfariamm/ptbrr)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**ptbrr** é um middleware de proxy reverso ultraleve e de alta performance desenvolvido em Go (exclusivamente com a biblioteca padrão). Ele intercepta as requisições Torznab enviadas pelo **Radarr** em direção ao **Prowlarr**, identifica o identificador `tmdbid` do filme, traduz em tempo real o termo de busca em inglês (`q`) para o título correspondente em português brasileiro (**pt-BR**) via API do TMDB, e repassa a requisição com o título traduzido.

Isso contorna a limitação clássica de rastreadores públicos e trackers nacionais que indexam lançamentos com títulos em português (ex: *Dual Áudio*, *Dublado*, *Nacional*).

---

## 🏛️ Fluxo Arquitetural

```
┌──────────┐                               ┌──────────┐
│  Radarr  │──(1) Torznab GET com tmdbid──>│  ptbrr   │
└──────────┘                               └────┬─────┘
     ▲                                          │
     │                                     (2) Consulta API
     │                                    TMDB (pt-BR) + Cache
     │                                          │
     │                                          ▼
     │                                     ┌──────────┐
     │                                     │ TMDB API │
     │                                     └──────────┘
     │                                          │
     │                                     (3) Substitui 'q'
     │                                     pelo título em pt-BR
     │                                          │
     │                                          ▼
     │                                     ┌──────────┐
     │────────(4) Encaminha GET Torznab───>│ Prowlarr │
     │                                     └────┬─────┘
     │                                          │
     │                                     (5) Busca nos Trackers
     │                                     com título brasileiro
     │                                          │
     │                                          ▼
     └───────(6) Resposta XML Torznab───────────┘
```

---

## ⚙️ Variáveis de Ambiente

| Variável | Padrão | Descrição |
| :--- | :--- | :--- |
| `PORT` | `9697` | Porta HTTP na qual o proxy aceita conexões |
| `HOST` | `0.0.0.0` | Interface de rede para bind (`0.0.0.0` expõe na LAN/LXC) |
| `PROWLARR_URL` | `http://localhost:9696` | URL upstream da sua instância do Prowlarr |
| `TMDB_API_KEY` | *(Obrigatório para tradução)* | Chave de API v3 do The Movie Database (TMDB) |
| `CACHE_TTL_MINUTES` | `1440` (24 horas) | Tempo de retenção do título traduzido em cache de memória |
| `REQUEST_TIMEOUT_SECONDS` | `15` | Timeout em segundos para requisições externas |

---

## 🚀 Guia de Instalação no Proxmox LXC (systemd)

Este guia ensina como configurar o **ptbrr** diretamente em um container LXC no Proxmox (Debian ou Ubuntu) para execução como serviço nativo de inicialização automática.

### 1. Aceder ao Terminal do LXC
Aceda ao container LXC via Proxmox Console ou via SSH:
```bash
pct enter <LXC_ID>
# ou via SSH direto na máquina virtual/container
```

### 2. Criar Usuário do Sistema e Diretórios
Por boas práticas de segurança (mínimo privilégio), crie um usuário de sistema sem shell de login:
```bash
# Cria o grupo e usuário dedicado ptbrr
sudo useradd --system --no-create-home --shell /usr/sbin/nologin ptbrr

# Cria diretório de configuração
sudo mkdir -p /etc/ptbrr
```

### 3. Baixar a Release Compilada
Identifique a arquitetura do seu processador (`x86_64` para computadores comuns / Intel / AMD ou `aarch64` para ARM) e baixe o binário oficial mais recente via `curl` ou `wget`:

#### Opção A: Usando `curl` (Linux amd64 - padrão Proxmox x86_64)
```bash
sudo curl -sSL -o /usr/local/bin/ptbrr \
  https://github.com/lucasfariamm/ptbrr/releases/latest/download/ptbrr-linux-amd64

sudo chmod +x /usr/local/bin/ptbrr
```

#### Opção B: Usando `wget` (Linux arm64 / ARM)
```bash
sudo wget -O /usr/local/bin/ptbrr \
  https://github.com/lucasfariamm/ptbrr/releases/latest/download/ptbrr-linux-arm64

sudo chmod +x /usr/local/bin/ptbrr
```

#### Testar execução do binário
```bash
/usr/local/bin/ptbrr -version
```

---

### 4. Criar o Arquivo de Variáveis de Ambiente
Crie o arquivo `/etc/ptbrr/ptbrr.env`:
```bash
sudo nano /etc/ptbrr/ptbrr.env
```

Insira o conteúdo abaixo, adaptando o IP do Prowlarr e sua chave TMDB:
```ini
PORT=9697
HOST=0.0.0.0
PROWLARR_URL=http://192.168.1.100:9696
TMDB_API_KEY=sua_chave_tmdb_aqui
CACHE_TTL_MINUTES=1440
REQUEST_TIMEOUT_SECONDS=15
```

Ajuste as permissões do arquivo para proteger a chave de API:
```bash
sudo chown -R ptbrr:ptbrr /etc/ptbrr
sudo chmod 600 /etc/ptbrr/ptbrr.env
```

---

### 5. Configurar o Serviço `systemd`
Crie o arquivo da unidade systemd em `/etc/systemd/system/ptbrr.service`:
```bash
sudo nano /etc/systemd/system/ptbrr.service
```

Cole a configuração:
```ini
[Unit]
Description=ptbrr - Torznab pt-BR Translation Reverse Proxy
Documentation=https://github.com/lucasfariamm/ptbrr
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
User=ptbrr
Group=ptbrr
EnvironmentFile=/etc/ptbrr/ptbrr.env
ExecStart=/usr/local/bin/ptbrr
Restart=always
RestartSec=5s

# Hardening de Segurança
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

# Limite de descritores de ficheiro para concorrência elevada
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

---

### 6. Ativar e Iniciar o Serviço
Recarregue o daemon do systemd e inicie o serviço com autostart habilitado:
```bash
sudo systemctl daemon-reload
sudo systemctl enable --now ptbrr
```

Verifique o estado do serviço:
```bash
sudo systemctl status ptbrr
```

Acompanhe os logs em tempo real para verificar as traduções:
```bash
journalctl -u ptbrr -f -o cat
```

Exemplo de log esperado:
```text
==================================================
  ptbrr - Torznab pt-BR Translation Reverse Proxy 
  Version: v1.0.0
==================================================
[CONFIG] TMDB API Key: configured (first 4 chars: a1b2****)
[CONFIG] Listening Address : 0.0.0.0:9697
[CONFIG] Upstream Prowlarr : http://192.168.1.100:9696
[CONFIG] TMDB Base URL     : https://api.themoviedb.org/3
[CONFIG] Cache TTL         : 24h0m0s
[CONFIG] Request Timeout   : 15s
[SERVER] Starting reverse proxy on http://0.0.0.0:9697 ...
[TMDB TRANSLATE] tmdbid=12345: "Pirates of Silicon Valley" -> "Piratas do Vale do Silício"
[PROXY] GET /1/api?apikey=xyz&q=Piratas+do+Vale+do+Sil%C3%ADcio&t=movie&tmdbid=12345 -> status=200 duration=4.2ms
```

---

## 🎯 Configuração no Radarr

Para fazer o Radarr utilizar o proxy com traduções automáticas para o Prowlarr:

1. No **Radarr**, aceda a **Settings** > **Indexers**.
2. Abra o indexador Torznab sincronizado pelo Prowlarr (ou adicione um indexador Torznab personalizado).
3. No campo **URL**, altere apenas o host/porta de destino:
   - **Antes (Prowlarr direto):** `http://192.168.1.100:9696/1/api`
   - **Depois (ptbrr Proxy):** `http://192.168.1.100:9697/1/api` *(ou o IP onde o ptbrr está rodando)*
4. Mantenha a **API Key** original do Prowlarr inalterada.
5. Clique em **Test** (o Radarr fará uma consulta de recursos `t=caps`, que o proxy repassará ao Prowlarr de forma transparente).
6. Clique em **Save**.

Pronto! Qualquer pesquisa enviada pelo Radarr com `tmdbid` terá sua query automaticamente convertida para o título nacional brasileiro antes de alcançar os rastreadores do Prowlarr.

---

## 🛠️ Compilação Manual e Testes Locais

Para desenvolvedores que desejam compilar diretamente a partir do código-fonte:

```bash
# Clonar o repositório
git clone https://github.com/lucasfariamm/ptbrr.git
cd ptbrr

# Executar suíte completa de testes unitários e de integração
go test -v ./...

# Compilar binário nativo
go build -ldflags="-s -w -X main.version=local" -o ptbrr .

# Executar localmente
PORT=9697 PROWLARR_URL=http://localhost:9696 TMDB_API_KEY=sua_chave ./ptbrr
```
