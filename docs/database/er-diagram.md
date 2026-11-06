# ER-диаграмма PostgreSQL

Статус: **принято для MVP** (этап 2). Описание полей и типов — в [database-schema.md](database-schema.md), ограничения и индексы — в [data-integrity.md](data-integrity.md), решения — в [../architecture/decisions.md](../architecture/decisions.md).

Диаграмма разбита на части по модулям. Таблицы, которые встречаются в нескольких частях, на последующих диаграммах показаны только ключевыми полями. Обозначения: `PK` — первичный ключ, `FK` — внешний ключ, `UK` — уникальный. Типы упрощены для Mermaid: `char3` = `CHAR(3)`, `text_array` = `TEXT[]`, `text` = `TEXT` с CHECK на перечень значений.

Схемы PostgreSQL по модулям ([D-18](../architecture/decisions.md#d-18)): `auth`, `merchants`, `accounts`, `ledger`, `payments`, `transfers`, `commissions`, `limits`, `simulator`, `notifications`, `audit`, `platform` (idempotency, outbox, inbox). После MVP: `fraud`, `reconciliation`.

Этап 3.3 создаёт `accounts` и `ledger` целиком. `auth.users` в `00002` — заглушка PK ради FK со счетов; этап 3.20 (`00005`) расширяет `auth` (users, roles, sessions, tokens, login_attempts, rate_limit_counters, action_confirmations) без DROP. `merchants.merchants` остаётся заглушкой PK. Go-модуля auth и bootstrap ADMIN нет. Схемы payments/transfers в этой фазе не реализуются. Контракт — [../architecture/identity-auth.md](../architecture/identity-auth.md).

## 1. Финансовое ядро: счета и ledger

```mermaid
erDiagram
    currencies ||--o{ accounts : "валюта"
    users |o--o{ accounts : "владеет (клиент)"
    merchants |o--o{ accounts : "владеет (магазин)"
    accounts ||--|| account_balances : "проекция"
    accounts ||--o{ account_status_history : "история статуса"
    accounts |o--o{ ledger_accounts : "2 ledger-счёта"
    chart_of_accounts ||--o{ ledger_accounts : "тип счёта"
    currencies ||--o{ ledger_accounts : "валюта"
    ledger_accounts ||--o{ system_balance_buckets : "бакеты (SYSTEM)"
    ledger_accounts ||--o{ ledger_entries : "проводки"
    journal_transactions ||--|{ ledger_entries : "содержит >=2"
    journal_transactions |o--o| journal_transactions : "reverses"
    ledger_accounts ||--o{ ledger_balance_snapshots : "снапшоты"
    manual_adjustments |o--o| journal_transactions : "порождает"
    accounts ||--o{ holds : "резервы"

    currencies {
        char3 code PK "MVP: RUB"
        smallint minor_units "0..4"
        text name
        boolean is_active
    }
    accounts {
        uuid id PK "UUIDv7; UK (id, currency)"
        text account_number UK
        uuid user_id FK "NULL для магазина"
        uuid merchant_id FK "NULL для клиента"
        text account_purpose "MAIN, SAVINGS, MERCHANT_SETTLEMENT"
        char3 currency FK
        text status "ACTIVE, BLOCKED, CLOSED"
        text name
        bigint version
        timestamptz created_at
        timestamptz updated_at
        timestamptz closed_at
    }
    account_balances {
        uuid account_id PK, FK
        char3 currency FK "= accounts.currency"
        bigint available_amount "CHECK >= 0"
        bigint reserved_amount "CHECK >= 0"
        bigint version "монотонно"
        timestamptz updated_at
    }
    account_status_history {
        uuid id PK
        uuid account_id FK
        text from_status
        text to_status
        text reason
        uuid changed_by
        timestamptz changed_at
    }
    chart_of_accounts {
        text code PK "1100, 2100, 4200..."
        text name
        text account_class "ASSET..EXPENSE"
        text normal_balance "DEBIT, CREDIT"
        boolean allow_negative
        boolean is_contra
        boolean in_mvp
    }
    ledger_accounts {
        uuid id PK "UK (id, currency)"
        text owner_type "CUSTOMER, MERCHANT, SYSTEM"
        uuid account_id FK "NULL для SYSTEM"
        text account_type FK "код CoA"
        text kind "AVAILABLE, HOLD, NULL"
        text system_code "SIM_FUNDING_SOURCE..."
        char3 currency FK
        text account_class
        text normal_balance
        boolean allow_negative "false в MVP"
        smallint bucket_count "16 для 1100, 4100, 4200"
        text status "ACTIVE, CLOSED; не ACTIVE только у SYSTEM"
        timestamptz created_at
    }
    system_balance_buckets {
        uuid ledger_account_id PK, FK
        smallint bucket_no PK "0..bucket_count-1"
        bigint balance "CHECK allow_negative OR >= 0"
        text owner_type "копия, = SYSTEM"
        boolean allow_negative "копия"
        smallint bucket_count "копия"
        timestamptz updated_at
    }
    journal_transactions {
        uuid id PK
        text operation "SIM_TOPUP, TRANSFER, HOLD_PLACE..."
        text reference_type "SIM_TOPUP, TRANSFER, HOLD, REFUND, PAYOUT, ADJUSTMENT"
        uuid reference_id
        smallint operation_seq "default 1"
        uuid reverses_journal_id FK, UK "NULL кроме REVERSAL"
        text status "всегда POSTED"
        text description
        uuid initiated_by
        date accounting_date "UTC"
        timestamptz posted_at
    }
    ledger_entries {
        uuid id PK
        uuid journal_transaction_id FK
        uuid ledger_account_id FK
        text entry_type "DEBIT, CREDIT"
        bigint amount "CHECK > 0 AND <= 10^14"
        char3 currency "FK (ledger_account_id, currency)"
        bigint account_version "NULL для SYSTEM"
        bigint balance_after "NULL для SYSTEM"
        timestamptz created_at
    }
    ledger_balance_snapshots {
        uuid ledger_account_id PK, FK
        date snapshot_date PK
        bigint debit_total
        bigint credit_total
        bigint balance
        timestamptz computed_at
    }
    holds {
        uuid id PK
        uuid account_id FK
        char3 currency
        bigint amount "CHECK > 0"
        text status "ACTIVE, CAPTURED, RELEASED, EXPIRED"
        text reference_type "PAYMENT в MVP"
        uuid reference_id "UK с reference_type"
        date limit_period_start
        timestamptz expires_at
        timestamptz created_at
        timestamptz finalized_at
    }
    manual_adjustments {
        uuid id PK
        text status "DRAFT, PENDING_APPROVAL, APPROVED, REJECTED, POSTED"
        jsonb proposed_entries
        text reason
        text incident_ref
        uuid created_by
        uuid approved_by "<> created_by"
        uuid journal_transaction_id FK
        timestamptz created_at
        timestamptz decided_at
    }
```

`account_restrictions` из этапа 1 перенесена на после MVP: в MVP ограничение операций выражается статусом `BLOCKED` ([D-26](../architecture/decisions.md#d-26)).

## 2. Платежи и checkout

```mermaid
erDiagram
    merchants ||--o{ stores : "магазины"
    merchants ||--o{ payments : "принимает"
    stores |o--o{ payments : "магазин"
    accounts |o--o{ payments : "счёт клиента (после подтверждения)"
    accounts ||--o{ payments : "счёт магазина"
    payments ||--|| checkout_sessions : "сессия оплаты"
    users |o--o{ checkout_sessions : "подтвердил"
    payments ||--o{ payment_attempts : "попытки"
    payments ||--o{ payment_status_history : "история"
    payments ||--o{ refunds : "возвраты"
    payments |o--o| holds : "резерв"
    commission_tariffs |o--o{ payments : "тариф (при capture)"

    payments {
        uuid id PK "UK (id, currency)"
        uuid merchant_id FK
        uuid store_id FK
        uuid merchant_account_id FK
        uuid customer_user_id FK "NULL до подтверждения"
        uuid customer_account_id FK "NULL до подтверждения"
        bigint amount "CHECK > 0"
        char3 currency
        bigint fee_amount "при capture, CHECK < amount"
        uuid fee_tariff_id FK
        bigint captured_amount "0 или amount"
        bigint refunded_amount "0..captured"
        text status "CREATED, AUTHORIZED, CAPTURED, PARTIALLY_REFUNDED, REFUNDED, FAILED, CANCELLED"
        text failure_code
        text cancel_reason "MERCHANT_CANCELLED, CHECKOUT_EXPIRED, VOIDED, AUTH_EXPIRED"
        uuid hold_id
        text idempotency_key "UK с merchant_id"
        text description
        bigint version
        timestamptz authorized_at
        timestamptz captured_at
        timestamptz created_at
        timestamptz updated_at
    }
    checkout_sessions {
        uuid id PK
        uuid payment_id FK, UK
        bytea token_hash UK
        text status "ACTIVE, CONFIRMED, FAILED, CANCELLED, EXPIRED"
        uuid customer_user_id FK
        timestamptz expires_at "+30 мин"
        timestamptz created_at
        timestamptz confirmed_at
    }
    payment_attempts {
        uuid id PK
        uuid payment_id FK
        int attempt_number "UK с payment_id"
        text operation "CONFIRM, CAPTURE, VOID, REFUND"
        text status "SUCCEEDED, FAILED"
        text error_code "FEE_EXCEEDS_PAYMENT, ACCOUNT_NOT_ACTIVE..."
        uuid actor_id
        timestamptz started_at
        timestamptz finished_at
    }
    payment_status_history {
        uuid id PK
        uuid payment_id FK
        text from_status
        text to_status
        text reason
        uuid changed_by
        timestamptz changed_at
    }
    refunds {
        uuid id PK
        uuid payment_id FK
        bigint amount "CHECK > 0"
        char3 currency
        text status "SUCCEEDED, FAILED"
        text failure_code "MERCHANT_INSUFFICIENT_FUNDS..."
        text reason
        text idempotency_key "UK с payment_id"
        uuid initiated_by
        timestamptz created_at
    }
    commission_tariffs {
        uuid id PK
        text tariff_code "UK с version"
        int version
        text operation_type "TRANSFER_P2P, TRANSFER_OWN, PAYMENT"
        uuid merchant_id FK "NULL = базовый"
        char3 currency
        int rate_bp "0..10000"
        bigint fixed_amount
        bigint min_amount
        bigint max_amount "NULL = без max"
        text rounding_mode "HALF_UP"
        timestamptz valid_from
        timestamptz valid_to "EXCLUDE overlap"
        uuid created_by
        uuid approved_by
    }
```

## 3. Переводы, симулятор, выплаты, лимиты

```mermaid
erDiagram
    accounts ||--o{ transfers : "отправитель"
    accounts ||--o{ transfers : "получатель"
    transfers ||--o{ transfer_status_history : "история"
    commission_tariffs |o--o{ transfers : "тариф"
    users ||--o{ test_topups : "пополняет"
    accounts ||--o{ test_topups : "зачисление"
    merchants ||--o{ payouts : "выводит"
    accounts ||--o{ payouts : "списание"
    users ||--o{ limit_counters : "использование лимитов"
    limit_rules ||--o{ limit_counters : "правило по limit_type"

    transfers {
        uuid id PK
        uuid initiated_by FK
        uuid sender_account_id FK
        uuid receiver_account_id FK "<> sender"
        bigint amount "CHECK > 0"
        char3 currency
        bigint fee_amount "сверх суммы"
        uuid fee_tariff_id FK
        text status "COMPLETED, FAILED"
        text failure_code
        text idempotency_key "UK с initiated_by"
        text description
        timestamptz created_at
        timestamptz completed_at
    }
    transfer_status_history {
        uuid id PK
        uuid transfer_id FK
        text from_status
        text to_status
        text reason
        uuid changed_by
        timestamptz changed_at
    }
    test_topups {
        uuid id PK "схема simulator"
        uuid user_id FK
        uuid account_id FK "MAIN или SAVINGS"
        bigint amount "CHECK > 0"
        char3 currency
        text status "COMPLETED, FAILED"
        text failure_code
        text idempotency_key "UK с user_id"
        uuid initiated_by
        timestamptz created_at
    }
    payouts {
        uuid id PK "схема merchants"
        uuid merchant_id FK
        uuid account_id FK
        bigint amount "CHECK > 0"
        char3 currency
        text status "COMPLETED, FAILED"
        text failure_code "PAYOUT_SOURCE_INSUFFICIENT..."
        text idempotency_key "UK с merchant_id"
        uuid requested_by
        timestamptz created_at
    }
    limit_rules {
        uuid id PK
        text limit_type "MAX_SINGLE_OPERATION, DAILY_OUTGOING, DAILY_SIM_TOPUP"
        char3 currency
        text scope "GLOBAL в MVP"
        bigint amount "CHECK > 0"
        timestamptz valid_from
        timestamptz valid_to "EXCLUDE overlap"
        uuid created_by
        uuid approved_by
    }
    limit_counters {
        uuid user_id PK
        char3 currency PK
        text limit_type PK "DAILY_OUTGOING, DAILY_SIM_TOPUP"
        date period_start PK "сутки UTC"
        bigint used_amount "CHECK >= 0"
        timestamptz updated_at
    }
```

Связь `limit_rules — limit_counters` логическая (по `limit_type` и `currency`), FK нет.

## 4. Идентификация, мерчанты, платформа, аудит

```mermaid
erDiagram
    users ||--o{ user_roles : "роли"
    roles ||--o{ user_roles : ""
    users ||--o{ sessions : "сессии"
    sessions ||--o{ refresh_tokens : "токены (ротация)"
    refresh_tokens |o--o| refresh_tokens : "replaced_by"
    users |o--o{ login_attempts : "попытки"
    users ||--o{ action_confirmations : "подтверждения"
    users |o--o{ rate_limit_counters : "окна лимитов (логически)"
    merchants ||--o{ merchant_members : "участники"
    users ||--o{ merchant_members : ""
    merchants ||--o{ stores : ""
    merchants ||--o{ api_keys : "ключи"
    stores |o--o{ api_keys : ""
    merchants ||--o{ webhook_endpoints : ""
    webhook_endpoints ||--o{ webhook_deliveries : "доставки"
    outbox_events ||--o{ webhook_deliveries : "event_id (логически)"

    users {
        uuid id PK
        text email UK "lower(email)"
        text password_hash "Argon2id"
        text first_name
        text last_name
        text status "ACTIVE, LOCKED, DISABLED"
        timestamptz created_at
        timestamptz updated_at
    }
    roles {
        text code PK "CUSTOMER..ADMIN"
        text description
    }
    user_roles {
        uuid user_id PK, FK
        text role_code PK, FK
        uuid granted_by
        timestamptz granted_at
    }
    sessions {
        uuid id PK
        uuid user_id FK
        text user_agent
        inet ip
        timestamptz created_at
        timestamptz last_seen_at
        timestamptz revoked_at
    }
    refresh_tokens {
        uuid id PK
        uuid session_id FK
        bytea token_hash UK
        uuid replaced_by FK
        timestamptz expires_at
        timestamptz used_at
        timestamptz revoked_at
    }
    login_attempts {
        uuid id PK
        text email
        uuid user_id FK
        inet ip
        boolean success
        text failure_reason
        timestamptz attempted_at
    }
    action_confirmations {
        uuid id PK
        uuid user_id FK
        text action_type
        bytea code_hash
        int attempts_left
        timestamptz expires_at
        timestamptz confirmed_at
    }
    rate_limit_counters {
        text bucket PK "LOGIN_EMAIL, LOGIN_IP, REGISTER_IP, REFRESH_IP"
        text counter_key PK
        timestamptz window_start PK "15 мин UTC"
        int count "CHECK >= 0"
        timestamptz updated_at
    }
    merchants {
        uuid id PK
        text legal_name
        text display_name
        text status "PENDING, ACTIVE, SUSPENDED"
        timestamptz created_at
    }
    merchant_members {
        uuid merchant_id PK, FK
        uuid user_id PK, FK
        text member_role "OWNER, DEVELOPER, VIEWER"
    }
    stores {
        uuid id PK
        uuid merchant_id FK
        text name
        text url
        text status
    }
    api_keys {
        uuid id PK
        uuid merchant_id FK
        uuid store_id FK
        text key_prefix UK
        bytea key_hash
        text mode "TEST в MVP"
        text_array scopes
        timestamptz expires_at
        timestamptz revoked_at
    }
    webhook_endpoints {
        uuid id PK
        uuid merchant_id FK
        text url
        bytea secret_encrypted
        text_array event_types
        boolean is_active
    }
    webhook_deliveries {
        uuid id PK
        uuid endpoint_id FK
        uuid event_id "outbox_events.id"
        int attempt
        text status "PENDING, DELIVERED, DEAD"
        int response_code
        text last_error
        timestamptz next_attempt_at
        timestamptz processing_until "lease"
        text lease_owner
        timestamptz delivered_at
    }
    idempotency_keys {
        uuid id PK
        text scope "user:id / merchant:id"
        text endpoint "UK с scope, key"
        text key
        bytea request_hash "SHA-256"
        text status "IN_PROGRESS, COMPLETED"
        smallint response_code
        jsonb response_body
        text resource_type
        uuid resource_id
        timestamptz created_at
        timestamptz completed_at
        timestamptz expires_at "+24 ч"
    }
    outbox_events {
        uuid id PK "event_id"
        bigint seq UK "identity, порядок"
        text aggregate_type
        uuid aggregate_id "ключ порядка"
        bigint aggregate_version
        text event_type "payment.captured"
        smallint event_version
        jsonb payload
        jsonb headers "trace_id"
        text status "PENDING, PUBLISHED, DEAD"
        int attempts
        timestamptz next_attempt_at
        timestamptz processing_until "lease"
        text lease_owner
        text last_error
        timestamptz created_at
        timestamptz published_at
    }
    inbox_messages {
        text consumer PK
        uuid message_id PK
        timestamptz received_at
        timestamptz processed_at
    }
    audit_logs {
        uuid id PK
        bigint seq UK "порядок"
        uuid actor_id
        text actor_type "USER, STAFF, SYSTEM, API_KEY"
        text action "ACCOUNT_BLOCKED, TARIFF_CHANGED..."
        text resource_type
        uuid resource_id
        jsonb before_state
        jsonb after_state
        inet ip
        text request_id
        bytea prev_hash "если Q-20"
        bytea hash
        timestamptz occurred_at
    }
    notifications {
        uuid id PK
        uuid user_id
        text channel "EMAIL, IN_APP"
        text template
        jsonb payload
        text status
        timestamptz created_at
        timestamptz read_at
    }
```

Таблицы `idempotency_keys`, `outbox_events`, `inbox_messages` (схема `platform`), `audit_logs` и `notifications` не имеют FK на бизнес-таблицы: связи показаны в [§7](#7-межмодульные-связи).

## 5. После MVP: fraud и сверка

Схемы `fraud` и `reconciliation` в MVP не создаются ([D-20](../architecture/decisions.md#d-20)): нет внешнего провайдера, сверять не с чем, а антифрод в MVP сводится к лимитам ([D-24](../architecture/decisions.md#d-24)). Модель этапа 1 сохранена как задел. Сверка внутренних данных (ledger против проекций) в MVP есть, но это фоновая проверка без собственных таблиц.

### 5.1. Fraud

```mermaid
erDiagram
    fraud_rules ||--o{ fraud_check_hits : "сработало"
    fraud_checks ||--o{ fraud_check_hits : ""
    fraud_checks |o--o| fraud_cases : "инцидент"

    fraud_rules {
        uuid id PK
        text code UK
        int version
        jsonb params
        int score
        text action "ALLOW, REVIEW, BLOCK"
        boolean is_active
    }
    fraud_checks {
        uuid id PK
        text subject_type "TRANSFER, PAYMENT"
        uuid subject_id
        int risk_score
        text decision "ALLOW, REVIEW, BLOCK"
        timestamptz checked_at
    }
    fraud_check_hits {
        uuid check_id PK, FK
        uuid rule_id PK, FK
        int score
        jsonb details
    }
    fraud_cases {
        uuid id PK
        uuid check_id FK, UK
        text status "OPEN, IN_REVIEW, APPROVED, REJECTED"
        uuid assigned_to
        uuid resolved_by
        text resolution
        timestamptz created_at
        timestamptz resolved_at
    }
```

Вместе с fraud вернутся `transfers.hold_id`, статусы `ON_HOLD`/`CANCELLED` у переводов и `holds.reference_type = 'TRANSFER'` ([D-05](../architecture/decisions.md#d-05)).

### 5.2. Сверка с внешним провайдером

```mermaid
erDiagram
    reconciliation_runs ||--o{ provider_statement_lines : "реестр"
    reconciliation_runs ||--o{ reconciliation_items : "результаты"
    provider_statement_lines |o--o| reconciliation_items : ""

    reconciliation_runs {
        uuid id PK
        text provider
        date period_from
        date period_to
        text status "RUNNING, COMPLETED, FAILED"
        int matched_count
        int mismatched_count
        uuid started_by
        timestamptz started_at
        timestamptz finished_at
    }
    provider_statement_lines {
        uuid id PK
        uuid run_id FK
        text provider_reference "UK с run_id"
        text operation
        bigint amount
        char3 currency
        timestamptz provider_time
        jsonb raw
    }
    reconciliation_items {
        uuid id PK
        uuid run_id FK
        uuid statement_line_id FK
        uuid payment_id
        uuid payment_attempt_id
        text result "MATCHED, MISSING_INTERNAL, MISSING_PROVIDER, AMOUNT_MISMATCH, STATUS_MISMATCH"
        bigint internal_amount
        bigint provider_amount
        text resolution_status "OPEN, RESOLVED, IGNORED"
        uuid resolved_by
        text resolution_note
        uuid adjustment_id "manual_adjustments"
    }
```

Вместе со сверкой вернутся `payment_attempts.provider_reference` и статусы `STARTED`/`UNKNOWN`.

## 6. Изменения относительно этапа 1

| Было | Стало | Решение |
|---|---|---|
| Проекция только у пользовательских счетов | `system_balance_buckets` для системных счетов | [D-02](../architecture/decisions.md#d-02) |
| Тип счёта — свободный текст | Справочник `chart_of_accounts` | [D-03](../architecture/decisions.md#d-03) |
| `topups` | `simulator.test_topups` | [D-15](../architecture/decisions.md#d-15) |
| `payouts`: `INITIATED, SENT, SETTLED, FAILED`, `bank_reference` | `COMPLETED, FAILED`, без банка | [D-25](../architecture/decisions.md#d-25) |
| Платёж без сессии оплаты | `checkout_sessions` | [D-22](../architecture/decisions.md#d-22) |
| Статус платежа `PENDING` | Нет в MVP | [D-21](../architecture/decisions.md#d-21) |
| `refunds.fee_refund_amount`, `payments.refunded_fee_amount` | Удалены: комиссия не возвращается | [D-09](../architecture/decisions.md#d-09) |
| `transfers.ON_HOLD`, `hold_id` | После MVP | [D-05](../architecture/decisions.md#d-05) |
| Лимитов в схеме нет | `limit_rules`, `limit_counters`, `holds.limit_period_start` | [D-24](../architecture/decisions.md#d-24) |
| `idempotency_keys.locked_until` | Статусы `IN_PROGRESS`/`COMPLETED`, блокировка на уникальном индексе, `completed_at` | [D-07](../architecture/decisions.md#d-07) |
| `outbox_events.published_at` как единственный признак | `seq`, `status`, lease (`processing_until`, `lease_owner`), backoff | [D-23](../architecture/decisions.md#d-23) |
| Fraud и сверка в MVP | После MVP (§5) | [D-20](../architecture/decisions.md#d-20) |

## 7. Межмодульные связи

Полиморфные ссылки без FK — сознательное решение, рассчитанное на будущее разделение модулей ([D-12](../architecture/decisions.md#d-12)):

| Откуда | Куда | Почему без FK |
|---|---|---|
| `journal_transactions.reference_id` | holds / transfers / refunds / test_topups / payouts / manual_adjustments | Ledger не зависит от бизнес-модулей; целостность — `UNIQUE` и инвариант «у каждой бизнес-операции есть журнал» ([ledger-model.md §12](../architecture/ledger-model.md#12-финансовые-инварианты)) |
| `holds.reference_id` | payments | то же |
| `payments.hold_id` | `ledger.holds` | обратная ссылка на резерв; направление зависимости payments → ledger не фиксируем FK ([D-12](../architecture/decisions.md#d-12)) |
| `outbox_events.aggregate_id` | любой агрегат | инфраструктурная таблица |
| `webhook_deliveries.event_id` | outbox_events | события архивируются раньше доставок |
| `idempotency_keys.resource_id` | созданный объект | ключи удаляются по TTL |
| `audit_logs.resource_id` | любой ресурс | аудит переживает архивирование ресурсов |
| `limit_counters.limit_type` | limit_rules | правила версионируются по периодам |

FK внутри монолита, которые **разрешены** и будут заменены при выделении модуля: `ledger_accounts.account_id → accounts`, `account_balances.account_id → accounts`, `holds.account_id → accounts`, `payments.*_account_id → accounts`, `transfers.*_account_id → accounts`, `test_topups.account_id → accounts`, `payouts.account_id → accounts`, `accounts.user_id → users`, `accounts.merchant_id → merchants`.
