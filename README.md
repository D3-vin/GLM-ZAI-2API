# ⚡ GLM-ZAI-2API

OpenAI-compatible proxy for Z.AI (GLM models) — single Go binary.

[![Version](https://img.shields.io/badge/version-1.1.0-blue)](https://github.com/D3-vin/GLM-ZAI-2API/releases)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-AS_IS-green)](#license)

[![Telegram](https://img.shields.io/badge/Telegram-@D3_vin-blue?logo=telegram)](https://t.me/D3_vin)
[![Author](https://img.shields.io/badge/Author-@D3vin_dev-blue?logo=telegram)](https://t.me/D3vin_dev)
[![GitHub](https://img.shields.io/badge/GitHub-Repository-black?logo=github)](https://github.com/D3-vin/GLM-ZAI-2API)

[Features](#features) • [Quick Start](#quick-start) • [Usage](#usage) • [Configuration](#configuration) • [Troubleshooting](#troubleshooting) • [Changelog](CHANGELOG.md) • [Contact](#contact)

[English](#) | [Русский](README_RU.md)

---

## Features

- 🚀 **Single binary** — server, captcha solver, and token store in one Go binary; no pipes, no external services
- 🔓 **Three protocols** — `/v1/chat/completions` (OpenAI), `/v1/messages` (Anthropic — Claude Code), `/v1/responses` (Responses API — Codex CLI)
- 🧩 **Tool calling** — full function calling incl. live streaming interception of `<<<TOOL_CALL>>>` blocks; works out of the box
- 🖼️ **Vision** — image input (URL or base64) on all three protocols via `GLM-5v-Turbo` (files are uploaded to Z.AI automatically)
- 🛡️ **In-process captcha** — Aliyun Captcha V3 solved directly in-process (DeviceData-bound, HAR-matched timings)
- 🧠 **Live model catalog** — fetched from Z.AI with a browser fingerprint + snapshot fallback; `reasoning_effort` support
- 👥 **Account pool** — `ZAI_TOKENS_FILE` with startup validation and auto-rotation on USER_BLOCKED
- 🌐 **WAF proxy rotation** — chat goes direct first; on an Aliyun WAF block retries rotate through `PROXY_FILE`
- 📊 **Embedded dashboard** — live status UI + per-model feature toggles
- 🔒 **Browser TLS fingerprint** — all Z.AI traffic rides a Chrome_146 profile (tls-client); stable SSE (edit_index UTF-16, holdback) and a 120s idle watchdog
- 🌐 **Cross-platform** — Windows, macOS, Linux (pure-Go SQLite, zero CGO)

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
go build -trimpath -ldflags="-s -w" -o glm-zai-2api ./cmd/server
```

### 2. Collect device tokens (requires Google Chrome installed)

The captcha solver needs a pool of device tokens:

```bash
go build -o token-collector ./cmd/token-collector/
./token-collector --count 750 --out tokens.sqlite
```

First run downloads the playwright driver (~40 MB, one-time); the browser itself is your system Chrome — nothing else gets installed.

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

The model list is fetched live from Z.AI (5-min cache + snapshot fallback) — see `GET /v1/models`.

| Model | Guest | With `ZAI_TOKEN` | Notes |
|---|---|---|---|
| `glm-4.7` | yes | yes | text |
| `GLM-5-Turbo` | no | yes | text |
| `GLM-5v-Turbo` | no | yes | **vision** (image input) |
| `GLM-5.1` / `glm-5.2` / … | no | yes | text; `reasoning_effort` on some |

> `glm-4.7` works for guest accounts. GLM-5 models require a real `ZAI_TOKEN`.

### API endpoints

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | yes | Chat (stream / non-stream, tool calling, images) |
| `POST` | `/v1/messages` | yes | **Anthropic protocol** — Claude Code & Anthropic clients |
| `POST` | `/v1/responses` | yes | **Responses API** — Codex CLI |
| `GET` | `/v1/models` | yes | Live model catalog |
| `POST` | `/features` | yes | Per-model feature toggles (dashboard-backed) |
| `POST` | `/prompt` | yes | Legacy: single text prompt |
| `GET` | `/status` | no | Z.AI session status + account pool info |
| `GET` | `/admin/health`, `/admin/stats` | no | Health-check, stats |
| `GET` | `/admin/models` | yes | Model capabilities |
| `POST` | `/admin/session/clear` | yes | Clear all session history |

**Session headers:**

| Header | Purpose |
|---|---|
| `X-Session-Id` | Conversation ID (default `default`) |
| `X-Fresh-Session` | `true` — new `chat_id`, empty history |

**Extra request fields:** `webSearch` / `search`, `deepThink`, `reasoning` / `thinking:{"type":...}`, `reasoning_effort` (`low` / `high` / `max` — supported models only).

### Vision (image input)

```bash
curl -X POST http://localhost:5082/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer d3vin" \
  -d '{"model":"GLM-5v-Turbo","messages":[{"role":"user","content":[
        {"type":"text","text":"What is on this image?"},
        {"type":"image_url","image_url":{"url":"https://... or data:image/png;base64,..."}}]}]}'
```

Images are uploaded to Z.AI and referenced as files automatically. Requires a logged-in `ZAI_TOKEN` (guest uploads are rejected with 401) and a vision model (`GLM-5v-Turbo`).

### Tool Calling

Z.AI has no native function calling. The proxy implements an adapter:

- `tools` → strict `<<<TOOL_CALL>>>` execution contract → parsed into `tool_calls`
- 6 parsing strategies: marker blocks, JSON code blocks, direct/embedded JSON, multi-line JSON, `<<TOOL>>` tags, natural language — plus live streaming interception and a safety-net parse over the final text
- Compact tool signatures (~2-3k chars instead of ~60k)
- Claude Code quirks handled: auxiliary (suggestion/recap) requests are answered text-only; empty continuation nudges get explicit continue-framing

### Token collector

```bash
./token-collector --count 750 --out tokens.sqlite
./token-collector --headed              # visible browser (control)
```

Collects up to 1250 device tokens via **playwright-go** with a stealth headless Chrome (flag-only stealth: `--headless=new`, real Chrome UA, automation flag stripped). Tokens are consumed by the captcha solver and removed after use. The collector rewrites `tokens.sqlite` from scratch — stop the bridge while collecting.

---

## Project Structure

```
├── cmd/
│   ├── server/              # Thin entry point → internal/app.Main()
│   └── token-collector/     # Device token collector (playwright-go, stealth headless)
├── internal/app/            # All bridge logic + tests (package app)
│   ├── main.go              # Config, routes, handlers, startup
│   ├── zai.go               # Z.AI session, signature, SSE (tls-client Chrome_146)
│   ├── captcha.go           # Aliyun Captcha V3 (in-process, DeviceData)
│   ├── anthropic.go         # /v1/messages (Claude/Anthropic protocol)
│   ├── responses.go         # /v1/responses (Responses API for Codex)
│   ├── vision.go            # Image input: upload → files array
│   ├── tools.go             # Tool calling adapter (EXECUTION LAW contract)
│   ├── sse.go / toolstream.go  # SSE parser + live tool-call interceptor
│   ├── models.go / features.go # Live model catalog + per-model toggles
│   ├── zaitoken.go / proxy.go  # Account pool + WAF proxy rotation
│   └── ...
├── build/                   # Release binaries
├── .env.example             # Configuration template
├── go.mod / go.sum
└── README.md
```

---

## Configuration

Settings in `.env` (auto-loaded) or environment variables:

| Variable | Default | Description |
|---|---|---|
| `ZAI_TOKEN` | *(empty)* | Z.AI JWT. Without it — guest mode (only `glm-4.7`); **required for vision** |
| `ZAI_TOKENS_FILE` | `zai_tokens.txt` | Account pool, one JWT per line |
| `USE_ACCOUNT_POOL` | `false` | `true` — pool with startup validation + rotation on USER_BLOCKED |
| `PORT` | `5082` | Server port |
| `HOST` | `0.0.0.0` | Bind address |
| `AUTH_TOKEN` | `d3vin` | Bearer token for protected endpoints |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DB_PATH` | `tokens.sqlite` | Device tokens database |
| `USE_PROXY` / `PROXY_FILE` | `false` / `proxy.txt` | WAF rotation: after a block, chat retries ride the proxy pool |
| `TIMEOUT` | `300000` | Request timeout, ms |
| `CLAUDE_DESKTOP` | `false` | `claude-*` aliases on `/v1/models` + `/v1/messages` |
| `ANTHROPIC_ZAI_MIN_MS` | `1500` | Min gap between Z.AI sends for `/v1/messages` |

> `.env` is read **at startup only** — restart the bridge after changing it.

**Getting `ZAI_TOKEN`:**
1. Open https://chat.z.ai and log in
2. DevTools (F12) → Application → Local Storage → `token`
3. Copy the value (starts with `eyJ...`)
4. Add to `.env`: `ZAI_TOKEN=eyJ...`

Optional CLI flags (override env / defaults):

- `--db-path <path>` — override `DB_PATH` (default `tokens.sqlite`)
- `--zai-tokens <path>` — override `ZAI_TOKENS_FILE`
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
| `blocked by Aliyun WAF` (405) | Rate limit on your IP | Set a fresh `PROXY_FILE` + `USE_PROXY=true` (chat retries rotate proxies), or wait the ban out |
| Empty answer via proxy | Proxy drops long thinking streams | Prefer short answers / `deepThink:false`, or a better proxy |
| `file upload unauthorized (401)` | Guest session + image input | Set a logged-in `ZAI_TOKEN` — vision requires it |
| `Model not available for current user level` | Guest account + GLM-5 model | Set `ZAI_TOKEN` with a real account |

---

## Contact

- **GitHub**: https://github.com/D3-vin/GLM-ZAI-2API
- **Telegram**: [@D3_vin](https://t.me/D3_vin)
- **Author**: [@D3vin_dev](https://t.me/D3vin_dev)

---

## License

Provided as-is for educational and interoperability purposes. Use responsibly and in accordance with Z.AI's terms of service.
