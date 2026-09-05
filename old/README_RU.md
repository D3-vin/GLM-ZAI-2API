# ⚡ GLM-ZAI-2API

OpenAI-совместимый прокси для Z.AI (GLM-модели) — один Go-бинарник.

[![Version](https://img.shields.io/badge/version-1.0.0-blue)](https://github.com/D3-vin/GLM-ZAI-2API/releases)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/license-AS_IS-green)](#лицензия)

[![Telegram](https://img.shields.io/badge/Telegram-@D3_vin-blue?logo=telegram)](https://t.me/D3_vin)
[![Author](https://img.shields.io/badge/Author-@D3vin_dev-blue?logo=telegram)](https://t.me/D3vin_dev)
[![GitHub](https://img.shields.io/badge/GitHub-Repository-black?logo=github)](https://github.com/D3-vin/GLM-ZAI-2API)

[Возможности](#возможности) • [Быстрый старт](#быстрый-старт) • [Использование](#использование) • [Конфигурация](#конфигурация) • [Устранение неполадок](#устранение-неполадок) • [Контакты](#контакты)

[English](README.md) | [Русский](#)

---

## Возможности

- 🚀 **Один бинарник** — сервер, решатель капчи и хранилище токенов в одном Go-бинарнике; без пайпов, без внешних сервисов
- 🔓 **OpenAI-совместимость** — `/v1/chat/completions` и `/v1/models`; работает с OpenCode, Claude Code, Roo Code и любым клиентом формата OpenAI
- 🧩 **Tool calling** — полная поддержка function calling для OpenCode и других IDE; работает из коробки
- 🛡️ **Капча in-process** — Aliyun Captcha V3 решается прямо в процессе, замена старому captcha-server через named pipe
- 📊 **Встроенный дашборд** — live HTML-статус на корневом URL
- 🌐 **Кросс-платформенность** — Windows, macOS, Linux
- 📦 **Автономность** — pure-Go SQLite-драйвер (`modernc.org/sqlite`), без CGO-зависимостей

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
go build -trimpath -ldflags="-s -w" -o glm-zai-2api .
```

### 2. Сбор device-токенов (нужен Chrome/Chromium)

Решателю капчи нужен пул device-токенов:

```bash
go build -o token-collector ./token-collector/
./token-collector --count 750 --out tokens.sqlite
```

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

| Модель | Guest | С `ZAI_TOKEN` |
|---|---|---|
| `glm-4.7` | да | да |
| `GLM-5-Turbo` | нет | да |
| `GLM-5v-Turbo` | нет | да |
| `GLM-5.1` | нет | да |
| `glm-5.2` | нет | иногда |

> `glm-4.7` работает для guest-аккаунтов. Все GLM-5 модели требуют реальный `ZAI_TOKEN`.

### API-эндпоинты

| Метод | Путь | Auth | Описание |
|---|---|---|---|
| `POST` | `/v1/chat/completions` | да | Чат (stream / non-stream, tool calling) |
| `GET` | `/v1/models` | да | Список моделей |
| `POST` | `/features` | да | Переключение `webSearch`, `thinking`, `imageGen`, `persistHistory` |
| `POST` | `/prompt` | да | Legacy: одиночный текстовый промпт |
| `GET` | `/status` | нет | Статус сессии Z.AI |
| `GET` | `/admin/health` | нет | Health-check |
| `GET` | `/admin/stats` | нет | Статистика |
| `POST` | `/admin/session/clear` | да | Очистка истории всех сессий |

**Заголовки сессии:**

| Заголовок | Назначение |
|---|---|
| `X-Session-Id` | ID диалога (по умолчанию `default`) |
| `X-Fresh-Session` | `true` — новый `chat_id`, пустая история |

**Дополнительные поля запроса:** `webSearch` / `search`, `deepThink`.

### Tool Calling

Z.AI не поддерживает нативный function calling. Прокси реализует адаптер:

- `tools` → переписывается как «unit test» промпт → модель выдаёт JSON → парсится в `tool_calls`
- 5 стратегий парсинга: JSON code blocks, прямой JSON, multi-line JSON, теги `<<TOOL>>`, естественный язык
- Компактные сигнатуры инструментов (~2-3k символов вместо ~60k)

### Сборщик токенов

```bash
./token-collector --count 750 --out tokens.sqlite
./token-collector --headless=false      # видимый браузер
```

Собирает до 1250 device-токенов через headless Chrome (`chromedp`). Токены расходуются решателем капчи и удаляются после использования.

---

## Структура проекта

```
├── main.go              # HTTP-сервер, конфиг, OpenAI-роуты, .env loader
├── zai.go               # Сессия Z.AI, HMAC-SHA256 подпись, SSE-стриминг
├── captcha.go           # Aliyun Captcha V3 (in-process)
├── tokens.go            # SQLite-хранилище device-токенов
├── tools.go             # Адаптер tool calling (unit test framing)
├── dashboard.go         # Встроенный HTML-дашборд
├── token-collector/     # Сборщик device-токенов (chromedp → Chrome)
├── .env.example         # Шаблон конфигурации
├── go.mod / go.sum
└── README.md
```

---

## Конфигурация

Настройки в файле `.env` (загружается автоматически) или переменных окружения:

| Переменная | По умолчанию | Описание |
|---|---|---|
| `ZAI_TOKEN` | *(пусто)* | JWT Z.AI; без него — guest-режим (только `glm-4.7`) |
| `PORT` | `5082` | Порт сервера |
| `HOST` | `0.0.0.0` | Адрес привязки |
| `AUTH_TOKEN` | `d3vin` | Bearer-токен для защищённых эндпоинтов |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `DB_PATH` | `tokens.sqlite` | База device-токенов |

**Получение `ZAI_TOKEN`:**
1. Откройте https://chat.z.ai и войдите в аккаунт
2. DevTools (F12) → Application → Local Storage → `token`
3. Скопируйте значение (начинается с `eyJ...`)
4. Добавьте в `.env`: `ZAI_TOKEN=eyJ...`

Опциональные флаги CLI (переопределяют env / значения по умолчанию):

- `--db-path <path>` — переопределить `DB_PATH` (по умолчанию `tokens.sqlite`)
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
| `Model not available for current user level` | Guest-аккаунт + GLM-5 модель | Задайте `ZAI_TOKEN` реального аккаунта |

---

## Контакты

- **GitHub**: https://github.com/D3-vin/GLM-ZAI-2API
- **Telegram**: [@D3_vin](https://t.me/D3_vin)
- **Автор**: [@D3vin_dev](https://t.me/D3vin_dev)

---

## Лицензия

Предоставляется «как есть» в образовательных целях и для целей совместимости. Используйте ответственно и в соответствии с условиями сервиса Z.AI.
