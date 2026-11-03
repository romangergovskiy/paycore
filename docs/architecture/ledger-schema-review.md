# Ревью окончательной SQL-схемы Ledger

Статус: **ревью этапа 3, после слияния 3.2**. Это не миграции и не реализация Ledger: финансовых таблиц, функции `post_journal` и проводок в этом PR нет. Цель — зафиксировать, готова ли схема из актуальной документации в `main` к следующей реализации, и какие решения ещё нужны.

База: [database-schema.md](../database/database-schema.md), [data-integrity.md](../database/data-integrity.md), [concurrency.md](../database/concurrency.md), [er-diagram.md](../database/er-diagram.md), [ledger-model.md](ledger-model.md), [chart-of-accounts.md](chart-of-accounts.md), [money-flows.md](money-flows.md), [decisions.md](decisions.md), [open-questions.md](open-questions.md). Код 3.2: `migrations/00001_platform_schema.sql`, `internal/platform/postgres/bootstrap.go`.

---

## 1. Вердикт

**Схема таблиц Ledger готова к реализации.** Модель счетов, журналов, проводок, проекций, CHECK/FK/UNIQUE и порядок блокировок L1…L8 согласованы между документами. Следующий этап реализации может создавать схемы `accounts` / `ledger` (и связанные справочники) по этим спецификациям.

Не начинать реализацию в этом PR. Не копировать blanket-гранты 3.2 на финансовые схемы.

Что ещё не является блокером DDL, но должно быть в плане миграций:

| Тема | Статус после этого ревью |
|---|---|
| Индекс оборотов системного счёта по `ledger_entries.posted_at` | **Исправлено:** колонки нет; индекс по `(ledger_account_id, created_at, id)`, период — JOIN к `journal_transactions.posted_at` |
| `REVERSAL` на `BLOCKED` требовал `reference_type = 'ADJUSTMENT'` | **Исправлено:** `reference_type` остаётся исходным; связь с заявкой — параметр `adjustment_id` |
| Гранты `paycore_app` на `platform.schema_info` и `DEFAULT PRIVILEGES` | **Расхождение с 3.2.** План — §6. Саму миграцию `00001` здесь не меняем |
| Q-12, Q-13(а), Q-16, Q-20, Q-30 | Не блокируют DDL таблиц Ledger; Q-13(а) — формат номера можно добавить CHECK позже |
| Функция `post_journal`, бизнес-модули | Следующий этап, не этот PR |

---

## 2. Расхождения с кодом этапа 3.2

В коде есть только техническая схема `platform` и четыре роли. Финансовых схем нет — это соответствует задаче 3.2.

| Документы (целевая модель) | Код 3.2 | Следствие |
|---|---|---|
| `paycore_app` имеет `DELETE` только на `outbox_events`, `idempotency_keys`, `inbox_messages`; `schema_info` в целевой модели приложению **не нужна для записи** | `GRANT SELECT, INSERT, UPDATE ON platform.schema_info TO paycore_app`. Тест `postgres_test.go` **ожидает**, что приложение может вставить строку | Слишком широкие права на техническую таблицу. Не чинить в этом PR |
| Гранты **по таблице**, без blanket | `ALTER DEFAULT PRIVILEGES FOR ROLE paycore_owner IN SCHEMA platform GRANT SELECT, INSERT, UPDATE ON TABLES TO paycore_app` | Любая новая таблица в `platform` автоматически получит `INSERT, UPDATE` у приложения, в том числе если позже появится что-то кроме outbox/idempotency/inbox. **Не переносить этот шаблон** в `ledger`, `accounts` и остальные схемы |
| `GRANT USAGE` на каждую модульную схему | Есть только `GRANT USAGE ON SCHEMA platform` | Будущие миграции обязаны явно выдать `USAGE` на `accounts`, `ledger`, `payments`, … ролям `paycore_app`, `paycore_readonly` и (для `ledger`/`accounts`) `paycore_ledger` |
| Goose-таблица версий | `goose_db_version` в `public` после `REVOKE ALL ON SCHEMA public FROM PUBLIC` | Ожидаемо: таблицей владеет `paycore_owner`, `paycore_app` её не читает (тест это проверяет) |
| L1 — `platform.idempotency_keys` | Таблицы нет, есть только `schema_info` | `schema_info` **не входит** в L1…L8. Не блокировать её в денежных транзакциях |
| Роли D-33 | `migrate bootstrap` создаёт четыре роли; `paycore_ledger` — `NOLOGIN`; у `paycore_app` — `idle_in_transaction_session_timeout = 30s` и `default_transaction_isolation = read committed` | Совпадает с документами. Смена пароля при каждом bootstrap и кеш `/readyz` — заметки §8, не этот PR |

