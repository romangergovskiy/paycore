# ER-диаграмма PostgreSQL

Статус: **проект**. Описание полей и типов — в [database-schema.md](database-schema.md), ограничения и индексы — в [data-integrity.md](data-integrity.md).

Диаграмма разбита на пять частей по модулям. Таблицы, которые встречаются в нескольких частях, на последующих диаграммах показаны только ключевыми полями. Обозначения: `PK` — первичный ключ, `FK` — внешний ключ, `UK` — уникальный. Типы упрощены для Mermaid: `char3` = `CHAR(3)`, `text_array` = `TEXT[]`, `text` = `TEXT` с CHECK на перечень значений.

Схемы PostgreSQL (namespaces) по модулям ([D-18](../architecture/open-questions.md#d-18)): `auth`, `merchants`, `accounts`, `ledger`, `payments`, `transfers`, `commissions`, `fraud`, `reconciliation`, `notifications`, `audit`, `platform` (idempotency, outbox, inbox).

## 1. Финансовое ядро: счета и ledger

```mermaid
erDiagram
    currencies ||--o{ accounts : "валюта"
    users |o--o{ accounts : "владеет (клиент)"
    merchants |o--o{ accounts : "владеет (мерчант)"
    accounts ||--|| account_balances : "проекция"
    accounts ||--o{ account_status_history : "история статуса"
    accounts ||--o{ account_restrictions : "ограничения"
    accounts |o--o{ ledger_accounts : "2 ledger-счёта"
    currencies ||--o{ ledger_accounts : "валюта"
    ledger_accounts ||--o{ ledger_entries : "проводки"
    journal_transactions ||--|{ ledger_entries : "содержит >=2"
    journal_transactions |o--o| journal_transactions : "reverses"
    ledger_accounts ||--o{ ledger_balance_snapshots : "снапшоты"
    manual_adjustments |o--o| journal_transactions : "порождает"
    accounts ||--o{ holds : "резервы"

    currencies {
        char3 code PK "ISO 4217"
        smallint minor_units "0..4"
        text name
        boolean is_active
    }
    accounts {
        uuid id PK "UUIDv7"
        text account_number UK
        uuid user_id FK "NULL для мерчанта"
        uuid merchant_id FK "NULL для клиента"
        text account_purpose "MAIN, SAVINGS, MERCHANT_SETTLEMENT"
        char3 currency FK
        text status "ACTIVE, BLOCKED, CLOSED"
        text name
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
    account_restrictions {
        uuid id PK
        uuid account_id FK
        text restriction "DEBIT, CREDIT, ALL"
        text reason
        uuid set_by
        timestamptz valid_from
        timestamptz valid_to
        timestamptz lifted_at
    }
    ledger_accounts {
        uuid id PK
        text owner_type "CUSTOMER, MERCHANT, SYSTEM"
        uuid account_id FK "NULL для SYSTEM"
        text account_type "код CoA: 2100, 4200"
        text kind "AVAILABLE, HOLD, NULL"
        text system_code "FEE_INCOME_PAYMENTS..."
        char3 currency FK
        text account_class "ASSET..EXPENSE"
        text normal_balance "DEBIT, CREDIT"
        boolean allow_negative
        boolean has_projection
        text status "ACTIVE, CLOSED"
        timestamptz created_at
    }
    journal_transactions {
        uuid id PK
        text operation "TOPUP, TRANSFER, HOLD_PLACE..."
        text reference_type "TRANSFER, PAYMENT, REFUND, HOLD..."
        uuid reference_id
        smallint operation_seq "default 1"
        uuid reverses_journal_id FK, UK "NULL кроме REVERSAL"
        text status "всегда POSTED"
        text description
        uuid initiated_by
        date accounting_date
        timestamptz posted_at
    }
    ledger_entries {
        uuid id PK
        uuid journal_transaction_id FK
        uuid ledger_account_id FK
        text entry_type "DEBIT, CREDIT"
        bigint amount "CHECK > 0"
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
        text reference_type "PAYMENT, TRANSFER"
        uuid reference_id UK
        timestamptz expires_at
        timestamptz created_at
        timestamptz finalized_at
    }
    manual_adjustments {
        uuid id PK
        text status "DRAFT, PENDING_APPROVAL, APPROVED, REJECTED, POSTED"
        jsonb proposed_entries
        text reason
        uuid created_by
        uuid approved_by "<> created_by"
        uuid journal_transaction_id FK
        timestamptz created_at
        timestamptz decided_at
    }
```

## 2. Платежи, переводы, комиссии

```mermaid
erDiagram
    merchants ||--o{ stores : "магазины"
    merchants ||--o{ payments : "принимает"
    stores |o--o{ payments : "магазин"
    accounts |o--o{ payments : "счёт клиента"
    accounts ||--o{ payments : "счёт мерчанта"
    payments ||--o{ payment_attempts : "попытки"
    payments ||--o{ payment_status_history : "история"
    payments ||--o{ refunds : "возвраты"
    payments |o--o| holds : "резерв"
    accounts ||--o{ transfers : "отправитель"
    accounts ||--o{ transfers : "получатель"
    transfers |o--o| holds : "резерв (REVIEW)"
    transfers ||--o{ transfer_status_history : "история"
    commission_tariffs |o--o{ payments : "тариф"
    commission_tariffs |o--o{ transfers : "тариф"
    accounts ||--o{ topups : "пополнения"
    accounts ||--o{ payouts : "выплаты"

    payments {
        uuid id PK
        uuid merchant_id FK
        uuid store_id FK
        uuid merchant_account_id FK
        uuid customer_account_id FK "NULL до authorize"
        uuid customer_user_id FK
        bigint amount "CHECK > 0"
        char3 currency
        bigint fee_amount "CHECK >= 0"
        uuid fee_tariff_id FK
        bigint captured_amount "0..amount"
        bigint refunded_amount "0..captured"
        bigint refunded_fee_amount "если D-09 B"
        text status "CREATED..REFUNDED"
        text failure_code
        text cancel_reason
        uuid hold_id FK
        text idempotency_key "UK с merchant_id"
        text description
        bigint version
        timestamptz expires_at "срок оплаты"
        timestamptz authorized_at
        timestamptz captured_at
        timestamptz created_at
        timestamptz updated_at
    }
    payment_attempts {
        uuid id PK
        uuid payment_id FK
        int attempt_number "UK с payment_id"
        text operation "AUTHORIZE, CAPTURE, VOID, REFUND"
        text provider_reference
        text status "STARTED, SUCCEEDED, FAILED, UNKNOWN"
        text error_code
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
        bigint fee_refund_amount
        text status "PENDING, SUCCEEDED, FAILED"
        text failure_code
        text reason
        text idempotency_key "UK с payment_id"
        uuid initiated_by
        timestamptz created_at
        timestamptz completed_at
    }
    transfers {
        uuid id PK
        uuid initiated_by FK
        uuid sender_account_id FK
        uuid receiver_account_id FK "<> sender"
        bigint amount "CHECK > 0"
        char3 currency
        bigint fee_amount "CHECK >= 0"
        uuid fee_tariff_id FK
        text status "ON_HOLD, COMPLETED, FAILED, CANCELLED"
        text failure_code
        uuid hold_id FK
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
    commission_tariffs {
        uuid id PK
        text tariff_code "TRANSFER_P2P, PAYMENT_DEFAULT"
        int version "UK с tariff_code"
        text operation_type "TRANSFER, PAYMENT"
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
    topups {
        uuid id PK
        uuid account_id FK
        bigint amount
        char3 currency
        text status
        text idempotency_key
        uuid initiated_by
        timestamptz created_at
    }
    payouts {
        uuid id PK
        uuid merchant_id FK
        uuid account_id FK
        bigint amount
        char3 currency
        text status "INITIATED, SENT, SETTLED, FAILED"
        text bank_reference
        text idempotency_key
        timestamptz created_at
    }
```

## 3. Идентификация и мерчанты

```mermaid
erDiagram
    users ||--o{ user_roles : "роли"
    roles ||--o{ user_roles : ""
    users ||--o{ sessions : "сессии"
    sessions ||--o{ refresh_tokens : "токены (ротация)"
    refresh_tokens |o--o| refresh_tokens : "replaced_by"
    users |o--o{ login_attempts : "попытки"
    users ||--o{ action_confirmations : "подтверждения"
    merchants ||--o{ merchant_members : "участники"
    users ||--o{ merchant_members : ""
    merchants ||--o{ stores : ""
    merchants ||--o{ api_keys : "ключи"
    stores |o--o{ api_keys : ""
    merchants ||--o{ webhook_endpoints : ""
    webhook_endpoints ||--o{ webhook_deliveries : "доставки"

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
        text mode "TEST, LIVE"
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
        text status "PENDING, DELIVERED, FAILED, DEAD"
        int response_code
        timestamptz next_attempt_at
        timestamptz delivered_at
    }
```

## 4. Надёжность, аудит, фрод

```mermaid
erDiagram
    fraud_rules ||--o{ fraud_check_hits : "сработало"
    fraud_checks ||--o{ fraud_check_hits : ""
    fraud_checks |o--o| fraud_cases : "инцидент"

    idempotency_keys {
        uuid id PK
        text scope "user:<id> / merchant:<id>"
        text key "UK с scope, endpoint"
        text endpoint "POST /api/v1/transfers"
        bytea request_hash "SHA-256"
        text status "IN_PROGRESS, COMPLETED"
        int response_code
        jsonb response_body
        text resource_type
        uuid resource_id
        timestamptz locked_until
        timestamptz created_at
        timestamptz expires_at
    }
    outbox_events {
        uuid id PK
        text aggregate_type
        uuid aggregate_id "ключ партиции Kafka"
        text event_type "payment.captured"
        int event_version
        jsonb payload
        jsonb headers "trace_id"
        timestamptz created_at
        timestamptz published_at "NULL = не опубликовано"
        int publish_attempts
        text last_error
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

## 5. Сверка (Reconciliation)

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

## 6. Межмодульные связи

Полиморфные ссылки (без FK) — сознательное решение, рассчитанное на будущее разделение на сервисы ([D-12](../architecture/open-questions.md#d-12)):

| Откуда | Куда | Почему без FK |
|---|---|---|
| `journal_transactions.reference_id` | transfers / payments / refunds / holds / topups / payouts / manual_adjustments | Ledger не зависит от бизнес-модулей; целостность — `UNIQUE` + инвариант I-10 |
| `holds.reference_id` | payments / transfers | то же |
| `outbox_events.aggregate_id` | любой агрегат | инфраструктурная таблица |
| `audit_logs.resource_id` | любой ресурс | аудит переживает удаление и архивирование ресурсов |
| `fraud_checks.subject_id` | transfers / payments | Fraud будет выделен в сервис первым (ТЗ §3.3) |
| `reconciliation_items.payment_id` | payments | слабая связь; совпадение может не найтись |

FK внутри монолита, которые **разрешены** и будут удалены при выделении сервисов: `ledger_accounts.account_id → accounts`, `payments.*_account_id → accounts`, `transfers.*_account_id → accounts`, `accounts.user_id → users`, `accounts.merchant_id → merchants`.
