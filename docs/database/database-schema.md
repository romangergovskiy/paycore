# Схема базы данных PostgreSQL

Статус: **принято для MVP** (этап 2). SQL-фрагменты иллюстративны. В коде миграции `00002_accounts.sql` и `00003_ledger.sql` создают таблицы `accounts` и `ledger` (этап 3.3); функций `post_journal` нет. Down у финансовых миграций нужен тестам apply/rollback; [D-38](../architecture/decisions.md#d-38) по-прежнему означает, что в production финансовые таблицы не откатывают. Ревью схемы — [../architecture/ledger-schema-review.md](../architecture/ledger-schema-review.md).

Связанные документы: [er-diagram.md](er-diagram.md), [data-integrity.md](data-integrity.md) (CHECK, FK, индексы, права), [concurrency.md](concurrency.md), [../architecture/ledger-model.md](../architecture/ledger-model.md), [../architecture/decisions.md](../architecture/decisions.md).

## 1. Общие соглашения

| Тема | Решение |
|---|---|
| PostgreSQL | 16+ ([D-34](../architecture/decisions.md#d-34)) |
| Доступ | `pgx/v5`, явный SQL, без ORM (ТЗ §4). Запись в ledger — только через функции `ledger.*` ([D-03](../architecture/decisions.md#d-03)) |
| Миграции | Goose, SQL-файлы ([D-38](../architecture/decisions.md#d-38)); для финансовых таблиц только forward |
| Схемы | По модулям ([D-18](../architecture/decisions.md#d-18)): `auth`, `merchants`, `accounts`, `ledger`, `payments`, `transfers`, `commissions`, `limits`, `simulator`, `notifications`, `audit`, `platform`. После MVP: `fraud`, `reconciliation` |
| PK | `UUID`, значения UUIDv7 из Go ([D-14](../architecture/decisions.md#d-14)) |
| Деньги | `BIGINT` в минорных единицах, `CHECK` на диапазон |
| Валюта | `CHAR(3)`, FK на `accounts.currencies`. MVP: только RUB ([D-30](../architecture/decisions.md#d-30)); колонки и составные FK остаются, чтобы добавить валюту без изменения схемы |
| Перечисления | `TEXT + CHECK` ([D-13](../architecture/decisions.md#d-13)) |
| Время | `TIMESTAMPTZ`, UTC ([D-29](../architecture/decisions.md#d-29)) |
| Удаление | Финансовые и аудиторские данные не удаляются; FK без каскадов |
| `updated_at` | Только у изменяемых таблиц |

`NN` — `NOT NULL`. Подробные CHECK — [data-integrity.md §1](data-integrity.md#1-check-ограничения-финансовые-таблицы).

## 2. Схема `accounts`

### `currencies`

`code CHAR(3) PK` (`^[A-Z]{3}$`), `minor_units SMALLINT NN` (0…4), `name TEXT NN`, `is_active BOOLEAN NN`. Сид: только RUB (`minor_units = 2`, `is_active = true`). Другие валюты добавляются в следующих фазах строкой справочника и системными счетами в этой валюте; до тех пор API отклоняет их кодом `CURRENCY_NOT_SUPPORTED` ([D-30](../architecture/decisions.md#d-30)).

### `accounts` — бизнес-счёт

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK; `UNIQUE (id, currency)` для составных FK |
| account_number | TEXT NN | UNIQUE; формат — [Q-13](../architecture/open-questions.md#q-13) |
| user_id | UUID | FK → `auth.users` |
| merchant_id | UUID | FK → `merchants.merchants` |
| account_purpose | TEXT NN | `MAIN`, `SAVINGS`, `MERCHANT_SETTLEMENT` |
| currency | CHAR(3) NN | FK → currencies |
| status | TEXT NN | `ACTIVE`, `BLOCKED`, `CLOSED` ([D-26](../architecture/decisions.md#d-26), [D-28](../architecture/decisions.md#d-28)) |
| name | TEXT | |
| version | BIGINT NN | инкремент при смене статуса |
| created_at, updated_at, closed_at | TIMESTAMPTZ | `closed_at` ⇔ `CLOSED` |

`CHECK (num_nonnulls(user_id, merchant_id) = 1)` ([D-11](../architecture/decisions.md#d-11)); `merchant_id IS NOT NULL` ⇔ `MERCHANT_SETTLEMENT`. Магазин имеет один счёт на валюту: `UNIQUE (merchant_id, currency) WHERE merchant_id IS NOT NULL`.

### `account_status_history` — append-only

`id, account_id FK, from_status, to_status, reason TEXT NN, changed_by UUID NN, changed_at`. Пишется в той же транзакции, что и смена статуса (ТЗ §2.2).

### `account_restrictions` — после MVP

Частичные ограничения (DEBIT/CREDIT). В MVP «ограничение операций» реализуется статусом `BLOCKED` ([D-26](../architecture/decisions.md#d-26)).

## 3. Схема `ledger`

Писать в таблицы этой схемы может только роль `paycore_ledger` через функции ([data-integrity.md §4](data-integrity.md#4-неизменяемость-ledger)).

### `chart_of_accounts` — справочник

`code TEXT PK` (`1100`, `2100`, …), `name`, `account_class`, `normal_balance`, `allow_negative`, `is_contra`, `in_mvp`. Содержимое — [chart-of-accounts.md](../architecture/chart-of-accounts.md).

### `ledger_accounts`

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK; `UNIQUE (id, currency)` |
| owner_type | TEXT NN | `CUSTOMER`, `MERCHANT`, `SYSTEM` |
| account_id | UUID | FK `(account_id, currency)` → `accounts.accounts(id, currency)`; NULL для SYSTEM |
| account_type | TEXT NN | FK → `chart_of_accounts(code)` |
| kind | TEXT | `AVAILABLE`, `HOLD`; NULL для SYSTEM |
| system_code | TEXT | `SIM_FUNDING_SOURCE`, `FEE_INCOME_TRANSFERS`, `FEE_INCOME_PAYMENTS`, `OPERATIONAL_LOSSES`; NULL для пользовательских |
| currency | CHAR(3) NN | |
| account_class, normal_balance | TEXT NN | согласованы CHECK |
| allow_negative | BOOLEAN NN | `false` для всех счетов MVP |
| bucket_count | SMALLINT NN | 1 для пользовательских; 16 для 1100, 4100, 4200; 1 для 5900 |
| status | TEXT NN | `ACTIVE`, `CLOSED`. Имеет смысл только для системных счетов: `CHECK (owner_type = 'SYSTEM' OR status = 'ACTIVE')`. Статус пользовательских ledger-счетов не дублируется — `post_journal` читает статус бизнес-счёта из `accounts.accounts` ([ledger-model.md §11.4](../architecture/ledger-model.md#114-статусы-счетов-и-допустимые-операции)) |
| created_at | TIMESTAMPTZ NN | |

### `account_balances` — проекция бизнес-счёта

| Колонка | Тип | Ограничения |
|---|---|---|
| account_id | UUID | PK; FK `(account_id, currency)` → `accounts.accounts` |
| currency | CHAR(3) NN | |
| available_amount | BIGINT NN | DEFAULT 0, `>= 0` |
| reserved_amount | BIGINT NN | DEFAULT 0, `>= 0` |
| version | BIGINT NN | DEFAULT 0 |
| updated_at | TIMESTAMPTZ NN | |

Таблица остаётся в схеме `ledger`, хотя в ТЗ §4.3 она описана рядом со счетами: изменяет её только ledger.

### `system_balance_buckets` — проекция системных счетов ([D-02](../architecture/decisions.md#d-02))

| Колонка | Тип | Ограничения |
|---|---|---|
| ledger_account_id | UUID | PK (часть), FK → ledger_accounts (только `owner_type = 'SYSTEM'`) |
| bucket_no | SMALLINT | PK (часть), `0 <= bucket_no < bucket_count` |
| balance | BIGINT NN | DEFAULT 0; `CHECK (allow_negative OR balance >= 0)` |
| allow_negative | BOOLEAN NN | копия из ledger_accounts |
| updated_at | TIMESTAMPTZ NN | |

Бакеты создаются вместе с системным счётом. Баланс = `SUM(balance)`.

### `journal_transactions` — append-only

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK |
| operation | TEXT NN | `SIM_TOPUP`, `TRANSFER`, `HOLD_PLACE`, `HOLD_CAPTURE`, `HOLD_RELEASE`, `REFUND`, `PAYOUT`, `REVERSAL`, `ADJUSTMENT` |
| reference_type | TEXT NN | `SIM_TOPUP`, `TRANSFER`, `HOLD`, `REFUND`, `PAYOUT`, `ADJUSTMENT` |
| reference_id | UUID NN | |
| operation_seq | SMALLINT NN | DEFAULT 1 |
| reverses_journal_id | UUID | FK → journal_transactions, UNIQUE |
| status | TEXT NN | `= 'POSTED'` ([D-06](../architecture/decisions.md#d-06)) |
| description | TEXT | |
| initiated_by | UUID | |
| accounting_date | DATE NN | UTC |
| posted_at | TIMESTAMPTZ NN | |

`UNIQUE (reference_type, reference_id, operation, operation_seq)`.

### `ledger_entries` — append-only

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK |
| journal_transaction_id | UUID NN | FK |
| ledger_account_id | UUID NN | FK `(ledger_account_id, currency)` → ledger_accounts |
| entry_type | TEXT NN | `DEBIT`, `CREDIT` |
| amount | BIGINT NN | `> 0 AND <= 10^14` |
| currency | CHAR(3) NN | |
| account_version | BIGINT | только пользовательские счета |
| balance_after | BIGINT | только пользовательские счета |
| created_at | TIMESTAMPTZ NN | |

`UNIQUE (journal_transaction_id, ledger_account_id)`; частичный `UNIQUE (ledger_account_id, account_version) WHERE account_version IS NOT NULL` (системные проводки с `NULL` не входят).

### `ledger_balance_snapshots`

PK `(ledger_account_id, snapshot_date)`; `debit_total`, `credit_total`, `balance`, `computed_at`. Производные данные для отчётов, пересчитываемые. На проверку остатков не влияют.

### `holds` ([D-37](../architecture/decisions.md#d-37))

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK |
| account_id | UUID NN | FK `(account_id, currency)` |
| currency | CHAR(3) NN | |
| amount | BIGINT NN | `> 0` |
| status | TEXT NN | `ACTIVE`, `CAPTURED`, `RELEASED`, `EXPIRED` |
| reference_type | TEXT NN | `PAYMENT` (MVP); `TRANSFER` после MVP |
| reference_id | UUID NN | `UNIQUE (reference_type, reference_id)` |
| limit_period_start | DATE | период лимита, использованного при резерве ([D-24](../architecture/decisions.md#d-24)) |
| expires_at | TIMESTAMPTZ NN | |
| created_at, finalized_at | TIMESTAMPTZ | `finalized_at` ⇔ не ACTIVE |

### `manual_adjustments`

`id, status (DRAFT, PENDING_APPROVAL, APPROVED, REJECTED, POSTED), proposed_entries JSONB NN, reason TEXT NN, incident_ref TEXT NN, created_by NN, approved_by, journal_transaction_id FK, created_at, decided_at`. `approved_by <> created_by` ([D-31](../architecture/decisions.md#d-31)).

## 4. Схема `payments`

### `payments`

| Колонка | Тип | Примечание |
|---|---|---|
| id | UUID | PK; `UNIQUE (id, currency)` |
| merchant_id | UUID NN | FK |
| store_id | UUID | FK → `merchants.stores` |
| merchant_account_id | UUID NN | FK `(…, currency)` |
| customer_user_id | UUID | заполняется при подтверждении checkout |
| customer_account_id | UUID | FK `(…, currency)`; заполняется при подтверждении |
| amount | BIGINT NN | `> 0` |
| currency | CHAR(3) NN | |
| fee_amount | BIGINT NN | DEFAULT 0; рассчитывается при capture; `CHECK (fee_amount < amount)` — capture с `fee ≥ amount` отклоняется `FEE_EXCEEDS_PAYMENT` ([D-27](../architecture/decisions.md#d-27)) |
| fee_tariff_id | UUID | FK; при capture |
| captured_amount, refunded_amount | BIGINT NN | DEFAULT 0 |
| status | TEXT NN | `CREATED`, `AUTHORIZED`, `CAPTURED`, `PARTIALLY_REFUNDED`, `REFUNDED`, `FAILED`, `CANCELLED` ([D-21](../architecture/decisions.md#d-21)) |
| failure_code, cancel_reason | TEXT | `cancel_reason`: `MERCHANT_CANCELLED`, `CHECKOUT_EXPIRED`, `VOIDED`, `AUTH_EXPIRED` |
| hold_id | UUID | |
| idempotency_key | TEXT NN | `UNIQUE (merchant_id, idempotency_key)` |
| description | TEXT | |
| version | BIGINT NN | |
| authorized_at, captured_at, created_at, updated_at | TIMESTAMPTZ | |

### `checkout_sessions` ([D-22](../architecture/decisions.md#d-22))

| Колонка | Тип | Примечание |
|---|---|---|
| id | UUID | PK |
| payment_id | UUID NN | FK, UNIQUE |
| token_hash | BYTEA NN | UNIQUE; хранится только хеш токена |
| status | TEXT NN | `ACTIVE`, `CONFIRMED`, `FAILED`, `CANCELLED`, `EXPIRED` |
| customer_user_id | UUID | кто подтвердил |
| expires_at | TIMESTAMPTZ NN | `created_at + 30 мин` ([D-32](../architecture/decisions.md#d-32)) |
| created_at, confirmed_at | TIMESTAMPTZ | |

### `payment_attempts`

`id, payment_id FK, attempt_number (UNIQUE с payment_id), operation (CONFIRM, CAPTURE, VOID, REFUND), status (SUCCEEDED, FAILED), error_code, actor_id, started_at, finished_at`. Коды `error_code` для `CAPTURE`: `FEE_EXCEEDS_PAYMENT` ([D-27](../architecture/decisions.md#d-27)), `ACCOUNT_NOT_ACTIVE`, `AUTH_EXPIRED`, `INVALID_STATE`. В MVP это история попыток обработки (ТЗ §2.4). `provider_reference` и статусы `STARTED`/`UNKNOWN` добавляются вместе с внешним провайдером.

### `payment_status_history` — append-only

### `refunds`

`id, payment_id FK (составной с currency), amount > 0, currency, status (SUCCEEDED, FAILED), failure_code (REFUND_EXCEEDS_CAPTURED, MERCHANT_INSUFFICIENT_FUNDS, INVALID_STATE, ACCOUNT_NOT_ACTIVE), reason, idempotency_key (UNIQUE с payment_id), initiated_by, created_at`. Поле `fee_refund_amount` этапа 1 убрано из MVP, потому что комиссия не возвращается ([D-09](../architecture/decisions.md#d-09)). Оно вернётся вместе со счётом 4900, если политика изменится.

## 5. Схема `transfers`

### `transfers`

`id, initiated_by FK, sender_account_id, receiver_account_id (оба FK (…, currency)), amount > 0, currency, fee_amount >= 0, fee_tariff_id, status (COMPLETED, FAILED), failure_code, idempotency_key (UNIQUE с initiated_by), description, created_at, completed_at`. `hold_id` и статусы `ON_HOLD`/`CANCELLED` — после MVP ([D-05](../architecture/decisions.md#d-05)).

### `transfer_status_history` — append-only. В MVP по одной строке на перевод (создание в финальном статусе).

## 6. Схема `merchants` (выплаты)

### `payouts` ([D-25](../architecture/decisions.md#d-25))

`id, merchant_id FK, account_id FK (…, currency), amount > 0, currency, status (COMPLETED, FAILED), failure_code, idempotency_key (UNIQUE с merchant_id), requested_by, created_at`. Коды `failure_code`: `INSUFFICIENT_FUNDS` (остаток магазина), `ACCOUNT_NOT_ACTIVE`, `PAYOUT_SOURCE_INSUFFICIENT`. `PAYOUT_SOURCE_INSUFFICIENT` — нехватка бакетов 1100; отказ фиксируется через `SAVEPOINT` ([concurrency.md §5.6](concurrency.md#56-тестовое-пополнение-и-выплата), [chart-of-accounts.md §3](../architecture/chart-of-accounts.md#3-системные-счета-остаток-контроль-проекция)).

## 7. Схема `simulator`

### `test_topups` ([D-15](../architecture/decisions.md#d-15))

`id, user_id FK, account_id FK (…, currency), amount > 0, currency, status (COMPLETED, FAILED), failure_code, idempotency_key (UNIQUE с user_id), initiated_by (клиент или ADMIN), created_at`. Только счета клиентов (`MAIN`/`SAVINGS`).

## 8. Схема `limits` ([D-24](../architecture/decisions.md#d-24))

### `limit_rules`

`id, limit_type (MAX_SINGLE_OPERATION, DAILY_OUTGOING, DAILY_SIM_TOPUP), currency, scope (GLOBAL — в MVP), amount BIGINT > 0, valid_from, valid_to, created_by, approved_by`. Непересечение периодов — EXCLUDE, как у тарифов. Изменение пишется в аудит.

### `limit_counters`

| Колонка | Тип | Примечание |
|---|---|---|
| user_id | UUID | PK (часть) |
| currency | CHAR(3) | PK (часть) |
| limit_type | TEXT | PK (часть): `DAILY_OUTGOING`, `DAILY_SIM_TOPUP` |
| period_start | DATE | PK (часть), сутки UTC |
| used_amount | BIGINT NN | `>= 0` |
| updated_at | TIMESTAMPTZ NN | |

Строка создаётся `INSERT … ON CONFLICT DO NOTHING` в первой операции периода. Старые периоды удаляются по сроку хранения.

## 9. Схема `commissions`

### `commission_tariffs`

`UNIQUE (tariff_code, version)`; `operation_type (TRANSFER_P2P, TRANSFER_OWN, PAYMENT)`; `merchant_id` (NULL — базовый тариф); `currency`; `rate_bp 0…10000`; `fixed_amount`, `min_amount`, `max_amount`; `rounding_mode` (`HALF_UP`); `valid_from`, `valid_to`; `created_by`, `approved_by`.

Непересечение периодов (`btree_gist`):

```sql
EXCLUDE USING gist (
  operation_type WITH =, currency WITH =,
  coalesce(merchant_id, '00000000-0000-0000-0000-000000000000'::uuid) WITH =,
  tstzrange(valid_from, valid_to, '[)') WITH &&)
```

Начавший действовать тариф неизменяем. Изменение — новая версия и аудит (ТЗ §2.7, §2.12).

## 10. Схема `platform`

### `idempotency_keys` ([concurrency.md §4](concurrency.md#4-идемпотентность-http))

| Колонка | Тип | Примечание |
|---|---|---|
| id | UUID | PK |
| scope | TEXT NN | `user:<id>` / `merchant:<id>` |
| endpoint | TEXT NN | метод + шаблон маршрута |
| key | TEXT NN | 1…255 символов; `UNIQUE (scope, endpoint, key)` |
| request_hash | BYTEA NN | SHA-256 |
| status | TEXT NN | `IN_PROGRESS`, `COMPLETED` |
| response_code | SMALLINT | при `COMPLETED` |
| response_body | JSONB | при `COMPLETED`, ограничение размера |
| resource_type, resource_id | TEXT, UUID | созданный объект |
| created_at, completed_at, expires_at | TIMESTAMPTZ | `expires_at = created_at + 24 ч` |

### `outbox_events` ([concurrency.md §7](concurrency.md#7-transactional-outbox-и-inbox))

| Колонка | Тип | Примечание |
|---|---|---|
| id | UUID | PK = `event_id` для потребителей |
| seq | BIGINT | `GENERATED ALWAYS AS IDENTITY`, UNIQUE; порядок |
| aggregate_type, aggregate_id | TEXT, UUID NN | ключ порядка (и партиции Kafka) |
| aggregate_version | BIGINT NN | версия агрегата после изменения |
| event_type | TEXT NN | `payment.captured`, … |
| event_version | SMALLINT NN | версия схемы payload |
| payload, headers | JSONB | |
| status | TEXT NN | `PENDING`, `PUBLISHED`, `DEAD` |
| attempts | INT NN | DEFAULT 0 |
| next_attempt_at | TIMESTAMPTZ NN | DEFAULT now() |
| processing_until | TIMESTAMPTZ | lease |
| lease_owner | TEXT | id экземпляра relay |
| last_error | TEXT | |
| created_at, published_at | TIMESTAMPTZ | |

### `inbox_messages`

PK `(consumer, message_id)`; `received_at`, `processed_at`.

### `webhook_deliveries` (схема `merchants`)

`id, endpoint_id FK, event_id, attempt, status (PENDING, DELIVERED, DEAD), next_attempt_at, processing_until, lease_owner, response_code, last_error, delivered_at`.

## 11. Схема `audit`

### `audit_logs`

Append-only: права `SELECT, INSERT` и триггер запрета мутаций. `seq BIGINT GENERATED ALWAYS AS IDENTITY`; актор, действие, ресурс, `before_state`, `after_state`, `ip`, `request_id`, `occurred_at`. Хеш-цепочка — [Q-20](../architecture/open-questions.md#q-20). Отделён от технических логов (ТЗ §2.12).

## 12. Прочие схемы

`auth`, `merchants` (кроме выплат), `notifications` описаны на уровне ER ([er-diagram.md](er-diagram.md)). Обязательные ограничения:

- `auth.users`: `UNIQUE (lower(email))`; `password_hash LIKE '$argon2id$%'`.
- `auth.refresh_tokens`: хранится только хеш; ротация через `replaced_by`; повторное использование заменённого токена отзывает сессию.
- `merchants.api_keys`: хранятся хеш и префикс ключа.

Схемы `fraud` и `reconciliation` — после MVP. Их ER из этапа 1 сохранён в [er-diagram.md §5](er-diagram.md#5-после-mvp-fraud-и-сверка).