---

## 3. Модель счетов

Схема `accounts` + `ledger.ledger_accounts` + справочник `chart_of_accounts` согласованы.

- Бизнес-счёт: ровно один владелец (`user_id` XOR `merchant_id`), статус `ACTIVE` / `BLOCKED` / `CLOSED`, версия при смене статуса, `UNIQUE (id, currency)` для составных FK.
- Два ledger-счёта на бизнес-счёт (`AVAILABLE` / `HOLD`). Статус пользовательского ledger-счёта не дублируется: `CHECK (owner_type = 'SYSTEM' OR status = 'ACTIVE')`; `post_journal` читает `accounts.accounts`.
- Системные счета MVP: `1100:RUB`, `4100:RUB`, `4200:RUB`, `5900:RUB`. Счёт **2210** создаётся в паре с 2200 и в MVP не используется (остаток 0) — это намеренно, не дыра в схеме.
- `allow_negative = false` для всех счетов MVP; для 2900 после MVP CHECK на бакетах снимается — колонка `allow_negative` на бакетах это допускает.
- Валюта: колонки и составные FK мультивалютные, сид только RUB (D-30). Схема от Q-30 не зависит.

`account_number TEXT UNIQUE` можно создать без формата. CHECK формата — когда закроют [Q-13(а)](open-questions.md#q-13).

---

## 4. Журналы, проводки, резервы

- Журнал append-only, `status = 'POSTED'` (D-06). Операции и `reference_type` закрыты CHECK. `REVERSAL` хранит исходный `reference_type` (не `ADJUSTMENT`) — это согласовано с §6 [ledger-model.md](ledger-model.md#6-перечень-операций-operation) после правки §11.2.
- Идемпотентность: `UNIQUE (reference_type, reference_id, operation, operation_seq)` плюс `UNIQUE (reverses_journal_id)` (в PostgreSQL несколько `NULL` допустимы).
- Проводка: `amount > 0`, составной FK валюты, не больше одной проводки на ledger-счёт в журнале.
- `account_version` / `balance_after` только у пользовательских счетов. Ограничение уникальности версии — **частичный** индекс `WHERE account_version IS NOT NULL`: у системных проводок оба поля `NULL`, и обычный `UNIQUE (ledger_account_id, account_version)` в PostgreSQL тоже позволил бы несколько `NULL`, но частичный индекс явно фиксирует намерение.
- `holds` меняет только `post_journal`. `payments.hold_id` без FK — сознательная межмодульная ссылка; симметрично `holds.reference_id` → платежу ([er-diagram.md §7](../database/er-diagram.md#7-межмодульные-связи)).
- `manual_adjustments.approved_by <> created_by`, `POSTED` ⇔ `journal_transaction_id`.

Контракт `post_journal` для `BLOCKED`: и `ADJUSTMENT`, и `REVERSAL` требуют утверждённую заявку. Для `ADJUSTMENT` заявка — это `reference_id`. Для `REVERSAL` исходный `reference_id` нельзя подменить, поэтому в сигнатуру добавлен необязательный `adjustment_id` ([ledger-model.md §11.2](ledger-model.md#112-sql-ledgerpost_journal--security-definer)).

---

## 5. Балансы, целостность, блокировки

Проекции:

- `account_balances` — одна строка на бизнес-счёт, `CHECK >= 0`, блокировка L5 через `lock_account_balances` (у `paycore_app` нет `UPDATE` на этой таблице).
- `system_balance_buckets` — 16 бакетов у 1100/4100/4200, 1 у 5900; увеличение пишет в `hash(journal_id) % n`, уменьшение блокирует все бакеты счёта (L6).
- Снапшоты `ledger_balance_snapshots` — производные, на инварианты в момент записи не влияют.

Целостность (уровни 1–7 в [data-integrity.md](../database/data-integrity.md)) достаточна для DDL:

- CHECK именованные (`ck_<таблица>_<правило>`).
- FK без каскадов; составные FK по валюте обязательны.
- Триггеры неизменяемости на журналах, проводках, историях статусов, аудите; отложенный триггер баланса на `journal_transactions`.
- `SET search_path = pg_catalog, ledger` у функций: объекты вне `ledger` (в том числе `accounts.accounts`, `accounts.currencies`) нужно **квалифицировать схемой**. Иначе поиск остановится на `pg_catalog` + `ledger`.

Порядок L1…L8 совместим с уже существующей `platform`:

- L1 появится вместе с `idempotency_keys`, не со `schema_info`.
- L3 `FOR SHARE` / `FOR NO KEY UPDATE` на `accounts.accounts`; внутри функции — повторный `FOR SHARE NOWAIT` (PC460, без ожидания «вниз»).
- Колоночная привилегия `UPDATE (updated_at)` у `paycore_ledger` на `accounts.accounts` нужна PostgreSQL для любой row-lock-клаузы; функция строку не обновляет.
- L8 после L2–L7; бизнес-объекты вставляются после `post_journal`; id генерирует Go.

Нужно в миграции тарифов: `CREATE EXTENSION IF NOT EXISTS btree_gist` (EXCLUDE периодов). `paycore_owner` может не иметь права на `CREATE EXTENSION` — расширение ставит bootstrap/суперпользователь, не Goose-миграция финансовых таблиц.

---

## 6. План грантов будущих таблиц

Правило: **каждая таблица — свой `GRANT`, без `ALTER DEFAULT PRIVILEGES … ON TABLES`.** Схему `platform` в 3.2 не трогаем; при создании следующих объектов `DEFAULT PRIVILEGES` из `00001` всё ещё сработают на новые таблицы в `platform` — это надо явно перекрыть `GRANT`/`REVOKE` в той миграции, которая создаст `idempotency_keys` / outbox / inbox (не этот PR).

### 6.1. Схема `platform` (целевое состояние)

| Таблица | `paycore_app` | `paycore_readonly` | `paycore_ledger` |
|---|---|---|---|
| `schema_info` | `SELECT` | `SELECT` | — |
| `idempotency_keys` | `SELECT, INSERT, UPDATE, DELETE` | `SELECT` | — |
| `outbox_events` | `SELECT, INSERT, UPDATE, DELETE` | `SELECT` | — |
| `inbox_messages` | `SELECT, INSERT, UPDATE, DELETE` | `SELECT` | — |

Сейчас у приложения ещё `INSERT, UPDATE` на `schema_info`. Сузить — отдельная задача, вместе с тестом, который сегодня требует вставку.

### 6.2. Схемы `ledger` и `accounts`

| Объект | `paycore_app` | `paycore_ledger` | `paycore_readonly` |
|---|---|---|---|
| `USAGE` на схему | да | да | да |
| `journal_transactions`, `ledger_entries` | `SELECT` | `SELECT, INSERT` | `SELECT` |
| `account_balances`, `system_balance_buckets`, `holds` | `SELECT` | `SELECT, INSERT, UPDATE` | `SELECT` |
| `ledger_accounts`, `chart_of_accounts`, `ledger_balance_snapshots` | `SELECT` | `SELECT` | `SELECT` |
| `manual_adjustments` | `SELECT, INSERT, UPDATE` | `SELECT` | `SELECT` |
| функции `ledger.*` | `EXECUTE` | владелец (`SECURITY DEFINER`) | — |
| `accounts.accounts` | `SELECT, INSERT, UPDATE` | `SELECT` + `UPDATE (updated_at)` | `SELECT` |
| `accounts.currencies` | `SELECT` | `SELECT` | `SELECT` |
| `account_status_history` | `SELECT, INSERT` | — | `SELECT` |

Нет `DELETE`/`TRUNCATE` на финансовых таблицах ни у `paycore_app`, ни у `paycore_ledger`. Нет `INSERT` у приложения на журналах, проводках, проекциях и `holds`.

### 6.3. Прочие бизнес-схемы (когда появятся)

`payments`, `transfers`, `refunds`, `payouts`, `test_topups`, `limit_counters`, `commission_tariffs`, …: `SELECT, INSERT, UPDATE` приложению; `SELECT, INSERT` на `*_status_history` и `audit.audit_logs`; `SELECT` readonly. `DELETE` не выдавать, кроме трёх platform-таблиц очистки.

`REVOKE EXECUTE ON FUNCTION … FROM PUBLIC` для каждой ledger-функции.

---

## 7. Открытые вопросы (на схему)

Ни один не блокирует создание таблиц Ledger.

| Вопрос | Влияние на схему |
|---|---|
| [Q-12](open-questions.md#q-12) значения лимитов | Только сид `limit_rules`, не DDL |
| [Q-13(а)](open-questions.md#q-13) формат `account_number` | Колонка `TEXT UNIQUE` достаточна; CHECK формата — позже |
| [Q-13(б)](open-questions.md#q-13) возврат на `CLOSED` | Поведение use case; `REFUND` на `CLOSED` уже запрещён матрицей |
| [Q-16](open-questions.md#q-16) отмена перевода | В MVP колонок `hold_id` / `ON_HOLD` у перевода нет |
| [Q-20](open-questions.md#q-20) хеш аудита | Колонки можно заложить `NULL`; расчёт не нужен |
| [Q-30](open-questions.md#q-30) только RUB | Схема мультивалютная, сид RUB; не меняется |

Не относится к схеме, но зафиксировано как заметки (не чинить здесь): bootstrap каждый раз делает `ALTER ROLE … PASSWORD`; `/readyz` на каждый запрос ходит в БД без кеша. HTTP timeout хендлеров и опциональный JSON `details` — задачи 3.1, не схема.

---

## 8. Заметки, которые не входят в этот PR

- **Bootstrap и пароли.** `ensureLoginRole` при каждом запуске выставляет пароль из DSN, в том числе существующей роли. Это удобно для локальной среды и опасно как «тихая ротация» в production. Документировать/менять поведение — отдельно.
- **`/readyz`.** Проверка БД без кеша, таймаут 2 с. При частых пробах оркестратора лишняя нагрузка на пул. Кеш — отдельно.
- **Сужение грантов `schema_info`.** Нужно менять `00001` или follow-up миграцию **и** тест, который делает `INSERT` от `paycore_app`. Не смешивать с ревью схемы Ledger.
- **HTTP timeout / JSON `details`.** Не схема.

---

## 9. Что исправлено в документах этим PR

1. Индекс оборотов: `posted_at` на `ledger_entries` заменён на `created_at`; запросы за период идут через журнал.
2. Контракт `post_journal`: `REVERSAL` не требует `reference_type = 'ADJUSTMENT'`; добавлен `adjustment_id`.
3. UNIQUE версии проводок описан как частичный индекс.
4. `payments.hold_id` явно указан в списке ссылок без FK.
5. Статус архитектуры: в коде есть миграция `platform` (3.2), финансовых модулей ещё нет.
6. Зафиксирован запрет blanket `DEFAULT PRIVILEGES` на будущие финансовые схемы.

Реализацию Ledger (таблицы, функции, проводки) этот документ не начинает.
