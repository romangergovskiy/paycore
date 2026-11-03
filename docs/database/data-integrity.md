# Целостность данных: ограничения, FK, индексы, неизменяемость, права

Статус: **принято для MVP** (этап 2). SQL-фрагменты иллюстративны и не являются миграциями. Поля — [database-schema.md](database-schema.md), транзакции и блокировки — [concurrency.md](concurrency.md), решения — [../architecture/decisions.md](../architecture/decisions.md).

Принцип «защиты в глубину»:

| Уровень | Что гарантирует | Пример |
|---|---|---|
| 1. Домен (Go) | Бизнес-правила, машины состояний, расчёт сумм, понятные ошибки | `Payment.Capture()` запрещает переход из `CREATED` |
| 2. Шлюз ledger (`SECURITY DEFINER`) | Бухгалтерские правила, атомарное создание журнала, проекции, резервы | `ledger.post_journal` |
| 3. Права ролей БД | Запись в ledger только через шлюз | у `paycore_app` нет `INSERT/UPDATE/DELETE` на ledger-таблицах |
| 4. CHECK (строка) | Невозможные значения | `amount > 0`, `available_amount >= 0`, бакет `>= 0` |
| 5. FK и UNIQUE | Согласованность между таблицами, однократность | составные FK по валюте, `UNIQUE` журнала по ссылке |
| 6. Триггеры | Запрет мутаций для всех ролей, страховка баланса | `BEFORE UPDATE/DELETE/TRUNCATE`, отложенная проверка баланса |
| 7. Задача контроля инвариантов | Ошибки в самих механизмах | I-03, I-09, I-10, I-12 ([ledger-model.md §12](../architecture/ledger-model.md#12-финансовые-инварианты)) |

Запрещённый отрицательный остаток не допускается уже на уровнях 2 и 4 в момент записи. Уровень 7 — контроль, а не основной механизм.

## 1. CHECK-ограничения (финансовые таблицы)

| Таблица | CHECK |
|---|---|
| `ledger_entries` | `amount > 0 AND amount <= 10^14`; `entry_type IN ('DEBIT','CREDIT')`; `(account_version IS NULL) = (balance_after IS NULL)` |
| `journal_transactions` | `status = 'POSTED'`; `operation IN (…)`; `reference_type IN (…)`; `(operation = 'REVERSAL') = (reverses_journal_id IS NOT NULL)` |
| `ledger_accounts` | SYSTEM ⇔ `account_id IS NULL` ⇔ `system_code IS NOT NULL` ⇔ `kind IS NULL`; класс ⇔ нормальное сальдо; `bucket_count BETWEEN 1 AND 256`; `bucket_count = 1` для пользовательских счетов; `owner_type = 'SYSTEM' OR status = 'ACTIVE'` (статус бизнес-счёта хранится только в `accounts.accounts`) |
| `account_balances` | `available_amount >= 0`; `reserved_amount >= 0`; `version >= 0` |
| `system_balance_buckets` | `owner_type = 'SYSTEM'`; `0 <= bucket_no < bucket_count`; `CHECK (allow_negative OR balance >= 0)`. `owner_type`, `allow_negative`, `bucket_count` — копии родителя, зафиксированные составным FK. Для всех системных счетов MVP `allow_negative = false` |
| `holds` | `amount > 0`; статус; `finalized_at` ⇔ не `ACTIVE` |
| `accounts` | `num_nonnulls(user_id, merchant_id) = 1`; статус; `closed_at` ⇔ `CLOSED` |
| `payments` | `amount > 0`; `0 <= fee_amount < amount` ([D-27](../architecture/decisions.md#d-27): при `fee ≥ amount` capture отклоняется `FEE_EXCEEDS_PAYMENT`, комиссия не урезается); `0 <= captured_amount <= amount`; `0 <= refunded_amount <= captured_amount`; статус ⇔ суммы; `status NOT IN ('AUTHORIZED','CAPTURED','PARTIALLY_REFUNDED','REFUNDED') OR customer_account_id IS NOT NULL`; `status IN ('CREATED','AUTHORIZED','CAPTURED','PARTIALLY_REFUNDED','REFUNDED','FAILED','CANCELLED')` (без `PENDING`, [D-21](../architecture/decisions.md#d-21)) |
| `checkout_sessions` | статус; `expires_at > created_at`; `CONFIRMED` ⇔ `confirmed_at IS NOT NULL` |
| `refunds` | `amount > 0`; `status IN ('SUCCEEDED','FAILED')`; `FAILED` ⇔ `failure_code` |
| `transfers` | `amount > 0`; `fee_amount >= 0`; `sender <> receiver`; `status IN ('COMPLETED','FAILED')`; `FAILED` ⇔ `failure_code` |
| `payouts`, `test_topups` | `amount > 0`; статус; `FAILED` ⇔ `failure_code` |
| `limit_counters` | `used_amount >= 0` |
| `commission_tariffs` | `rate_bp BETWEEN 0 AND 10000`; `min <= max`; `valid_to > valid_from`; EXCLUDE на пересечение периодов |
| `manual_adjustments` | `approved_by <> created_by`; `POSTED` ⇔ `journal_transaction_id` |
| `idempotency_keys` | `status IN ('IN_PROGRESS','COMPLETED')`; `COMPLETED` ⇔ `response_code IS NOT NULL` |
| `outbox_events` | `status IN ('PENDING','PUBLISHED','DEAD')`; `PUBLISHED` ⇔ `published_at IS NOT NULL`; `(processing_until IS NULL) = (lease_owner IS NULL)` |

Все ограничения именованные (`ck_<таблица>_<правило>`). Go отображает их в доменные ошибки по имени.

## 2. Внешние ключи

- **Удаление:** `ON DELETE RESTRICT` / `NO ACTION` везде, каскадов на финансовых данных нет. Сущности не удаляются, а получают статус `CLOSED`/`DISABLED`.
- **Составные FK для валюты** — валюты счёта, проводки, платежа, перевода и резерва не могут разойтись:

```sql
ALTER TABLE accounts.accounts      ADD CONSTRAINT uq_accounts_id_currency        UNIQUE (id, currency);
ALTER TABLE ledger.ledger_accounts ADD CONSTRAINT uq_ledger_accounts_id_currency UNIQUE (id, currency);

ALTER TABLE ledger.ledger_entries ADD CONSTRAINT fk_entries_account_currency
  FOREIGN KEY (ledger_account_id, currency) REFERENCES ledger.ledger_accounts (id, currency);
ALTER TABLE transfers.transfers ADD CONSTRAINT fk_transfers_sender_currency
  FOREIGN KEY (sender_account_id, currency) REFERENCES accounts.accounts (id, currency);
-- аналогично: receiver, payments.customer/merchant, holds, account_balances, ledger_accounts.account_id
```

- **FK между модулями** — только по списку [er-diagram.md §7](er-diagram.md#7-межмодульные-связи) ([D-12](../architecture/decisions.md#d-12)).
- **Индексы под FK** создаются явно (§5).

## 3. Уникальность (защита от повторов)

| Ограничение | От чего защищает |
|---|---|
| `journal_transactions UNIQUE (reference_type, reference_id, operation, operation_seq)` | Повторное проведение одного шага (двойной capture, двойной возврат одного refund) |
| `journal_transactions UNIQUE (reverses_journal_id)` | Двойное сторно |
| `ledger_entries UNIQUE (journal_transaction_id, ledger_account_id)` | Дубли проводок в журнале |
| `ledger_entries UNIQUE (ledger_account_id, account_version) WHERE account_version IS NOT NULL` | Потерянное обновление проекции. Частичный индекс: у системных проводок `account_version` IS NULL, несколько NULL на одном счёте допустимы |
| `system_balance_buckets PK (ledger_account_id, bucket_no)` | Дубли бакетов |
| `holds UNIQUE (reference_type, reference_id)` | Два резерва на платёж |
| `payments UNIQUE (merchant_id, idempotency_key)` | Два платежа по одному запросу |
| `checkout_sessions UNIQUE (payment_id)`, `UNIQUE (token_hash)` | Две сессии на платёж, коллизия токена |
| `transfers UNIQUE (initiated_by, idempotency_key)` | Двойной перевод |
| `refunds UNIQUE (payment_id, idempotency_key)` | Двойной возврат |
| `payouts UNIQUE (merchant_id, idempotency_key)`, `test_topups UNIQUE (user_id, idempotency_key)` | Двойная выплата или пополнение |
| `idempotency_keys UNIQUE (scope, endpoint, key)` | Повтор HTTP-запроса |
| `limit_counters PK (user_id, currency, limit_type, period_start)` | Два счётчика на период |
| `inbox_messages PK (consumer, message_id)` | Повторная обработка события |
| `ledger_accounts UNIQUE (account_id, kind)`, `UNIQUE (system_code, currency)` | Дубли ledger-счетов |

## 4. Неизменяемость ledger

Решение [D-03](../architecture/decisions.md#d-03), пересмотрено на этапе 2. Защита через `xmin` **не используется**.

### 4.1. Механизм: функция-шлюз + права

**Единственный путь записи в ledger** — функции PostgreSQL с `SECURITY DEFINER`, которыми владеет отдельная роль `paycore_ledger` (NOLOGIN):

| Функция | Назначение |
|---|---|
| `ledger.open_ledger_accounts(account_id, owner_type, currency)` | Создаёт пару ledger-счетов (AVAILABLE/HOLD) и строку `account_balances` |
| `ledger.lock_account_balances(account_ids uuid[])` | Сортирует id и выполняет `SELECT … FOR UPDATE` (L5). Возвращает остатки и версии. Блокировки принадлежат транзакции вызывающего и держатся до её конца |
| `ledger.post_journal(journal_id, operation, reference_type, reference_id, operation_seq, reverses_journal_id, adjustment_id, description, initiated_by, accounting_date, entries jsonb, hold_action jsonb)` | Валидирует, обновляет проекции и бакеты, вставляет журнал и **все** проводки, меняет резерв. `adjustment_id` нужен для `REVERSAL` с участием `BLOCKED`-счёта. Контракт — [ledger-model.md §11.2](../architecture/ledger-model.md#112-sql-ledgerpost_journal--security-definer) |

Требования к функциям: `SET search_path = pg_catalog, ledger, pg_temp` (защита от подмены объектов: `pg_temp` явно последним, чтобы временная схема сессии не перекрывала `pg_catalog` и `ledger`); объекты вне схемы `ledger` (в том числе `accounts.accounts`, `accounts.currencies`) указываются только с именем схемы; `REVOKE EXECUTE … FROM PUBLIC`; `GRANT EXECUTE … TO paycore_app`. Goose создаёт функции от `paycore_owner`, затем `ALTER FUNCTION … OWNER TO paycore_ledger` — [ledger-schema-review.md §6.4](../architecture/ledger-schema-review.md#64-создание-и-владение-ledger-функциями). Тело функций проходит ревью как критичный код и покрывается интеграционными тестами из Go.

Порядок создания и владения — [D-40](../architecture/decisions.md#d-40): Goose от `paycore_owner`, `REVOKE EXECUTE FROM PUBLIC`, `GRANT EXECUTE TO paycore_app`, `ALTER FUNCTION … OWNER TO paycore_ledger`. Bootstrap выдаёт `GRANT paycore_ledger TO paycore_owner WITH INHERIT FALSE`; миграция функций выдаёт `GRANT CREATE ON SCHEMA ledger TO paycore_ledger`.

Почему этот механизм надёжен:

| Требование этапа 2 | Как обеспечено |
|---|---|
| Права ролей | У `paycore_app` только `SELECT` на ledger-таблицах и `EXECUTE` на функциях. `INSERT/UPDATE/DELETE/TRUNCATE` на `journal_transactions`, `ledger_entries`, `account_balances`, `system_balance_buckets`, `holds` не выданы (§6) |
| Запрет UPDATE/DELETE/TRUNCATE проведённых проводок | Прав нет ни у `paycore_app`, ни у `paycore_ledger` (у неё только `INSERT` на журналы и проводки). Дополнительно триггеры §4.4 срабатывают для **любой** роли, включая владельца схемы |
| Запрет добавления проводок в проведённый журнал | Вставить проводку можно только через `post_journal`. Функция всегда **создаёт новый** журнал с переданным `journal_id` и вставляет проводки только для него в том же вызове. Если журнал с тем же бизнес-ключом уже есть, функция ничего не вставляет: при идентичных проводках возвращает существующий журнал (`already_posted`), иначе — `PC409` ([ledger-model.md §11.2](../architecture/ledger-model.md#112-sql-ledgerpost_journal--security-definer)). API «добавить проводку в журнал X» не существует |
| Проверка сбалансированности | В функции до любой записи: \(\sum D = \sum C\) по каждой валюте, ≥ 2 проводок, суммы в диапазоне. Страховка — отложенный триггер §4.4 |
| Запрет `POSTED` для пустого или несбалансированного журнала | Журнал вставляется **после** успешной валидации и в одном вызове с проводками. Отложенный триггер на `journal_transactions` проверяет ≥ 2 проводок и баланс на `COMMIT` |
| Журнал и проводки в одной транзакции | Один вызов функции = одна транзакция вызывающего. Ошибка внутри функции откатывает всё, включая бизнес-изменения use case |
| Статус счёта не обходится | Функция сама читает статус каждого бизнес-счёта (`FOR SHARE NOWAIT`) и системного ledger-счёта и сверяет каждую проводку с матрицей «операция × роль × статус» ([ledger-model.md §11.4](../architecture/ledger-model.md#114-статусы-счетов-и-допустимые-операции)). Нарушение → `PC403`; вызов без L3 при параллельной смене статуса → `PC460`. Исключения матрицы (`HOLD_RELEASE` и `REFUND` на `BLOCKED`-покупателя) ослабляют только проверку статуса, бухгалтерские проверки действуют полностью |

### 4.2. Почему не другие механизмы

| Вариант | Причина отказа |
|---|---|
| Только Go-сервис | Ручной SQL, миграция или баг обходят проверки |
| Отложенные триггеры + `xmin` как защита от «дописывания» | Хрупко: `xmin` — системная деталь; ложные срабатывания внутри `SAVEPOINT`; не объяснимо как гарантия |
| `journal.sealed`-флаг + триггер на вставку проводок | Требует `UPDATE` журнала (нарушает append-only) и гонки «запечатывания» |
| Логическая репликация / внешний WORM-лог | Избыточно для MVP |

### 4.3. Роль Go

Go-сервис `Ledger.Post` строит журнал, валидирует его в домене (быстрые и понятные ошибки, unit-тесты без БД) и вызывает функцию. Бизнес-правила (статусы платежей, лимиты, комиссии) остаются в Go. Функция проверяет бухгалтерские инварианты и допустимость операции по статусу счёта ([D-26](../architecture/decisions.md#d-26)); use case проверяет статус заранее, чтобы сохранить отказ как `FAILED`-объект.

### 4.4. Вторая линия: триггеры

```sql
-- запрет мутаций для любой роли
CREATE FUNCTION ledger.forbid_mutation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'ledger is append-only: % on % is forbidden', TG_OP, TG_TABLE_NAME
    USING ERRCODE = 'P0001';
END $$;
CREATE TRIGGER trg_entries_immutable  BEFORE UPDATE OR DELETE ON ledger.ledger_entries
  FOR EACH ROW EXECUTE FUNCTION ledger.forbid_mutation();
CREATE TRIGGER trg_entries_no_truncate BEFORE TRUNCATE ON ledger.ledger_entries
  FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_mutation();
-- то же для journal_transactions, *_status_history, audit.audit_logs

-- страховка баланса на COMMIT (ловит вставки в обход функции: сиды, миграции)
CREATE CONSTRAINT TRIGGER trg_journal_balanced
  AFTER INSERT ON ledger.journal_transactions
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION ledger.check_journal_balanced();   -- ≥ 2 проводок и Σ D = Σ C по валютам
```

Отложенный триггер повешен на **журнал** (по одной проверке на журнал), а не на каждую проводку. Он видит все проводки, вставленные до `COMMIT`, и ловит и пустой, и несбалансированный журнал.

Владелец схемы (`paycore_owner`) может отключить триггеры. Это осознанная граница доверия: миграции проходят ревью, а production-доступ под `paycore_owner` выдаётся только на время миграций.

### 4.5. Прочие триггеры

- `account_balances` и бакеты: прямой записи нет, поэтому триггер на `version` не нужен: версию увеличивает только функция.
- Допустимость переходов статусов `payments`/`transfers`: домен + условный `UPDATE … WHERE status = …`. Триггер не используется (MVP).
- `updated_at` — явно в SQL (прозрачнее, ТЗ §4).

## 5. Индексы

### 5.1. Ledger

| Индекс | Запрос |
|---|---|
| `ledger_entries (journal_transaction_id)` (ТЗ §4.7) | проводки журнала, отложенный триггер, Ledger Explorer |
| `ledger_entries (ledger_account_id, account_version) WHERE account_version IS NOT NULL` UNIQUE | выписка, последний `balance_after`, I-09 |
| `ledger_entries (ledger_account_id, created_at, id)` | обороты системного счёта; `posted_at` живёт на журнале, JOIN `journal_transactions` |
| `journal_transactions (reference_type, reference_id, operation, operation_seq)` UNIQUE | идемпотентность, журналы объекта |
| BRIN `journal_transactions (posted_at)` | отчёты за период (append-only таблица) |
| `journal_transactions (reverses_journal_id)` UNIQUE WHERE NOT NULL | поиск сторно |
| `ledger_accounts (account_id, kind)` UNIQUE WHERE account_id IS NOT NULL | ledger-счета бизнес-счёта |
| `ledger_accounts (system_code, currency)` UNIQUE WHERE owner_type = 'SYSTEM' | поиск системного счёта |
| `system_balance_buckets` PK `(ledger_account_id, bucket_no)` | баланс = `SUM`, блокировка бакетов по порядку |

### 5.2. Счета, резервы, лимиты

| Индекс | Запрос |
|---|---|
| `accounts (user_id)` WHERE NOT NULL; `accounts (merchant_id)` WHERE NOT NULL | счета владельца |
| `accounts (account_number)` UNIQUE | поиск получателя |
| `holds (account_id) WHERE status = 'ACTIVE'` | резервы счёта, I-05 |
| `holds (expires_at) WHERE status = 'ACTIVE'` | scheduler истечения |
| `limit_counters` PK | счётчик лимита |
| `account_status_history (account_id, changed_at DESC)` | история статусов |

### 5.3. Платежи, переводы, возвраты, выплаты

| Индекс | Запрос |
|---|---|
| `payments (merchant_id, created_at DESC)` (ТЗ) | платежи магазина |
| `payments (merchant_id, idempotency_key)` UNIQUE | идемпотентность |
| `payments (customer_account_id, created_at DESC)` | история покупателя |
| `payments (created_at DESC) WHERE status IN ('CREATED','AUTHORIZED')` | операционные очереди; заменяет низкоселективный `(status, created_at)` из ТЗ, если `EXPLAIN ANALYZE` подтвердит |
| `checkout_sessions (expires_at) WHERE status = 'ACTIVE'` | истечение checkout |
| `checkout_sessions (token_hash)` UNIQUE | открытие checkout |
| `payment_attempts (payment_id, attempt_number)` UNIQUE | история попыток |
| `refunds (payment_id, created_at)` | возвраты платежа |
| `transfers (sender_account_id, created_at DESC)` (ТЗ), `(receiver_account_id, created_at DESC)` | история |
| `transfers (initiated_by, idempotency_key)` UNIQUE | идемпотентность |
| `payouts (merchant_id, created_at DESC)` | выплаты |

### 5.4. Инфраструктура

| Индекс | Запрос |
|---|---|
| `outbox_events (seq) WHERE status = 'PENDING'` | захват пачки relay |
| `outbox_events (aggregate_id, seq) WHERE status = 'PENDING'` | условие порядка `NOT EXISTS` |
| `outbox_events (published_at) WHERE status = 'PUBLISHED'` | очистка |
| `idempotency_keys (scope, endpoint, key)` UNIQUE; `(expires_at)` | протокол и очистка |
| `webhook_deliveries (next_attempt_at) WHERE status = 'PENDING'` | worker доставки |
| `audit_logs (resource_type, resource_id, occurred_at DESC)`, `(actor_id, occurred_at DESC)`, BRIN `(occurred_at)` | поиск в аудите |
| `commission_tariffs` GiST (EXCLUDE) | действующий тариф |
| `login_attempts (email, attempted_at DESC)`, `(ip, attempted_at DESC)` | rate limiting |

## 6. Роли и права БД

| Роль | LOGIN | Права |
|---|---|---|
| `paycore_owner` | да (только для миграций) | Владелец схем и таблиц; DDL |
| `paycore_ledger` | **нет** | Владелец ledger-функций ([D-40](../architecture/decisions.md#d-40)). `SELECT, INSERT` на `journal_transactions`, `ledger_entries`, `ledger_accounts` (`open_ledger_accounts`); `SELECT, INSERT, UPDATE` на `account_balances`, `system_balance_buckets`, `holds`; `SELECT` на `chart_of_accounts`, `accounts.accounts`, `currencies`, `manual_adjustments`; колоночная привилегия `UPDATE (updated_at)` на `accounts.accounts` — только ради `SELECT … FOR SHARE NOWAIT` (шаг 3 [ledger-model.md §11.2](../architecture/ledger-model.md#112-sql-ledgerpost_journal--security-definer)). **Нет** `UPDATE/DELETE/TRUNCATE` на журналах и проводках |
| `paycore_app` | да | `EXECUTE` на `ledger.*`-функциях; `SELECT` на ledger-таблицах; `SELECT, INSERT, UPDATE` на изменяемых бизнес-таблицах (`payments`, `transfers`, `refunds`, `payouts`, `accounts`, `limit_counters`, …); `SELECT, INSERT` на `*_status_history`, `audit.audit_logs`; `DELETE` только на `platform.outbox_events`, `platform.idempotency_keys`, `platform.inbox_messages` (очистка). `idle_in_transaction_session_timeout = 30s` |
| `paycore_readonly` | да | `SELECT` для отчётов и Ledger Explorer |

`SELECT … FOR UPDATE` требует права `UPDATE`. Поэтому `paycore_app` не может сама заблокировать `account_balances` и делает это через `ledger.lock_account_balances`. Это ещё и гарантирует сортировку L5.

Любая row-lock-клауза (`FOR UPDATE`, `FOR NO KEY UPDATE`, `FOR SHARE`, `FOR KEY SHARE`) требует права `UPDATE` хотя бы на одну колонку таблицы. Поэтому `paycore_ledger` получает `UPDATE` только на `accounts.accounts.updated_at`: это минимально достаточно для `FOR SHARE NOWAIT` и не позволяет менять статус, валюту или владельца. Ledger-функции `accounts.accounts` не обновляют; это проверяется ревью и интеграционным тестом (после вызова `post_journal` строка счёта не изменилась).

Так выполняется ТЗ §2.1: даже ADMIN приложения не может изменить баланс в обход ledger. Любое движение денег — журнал через шлюз, любая ручная корректировка — maker-checker и аудит ([D-31](../architecture/decisions.md#d-31)).

На этапе 3.2 роли создаёт `migrate bootstrap` (суперпользователь, `CREATE ROLE`). С этапа 3.4 bootstrap также делает `GRANT paycore_ledger TO paycore_owner WITH INHERIT FALSE` ([D-40](../architecture/decisions.md#d-40)). Goose-миграции выполняет `paycore_owner`. Приложение и интеграционные тесты подключаются только как `paycore_app`. `paycore_ledger` — `NOLOGIN`. Этап 3.3 создаёт таблицы `accounts` и `ledger` с грантами по таблице. Этап 3.4 создаёт шлюзовые функции в `00004` и передаёт их `paycore_ledger`.

Миграция `00001_platform_schema.sql` выдаёт `paycore_app` права `INSERT, UPDATE` на `platform.schema_info` и ставит `ALTER DEFAULT PRIVILEGES … ON TABLES` в схеме `platform`. Это шире целевой модели: будущие гранты — **по таблице**, без blanket `GRANT` на все таблицы схемы. План — [ledger-schema-review.md §6](../architecture/ledger-schema-review.md#6-план-грантов-будущих-таблиц). Саму миграцию 3.2 этот документ не меняет.

## 7. Партиционирование (после MVP)

Кандидаты — `ledger_entries`, `journal_transactions`, `audit_logs`, `outbox_events` по месяцам. В партиционированной таблице PK/UNIQUE должны включать ключ партиции, поэтому глобальные `UNIQUE (ledger_account_id, account_version)` и `UNIQUE (reference_type, …)` потеряют силу. Партиционирование ledger требует отдельного ADR. В MVP — индексы, BRIN и снапшоты ([D-34](../architecture/decisions.md#d-34)).
