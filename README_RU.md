# ⚡ GLM-ZAI-2API

OpenAI-совместимый прокси для Z.AI (GLM-модели) — один Go-бинарник.

[![Version](https://img.shields.io/badge/version-1.1.0-blue)](https://github.com/D3-vin/GLM-ZAI-2API/releases)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-AS_IS-green)](#лицензия)

[![Telegram](https://img.shields.io/badge/Telegram-@D3_vin-blue?logo=telegram)](https://t.me/D3_vin)
[![Author](https://img.shields.io/badge/Author-@D3vin_dev-blue?logo=telegram)](https://t.me/D3vin_dev)
[![GitHub](https://img.shields.io/badge/GitHub-Repository-black?logo=github)](https://github.com/D3-vin/GLM-ZAI-2API)

[Возможности](#возможности) • [Быстрый старт](#быстрый-старт) • [Использование](#использование) • [Конфигурация](#конфигурация) • [Устранение неполадок](#устранение-неполадок) • [Changelog](CHANGELOG.md) • [Контакты](#контакты)

[English](README.md) | [Русский](#)

---

## Возможности

- 🚀 **Один бинарник** — сервер, решатель капчи и хранилище токенов в одном Go-бинарнике; без пайпов, без внешних сервисов
- 🔓 **Три протокола** — `/v1/chat/completions` (OpenAI), `/v1/messages` (Anthropic — Claude Code), `/v1/responses` (Responses API — Codex CLI)
- 🧩 **Tool calling** — полная поддержка function calling, включая перехват блоков `<<<TOOL_CALL>>>` в живом стриме; работает из коробки
- 🖼️ **Vision** — картинки (URL или base64) на всех трёх протоколах через `GLM-5v-Turbo` (файлы загружаются в Z.AI автоматически)
- 🛡️ **Капча in-process** — Aliyun Captcha V3 решается прямо в процессе (DeviceData, тайминги по HAR)
- 🧠 **Живой каталог моделей** — тянется из Z.AI с браузерным отпечатком + snapshot-фолбэк; поддержка `reasoning_effort`
- 👥 **Пул аккаунтов** — `ZAI_TOKENS_FILE` с валидацией при старте и авторотацией при USER_BLOCKED
- 🌐 **Ротация прокси при WAF** — чат сначала идёт напрямую; после блока Aliyun WAF ретраи уезжают через `PROXY_FILE`
- 📊 **Встроенный дашборд** — live-статус + тумблеры фич по моделям
- 🔒 **Браузерный TLS-отпечаток** — весь трафик к Z.AI через профиль Chrome_146 (tls-client); стабильный SSE (edit_index UTF-16, holdback) и idle-сторож 120с
- 🌐 **Кросс-платформенность** — Windows, macOS, Linux (pure-Go SQLite, без CGO)

> 💡 **Работает с OpenCode из коробки!** Идеально для AI-ассистированной разработки.

---

## Быстрый старт

### 1. Скачать

Скачайте последний релиз для вашей платформы:
- [Windows (64-bit)](https://github.com/D3-vin/GLM-ZAI-2API/releases)
- [Linux (64-bit)](https://github.com/D3-vin/GLM-ZAI-2API/releases)
- [macOS Intel](https://github.com/D3-vin/GLM-ZAI-2API/releases)
- [macOS Apple Silicon](https://github.com/D3-vin/GLM-ZAI-2API/releases)

Или соберите из исходников:

```bash
git clone https://github.com/D3-vin/GLM-ZAI-2API.git
cd GLM-ZAI-2API
go build -trimpath -ldflags="-s -w" -o glm-zai-2api ./cmd/server
```

### 2. Сбор device-токенов (нужен Chrome/Chromium)

Решателю капчи нужен пул device-токенов:

```bash
go build -o token-collector ./cmd/token-collector/
./token-collector --count 750 --out tokens.sqlite
```

First run downloads the playwright driver (~40 MB, one-time); the browser itself is your system Chrome — ничего больше не устанавливается.

### 3. Конфигурация

```bash
cp .env.example .env
# Отредактируйте .env — задайте ZAI_TOKEN для GLM-5 моделей (см. Конфигурацию)
```

### 4. Запуск

```bash
./glm-zai-2api
```

Дашборд: http://localhost:5082

### 5. Тест

```bash
curl -X POST http://localhost:5082/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer d3vin" \
  -d '{"model":"glm-4.7","messages":[{"role":"user","content":"Привет!"}],"stream":false}'
```

---

## Использование

### Стриминг

```bash
curl -N -X POST http://localhost:5082/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer d3vin" \
  -d '{"model":"glm-4.7","stream":true,"messages":[{"role":"user","content":"Скажи hi"}]}'
```

### Подключение из IDE

| Настройка | Значение |
|---|---|
| Base URL | `http://localhost:5082/v1` |
| API Key | `d3vin` (значение `AUTH_TOKEN`) |
| Модель | `glm-4.7` (guest) или `GLM-5.1` (с `ZAI_TOKEN`) |

> 💡 **Пользователям OpenCode**: этот API отлично работает с OpenCode! Просто добавьте его как custom OpenAI endpoint и наслаждайтесь AI-ассистированной разработкой с GLM моделями.

### Модели

Список моделей тянется из Z.AI живьём (кэш 5 мин + snapshot-фолбэк) — см. `GET /v1/models`.

| Модель | Guest | С `ZAI_TOKEN` | Примечание |
|---|---|---|---|
| `glm-4.7` | да | да | текст |
| `GLM-5-Turbo` | нет | да | текст |
| `GLM-5v-Turbo` | нет | да | **vision** (картинки) |
| `GLM-5.1` / `glm-5.2` / … | нет | да | текст; на части — `reasoning_effort` |

> `glm-4.7` работает для guest-аккаунтов. GLM-5 модели требуют реальный `ZAI_TOKEN`.

### API-эндпоинты

| Метод | Путь | Auth | Описание |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | да | Чат (stream / non-stream, tool calling, картинки) |
| `POST` | `/v1/messages` | да | **Anthropic-протокол** — Claude Code и Anthropic-клиенты |
| `POST` | `/v1/responses` | да | **Responses API** — Codex CLI |
| `GET` | `/v1/models` | да | Живой каталог моделей |
| `POST` | `/features` | да | Тумблеры фич по моделям (для дашборда) |
| `POST` | `/prompt` | да | Legacy: одиночный текстовый промпт |
| `GET` | `/status` | нет | Статус сессии Z.AI + инфо о пуле аккаунтов |
| `GET` | `/admin/health`, `/admin/stats` | нет | Health-check, статистика |
| `GET` | `/admin/models` | да | Возможности моделей |
| `POST` | `/admin/session/clear` | да | Очистка истории всех сессий |

**Заголовки сессии:**

| Заголовок | Назначение |
|---|---|
| `X-Session-Id` | ID диалога (по умолчанию `default`) |
| `X-Fresh-Session` | `true` — новый `chat_id`, пустая история |

**Дополнительные поля запроса:** `webSearch` / `search`, `deepThink`, `reasoning` / `thinking:{"type":...}`, `reasoning_effort` (`low` / `high` / `max` — только для поддерживающих моделей).

### Vision (картинки)

```bash
curl -X POST http://localhost:5082/v1/chat/completions \
  -H "Content-Type: application/json" -H "Authorization: Bearer d3vin" \
  -d '{"model":"GLM-5v-Turbo","messages":[{"role":"user","content":[
        {"type":"text","text":"Что на этой картинке?"},
        {"type":"image_url","image_url":{"url":"https://... или data:image/png;base64,..."}}]}]}'
```

Картинки загружаются в Z.AI и подключаются как файлы автоматически. Требуется залогиненный `ZAI_TOKEN` (guest получает 401 на загрузку) и vision-модель (`GLM-5v-Turbo`).

### Tool Calling

Z.AI не поддерживает нативный function calling. Прокси реализует адаптер:

- `tools` → строгий контракт исполнения `<<<TOOL_CALL>>>` → парсится в `tool_calls`
- 6 стратегий парсинга: маркер-блоки, JSON code blocks, прямой/встроенный JSON, multi-line JSON, теги `<<TOOL>>`, естественный язык — плюс перехват в живом стриме и страхующий парс по финальному тексту
- Компактные сигнатуры инструментов (~2-3k символов вместо ~60k)
- Обработаны quirks Claude Code: aux-запросы (suggestion/recap) отвечаются текстом без tools; пустые nudge получают явный continue-фрейминг

### Сборщик токенов

```bash
./token-collector --count 750 --out tokens.sqlite
./token-collector --headed              # видимый браузер (контроль)
```

Собирает до 1250 device-токенов через **playwright-go** со стелс-headless Chrome (стелс только флагами: `--headless=new`, настоящий UA Chrome, флаг автоматизации снят). Токены расходуются решателем капчи и удаляются после использования. Сборщик перезаписывает `tokens.sqlite` с нуля — на время сбора остановите мост.

---

## Структура проекта

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

## Конфигурация

Настройки в файле `.env` (загружается автоматически) или переменных окружения:

| Переменная | По умолчанию | Описание |
|---|---|---|
| `ZAI_TOKEN` | *(пусто)* | JWT Z.AI; без него — guest-режим (только `glm-4.7`); **обязателен для vision** |
| `ZAI_TOKENS_FILE` | `zai_tokens.txt` | Пул аккаунтов, по одному JWT на строку |
| `USE_ACCOUNT_POOL` | `false` | `true` — пул с валидацией при старте + ротация при USER_BLOCKED |
| `PORT` | `5082` | Порт сервера |
| `HOST` | `0.0.0.0` | Адрес привязки |
| `AUTH_TOKEN` | `d3vin` | Bearer-токен для защищённых эндпоинтов |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DB_PATH` | `tokens.sqlite` | База device-токенов |
| `USE_PROXY` / `PROXY_FILE` | `false` / `proxy.txt` | Ротация при WAF: после блока чат-ретраи идут через пул прокси |
| `TIMEOUT` | `300000` | Таймаут запроса, мс |
| `CLAUDE_DESKTOP` | `false` | Алиасы `claude-*` на `/v1/models` + `/v1/messages` |
| `ANTHROPIC_ZAI_MIN_MS` | `1500` | Минимальный пауз между отправками Z.AI для `/v1/messages` |

> `.env` читается **только при старте** — после правки перезапустите мост.

**Получение `ZAI_TOKEN`:**
1. Откройте https://chat.z.ai и войдите в аккаунт
2. DevTools (F12) → Application → Local Storage → `token`
3. Скопируйте значение (начинается с `eyJ...`)
4. Добавьте в `.env`: `ZAI_TOKEN=eyJ...`

Опциональные флаги CLI (переопределяют env / значения по умолчанию):

- `--db-path <path>` — переопределить `DB_PATH` (по умолчанию `tokens.sqlite`)
- `--zai-tokens <path>` — переопределить `ZAI_TOKENS_FILE`
- `--verbose` — включить отладочный лог капчи

```bash
./glm-zai-2api --db-path /путь/к/tokens.sqlite --verbose
```

---

## Устранение неполадок

| Симптом | Причина | Решение |
|---|---|---|
| `captcha verification failed — check tokens.sqlite` | База токенов отсутствует или пуста | Запустите `token-collector` для пересборки `tokens.sqlite` |
| `No device tokens available` | Все токены израсходованы | Перезапустите сборщик; каждый токен одноразовый |
| `401` от Z.AI | Истёк JWT | Задайте свежий `ZAI_TOKEN` или перезапустите (guest flow переинициализируется) |
| Пустой ответ стрима | Ошибка капчи/сети | Включите `LOG_LEVEL=debug`, смотрите логи SSE |
| `blocked by Aliyun WAF` (405) | Rate-limit на ваш IP | Пропишите свежий `PROXY_FILE` + `USE_PROXY=true` (ретраи чата уйдут через прокси) или подождите снятия бана |
| Пустой ответ через прокси | Прокси рвёт долгие thinking-стримы | Короткие ответы / `deepThink:false` или прокси получше |
| `file upload unauthorized (401)` | Guest-сессия + картинка | Задайте залогиненный `ZAI_TOKEN` — vision без него не работает |
| `Model not available for current user level` | Guest-аккаунт + GLM-5 модель | Задайте `ZAI_TOKEN` реального аккаунта |

---

## Контакты

- **GitHub**: https://github.com/D3-vin/GLM-ZAI-2API
- **Telegram**: [@D3_vin](https://t.me/D3_vin)
- **Автор**: [@D3vin_dev](https://t.me/D3vin_dev)

---

## Лицензия

Предоставляется «как есть» в образовательных целях и для целей совместимости. Используйте ответственно и в соответствии с условиями сервиса Z.AI.
