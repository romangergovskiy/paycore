# Схема базы данных PostgreSQL

Статус: **проект, не миграции**. SQL-фрагменты в документах `docs/database/` иллюстративны и служат для однозначного описания решений. Настоящие миграции пишутся на этапе реализации.

Связанные документы: [er-diagram.md](er-diagram.md), [data-integrity.md](data-integrity.md), [concurrency.md](concurrency.md), [../architecture/ledger-model.md](../architecture/ledger-model.md).

## 1. Общие соглашения

| Тема | Решение | Ссылка |
|---|---|---|
| Версия PostgreSQL | 16+ (допущение) | [Q-22](../architecture/open-questions.md#q-22) |
| Драйвер | `pgx/v5`, явный SQL, без ORM (ТЗ §4) | |
| Миграции | golang-migrate или Goose (ТЗ §7). Только forward, без `down` для финансовых таблиц в production | [Q-23](../architecture/open-questions.md#q-23) |
| Схемы | Отдельная схема PostgreSQL на модуль | [D-18](../architecture/open-questions.md#d-18) |
| Первичные ключи | `UUID`, значения — UUIDv7, генерируются в приложении | [D-14](../architecture/open-questions.md#d-14) |
| Деньги | `BIGINT` в минорных единицах, `CHECK` на диапазон | [ledger-model.md §3](../architecture/ledger-model.md#3-представление-денег) |
| Валюта | `CHAR(3)`, FK на `accounts.currencies(code)` | |
| Статусы и перечисления | `TEXT` + `CHECK (status IN (...))` | [D-13](../architecture/open-questions.md#d-13) |
| Время | `TIMESTAMPTZ`, хранится в UTC; `DEFAULT now()` | [Q-17](../architecture/open-questions.md#q-17) |
| Email | `TEXT` + уникальный индекс по `lower(email)` (или `citext`) | |
| Удаление | Финансовые и аудиторские данные не удаляются; `ON DELETE RESTRICT` (по умолчанию `NO ACTION`) везде; каскадов нет | [data-integrity.md §2](data-integrity.md#2-внешние-ключи) |
| `updated_at` | Только у изменяемых таблиц. У append-only таблиц его нет | |
| Оптимистичная версия | `version BIGINT` у `account_balances`, `payments` | |

Типы в таблицах ниже: `NN` — `NOT NULL`, иначе допускается NULL.

## 2. Схема `accounts`

### `currencies`

| Колонка | Тип | Ограничения |
|---|---|---|
| code | CHAR(3) | PK, `CHECK (code ~ '^[A-Z]{3}$')` |
| minor_units | SMALLINT NN | `CHECK (minor_units BETWEEN 0 AND 4)` |
| name | TEXT NN | |
| is_active | BOOLEAN NN | DEFAULT true |

### `accounts` — бизнес-счёт

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK |
| account_number | TEXT NN | UNIQUE; формат — [Q-13](../architecture/open-questions.md#q-13) |
| user_id | UUID | FK → `auth.users` |
| merchant_id | UUID | FK → `merchants.merchants` |
| account_purpose | TEXT NN | `IN ('MAIN','SAVINGS','MERCHANT_SETTLEMENT')` |
| currency | CHAR(3) NN | FK → currencies |
| status | TEXT NN | `IN ('ACTIVE','BLOCKED','CLOSED')`, DEFAULT 'ACTIVE' |
| name | TEXT | |
| created_at, updated_at | TIMESTAMPTZ NN | |
| closed_at | TIMESTAMPTZ | `CHECK ((status = 'CLOSED') = (closed_at IS NOT NULL))` |

- Ровно один владелец: `CHECK (num_nonnulls(user_id, merchant_id) = 1)` ([D-11](../architecture/open-questions.md#d-11)).
- `CHECK ((merchant_id IS NOT NULL) = (account_purpose = 'MERCHANT_SETTLEMENT'))`.
- `UNIQUE (id, currency)` — цель для составных FK (согласованность валюты).

### `account_balances` — проекция (ТЗ §4.3)

| Колонка | Тип | Ограничения |
|---|---|---|
| account_id | UUID | PK; FK `(account_id, currency)` → `accounts(id, currency)` |
| currency | CHAR(3) NN | часть составного FK; дублирует валюту счёта, как в ТЗ, но согласованность гарантирована |
| available_amount | BIGINT NN | DEFAULT 0, `CHECK (available_amount >= 0)` |
| reserved_amount | BIGINT NN | DEFAULT 0, `CHECK (reserved_amount >= 0)` |
| version | BIGINT NN | DEFAULT 0 |
| updated_at | TIMESTAMPTZ NN | |

Строка создаётся в той же транзакции, что и счёт. Изменяется **только** сервисом проведения ([ledger-model.md §11](../architecture/ledger-model.md#11-контракт-сервиса-проведения-концептуально)).

### `account_status_history` — append-only

`id, account_id FK, from_status, to_status, reason TEXT NN, changed_by UUID NN, changed_at`. Каждая смена `accounts.status` пишет строку в той же транзакции (ТЗ §2.2 «История изменений статуса»).

### `account_restrictions` — «ограничение операций» (ТЗ §2.2)

`id, account_id FK, restriction IN ('DEBIT','CREDIT','ALL'), reason, set_by, valid_from, valid_to, lifted_at, lifted_by`. Действующее ограничение: `lifted_at IS NULL AND now() BETWEEN valid_from AND coalesce(valid_to, 'infinity')`. Нужна ли эта таблица или достаточно `status` — [Q-14](../architecture/open-questions.md#q-14).

## 3. Схема `ledger`

### `ledger_accounts`

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK |
| owner_type | TEXT NN | `IN ('CUSTOMER','MERCHANT','SYSTEM')` |
| account_id | UUID | FK `(account_id, currency)` → `accounts(id, currency)` |
| account_type | TEXT NN | код CoA, `IN ('1100','1200',…,'5900')` или FK на справочник `chart_of_accounts` |
| kind | TEXT | `IN ('AVAILABLE','HOLD')` |
| system_code | TEXT | |
| currency | CHAR(3) NN | FK → currencies |
| account_class | TEXT NN | `IN ('ASSET','LIABILITY','EQUITY','REVENUE','EXPENSE')` |
| normal_balance | TEXT NN | `IN ('DEBIT','CREDIT')` |
| allow_negative | BOOLEAN NN | |
| has_projection | BOOLEAN NN | |
| status | TEXT NN | `IN ('ACTIVE','CLOSED')` |
| created_at | TIMESTAMPTZ NN | |

CHECK:

- `(owner_type = 'SYSTEM') = (account_id IS NULL)`;
- `(owner_type = 'SYSTEM') = (system_code IS NOT NULL)`;
- `(owner_type = 'SYSTEM') = (kind IS NULL)`;
- `has_projection = (owner_type <> 'SYSTEM')`;
- согласованность класса и сальдо: `(account_class IN ('ASSET','EXPENSE')) = (normal_balance = 'DEBIT')`. Исключение — контр-счёт 4900, его можно оформить флагом `is_contra`.

UNIQUE: `(account_id, kind)` WHERE `account_id IS NOT NULL`; `(system_code, currency)` WHERE `owner_type = 'SYSTEM'`; `(id, currency)` для составного FK из `ledger_entries`.

Справочник `chart_of_accounts(code PK, name, account_class, normal_balance, allow_negative)` — опционально. Он делает CoA данными, а не CHECK-перечнем. Рекомендация: справочник, а `ledger_accounts.account_type` — FK на него.

### `journal_transactions` — append-only

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK |
| operation | TEXT NN | `IN ('TOPUP','TRANSFER','HOLD_PLACE','HOLD_CAPTURE','HOLD_RELEASE','REFUND','PAYOUT_INIT','PAYOUT_SETTLE','PAYOUT_FAIL','REVERSAL','ADJUSTMENT', …)` |
| reference_type | TEXT NN | `IN ('TOPUP','TRANSFER','PAYMENT','REFUND','HOLD','PAYOUT','ADJUSTMENT')` |
| reference_id | UUID NN | |
| operation_seq | SMALLINT NN | DEFAULT 1, `CHECK (operation_seq >= 1)` |
| reverses_journal_id | UUID | FK → journal_transactions(id), UNIQUE |
| status | TEXT NN | `CHECK (status = 'POSTED')` ([D-06](../architecture/open-questions.md#d-06)) |
| description | TEXT | |
| initiated_by | UUID | |
| accounting_date | DATE NN | |
| posted_at | TIMESTAMPTZ NN | DEFAULT now() |

- **UNIQUE `(reference_type, reference_id, operation, operation_seq)`** — идемпотентность проведения.
- `CHECK ((operation = 'REVERSAL') = (reverses_journal_id IS NOT NULL))`.

### `ledger_entries` — append-only

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK |
| journal_transaction_id | UUID NN | FK → journal_transactions |
| ledger_account_id | UUID NN | FK `(ledger_account_id, currency)` → `ledger_accounts(id, currency)` |
| entry_type | TEXT NN | `IN ('DEBIT','CREDIT')` |
| amount | BIGINT NN | `CHECK (amount > 0 AND amount <= 100000000000000)` (MAX, [Q-11](../architecture/open-questions.md#q-11)) |
| currency | CHAR(3) NN | |
| account_version | BIGINT | |
| balance_after | BIGINT | |
| created_at | TIMESTAMPTZ NN | |

- `UNIQUE (journal_transaction_id, ledger_account_id)` — одна проводка на счёт в журнале.
- `UNIQUE (ledger_account_id, account_version)` — защита от потерянных обновлений проекции ([D-16](../architecture/open-questions.md#d-16)).
- `CHECK ((account_version IS NULL) = (balance_after IS NULL))`.
- Согласованность `has_projection ⇔ account_version IS NOT NULL` проверяется в сервисе проведения и тестах (CHECK не видит другую таблицу).

### `ledger_balance_snapshots`

PK `(ledger_account_id, snapshot_date)`; `debit_total`, `credit_total`, `balance BIGINT NN`, `computed_at`. Заполняется задачей по закрытому дню. Нужна для быстрых балансов системных счетов и отчётов. Это производные данные, поэтому их разрешено пересчитывать.

### `holds`

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK |
| account_id | UUID NN | FK `(account_id, currency)` → accounts |
| currency | CHAR(3) NN | |
| amount | BIGINT NN | `CHECK (amount > 0)` |
| status | TEXT NN | `IN ('ACTIVE','CAPTURED','RELEASED','EXPIRED')` |
| reference_type | TEXT NN | `IN ('PAYMENT','TRANSFER')` |
| reference_id | UUID NN | `UNIQUE (reference_type, reference_id)` — один резерв на объект (MVP) |
| expires_at | TIMESTAMPTZ NN | |
| created_at | TIMESTAMPTZ NN | |
| finalized_at | TIMESTAMPTZ | `CHECK ((status = 'ACTIVE') = (finalized_at IS NULL))` |

Модуль-владелец `holds` — Ledger (резерв — учётное понятие), а не Payments ([Q-24](../architecture/open-questions.md#q-24)).

### `manual_adjustments`

Структура — в ER. Ключевые CHECK: `approved_by IS NULL OR approved_by <> created_by`; `(status = 'POSTED') = (journal_transaction_id IS NOT NULL)`; `reason` NOT NULL.

## 4. Схема `payments`

### `payments`

Поля — в [ER](er-diagram.md#2-платежи-переводы-комиссии). Ограничения:

- `amount > 0`; `fee_amount >= 0`; `fee_amount <= amount` ([Q-09](../architecture/open-questions.md#q-09));
- `captured_amount BETWEEN 0 AND amount`; `refunded_amount BETWEEN 0 AND captured_amount`; `refunded_fee_amount BETWEEN 0 AND fee_amount`;
- статус и суммы согласованы:
  - `status IN ('CAPTURED','PARTIALLY_REFUNDED','REFUNDED') = (captured_amount > 0)`;
  - `(status = 'REFUNDED') = (captured_amount > 0 AND refunded_amount = captured_amount)`;
  - `(status = 'PARTIALLY_REFUNDED') = (refunded_amount > 0 AND refunded_amount < captured_amount)`;
- `customer_account_id IS NOT NULL OR status IN ('CREATED','CANCELLED')`;
- `UNIQUE (merchant_id, idempotency_key)` (бизнес-уровень идемпотентности, ТЗ §4.5);
- FK `(merchant_account_id, currency)` → `accounts(id, currency)`; FK `(customer_account_id, currency)` → `accounts(id, currency)` — валюта платежа = валюта обоих счетов;
- `version BIGINT NN` — инкремент при каждом изменении.

### `payment_attempts`

`UNIQUE (payment_id, attempt_number)`; `UNIQUE (provider_reference) WHERE provider_reference IS NOT NULL`; `status IN ('STARTED','SUCCEEDED','FAILED','UNKNOWN')`; `finished_at` заполнен ⇔ статус не `STARTED`.

### `payment_status_history` — append-only

Пишется в той же транзакции, что и смена статуса.

### `refunds`

- `amount > 0`, `fee_refund_amount >= 0`;
- `UNIQUE (payment_id, idempotency_key)`;
- FK `payment_id` → payments; `currency` = валюта платежа (составной FK `(payment_id, currency)` → `payments(id, currency)` + `UNIQUE (id, currency)` в payments);
- `status IN ('PENDING','SUCCEEDED','FAILED')`; `(status = 'FAILED') = (failure_code IS NOT NULL)`.

## 5. Схема `transfers`

### `transfers`

- `amount > 0`, `fee_amount >= 0`, `sender_account_id <> receiver_account_id`;
- FK `(sender_account_id, currency)` и `(receiver_account_id, currency)` → `accounts(id, currency)`. Это делает перевод между валютами невозможным на уровне БД (ТЗ §4.4 п.5);
- `UNIQUE (initiated_by, idempotency_key)`;
- `status IN ('ON_HOLD','COMPLETED','FAILED','CANCELLED')`; `CHECK (status <> 'ON_HOLD' OR hold_id IS NOT NULL)`; `(status = 'FAILED') = (failure_code IS NOT NULL)`.

### `transfer_status_history` — append-only.

## 6. Схема `commissions`

### `commission_tariffs`

- `UNIQUE (tariff_code, version)`;
- `rate_bp BETWEEN 0 AND 10000`; `fixed_amount >= 0`; `min_amount >= 0`; `max_amount IS NULL OR max_amount >= min_amount`;
- `rounding_mode IN ('HALF_UP','HALF_EVEN','UP','DOWN')`;
- `valid_to IS NULL OR valid_to > valid_from`;
- **Непересечение периодов** (требует `btree_gist`):

```sql
EXCLUDE USING gist (
  operation_type WITH =, currency WITH =,
  coalesce(merchant_id, '00000000-0000-0000-0000-000000000000'::uuid) WITH =,
  tstzrange(valid_from, valid_to, '[)') WITH &&
)
```

- Тариф неизменяем после того, как начал действовать. Изменение — это новая версия с новым `valid_from` и закрытие `valid_to` старой (ТЗ §2.7). Изменение пишется в `audit_logs` (ТЗ §2.12 «Изменение тарифов»).

## 7. Схема `platform`

### `idempotency_keys`

- `UNIQUE (scope, endpoint, key)`; `request_hash BYTEA NN`;
- `status IN ('IN_PROGRESS','COMPLETED')`; `(status = 'COMPLETED') = (response_code IS NOT NULL)`;
- `expires_at` — TTL ([Q-10](../architecture/open-questions.md#q-10)), очистка — фоновая задача.
- Протокол — [concurrency.md §4](concurrency.md#4-идемпотентность).

### `outbox_events`

- Частичный индекс `ON (created_at) WHERE published_at IS NULL` для relay.
- `aggregate_id` — ключ партиции Kafka: порядок событий одного агрегата сохраняется.
- Опубликованные события удаляются или архивируются через N дней. Это единственная «удаляемая» таблица среди транзакционных.

### `inbox_messages`

PK `(consumer, message_id)`. Вставка выполняется в той же транзакции, что и обработка сообщения.

## 8. Схема `audit`

### `audit_logs`

- Append-only (как ledger, [data-integrity.md §4](data-integrity.md#4-триггеры-и-защита-неизменяемости)); отдельная роль БД для записи.
- `seq BIGINT GENERATED ALWAYS AS IDENTITY` — порядок.
- Опционально хеш-цепочка `hash = sha256(prev_hash || canonical(row))` ([Q-20](../architecture/open-questions.md#q-20)).
- Отделён от технических логов (ТЗ §2.12): отдельная таблица, отдельная схема и, при необходимости, отдельная БД.

## 9. Прочие схемы

`auth`, `merchants`, `fraud`, `notifications`, `reconciliation` описаны на уровне ER ([er-diagram.md](er-diagram.md)). Детальная проработка этих схем — следующий этап, вне фокуса финансового ядра. Обязательные ограничения:

- `auth.users`: `UNIQUE (lower(email))`, `status IN (...)`, `password_hash` — только Argon2id-строка (`CHECK (password_hash LIKE '$argon2id$%')`).
- `auth.refresh_tokens`: хранится только хеш токена; `UNIQUE (token_hash)`; ротация через `replaced_by`. Повторное использование уже заменённого токена отзывает всю сессию (reuse detection).
- `merchants.api_keys`: хранится только хеш и префикс ключа.
- `fraud.fraud_cases`: `UNIQUE (check_id)`.
- `reconciliation.provider_statement_lines`: `UNIQUE (run_id, provider_reference, operation)`.
