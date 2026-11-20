# PayCore

Демонстрационная платёжная инфраструктура с double-entry ledger: Go, PostgreSQL, модульный монолит. Деньги виртуальные, соответствие PCI DSS и банковским требованиям не заявляется.

Статус: **этап B (чтение счетов)**. Identity/Auth HTTP, bootstrap ADMIN, Admin API пользователей, HTTP-идемпотентность и outbox/inbox есть. Клиент читает свои счета (`GET /api/v1/accounts…`) под Bearer + роль CUSTOMER. `POST /api/v1/accounts` нет: формат `account_number` — открытый [Q-13](docs/architecture/open-questions.md#q-13). Payments, Transfers, `cmd/worker` и webhooks ещё нет.

## Документация

- [docs/PROJECT_SPEC.md](docs/PROJECT_SPEC.md) — техническое задание.
- [docs/architecture/](docs/architecture/README.md) — решения, финансовая модель, потоки денег, план тестирования.
- [docs/database/](docs/database/README.md) — схема, целостность, конкурентный доступ.

## Запуск

Нужен Go 1.24+. Для интеграционных тестов — Docker (PostgreSQL 16 в testcontainers).

```sh
cp .env.example .env   # задать PAYCORE_AUTH_JWT_SECRET и PAYCORE_ACCOUNTS_CURSOR_SECRET (≥ 32 байт)
make run               # http://localhost:8080/livez
make check             # gofmt, go vet, staticcheck (если установлен), тесты с -race
```

Первый запуск с БД (роли, затем миграции):

```sh
make migrate-bootstrap
make migrate-up
make migrate-status
```

Откат одной миграции: `make migrate-down`. Для финансовых таблиц down-миграций не будет.

| Путь | Назначение |
|---|---|
| `GET /livez` | Процесс жив; зависимости не проверяет |
| `GET /readyz` | Готов принимать трафик; `503` при остановке или если БД недоступна |
| `POST /api/v1/auth/register\|login\|refresh\|logout` | Identity/Auth |
| `GET /api/v1/auth/me`, `GET /api/v1/auth/sessions` | Self-service |
| `GET /api/v1/accounts` | Список своих счетов (Bearer, CUSTOMER) |
| `GET /api/v1/accounts/{id}` | Свой счёт |
| `GET /api/v1/accounts/{id}/balance` | Баланс (int64 минорные единицы) |
| `GET /api/v1/accounts/{id}/statement` | Выписка (keyset, HMAC-signed opaque `cursor`) |

Остальной Accounts HTTP (`POST /api/v1/accounts`, Block/Unblock/Close, Admin accounts) не в этом этапе. Открытие счёта через API блокируется [Q-13](docs/architecture/open-questions.md#q-13): формат `account_number` не решён, генерация номера не добавлена.

Коды выхода: `0` — штатная остановка (SIGINT/SIGTERM), `1` — ошибка работы, `2` — неверная конфигурация.

## Структура

```text
cmd/api/                  HTTP API
cmd/migrate/              bootstrap ролей и Goose
internal/app/             сборка зависимостей и жизненный цикл
pkg/money/                суммы в минорных единицах (int64 + валюта, без float)
internal/accounts/        домен, use case, Unit of Work и customer HTTP счетов (domain/application без pgx/HTTP)
internal/ledger/          доменный контракт Poster, Validate, Open/Lock (без pgx/HTTP)
internal/platform/
  config/                 PAYCORE_* из окружения
  logging/                log/slog
  apperror/               ошибки приложения и JSON-формат ошибок API
  httpserver/             HTTP-сервер, middleware, health
  postgres/               пул pgx, bootstrap, миграции, ledger, accounts Store и UoW, auth, идемпотентность, outbox, inbox, retry Tx
  authcrypto/             Argon2id, JWT HS256, opaque refresh, UTC clock
  idempotency/            HTTP-ключ (D-07), без pgx
  outbox/                 контракт outbox (D-23), без pgx
  inbox/                  контракт inbox, без pgx
migrations/               SQL Goose (platform, accounts, ledger, функции, auth, idempotency/outbox/inbox)
docs/                     проектная документация
```

Бизнес-модули появятся в `internal/<модуль>/{domain,application,infrastructure,transport}` (ТЗ §3.2) вместе с их функциональностью.
