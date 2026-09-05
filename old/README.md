# ⚡ GLM-ZAI-2API

OpenAI-compatible proxy for Z.AI (GLM models) — single Go binary.

[![Version](https://img.shields.io/badge/version-1.0.0-blue)](https://github.com/D3-vin/GLM-ZAI-2API/releases)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-AS_IS-green)](#license)

[![Telegram](https://img.shields.io/badge/Telegram-@D3_vin-blue?logo=telegram)](https://t.me/D3_vin)
[![Author](https://img.shields.io/badge/Author-@D3vin_dev-blue?logo=telegram)](https://t.me/D3vin_dev)
[![GitHub](https://img.shields.io/badge/GitHub-Repository-black?logo=github)](https://github.com/D3-vin/GLM-ZAI-2API)

[Features](#features) • [Quick Start](#quick-start) • [Usage](#usage) • [Configuration](#configuration) • [Troubleshooting](#troubleshooting) • [Contact](#contact)

[English](#) | [Русский](README_RU.md)

---

## Features

- 🚀 **Single binary** — server, captcha solver, and token store in one Go binary; no pipes, no external services
- 🔓 **OpenAI-compatible** — `/v1/chat/completions` and `/v1/models`; drop-in for OpenCode, Claude Code, Roo Code, and any OpenAI-format client
- 🧩 **Tool calling** — full function calling support for OpenCode and other IDEs; works out of the box
- 🛡️ **In-process captcha** — Aliyun Captcha V3 solved directly in-process, replacing the old named-pipe captcha-server
- 📊 **Embedded dashboard** — live HTML status UI served at the root URL
- 🌐 **Cross-platform** — Windows, macOS, Linux
- 📦 **Standalone** — pure-Go SQLite driver (`modernc.org/sqlite`), zero CGO dependencies

> 💡 **Works with IDE out of the box!** Perfect for AI-assisted development.

---

## Quick Start

### 1. Download

Download the latest release for your platform:
- [Windows (64-bit)](https://github.com/D3-vin/GLM-ZAI-2API/releases)
- [Linux (64-bit)](https://github.com/D3-vin/GLM-ZAI-2API/releases)
- [macOS Intel](https://github.com/D3-vin/GLM-ZAI-2API/releases)
- [macOS Apple Silicon](https://github.com/D3-vin/GLM-ZAI-2API/releases)

Or build from source:

```bash
git clone https://github.com/D3-vin/GLM-ZAI-2API.git
cd GLM-ZAI-2API
go build -trimpath -ldflags="-s -w" -o glm-zai-2api .
```

### 2. Collect device tokens (requires Chrome/Chromium)

The captcha solver needs a pool of device tokens:

```bash
go build -o token-collector ./token-collector/
./token-collector --count 750 --out tokens.sqlite
```

### 3. Configure

```bash
cp .env.example .env
# Edit .env — set ZAI_TOKEN for GLM-5 models (see Configuration)
```

### 4. Run

```bash
./glm-zai-2api
```

Dashboard: http://localhost:5082

### 5. Test

```bash
curl -X POST http://localhost:5082/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer d3vin" \
  -d '{"model":"glm-4.7","messages":[{"role":"user","content":"Hello!"}],"stream":false}'
```

---

## Usage

### Streaming

```bash
curl -N -X POST http://localhost:5082/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer d3vin" \
  -d '{"model":"glm-4.7","stream":true,"messages":[{"role":"user","content":"Say hi"}]}'
```

### Connect from an IDE

| Setting | Value |
|---|---|
| Base URL | `http://localhost:5082/v1` |
| API Key | `d3vin` (the `AUTH_TOKEN`) |
| Model | `glm-4.7` (guest) or `GLM-5.1` (with `ZAI_TOKEN`) |

> 💡 **OpenCode users**: This API works perfectly with OpenCode! Just add it as a custom OpenAI endpoint and enjoy AI-assisted coding with GLM models.

### Models

| Model | Guest | With `ZAI_TOKEN` |
|---|---|---|
| `glm-4.7` | yes | yes |
| `GLM-5-Turbo` | no | yes |
| `GLM-5v-Turbo` | no | yes |
| `GLM-5.1` | no | yes |
| `glm-5.2` | no | sometimes |

> `glm-4.7` works for guest accounts. All GLM-5 models require a real `ZAI_TOKEN`.

### API endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | yes | Chat (stream / non-stream, tool calling) |
| `GET` | `/v1/models` | yes | List models |
| `POST` | `/features` | yes | Toggle `webSearch`, `thinking`, `imageGen`, `persistHistory` |
| `POST` | `/prompt` | yes | Legacy: single text prompt |
| `GET` | `/status` | no | Z.AI session status |
| `GET` | `/admin/health` | no | Health-check |
| `GET` | `/admin/stats` | no | Stats |
| `POST` | `/admin/session/clear` | yes | Clear all session history |

**Session headers:**

| Header | Purpose |
|---|---|
| `X-Session-Id` | Conversation ID (default `default`) |
| `X-Fresh-Session` | `true` — new `chat_id`, empty history |

**Extra request fields:** `webSearch` / `search`, `deepThink`.

### Tool Calling

Z.AI has no native function calling. The proxy implements an adapter:

- `tools` → rewritten as a "unit test" prompt → model emits JSON → parsed into `tool_calls`
- 5 parsing strategies: JSON code blocks, direct JSON, multi-line JSON, `<<TOOL>>` tags, natural language
- Compact tool signatures (~2-3k chars instead of ~60k)

### Token collector

```bash
./token-collector --count 750 --out tokens.sqlite
./token-collector --headless=false      # visible browser
```

Collects up to 1250 device tokens via headless Chrome (`chromedp`). Tokens are consumed by the captcha solver and removed after use.

---

## Project Structure

```
├── main.go              # HTTP server, config, OpenAI routes, .env loader
├── zai.go               # Z.AI session, HMAC-SHA256 signature, SSE streaming
├── captcha.go           # Aliyun Captcha V3 (in-process)
├── tokens.go            # SQLite device-token store
├── tools.go             # Tool calling adapter (unit test framing)
├── dashboard.go         # Embedded HTML dashboard
├── token-collector/     # Device token collector (chromedp → Chrome)
├── .env.example         # Configuration template
├── go.mod / go.sum
└── README.md
```

---

## Configuration

Settings in `.env` (auto-loaded) or environment variables:

| Variable | Default | Description |
|---|---|---|
| `ZAI_TOKEN` | *(empty)* | Z.AI JWT. Without it — guest mode (only `glm-4.7`) |
| `PORT` | `5082` | Server port |
| `HOST` | `0.0.0.0` | Bind address |
| `AUTH_TOKEN` | `d3vin` | Bearer token for protected endpoints |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DB_PATH` | `tokens.sqlite` | Device tokens database |

**Getting `ZAI_TOKEN`:**
1. Open https://chat.z.ai and log in
2. DevTools (F12) → Application → Local Storage → `token`
3. Copy the value (starts with `eyJ...`)
4. Add to `.env`: `ZAI_TOKEN=eyJ...`

Optional CLI flags (override env / defaults):

- `--db-path <path>` — override `DB_PATH` (default `tokens.sqlite`)
- `--verbose` — enable captcha debug logging

```bash
./glm-zai-2api --db-path /custom/tokens.sqlite --verbose
```

---

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `captcha verification failed — check tokens.sqlite` | Token DB missing or empty | Run `token-collector` to rebuild `tokens.sqlite` |
| `No device tokens available` | All tokens consumed | Re-run the collector; each token is single-use |
| `401` from Z.AI | JWT expired | Set a fresh `ZAI_TOKEN`, or restart (guest flow re-initializes) |
| Empty stream response | Captcha/network error | Set `LOG_LEVEL=debug`, inspect SSE logs |
| `Model not available for current user level` | Guest account + GLM-5 model | Set `ZAI_TOKEN` with a real account |

---

## Contact

- **GitHub**: https://github.com/D3-vin/GLM-ZAI-2API
- **Telegram**: [@D3_vin](https://t.me/D3_vin)
- **Author**: [@D3vin_dev](https://t.me/D3vin_dev)

---

## License

Provided as-is for educational and interoperability purposes. Use responsibly and in accordance with Z.AI's terms of service.
