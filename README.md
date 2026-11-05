# PayCore

Демонстрационная платёжная инфраструктура с double-entry ledger: Go, PostgreSQL, модульный монолит. Деньги виртуальные, соответствие PCI DSS и банковским требованиям не заявляется.

Статус: **этап 3.13 — смена статуса счёта (Block / Unblock / Close)**. Payments, Transfers и HTTP money ещё не реализованы.

## Документация

- [docs/PROJECT_SPEC.md](docs/PROJECT_SPEC.md) — техническое задание.
- [docs/architecture/](docs/architecture/README.md) — решения, финансовая модель, потоки денег, план тестирования.
- [docs/database/](docs/database/README.md) — схема, целостность, конкурентный доступ.

## Запуск

Нужен Go 1.24+. Для интеграционных тестов — Docker (PostgreSQL 16 в testcontainers).

```sh
cp .env.example .env   # необязательно: все переменные имеют значения по умолчанию
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

Коды выхода: `0` — штатная остановка (SIGINT/SIGTERM), `1` — ошибка работы, `2` — неверная конфигурация.

## Структура

```text
cmd/api/                  HTTP API
cmd/migrate/              bootstrap ролей и Goose
internal/app/             сборка зависимостей и жизненный цикл
pkg/money/                суммы в минорных единицах (int64 + валюта, без float)
internal/accounts/        домен, use case и Unit of Work счетов (без pgx/HTTP)
internal/ledger/          доменный контракт Poster, Validate, Open/Lock (без pgx/HTTP)
internal/platform/
  config/                 PAYCORE_* из окружения
  logging/                log/slog
  apperror/               ошибки приложения и JSON-формат ошибок API
  httpserver/             HTTP-сервер, middleware, health
  postgres/               пул pgx, bootstrap, миграции, ledger, accounts Store и UoW
migrations/               SQL Goose (platform, accounts, ledger, функции)
docs/                     проектная документация
```

Бизнес-модули появятся в `internal/<модуль>/{domain,application,infrastructure,transport}` (ТЗ §3.2) вместе с их функциональностью.
