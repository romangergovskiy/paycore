# PayCore — архитектура финансового ядра

Статус: **проект принят для MVP; идёт этап 3 (реализация)**. В коде — каркас сервиса ([D-38](decisions.md#d-38), [D-39](decisions.md#d-39)), схема `platform` (этап 3.2), DDL таблиц `accounts`/`ledger` (этап 3.3), шлюзовые функции `ledger.post_journal` / `open_ledger_accounts` / `lock_account_balances` (этап 3.4, [D-40](decisions.md#d-40)), типизированный Go-вызов `post_journal` (этап 3.5), domain-валидация `Command` без БД (этап 3.6), контракт Go Poster ↔ PostgreSQL (этап 3.7), типизированные вызовы `open_ledger_accounts` / `lock_account_balances` (этап 3.8), `pkg/money` (этап 3.9), домен Accounts (этап 3.10), PostgreSQL Store счетов (этап 3.11), Unit of Work открытия счёта (этап 3.12), смена статуса Block/Unblock/Close (этап 3.13), поиск счёта по номеру GetByNumber (этап 3.14), чтение баланса (этап 3.15), выписка Statement (этап 3.16), история статуса при Open (этап 3.17) и keyset-пагинация выписки (этап 3.18). Контракт Identity/Auth — [identity-auth.md](identity-auth.md), [D-41](decisions.md#d-41), [D-42](decisions.md#d-42), [D-43](decisions.md#d-43); DDL — `migrations/00005_auth.sql` (этап 3.20) и `00006` история статуса (этап 3.25); domain и application — `internal/auth` (этап 3.21); PostgreSQL-адаптеры портов auth — `internal/platform/postgres` (этап 3.22); сериализация Grant/Revoke ADMIN — `pg_advisory_xact_lock` (этап 3.23); Lock/Unlock/DisableUser и last-ADMIN lockout (этап 3.24); Argon2id / JWT HS256 / opaque refresh / UTC clock (этап 3.25). HTTP Auth: `POST /api/v1/auth/register|login|refresh|logout` (3.26.1–3.26.5) и PostgreSQL HTTP-тесты testcontainers (3.26.6). Bootstrap ADMIN ещё нет. Payments и Transfers не реализованы. Ревью SQL-схемы Ledger — [ledger-schema-review.md](ledger-schema-review.md). Первоисточник требований — [../PROJECT_SPEC.md](../PROJECT_SPEC.md).

MVP работает с виртуальными деньгами: источник средств — симулятор, реальных карт и внешних провайдеров нет. Соответствие PCI DSS, банковским требованиям и KYC/AML не заявляется ([D-20](decisions.md#d-20)).

## Документы

| Документ | Содержание |
|---|---|
| [decisions.md](decisions.md) | Реестр принятых решений (D-xx): выбранный вариант, альтернативы, обоснование; соответствие закрытых вопросов этапа 1 |
| [open-questions.md](open-questions.md) | Только нерешённые вопросы (Q-xx) |
| [spec-analysis.md](spec-analysis.md) | Анализ ТЗ: противоречия, недостающие требования, риски — со статусом разрешения; противоречия в документах, устранённые на этапе 2 |
| [ledger-model.md](ledger-model.md) | Финансовая модель: double-entry, журналы и проводки, правила дебета и кредита, балансы и бакеты, резервы, комиссии, неизменяемость, контракт `post_journal`, инварианты |
| [chart-of-accounts.md](chart-of-accounts.md) | План счетов; по каждому системному счёту — знак остатка, контроль, проекция, горячая строка |
| [money-flows.md](money-flows.md) | Машины состояний; симулятор; переводы; checkout → авторизация → capture; отмена и истечение; возвраты с числовыми примерами; сквозной сценарий; выплаты; сторно |
| [financial-invariants-testing.md](financial-invariants-testing.md) | План тестирования: unit, property-based, права и функции, идемпотентность, outbox, конкурентные S-01…S-28, fault injection |
| [ledger-schema-review.md](ledger-schema-review.md) | Ревью окончательной SQL-схемы Ledger после этапа 3.2: вердикт, расхождения с кодом, план грантов по таблице |
| [identity-auth.md](identity-auth.md) | Identity/Auth: границы, сущности, токены, Argon2id, RBAC, SQL, application и HTTP-контракты, лимиты, угрозы, тесты |
| [auth-http-integration-plan.md](auth-http-integration-plan.md) | План этапа 3.26: HTTP Identity/Auth (3.26.1–3.26.6 в коде) |

Модель данных — в [../database/README.md](../database/README.md).

## Ключевые решения (кратко)

1. **Ledger — первоисточник.** Журнал из ≥ 2 проводок, в каждой валюте \(\sum D = \sum C\), записи append-only, исправления — только сторно и корректировки (maker-checker) — [D-06](decisions.md#d-06), [D-31](decisions.md#d-31).
2. **Деньги — `BIGINT` в минорных единицах**, в MVP только RUB (модель мультивалютная), округление только в Commission Engine (`HALF_UP`, ставки в bp) — [D-17](decisions.md#d-17), [D-30](decisions.md#d-30).
3. **Бизнес-счёт ↔ два ledger-счёта** (доступно и резерв). Резерв — проводка — [D-01](decisions.md#d-01).
4. **Синхронная проекция у всех счетов:** `account_balances` у клиентов и магазинов, бакеты у системных счетов; CHECK ≥ 0 ловит отрицательный остаток в момент записи — [D-02](decisions.md#d-02).
5. **Единственный путь записи в ledger — функции `SECURITY DEFINER`** и права ролей; триггеры — вторая линия — [D-03](decisions.md#d-03).
6. **Платёж через checkout:** магазин не выбирает счёт покупателя; `PENDING` в MVP не хранится; комиссия удерживается при capture и не возвращается; возврат требует остатка магазина — [D-21](decisions.md#d-21), [D-22](decisions.md#d-22), [D-08](decisions.md#d-08)…[D-10](decisions.md#d-10).
7. **Конкурентность:** READ COMMITTED + единый порядок блокировок L1…L8 — [D-04](decisions.md#d-04). Запись счёта (строка `accounts.accounts` + `open_ledger_accounts`) идёт в одной транзакции PostgreSQL: `application.UnitOfWork` — контракт без pgx; `postgres.NewAccountUoW` делает Begin, собирает Store и один `NewLedger(tx)` как AccountOpener и BalanceLocker на одном `pgx.Tx`, Commit только если callback вернул nil и context жив, иначе Rollback (без отменённого context, чтобы не закрыть соединение). Open — Store+Opener. Block/Unblock — L3 `FOR NO KEY UPDATE`, без L5. Close — L3, затем L5, нулевые `available`/`reserved` как int64, затем L8 статус и `account_status_history`. Use case сам транзакцию не открывает. Адаптеры Store/Ledger по-прежнему не вызывают Begin. L1/audit/outbox у смены статуса ещё нет (как у Open).
8. **Идемпотентность в два уровня:** HTTP-ключ в той же транзакции и бизнес-`UNIQUE` — [D-07](decisions.md#d-07).
9. **Outbox с lease, публикация вне транзакции, inbox у потребителей;** Kafka в MVP не обязательна — [D-23](decisions.md#d-23).
10. **Identity/Auth:** JWT access (15 мин) + opaque refresh с ротацией; регистрация только `CUSTOMER`; `ADMIN` не двигает деньги; rate limit в PostgreSQL, не Redis — [D-41](decisions.md#d-41), [D-42](decisions.md#d-42), [identity-auth.md](identity-auth.md).

## Выписка счёта

`Service.Statement` (этапы 3.16–3.18, без HTTP) отдаёт POSTED-проводки пары AVAILABLE+HOLD. Страницы — keyset `(created_at, id)` по возрастанию, не OFFSET. `StatementCursor` — типизированные `CreatedAt` и `ID`, не SQL-строка и не непрозрачный токен. `Limit` 1…100; адаптер читает Limit+1, ответ содержит не больше Limit строк. `NextCursor` — ключ последней возвращённой строки; если следующей нет (в том числе когда число строк кратно Limit), курсора нет. Пустая история: пустой список без курсора. Keyset не снимок: между запросами в READ COMMITTED могут появиться новые проводки.
