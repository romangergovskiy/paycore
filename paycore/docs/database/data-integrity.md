# Целостность данных: ограничения, FK, индексы, триггеры, права

Статус: **проект**. SQL-фрагменты иллюстративны. Поля таблиц — в [database-schema.md](database-schema.md), транзакции и блокировки — в [concurrency.md](concurrency.md).

Принцип «защиты в глубину» для финансовых инвариантов:

| Уровень | Что гарантирует | Пример |
|---|---|---|
| 1. Домен (Go) | Бизнес-правила, машина состояний, расчёт сумм | `Payment.Capture()` запрещает переход из `CREATED` |
| 2. Сервис проведения | Бухгалтерские правила до записи | \(\sum D = \sum C\), валюта, ≥ 2 проводок |
| 3. Ограничения БД (строка) | Невозможные значения | `amount > 0`, `available_amount >= 0` |
| 4. Ограничения БД (связи и уникальность) | Согласованность между таблицами, однократность | составные FK по валюте, `UNIQUE (reference_type, reference_id, operation, operation_seq)` |
| 5. Триггеры БД | Многострочные инварианты, неизменяемость | deferred-проверка баланса журнала, запрет UPDATE/DELETE |
| 6. Права БД | Обход приложения | у роли приложения нет `UPDATE/DELETE` на ledger |
| 7. Задача сверки | То, что дорого проверять синхронно | I-03, I-05, I-09, I-10 (см. [ledger-model.md §12](../architecture/ledger-model.md#12-финансовые-инварианты)) |

## 1. CHECK-ограничения (сводка по финансовым таблицам)

| Таблица | CHECK |
|---|---|
| `ledger_entries` | `amount > 0 AND amount <= MAX`; `entry_type IN ('DEBIT','CREDIT')`; `(account_version IS NULL) = (balance_after IS NULL)` |
| `journal_transactions` | `status = 'POSTED'`; `operation IN (…)`; `reference_type IN (…)`; `(operation = 'REVERSAL') = (reverses_journal_id IS NOT NULL)` |
| `ledger_accounts` | взаимоисключения SYSTEM/пользовательский (см. [schema §3](database-schema.md#3-схема-ledger)); класс ⇔ нормальное сальдо |
| `account_balances` | `available_amount >= 0`; `reserved_amount >= 0` |
| `accounts` | `num_nonnulls(user_id, merchant_id) = 1`; статус; `closed_at` ⇔ CLOSED |
| `holds` | `amount > 0`; статус; `finalized_at` ⇔ не ACTIVE |
| `payments` | `amount > 0`; `0 <= fee_amount <= amount`; `0 <= captured_amount <= amount`; `0 <= refunded_amount <= captured_amount`; статус ⇔ суммы |
| `refunds` | `amount > 0`; FAILED ⇔ failure_code |
| `transfers` | `amount > 0`; `fee_amount >= 0`; `sender <> receiver`; ON_HOLD ⇒ hold_id |
| `commission_tariffs` | `rate_bp BETWEEN 0 AND 10000`; `min <= max`; `valid_to > valid_from`; EXCLUDE на пересечение периодов |
| `manual_adjustments` | `approved_by <> created_by`; POSTED ⇔ journal_transaction_id |
| `currencies` | `code ~ '^[A-Z]{3}$'`; `minor_units BETWEEN 0 AND 4` |

Все CHECK объявляются с именами (`ck_<table>_<rule>`), чтобы Go-код однозначно отображал нарушение в доменную ошибку (например, `ck_account_balances_available_nonneg` → `INSUFFICIENT_FUNDS`).

## 2. Внешние ключи

- **Политика удаления:** `ON DELETE RESTRICT` (или `NO ACTION`) для всех FK. Каскадов на финансовых данных нет. Сущности не удаляются, а получают статус `CLOSED` / `DISABLED`.
- **Составные FK для валюты.** Валюта проводки, счёта, платежа и перевода не может разойтись:

```sql
-- в целевых таблицах
ALTER TABLE accounts.accounts         ADD CONSTRAINT uq_accounts_id_currency        UNIQUE (id, currency);
ALTER TABLE ledger.ledger_accounts    ADD CONSTRAINT uq_ledger_accounts_id_currency UNIQUE (id, currency);
-- в ссылающихся
ALTER TABLE ledger.ledger_entries
  ADD CONSTRAINT fk_entries_account_currency
  FOREIGN KEY (ledger_account_id, currency) REFERENCES ledger.ledger_accounts (id, currency);
ALTER TABLE transfers.transfers
  ADD CONSTRAINT fk_transfers_sender_currency
  FOREIGN KEY (sender_account_id, currency) REFERENCES accounts.accounts (id, currency);
-- аналогично: receiver, payments.customer/merchant, holds, account_balances, ledger_accounts.account_id
```

- **FK между модулями** разрешены только по списку в [er-diagram.md §6](er-diagram.md#6-межмодульные-связи) ([D-12](../architecture/open-questions.md#d-12)).
- **Индексы под FK.** PostgreSQL не создаёт их автоматически. Для каждой ссылающейся колонки, по которой идут JOIN или проверки при изменении родителя, индекс создаётся явно (см. §5).

## 3. Уникальность (защита от повторов)

| Ограничение | От чего защищает |
|---|---|
| `journal_transactions UNIQUE (reference_type, reference_id, operation, operation_seq)` | Повторное проведение одного бизнес-шага (двойной capture, двойной возврат одного refund) |
| `journal_transactions UNIQUE (reverses_journal_id)` | Двойное сторно |
| `ledger_entries UNIQUE (journal_transaction_id, ledger_account_id)` | Дубли проводок в журнале |
| `ledger_entries UNIQUE (ledger_account_id, account_version)` | Потерянное обновление проекции |
| `holds UNIQUE (reference_type, reference_id)` | Два резерва на один платёж |
| `payments UNIQUE (merchant_id, idempotency_key)` | Два платежа по одному заказу |
| `transfers UNIQUE (initiated_by, idempotency_key)` | Двойной перевод при повторе запроса |
| `refunds UNIQUE (payment_id, idempotency_key)` | Двойной возврат при повторе запроса |
| `idempotency_keys UNIQUE (scope, endpoint, key)` | Повтор HTTP-запроса |
| `inbox_messages PK (consumer, message_id)` | Повторная обработка события из Kafka |
| `payment_attempts UNIQUE (payment_id, attempt_number)` | Гонка нумерации попыток |
| `fraud_cases UNIQUE (check_id)` | Двойной инцидент |
| `ledger_accounts UNIQUE (account_id, kind)` / `(system_code, currency)` | Дубли ledger-счетов |

## 4. Триггеры и защита неизменяемости

### 4.1. Запрет изменения проведённых записей (ТЗ §4.4 п.3)

```sql
CREATE FUNCTION ledger.forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ledger is append-only: % on % is forbidden', TG_OP, TG_TABLE_NAME
    USING ERRCODE = 'P0001';
END $$;

CREATE TRIGGER trg_entries_immutable
  BEFORE UPDATE OR DELETE ON ledger.ledger_entries
  FOR EACH ROW EXECUTE FUNCTION ledger.forbid_mutation();
CREATE TRIGGER trg_entries_no_truncate
  BEFORE TRUNCATE ON ledger.ledger_entries
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_mutation();
-- то же для journal_transactions, *_status_history, audit_logs
```

Триггер — вторая линия обороны. Первая — права (§6). Владелец схемы может отключить триггер, поэтому миграции под ролью владельца проходят ревью.

### 4.2. Сбалансированность журнала (ТЗ §4.4 п.2, [D-03](../architecture/open-questions.md#d-03))

Обычный CHECK видит одну строку, поэтому используется **отложенный constraint trigger**. Он срабатывает на `COMMIT`, когда все проводки уже вставлены:

```sql
CREATE FUNCTION ledger.check_journal_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE bad record;
BEGIN
  SELECT currency,
         SUM(CASE WHEN entry_type = 'DEBIT'  THEN amount ELSE 0 END) AS d,
         SUM(CASE WHEN entry_type = 'CREDIT' THEN amount ELSE 0 END) AS c,
         COUNT(*) AS n
    INTO bad
    FROM ledger.ledger_entries
   WHERE journal_transaction_id = NEW.journal_transaction_id
   GROUP BY currency
  HAVING SUM(CASE WHEN entry_type = 'DEBIT' THEN amount ELSE -amount END) <> 0
   LIMIT 1;
  IF FOUND THEN
    RAISE EXCEPTION 'unbalanced journal % in %: D=% C=%',
      NEW.journal_transaction_id, bad.currency, bad.d, bad.c USING ERRCODE = 'P0002';
  END IF;
  IF (SELECT COUNT(*) FROM ledger.ledger_entries
       WHERE journal_transaction_id = NEW.journal_transaction_id) < 2 THEN
    RAISE EXCEPTION 'journal % has < 2 entries', NEW.journal_transaction_id USING ERRCODE = 'P0002';
  END IF;
  RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER trg_journal_balanced
  AFTER INSERT ON ledger.ledger_entries
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION ledger.check_journal_balanced();
```

Замечания:

- Срабатывает по строке, поэтому для журнала из N проводок проверка выполняется N раз. Для 2–4 проводок это дёшево (индекс по `journal_transaction_id`). Оптимизация — statement-level trigger с transition table (`REFERENCING NEW TABLE`), но такие триггеры не бывают `DEFERRABLE`. Альтернатива — проверка в функции проведения ([D-03](../architecture/open-questions.md#d-03), вариант B).
- Журнал без проводок триггер не поймает: он срабатывает на вставку проводок. Поэтому нужен второй отложенный constraint trigger — `AFTER INSERT ON journal_transactions`, проверяющий ≥ 2 проводок.

### 4.3. Проводки только в транзакции создания журнала

Запрет «дописать» проводки в уже закоммиченный журнал (это изменило бы историю, даже если журнал остаётся сбалансированным):

```sql
-- в BEFORE INSERT ON ledger_entries
IF (SELECT xmin FROM ledger.journal_transactions WHERE id = NEW.journal_transaction_id)
   <> pg_current_xact_id()::xid THEN
  RAISE EXCEPTION 'entries may be added only in the journal''s creating transaction';
END IF;
```

Оговорки: приём опирается на системную колонку `xmin` и приведение `xid8 → xid`, поэтому его нужно проверить на целевой версии PostgreSQL. Внутри `SAVEPOINT` (вложенные транзакции pgx) `xmin` — это id подтранзакции, и сравнение даст ложный отказ, поэтому сервис проведения не использует savepoint между вставкой журнала и проводок.

Альтернатива — вставлять журнал и проводки только через функцию `ledger.post_journal(...)` с `SECURITY DEFINER` и отозвать прямой `INSERT` у роли приложения ([D-03](../architecture/open-questions.md#d-03), вариант B).

### 4.4. Прочие триггеры

- `account_balances`: только `UPDATE` колонок сумм, `version` и `updated_at`; `version` строго растёт (`NEW.version = OLD.version + 1`). Можно оформить триггером или оставить проверкой в коде — рекомендация: триггер (дёшево).
- `payments`, `transfers`, `holds`: триггер проверки допустимости перехода статуса (таблица разрешённых переходов) — опциональная третья линия после домена и условного `UPDATE ... WHERE status = ...`. Рекомендация: в MVP достаточно домена и условного UPDATE. Триггер — по желанию, для демонстрации.
- `updated_at` — триггером или явно в SQL. Рекомендация: явно в SQL (прозрачнее, ТЗ §4).

## 5. Индексы

### 5.1. Ledger

| Индекс | Запрос |
|---|---|
| `ledger_entries (journal_transaction_id)` (ТЗ §4.7) | проводки журнала, триггер баланса, Ledger Explorer |
| `ledger_entries (ledger_account_id, account_version)` UNIQUE | выписка по счёту в порядке версий, последний `balance_after`, проверка I-09 |
| `ledger_entries (ledger_account_id, created_at)` | баланс и обороты системного счёта за период |
| `journal_transactions (reference_type, reference_id, operation, operation_seq)` UNIQUE | идемпотентность, «все журналы платежа» |
| `journal_transactions (posted_at)` или BRIN `(posted_at)` | отчёты за период; BRIN выгоден на append-only таблице |
| `journal_transactions (reverses_journal_id)` UNIQUE WHERE NOT NULL | поиск сторно |
| `ledger_accounts (account_id, kind)` UNIQUE WHERE account_id IS NOT NULL | ledger-счета бизнес-счёта |
| `ledger_accounts (system_code, currency)` UNIQUE WHERE owner_type = 'SYSTEM' | поиск системного счёта |

### 5.2. Счета и резервы

| Индекс | Запрос |
|---|---|
| `accounts (user_id)` WHERE user_id IS NOT NULL | счета клиента |
| `accounts (merchant_id)` WHERE merchant_id IS NOT NULL | счета мерчанта |
| `accounts (account_number)` UNIQUE | поиск получателя по номеру |
| `holds (account_id) WHERE status = 'ACTIVE'` | проверка I-05, список резервов |
| `holds (expires_at) WHERE status = 'ACTIVE'` | scheduler истечения |
| `account_status_history (account_id, changed_at DESC)` | история статусов |

### 5.3. Платежи, переводы, возвраты (из ТЗ §4.7 + дополнения)

| Индекс | Запрос |
|---|---|
| `payments (merchant_id, created_at DESC)` (ТЗ) | платежи мерчанта |
| `payments (status, created_at DESC)` (ТЗ) | админка; лучше частичные индексы по «живым» статусам, см. ниже |
| `payments (merchant_id, idempotency_key)` UNIQUE | идемпотентность |
| `payments (customer_account_id, created_at DESC)` | история клиента |
| `payments (expires_at) WHERE status = 'CREATED'` | истечение неоплаченных платежей |
| `payment_attempts (payment_id, attempt_number)` UNIQUE | история попыток |
| `payment_attempts (provider_reference)` UNIQUE WHERE NOT NULL | сверка |
| `payment_attempts (started_at) WHERE status IN ('STARTED','UNKNOWN')` | восстановление зависших попыток |
| `refunds (payment_id)` | возвраты платежа |
| `transfers (sender_account_id, created_at DESC)` (ТЗ) | история отправителя |
| `transfers (receiver_account_id, created_at DESC)` | история получателя (входящие) |
| `transfers (initiated_by, idempotency_key)` UNIQUE | идемпотентность |
| `transfers (created_at) WHERE status = 'ON_HOLD'` | очередь проверки |

`payments (status, created_at DESC)` из ТЗ низкоселективен по статусу. Рекомендуется частичный индекс `(created_at DESC) WHERE status IN ('PENDING','AUTHORIZED')` для операционных очередей, а общий оставить, если подтвердится в `EXPLAIN ANALYZE`.

### 5.4. Инфраструктура

| Индекс | Запрос |
|---|---|
| `outbox_events (created_at) WHERE published_at IS NULL` | relay |
| `idempotency_keys (scope, endpoint, key)` UNIQUE | протокол идемпотентности |
| `idempotency_keys (expires_at)` | очистка |
| `audit_logs (resource_type, resource_id, occurred_at DESC)`, `(actor_id, occurred_at DESC)`, BRIN `(occurred_at)` | поиск в аудите |
| `webhook_deliveries (next_attempt_at) WHERE status = 'PENDING'` | воркер доставки |
| `commission_tariffs` GiST (EXCLUDE) | поиск действующего тарифа `@> now()` |
| `login_attempts (email, attempted_at DESC)`, `(ip, attempted_at DESC)` | rate limiting авторизации |

## 6. Роли и права БД

| Роль | Права |
|---|---|
| `paycore_owner` | Владелец схем, применяет миграции. Приложение под ней не работает |
| `paycore_app` | `SELECT, INSERT` на `ledger.*`, `*_status_history`, `audit.audit_logs`; `SELECT, INSERT, UPDATE` на изменяемых таблицах; **нет** `UPDATE/DELETE/TRUNCATE` на append-only таблицах; `DELETE` только на `platform.outbox_events`, `platform.idempotency_keys` (очистка) |
| `paycore_readonly` | `SELECT` для отчётов и Ledger Explorer; при наличии — на реплике |
| `paycore_audit_writer` | Опционально: единственная роль с `INSERT` в `audit_logs` ([Q-20](../architecture/open-questions.md#q-20)) |

Это реализует требование ТЗ §2.1: даже ADMIN приложения не может «тихо» изменить баланс. Любое движение денег — журнал, любая ручная корректировка — maker-checker и аудит.

## 7. Партиционирование и рост (на будущее)

- Кандидаты: `ledger_entries`, `journal_transactions`, `audit_logs`, `outbox_events` — по диапазону времени (месяц).
- Ограничение: в партиционированной таблице PK и UNIQUE должны включать ключ партиционирования. `UNIQUE (ledger_account_id, account_version)` и `UNIQUE (reference_type, reference_id, operation, operation_seq)` в этом случае не работают глобально. Поэтому партиционирование ledger откладывается до появления реальной потребности и требует отдельного ADR ([Q-25](../architecture/open-questions.md#q-25)).
- До тех пор достаточно индексов, BRIN по времени и снапшотов балансов.
