# PayCore

Демонстрационная платёжная инфраструктура с double-entry ledger: Go, PostgreSQL, модульный монолит. Деньги виртуальные, соответствие PCI DSS и банковским требованиям не заявляется.

Статус: **этап 3.1 — каркас сервиса** (конфигурация, логи, HTTP-сервер, graceful shutdown, формат ошибок). Бизнес-модулей, БД и Ledger в коде ещё нет.

## Документация

- [docs/PROJECT_SPEC.md](docs/PROJECT_SPEC.md) — техническое задание.
- [docs/architecture/](docs/architecture/README.md) — решения, финансовая модель, потоки денег, план тестирования.
- [docs/database/](docs/database/README.md) — схема, целостность, конкурентный доступ.

## Запуск

Нужен Go 1.24+.

```sh
cp .env.example .env   # необязательно: все переменные имеют значения по умолчанию
make run               # http://localhost:8080/livez
make check             # gofmt, go vet, staticcheck (если установлен), тесты с -race
```

| Путь | Назначение |
|---|---|
| `GET /livez` | Процесс жив; зависимости не проверяет |
| `GET /readyz` | Готов принимать трафик; `503` после начала остановки |

Коды выхода: `0` — штатная остановка (SIGINT/SIGTERM), `1` — ошибка работы, `2` — неверная конфигурация.

## Структура

```text
cmd/api/                  точка входа
internal/app/             сборка зависимостей и жизненный цикл
internal/platform/
  config/                 PAYCORE_* из окружения
  logging/                log/slog
  apperror/               ошибки приложения и JSON-формат ошибок API
  httpserver/             HTTP-сервер, middleware, health
docs/                     проектная документация
```

Бизнес-модули появятся в `internal/<модуль>/{domain,application,infrastructure,transport}` (ТЗ §3.2) вместе с их функциональностью.
