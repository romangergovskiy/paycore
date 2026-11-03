# Транзакции, конкурентный доступ и защита от двойного списания

Статус: **принято для MVP** (этап 2). SQL здесь — иллюстративный псевдокод шагов, не миграции. Связанные документы: [data-integrity.md](data-integrity.md), [../architecture/money-flows.md](../architecture/money-flows.md), [../architecture/decisions.md](../architecture/decisions.md), [../architecture/financial-invariants-testing.md](../architecture/financial-invariants-testing.md).

## 1. Цели

1. Доступный остаток не уходит ниже нуля ни при каком чередовании запросов (ТЗ §2.5: два перевода по 8 000 ₽ при 10 000 ₽). То же для системных счетов.
2. Один бизнес-шаг даёт ровно один финансовый эффект при любых повторах (клиент, сервер, worker, scheduler).
3. Deadlock исключён по построению единым порядком блокировок. Если deadlock всё же возник, повтор безопасен.
4. Транзакции короткие. Внутри них нет сетевых вызовов (Kafka, webhook, внешние системы).

## 2. Уровни изоляции ([D-04](../architecture/decisions.md#d-04))

| Операция | Уровень | Механизм |
|---|---|---|
| Денежные операции: пополнение, перевод, подтверждение checkout, capture, void, истечение, возврат, выплата, корректировка | **READ COMMITTED** | Явные блокировки в порядке §3, условные `UPDATE … WHERE status = …`, CHECK. Проекции меняются только в `ledger.post_journal` |
| Изменение статуса счёта | READ COMMITTED | `UPDATE accounts` конфликтует с `FOR SHARE` денежных операций |
| Отчёты, пробный баланс, проверка инвариантов | **REPEATABLE READ READ ONLY** | Один согласованный снимок |
| Захват пачек outbox, webhook и задач истечения | READ COMMITTED | `FOR UPDATE SKIP LOCKED` + lease |

Почему не SERIALIZABLE везде. Он корректен без явных блокировок, но на горячих счетах даёт много откатов `40001`, а поведение зависит от планов запросов. Явные блокировки дают ожидание вместо отката и легко объясняются.

Почему не только optimistic `version`. Это шторм повторов при конкуренции на одном счёте. `version` остаётся для `account_version` в проводках и для ETag.

Проверки достаточности средств и статуса выполняются **только на primary и только под блокировкой**. Реплика годится для выписок и отчётов.

## 3. Единый порядок захвата блокировок

Все use case берут блокировки строго в этом порядке. Внутри уровня — по возрастанию ключа. Уровень можно пропустить, но нельзя вернуться на предыдущий. Повторное обращение к строке, уже заблокированной на более раннем уровне (например, `UPDATE limit_counters` после проведения или `UPDATE payments` в конце транзакции), новой блокировкой не считается.

| Уровень | Объект | Режим | Кто берёт |
|---|---|---|---|
| **L1** | `platform.idempotency_keys` (вставка ключа) | ожидание на уникальном индексе | use case API |
| **L2** | Бизнес-агрегат: `payments` (вместе с его `checkout_sessions`), `manual_adjustments` | `FOR UPDATE` | use case, scheduler |
| **L3** | `accounts.accounts` — все участники | `FOR SHARE` для денежных операций; `FOR NO KEY UPDATE` для смены статуса (блокировка, разблокировка, закрытие); по `id` | use case |
| **L4** | `limits.limit_counters` — счётчики лимитов | `INSERT … ON CONFLICT DO NOTHING` + `FOR UPDATE`, по `(user_id, currency, limit_type, period_start)` | use case |
| **L5** | `ledger.account_balances` — все участники | `FOR UPDATE`, по `account_id` | `ledger.lock_account_balances` (повторно в `post_journal`, блокировка уже наша) |
| **L6** | `ledger.system_balance_buckets` | увеличение — один бакет; уменьшение — все бакеты счёта; по `(ledger_account_id, bucket_no)` | `ledger.post_journal` |
| **L7** | `ledger.holds` | вставка резерва или условный `UPDATE … WHERE status = 'ACTIVE'` | `ledger.post_journal` |
| **L8** | Вставки и обновления: журнал и проводки; бизнес-объекты (`transfers`, `refunds`, `payouts`, `test_topups`, `payment_attempts`, изменения `payments`/`checkout_sessions`/`accounts`); `*_status_history`; `outbox_events`; `audit_logs`; `UPDATE idempotency_keys → COMPLETED` | новые строки или строки, уже заблокированные на L1–L4 | use case + `post_journal` (шаг 7) |

На этапе 2 добавлены **L4** (лимиты, [D-24](../architecture/decisions.md#d-24)) и **L6** (бакеты системных счетов, [D-02](../architecture/decisions.md#d-02)). Уровень `holds` — L7: резерв меняется только внутри `post_journal`, все его переходы уже сериализованы блокировкой платежа на L2. На этапе 2.1 L8 включён в формулу и матрицу, а шаблон возврата исправлен: в нём бизнес-объект вставлялся до вызова `post_journal`, то есть до L6–L7.

**Правила L8.**

1. Ни одна запись L8 не выполняется до того, как взяты все нужные блокировки L2–L7. Поэтому бизнес-объекты (`refunds`, `payouts`, `transfers` …) вставляются **после** `post_journal`. Их id (UUIDv7) генерирует Go заранее, и он передаётся в функцию как `reference_id`. FK журнала на объект нет ([er-diagram.md §7](er-diagram.md#7-межмодульные-связи)), так что порядок вставки не важен для целостности.
2. Отказ (`FAILED`-объект, `payment_attempts`) записывается на шаге `finish` — после последней взятой блокировки. После него новые блокировки не берутся.
3. Вставка строки с FK берёт `FOR KEY SHARE` на родительской строке (`accounts`, `payments`, `ledger_accounts`). Этот режим совместим с `FOR SHARE` и `FOR NO KEY UPDATE`. Конфликтует он только с `FOR UPDATE` на той же строке, а `FOR UPDATE` на `payments` (L2) держит та же транзакция. Поэтому для смены статуса счёта используется `FOR NO KEY UPDATE`, а не `FOR UPDATE`: иначе вставка резерва или проводки ждала бы смены статуса на L8, то есть «вниз» по порядку.
4. Уникальные индексы L8 (`journal_transactions` по бизнес-ключу, `refunds (payment_id, idempotency_key)` …) могут заставить ждать только транзакцию с тем же бизнес-ключом. Такая транзакция уже сериализована с нами на L1 или L2, поэтому в момент нашей вставки она либо завершена, либо ещё не дошла до L8.
5. `audit_logs` в MVP без хеш-цепочки, вставка не блокирует других. Если будет принята хеш-цепочка ([Q-20](../architecture/open-questions.md#q-20)), строка «последний хеш» станет блокировкой L8, которую берут последней, перед `COMMIT`.

### 3.1. Матрица «сценарий × уровни»

| Сценарий | L1 | L2 | L3 | L4 | L5 | L6 | L7 | L8 |
|---|---|---|---|---|---|---|---|---|
| Тестовое пополнение | ✓ | | клиент | лимит пополнения | клиент | 1100: один бакет | | журнал; `test_topups`; outbox; ключ |
| Перевод другому клиенту | ✓ | | отправитель, получатель | суточный лимит отправителя | оба | 4100: один бакет | | журнал; `UPDATE limit_counters`; `transfers`; история; outbox; ключ |
| Перевод между своими счетами | ✓ | | оба | — | оба | — | | журнал; `transfers`; история; outbox; ключ |
| Перевод отклонён | ✓ | | (до отказа) | (до отказа) | (до отказа) | | | `transfers (FAILED)`; ключ |
| Создание платежа | ✓ | | счёт магазина | | | | | `payments`; `checkout_sessions`; outbox; ключ |
| Отмена до оплаты | ✓ | платёж + checkout | | | | | | `UPDATE payments`, checkout; история; outbox; ключ |
| Подтверждение checkout | ✓ | платёж + checkout | покупатель, магазин | суточный лимит покупателя | покупатель | | вставка резерва | журнал; `UPDATE payments`, checkout, `limit_counters`; `payment_attempts`; история; outbox; ключ |
| Capture | ✓ | платёж | покупатель, магазин | | покупатель, магазин | 4200: один бакет | резерв | журнал; `UPDATE payments`; `payment_attempts`; история; outbox; ключ |
| Capture отклонён (`FEE_EXCEEDS_PAYMENT`, статус счёта) | ✓ | платёж | (до отказа) | | | | | `payment_attempts (FAILED)`; ключ |
| Void | ✓ | платёж | покупатель | возврат лимита (тот же период) | покупатель | | резерв | журнал; `UPDATE payments`, `limit_counters`; история; outbox; ключ |
| Истечение резерва (scheduler) | | платёж | покупатель | возврат лимита | покупатель | | резерв | журнал; `UPDATE payments`, `limit_counters`; история; outbox |
| Истечение checkout (scheduler) | | платёж + checkout | | | | | | `UPDATE payments`, checkout; история; outbox |
| Возврат | ✓ | платёж | покупатель, магазин | | покупатель, магазин | | | журнал; `refunds`; `UPDATE payments`; история; outbox; ключ |
| Возврат отклонён | ✓ | платёж | (до отказа) | | (до отказа) | | | `refunds (FAILED)`; ключ |
| Выплата | ✓ | | магазин | | магазин | 1100: **все** бакеты | | журнал; `payouts`; outbox; ключ |
| Корректировка / сторно | ✓ | заявка | участники | | участники | затронутые системные счета | (резерв, если сторнируется `HOLD_*`) | журнал; `UPDATE manual_adjustments`; аудит; ключ |
| Блокировка / разблокировка счёта | ✓ | | `FOR NO KEY UPDATE` | | | | | `UPDATE accounts`; история; аудит; outbox; ключ |
| Закрытие счёта | ✓ | | `FOR NO KEY UPDATE` | | счёт (проверка нулевых остатков) | | | `UPDATE accounts`; история; аудит; outbox; ключ |

Доказательство отсутствия deadlock. Каждая транзакция берёт блокировки в одном глобальном порядке:

\[
(L1) < (L2,\ id) < (L3,\ id) < (L4,\ key) < (L5,\ account\_id) < (L6,\ account,\ bucket) < (L7,\ id) < (L8)
\]

Ожидание возможно только «вверх» по этому порядку, поэтому цикл ожиданий невозможен. Отдельно:

- L8 — последний уровень: после первой записи L8 транзакция не берёт блокировок L1–L7 (правило 1), а ожидания внутри L8 возможны только между транзакциями, уже сериализованными на L1–L2 (правило 4);
- увеличение системного счёта держит один бакет и после него ждёт только L7/L8;
- уменьшение берёт все бакеты по возрастанию `bucket_no`;
- смена статуса счёта держит только L3 (и L5 при закрытии, после L3);
- `post_journal` читает статус счёта с `FOR SHARE NOWAIT` ([ledger-model.md §11.2](../architecture/ledger-model.md#112-sql-ledgerpost_journal--security-definer)): эта блокировка уже наша с L3, а если нет — функция не ждёт, а падает, так что ожидания «вниз» не возникает;
- фоновые задачи не начинают с блокировки `holds`: кандидатов выбирают без блокировок, затем идут L2 → ….

В Go блокировки L3–L5 берёт один компонент (Unit of Work сервиса проведения), он сам сортирует ключи. L6–L7 и записи журнала в L8 берёт только SQL-функция.

Проверка шаблонов §5 на соответствие порядку выполнена по всем строкам матрицы и сценариям S-01…S-28 (§9). Фактический порядок совпадает с L1…L8 после исправления шаблона возврата.

## 4. Идемпотентность HTTP

Решение [D-07](../architecture/decisions.md#d-07). Второй уровень защиты — бизнес-`UNIQUE` ([data-integrity.md §3](data-integrity.md#3-уникальность-защита-от-повторов)).

### 4.1. Правила

- `Idempotency-Key` **обязателен** для всех `POST`, создающих денежный объект или двигающих деньги: `/transfers`, `/payments`, `/payments/{id}/capture`, `/payments/{id}/cancel`, `/payments/{id}/refunds`, `/checkout/{token}/confirm`, `/payouts`, `/simulator/topups`. Ключ — 1…255 печатных ASCII-символов. Без ключа — `400 IDEMPOTENCY_KEY_REQUIRED`.
- **Область ключа (scope):** `user:<id>` для клиента, `merchant:<id>` для магазина. Ключи API ротируются, поэтому scope — сам магазин. Уникальность — `(scope, endpoint, key)`, где `endpoint` — метод + шаблон маршрута (`POST /api/v1/payments/{id}/refunds`).
- **`request_hash`** = SHA-256 от канонического представления: `endpoint` + значения параметров пути + тело в каноническом JSON (ключи отсортированы, без пробелов). Заголовки не входят.
- **Статусы строки:** `IN_PROGRESS` (вставлена, операция выполняется) и `COMPLETED` (сохранён итоговый ответ). В MVP оба статуса пишутся **в одной транзакции** с бизнес-эффектом, поэтому другие сессии никогда не видят закоммиченный `IN_PROGRESS` (§4.4).
- **Срок хранения:** `expires_at = created_at + 24 ч` — гарантированный срок повтора исходного ответа. Очистка удаляет строки с `expires_at < now()`. После удаления ключа повтор с ним наталкивается на бизнес-`UNIQUE` и получает `409 IDEMPOTENCY_KEY_REUSED`, но не вторую операцию.
- Сохраняются ответы `2xx` и бизнес-отказы `4xx` (например, `422 INSUFFICIENT_FUNDS`, вместе с объектом `FAILED`). Ответы `5xx` и технические откаты не сохраняются: ключ откатывается вместе с транзакцией.

### 4.2. Последовательность SQL

```text
-- до транзакции: scope, endpoint, key, request_hash; id ключа = UUIDv7
BEGIN ISOLATION LEVEL READ COMMITTED;
SET LOCAL lock_timeout = '3s';
SET LOCAL statement_timeout = '5s';

-- (1) захват ключа
INSERT INTO platform.idempotency_keys
       (id, scope, endpoint, key, request_hash, status, created_at, expires_at)
VALUES ($id, $scope, $endpoint, $key, $hash, 'IN_PROGRESS', now(), now() + interval '24 hours')
ON CONFLICT (scope, endpoint, key) DO NOTHING
RETURNING id;

-- (2а) строка вставлена → мы владельцы ключа:
--      бизнес-операция (§5) — L2…L8 в этой же транзакции
--      UPDATE platform.idempotency_keys
--         SET status = 'COMPLETED', response_code = $code, response_body = $body,
--             resource_type = $rt, resource_id = $rid, completed_at = now()
--       WHERE id = $id;
--      COMMIT;  → вернуть ответ клиенту

-- (2б) 0 строк → ключ уже есть и ЗАКОММИЧЕН (см. §4.3):
SELECT request_hash, status, response_code, response_body
  FROM platform.idempotency_keys
 WHERE scope = $scope AND endpoint = $endpoint AND key = $key;
ROLLBACK;
--      request_hash ≠ $hash          → 409 IDEMPOTENCY_KEY_REUSED (операция не выполняется)
--      status = 'COMPLETED'          → вернуть сохранённые response_code и response_body,
--                                      заголовок Idempotent-Replayed: true
--      status = 'IN_PROGRESS'        → 409 REQUEST_IN_PROGRESS, Retry-After: 1
--                                      (в MVP недостижимо, защитная ветка)
```

### 4.3. Поведение второго запроса

Ключевое свойство PostgreSQL: `INSERT … ON CONFLICT` при **незакоммиченной** конфликтующей вставке другой транзакции **ждёт её завершения**. Затем:

- если та закоммитилась — выполняется ветка `DO NOTHING` (0 строк) → (2б);
- если откатилась — вставка проходит → (2а).

| # | Ситуация | Что происходит | Итог |
|---|---|---|---|
| 1 | Два одновременных запроса, один ключ, одно тело | B ждёт на шаге (1), пока A не завершится. A коммитит операцию и `COMPLETED`. B получает 0 строк → читает `COMPLETED` → повторяет ответ A | **Одна** операция, два одинаковых ответа |
| 2 | То же, но A откатилась (ошибка, `40001` после исчерпания повторов, падение) | Вставка B проходит, B выполняет операцию сама | Одна операция (от B) |
| 3 | B ждёт дольше `lock_timeout` (A долгая) | `55P03` → B откатывается → `409 REQUEST_IN_PROGRESS`, `Retry-After` | Клиент повторит позже и получит результат A |
| 4 | Тот же ключ, другое тело, одновременно | B ждёт A → 0 строк → `request_hash` ≠ → `409 IDEMPOTENCY_KEY_REUSED` | Операция только от A |
| 5 | Тот же ключ, другое тело, позже | Сразу 0 строк → `409 IDEMPOTENCY_KEY_REUSED` | — |
| 6 | Повтор после успешного `COMMIT`: клиент не получил ответ (таймаут, обрыв, падение приложения после коммита) | 0 строк → `COMPLETED` → сохранённый ответ | Второй операции нет |
| 7 | Обрыв соединения во время `COMMIT` (исход неизвестен серверу) | Сервер возвращает `5xx` и **не** повторяет сам. Клиент повторяет с тем же ключом: если коммит прошёл — случай 6, если нет — случай 2 | Ровно одна операция |
| 8 | Приложение упало **до** `COMMIT` | Соединение закрыто → PostgreSQL откатывает транзакцию, ключ исчезает. Повтор клиента выполняет операцию | Ровно одна операция |
| 9 | Сетевой разрыв без закрытия TCP (зависшая сессия) | `idle_in_transaction_session_timeout` (30 с) и TCP keepalive завершают сессию → откат. Повторы в это время ждут до `lock_timeout` и получают `409 REQUEST_IN_PROGRESS` | После отката — случай 2 |
| 10 | Бизнес-отказ (нет средств) | Объект `FAILED` + `COMPLETED` с `422` коммитятся. Повтор возвращает тот же `422`, даже если деньги уже появились | Нужен новый ключ, чтобы попробовать снова |
| 11 | Внутренний retry сервера на `40001` / `40P01` | Повторяется **вся** транзакция, включая шаг (1). Откат удалил ключ, поэтому повтор начинается с чистого листа | Одна операция |
| 12 | Повтор после очистки ключа (> 24 ч) | Шаг (1) вставляет новый ключ, но бизнес-`UNIQUE` (`transfers (initiated_by, idempotency_key)`, `payments (merchant_id, idempotency_key)`, `refunds (payment_id, idempotency_key)` …) отклоняет вставку объекта → откат → `409 IDEMPOTENCY_KEY_REUSED` | Второй операции нет |

### 4.4. Почему двух финансовых операций быть не может

1. Ключ и бизнес-эффект коммитятся **атомарно**: нет состояния «операция есть, ключа нет» и наоборот.
2. Уникальный индекс `(scope, endpoint, key)` пропускает к шагу (2а) ровно одну транзакцию. Остальные ждут её исхода.
3. Бизнес-`UNIQUE` в доменных таблицах и в ledger (`reference_type, reference_id, operation, operation_seq`) — независимый второй барьер. Он работает и для внутренних вызовов, и после очистки ключей.
4. Переходы статусов условные (`WHERE status = 'AUTHORIZED'`): повтор шага над тем же объектом с другим ключом получает `409 INVALID_STATE`.

Двухфазный протокол (коммит `IN_PROGRESS` + lease `locked_until` + отдельная транзакция результата) зарезервирован для будущих операций с внешними вызовами ([money-flows.md §12](../architecture/money-flows.md#12-после-mvp-внешний-провайдер-review-асинхронные-выплаты)). В MVP он не нужен.

### 4.5. Очистка

Фоновая задача раз в час: `DELETE FROM platform.idempotency_keys WHERE id IN (SELECT id FROM … WHERE expires_at < now() LIMIT 1000)` в цикле небольшими пачками. `response_body` ограничен по размеру: в нём только тело ответа API, без больших вложений.

## 5. Шаблоны транзакций

Каждая денежная транзакция начинается с `SET LOCAL lock_timeout` и `SET LOCAL statement_timeout` (§10) и выполняется в обёртке повторов (§6). «idempotency» — шаги §4.2.

### 5.1. Перевод

```text
BEGIN RC;
 L1  idempotency (1)
 L3  SELECT id, status, currency, user_id FROM accounts.accounts
       WHERE id IN ($sender, $receiver) ORDER BY id FOR SHARE;
     проверки: владелец sender = инициатор; оба ACTIVE (BLOCKED и CLOSED не отправляют и не получают, D-26);
       валюта активна и совпадает; sender ≠ receiver; лимит на операцию
     комиссия по тарифу (0 для своих счетов)
 L4  (только перевод другому клиенту) счётчик суточного лимита отправителя:
       INSERT … ON CONFLICT DO NOTHING; SELECT … FOR UPDATE;
       used + amount > limit → transfers(FAILED, LIMIT_EXCEEDED) → finish
 L5  SELECT * FROM ledger.lock_account_balances(ARRAY[$sender, $receiver]);   -- сортирует, FOR UPDATE
     available(sender) < amount + fee → transfers(FAILED, INSUFFICIENT_FUNDS) → finish
 L6–L8  ledger.post_journal(TRANSFER, reference_id = $transfer_id, …):
       Дт sender:2100 amount+fee / Кт receiver:2100 amount / Кт 4100 fee
       (внутри: статусы FOR SHARE NOWAIT → L6 один бакет 4100 → L8 журнал и проводки)
 L8  UPDATE limit_counters SET used = used + amount;            -- строка уже наша с L4
     INSERT transfers(COMPLETED, id = $transfer_id), transfer_status_history, outbox('transfer.completed')
finish:
     UPDATE idempotency_keys → COMPLETED (ответ 201 или 422)
COMMIT;
```

Почему S-01 безопасен. Второй перевод ждёт на L5 строки отправителя. После коммита первого `FOR UPDATE` в READ COMMITTED возвращает **последнюю закоммиченную** версию строки: `available = 196 000` (10 000 ₽ − 8 000 ₽ − комиссия 40 ₽). Проверка даёт отказ. Даже при ошибке проверки в коде `post_journal` упрётся в `CHECK (available_amount >= 0)` (`PC402`).

### 5.2. Подтверждение checkout (авторизация)

```text
BEGIN RC;
 L1  idempotency (scope = покупатель)
 L2  SELECT … FROM payments.payments WHERE id = $p FOR UPDATE;
     SELECT … FROM payments.checkout_sessions WHERE payment_id = $p FOR UPDATE;
     status = CREATED, checkout ACTIVE и не истёк, токен совпадает
 L3  accounts (покупатель, магазин) FOR SHARE по id; счёт покупателя принадлежит ему; оба ACTIVE; валюта
 L4  суточный лимит покупателя
 L5  lock_account_balances([customer])
     нет средств / лимит / статус → payments CREATED→FAILED, checkout FAILED, payment_attempts(FAILED) → finish
 L7–L8  post_journal(HOLD_PLACE, hold: PLACE, expires_at = now()+7d): Дт A:2100 / Кт A:2110
       (внутри: L7 вставка holds ACTIVE → L8 журнал и проводки; бакетов нет)
 L8  UPDATE payments SET status='AUTHORIZED', customer_account_id, hold_id, version+1 WHERE id=$p AND status='CREATED'
     checkout → CONFIRMED; limit used += amount; history; payment_attempts(SUCCEEDED); outbox('payment.authorized')
finish: idempotency → COMPLETED
COMMIT;
```

### 5.3. Capture

```text
BEGIN RC;
 L1  idempotency (scope = магазин)
 L2  SELECT … FROM payments WHERE id=$p FOR UPDATE;   status = AUTHORIZED, иначе 409 INVALID_STATE
     резерв (чтение): status ACTIVE и expires_at > now(), иначе 409 AUTH_EXPIRED
     комиссия по тарифу на now(): fee >= amount → payment_attempts(CAPTURE, FAILED, FEE_EXCEEDS_PAYMENT),
       платёж и резерв не меняются → finish (422)                     -- D-27
 L3  accounts (покупатель, магазин) FOR SHARE; оба ACTIVE, иначе payment_attempts(FAILED, ACCOUNT_NOT_ACTIVE) → finish (422)
 L5  lock_account_balances([customer, merchant])
 L6–L8  post_journal(HOLD_CAPTURE, hold: CAPTURE): Дт A:2110 amount / Кт MA:2200 amount−fee / Кт 4200 fee
        (внутри: L6 один бакет 4200 → L7 UPDATE holds SET status='CAPTURED'
         WHERE id=$h AND status='ACTIVE' — 0 строк → PC409 → L8 журнал и проводки)
 L8  UPDATE payments SET status='CAPTURED', captured_amount, fee_amount, fee_tariff_id, version+1
      WHERE id=$p AND status='AUTHORIZED';                           -- CHECK fee_amount < amount
     payment_attempts(SUCCEEDED); history; outbox('payment.captured')
finish: idempotency → COMPLETED (200 или 422)
COMMIT;
```

### 5.4. Void, истечение резерва, истечение checkout

- **Void**: L1; L2 платёж (`AUTHORIZED`, иначе `409 INVALID_STATE`); L3 — только покупатель (`ACTIVE` или `BLOCKED`; статус магазина не проверяется, [D-26](../architecture/decisions.md#d-26)); L4 — возврат лимита, если период тот же; L5 — покупатель; `post_journal(HOLD_RELEASE, hold: RELEASE)` (внутри: L7 резерв `ACTIVE → RELEASED` → L8 журнал); L8 платёж → `CANCELLED (VOIDED)`, история, outbox, ключ.
- **Истечение резерва** (scheduler, несколько экземпляров):

```text
-- (a) кандидаты без блокировок
SELECT reference_id FROM ledger.holds WHERE status='ACTIVE' AND expires_at < now() ORDER BY expires_at LIMIT 100;
-- (b) для каждого — своя транзакция, строго L2 → …
BEGIN RC;
  SELECT … FROM payments WHERE id=$p FOR UPDATE;                         -- L2
  IF status <> 'AUTHORIZED' OR hold.expires_at >= now() → COMMIT;        -- уже обработан другим
  L3 покупатель FOR SHARE; L4 возврат лимита; L5 покупатель
  post_journal(HOLD_RELEASE, hold: EXPIRE)                              -- L7 → L8; покупатель ACTIVE или BLOCKED
  payments → CANCELLED (AUTH_EXPIRED); history; outbox                  -- L8
COMMIT;
```

  Второй экземпляр ждёт на L2, затем видит `status <> 'AUTHORIZED'` и выходит без эффекта.
- **Истечение checkout**: кандидаты без блокировок; затем своя транзакция: L2 (платёж + checkout); если `CREATED` и истёк, то L8 → `CANCELLED (CHECKOUT_EXPIRED)`, checkout `EXPIRED`, история, outbox. Проводок нет.

### 5.5. Возврат

```text
BEGIN RC;
 L1  idempotency (scope = магазин)
 L2  SELECT … FROM payments WHERE id=$p FOR UPDATE;      -- сериализует все возвраты платежа
     status ∈ {CAPTURED, PARTIALLY_REFUNDED}; refunded_amount + $amount ≤ captured_amount
     иначе refunds(FAILED, REFUND_EXCEEDS_CAPTURED | INVALID_STATE) → finish
 L3  accounts (покупатель, магазин) FOR SHARE; магазин ACTIVE; покупатель ACTIVE или BLOCKED (не CLOSED)
     иначе refunds(FAILED, ACCOUNT_NOT_ACTIVE) → finish
 L5  lock_account_balances([customer, merchant])
     merchant.available < amount → refunds(FAILED, MERCHANT_INSUFFICIENT_FUNDS) → finish
 L8  post_journal(REFUND, reference_id = $refund_id): Дт MA:2200 / Кт A:2100 amount
       (бакетов и резервов нет; внутри: статусы NOWAIT → L8 журнал и проводки)
     INSERT refunds(SUCCEEDED, id = $refund_id);                    -- после post_journal (правило L8 №1)
     UPDATE payments SET refunded_amount = refunded_amount + $amount,
            status = CASE WHEN refunded_amount + $amount = captured_amount THEN 'REFUNDED' ELSE 'PARTIALLY_REFUNDED' END,
            version = version + 1
      WHERE id = $p;                                       -- CHECK refunded_amount ≤ captured_amount
     history; outbox('refund.succeeded')
finish: idempotency → COMPLETED (201 или 422)
COMMIT;
```

### 5.6. Тестовое пополнение и выплата

- **Пополнение**: L1; L3 счёт клиента (`ACTIVE`, не магазинный; `BLOCKED` и `CLOSED` → `FAILED (ACCOUNT_NOT_ACTIVE)`); L4 суточный лимит пополнений; L5 клиент; `post_journal(SIM_TOPUP)`: Дт 1100 (L6: один бакет) / Кт A:2100; L8 `test_topups(COMPLETED)`, `UPDATE limit_counters`, outbox, ключ.
- **Выплата**:

```text
BEGIN RC;
 L1  idempotency (scope = магазин)
 L3  счёт магазина FOR SHARE; ACTIVE, иначе payouts(FAILED, ACCOUNT_NOT_ACTIVE) → finish
 L5  lock_account_balances([merchant]); available < amount → payouts(FAILED, INSUFFICIENT_FUNDS) → finish
     SAVEPOINT sp_payout;
 L6–L8  post_journal(PAYOUT, reference_id = $payout_id): Дт MA:2200 / Кт 1100
       (внутри: L6 все бакеты 1100 по возрастанию bucket_no, SUM ≥ amount, списание по бакетам → L8 журнал)
     PC402 SYSTEM_ACCOUNT_INSUFFICIENT → ROLLBACK TO sp_payout;      -- блокировки бакетов снимаются
       payouts(FAILED, PAYOUT_SOURCE_INSUFFICIENT); алерт → finish
 L8  INSERT payouts(COMPLETED, id = $payout_id); outbox('payout.completed')
finish: idempotency → COMPLETED (201 или 422)
COMMIT;
```

  `SAVEPOINT` нужен только здесь: проверить бакеты заранее use case не может (их блокирует функция), а отказ должен сохраниться как `FAILED`. Нехватка 1100 возможна, только если компенсации превысили доход ([chart-of-accounts.md §3](../architecture/chart-of-accounts.md#3-системные-счета-остаток-контроль-проекция)).

### 5.7. Блокировка, разблокировка и закрытие счёта

- **Блокировка**: L1; L3 `SELECT … FROM accounts WHERE id=$a FOR NO KEY UPDATE` (статус `ACTIVE`, иначе `409 INVALID_STATE`); L8 `UPDATE accounts SET status='BLOCKED', version = version + 1` (неключевые колонки, строка уже наша), `account_status_history`, audit, outbox. `FOR NO KEY UPDATE` ждёт все транзакции, держащие `FOR SHARE` (L3) на счёте. Новые денежные транзакции после него видят `BLOCKED`. Блокировка не трогает резервы и проекции: ранее созданный резерв остаётся `ACTIVE` и может быть освобождён void или истечением.
- **Разблокировка**: то же, `BLOCKED → ACTIVE`.
- **Закрытие** ([D-28](../architecture/decisions.md#d-28)): L1; L3 `SELECT … FROM accounts WHERE id=$a FOR NO KEY UPDATE` (`ACTIVE` или `BLOCKED`); L5 `lock_account_balances([$a])`; `available = 0 AND reserved = 0`, иначе `409 ACCOUNT_NOT_EMPTY`; L8 `UPDATE accounts SET status='CLOSED', closed_at = now()`, история, audit, outbox. Используется `FOR NO KEY UPDATE`, а не `FOR UPDATE`, чтобы не конфликтовать с `FOR KEY SHARE` от вставок с FK (правило L8 №3).

Почему блокировка не обходит ограничения. Операция, взявшая L3 до блокировки, завершается по правилам `ACTIVE`; после коммита блокировки любая новая операция видит `BLOCKED` под L3, а `post_journal` повторно проверяет статус по матрице ([ledger-model.md §11.4](../architecture/ledger-model.md#114-статусы-счетов-и-допустимые-операции)). Исключения матрицы только возвращают владельцу его деньги и проходят все бухгалтерские проверки.

## 6. Повторы (retry)

| SQLSTATE | Причина | Действие |
|---|---|---|
| `40001` serialization_failure | REPEATABLE READ-участки | Повтор **всей** транзакции |
| `40P01` deadlock_detected | Нарушение порядка §3 — баг | Повтор + метрика + алерт |
| `55P03` lock_not_available | `lock_timeout` | До 2 повторов, затем `409 REQUEST_IN_PROGRESS` (на L1) или `503` + `Retry-After` |
| `57014` query_canceled | `statement_timeout` | Без повтора, `503` |
| `23505` unique_violation | Бизнес-дубль | Не повторять: прочитать существующий объект → идемпотентный ответ или `409` |
| `23514` check_violation, `PC402` | Нет средств (обход проверки) | Не повторять: доменная ошибка |
| `PC403` | Статус счёта запрещает операцию (use case пропустил проверку) | Не повторять: доменный отказ `ACCOUNT_NOT_ACTIVE` + алерт (use case должен был отклонить раньше) |
| `PC400`, `PC409`, `PC460`, `P0001` | Нарушение контракта ledger, протокола блокировок или триггера | Не повторять: `500` + алерт (баг) |
| Обрыв на `COMMIT` | Исход неизвестен | Сервер не повторяет. Клиент повторяет с тем же ключом (§4.3, случай 7) |

Правила обёртки:

- повторяется функция-транзакция целиком (closure над `pgx.Tx`);
- 3–5 попыток, экспоненциальная задержка 10 → 20 → 40 мс с jitter, в пределах дедлайна `context`;
- внутри функции нет побочных эффектов вне БД — всё внешнее идёт через outbox;
- метрики: число повторов по SQLSTATE и use case, время ожидания блокировок.

## 7. Transactional outbox и inbox

Решение [D-23](../architecture/decisions.md#d-23). В MVP Kafka не обязательна: outbox в PostgreSQL + фоновый worker-диспетчер.

### 7.1. Запись события

`INSERT INTO platform.outbox_events (…, status = 'PENDING', next_attempt_at = now())` — в **той же** транзакции, что и бизнес-изменение. `seq BIGINT GENERATED ALWAYS AS IDENTITY` задаёт порядок. Внутри одного агрегата порядок `seq` совпадает с порядком коммитов, потому что все изменения агрегата сериализованы его блокировкой (L2) или созданием.

### 7.2. Relay: захват, публикация, подтверждение

```text
-- (1) ЗАХВАТ — короткая транзакция, никаких сетевых вызовов
BEGIN;
WITH batch AS (
  SELECT o.id FROM platform.outbox_events o
   WHERE o.status = 'PENDING'
     AND o.next_attempt_at <= now()
     AND (o.processing_until IS NULL OR o.processing_until < now())
     AND NOT EXISTS (                               -- порядок внутри агрегата
           SELECT 1 FROM platform.outbox_events p
            WHERE p.aggregate_id = o.aggregate_id AND p.status = 'PENDING' AND p.seq < o.seq)
   ORDER BY o.seq
   LIMIT 100
   FOR UPDATE SKIP LOCKED)
UPDATE platform.outbox_events o
   SET processing_until = now() + interval '30 seconds',
       lease_owner = $worker_id,
       attempts = attempts + 1
  FROM batch WHERE o.id = batch.id
RETURNING o.*;
COMMIT;                                              -- блокировки строк сняты

-- (2) ПУБЛИКАЦИЯ — вне транзакции, с таймаутом < lease (например, 10 с)
--     для каждого события по возрастанию seq; при ошибке события агрегата X
--     остальные события X из этой пачки не публикуются

-- (3) ПОДТВЕРЖДЕНИЕ — отдельная короткая транзакция
UPDATE platform.outbox_events
   SET status = 'PUBLISHED', published_at = now(), processing_until = NULL
 WHERE id = ANY($published_ids) AND lease_owner = $worker_id AND status = 'PENDING';

-- (4) ОШИБКА — отдельная короткая транзакция
UPDATE platform.outbox_events
   SET processing_until = NULL, lease_owner = NULL, last_error = $err,
       next_attempt_at = now() + backoff(attempts),          -- 1 с, 5 с, 30 с, 5 мин …
       status = CASE WHEN attempts >= $max_attempts THEN 'DEAD' ELSE 'PENDING' END
 WHERE id = $id AND lease_owner = $worker_id;
```

Свойства:

- Между (1) и (3) **нет открытой транзакции и нет блокировок**. Lease — это данные (`processing_until`, `lease_owner`), а не блокировка.
- **Падение relay** после (1): lease истекает через 30 с, другой экземпляр захватывает события повторно (`processing_until < now()`).
- **Падение после публикации, до (3):** событие будет опубликовано повторно (at-least-once). Дубли устраняет inbox потребителя.
- **Lease истёк во время медленной публикации:** событие может захватить другой экземпляр. Подтверждение первого (`lease_owner = $worker_id`) затронет 0 строк. Возможен дубль — это допустимо.
- `DEAD` — аналог dead letter: событие не публикуется до ручного разбора (OPERATOR, Admin Dashboard), алерт. Последующие события того же агрегата продолжают публиковаться: условие `NOT EXISTS` учитывает только `PENDING`. Это осознанный компромисс, который фиксируется в алерте.
- Отсутствие publisher (Kafka недоступна или выключена) — события копятся со статусом `PENDING`. Метрика — возраст старейшего `PENDING`.
- Очистка: `PUBLISHED` старше 7 дней удаляются пачками.

### 7.3. Publisher в MVP и с Kafka

- **MVP:** publisher — это внутренний диспетчер. Для каждого события он вызывает подписанные обработчики модулей: уведомления (in-app/email-заглушка), создание `webhook_deliveries`, обновление read-моделей отчётов. Каждый обработчик выполняет свою короткую транзакцию с inbox (§7.4). Событие подтверждается, когда все обработчики завершились успешно. Повтор после частичного успеха безопасен благодаря inbox.
- **С Kafka:** publisher — idempotent producer (`acks=all`), ключ партиции `aggregate_id`, заголовок `event_id`. Остальная схема не меняется.

### 7.4. Inbox и идемпотентность потребителя

```text
BEGIN;
  INSERT INTO platform.inbox_messages (consumer, message_id, received_at)
  VALUES ($consumer, $event_id, now()) ON CONFLICT DO NOTHING RETURNING 1;
  -- 0 строк → уже обработано → COMMIT, считать успехом (ack)
  -- 1 строка → эффект обработчика в этой же транзакции
  UPDATE platform.inbox_messages SET processed_at = now() WHERE consumer = $consumer AND message_id = $event_id;
COMMIT;   -- с Kafka: затем commit offset
```

Эффект ровно один, потому что запись inbox и эффект коммитятся атомарно. Потребитель также сравнивает `aggregate_version` из события с последней применённой версией и игнорирует устаревшие события.

### 7.5. Webhook магазинам

Отдельный worker по тому же шаблону lease: захват `webhook_deliveries` (`status = 'PENDING' AND next_attempt_at <= now()`, `FOR UPDATE SKIP LOCKED`, `processing_until`) → HTTP-вызов вне транзакции (таймаут, подпись HMAC, заголовок `PayCore-Event-Id`) → отдельная транзакция результата (`DELIVERED` / повтор с backoff / `DEAD`). Магазины дедуплицируют события по `PayCore-Event-Id`. Это требование публикуется в документации API.

## 8. Горячие точки и масштабирование

| Точка | MVP | Развитие |
|---|---|---|
| Доход 4100/4200, источник 1100 | 16 бакетов; увеличение блокирует один бакет ([D-02](../architecture/decisions.md#d-02)) | Больше бакетов (`bucket_count` в данных) |
| Уменьшение системного счёта (выплаты, корректировки) | Блокирует все бакеты счёта; операции редкие | Выплаты пачками |
| Баланс крупного магазина | Одна строка `account_balances`; блокировка на миллисекунды | Бакеты и для счетов магазинов (тогда без `account_version`) или асинхронное зачисление |
| Пул соединений | `pgxpool`, размер ≈ (ядра БД × 2–4) | PgBouncer в transaction mode (только `SET LOCAL`) |

## 9. Каталог конкурентных сценариев

Каждый сценарий — обязательный конкурентный тест ([financial-invariants-testing.md §5](../architecture/financial-invariants-testing.md#5-конкурентные-тесты)).

| ID | Сценарий | Уровни | Механизм защиты | Ожидаемый результат |
|---|---|---|---|---|
| S-01 | **Параллельные переводы**: 10 000 ₽, два перевода по 8 000 ₽ | L3, L4, L5, L6 | `FOR UPDATE` строки отправителя (L5) → проверка `available ≥ amount + fee` → `CHECK ≥ 0` в `post_journal` | Один `COMPLETED`, один `FAILED (INSUFFICIENT_FUNDS)`; остаток 196 000 коп. |
| S-02 | **Встречные переводы** A→B и B→A (и цикл A→B→C→A) | L3, L5 | Сортировка по `id` / `account_id` | Нет deadlock; оба проходят при достаточных средствах |
| S-03 | **Два capture** одного платежа (разные ключи) | L2 | `FOR UPDATE` платежа + условный переход + переход резерва `ACTIVE → CAPTURED` в функции + `UNIQUE` журнала | Один `HOLD_CAPTURE`; второй — `409 INVALID_STATE` |
| S-04 | **Capture одновременно с void** | L2 | Обе операции начинают с L2 платежа; вторая видит новый статус | Ровно один из `HOLD_CAPTURE` / `HOLD_RELEASE`; вторая — `409` |
| S-05 | **Capture одновременно с истечением резерва** | L2 | Scheduler берёт L2 и перепроверяет `status` и `expires_at`; capture проверяет `expires_at > now()` под L2 | Ровно один финальный журнал |
| S-06 | **Два частичных возврата**: 3 000 + 3 000 ₽ по платежу 5 000 ₽ | L2, L5 | L2 сериализует возвраты; проверка `refunded + amount ≤ captured`; `CHECK refunded_amount ≤ captured_amount` | Один `SUCCEEDED`, один `FAILED (REFUND_EXCEEDS_CAPTURED)`; `refunded_amount = 300 000` |
| S-07 | **Возвраты двух разных платежей** одного магазина при остатке на один | L2 (разные), L5 (общий магазин) | L5 строки магазина + `CHECK available ≥ 0` | Один `SUCCEEDED`, второй — `FAILED (MERCHANT_INSUFFICIENT_FUNDS)` |
| S-08 | **Перевод и платёж с одного счёта** одновременно | L5 | Одна строка `account_balances` покупателя для обоих | \(\sum\) списаний и резервов ≤ остатка; второй — `INSUFFICIENT_FUNDS` |
| S-09 | **Блокировка счёта во время перевода или платежа** | L3 | `FOR SHARE` (L3) операции против `UPDATE accounts … status='BLOCKED'` (режим `FOR NO KEY UPDATE`); `post_journal` перепроверяет статус под `FOR SHARE NOWAIT` | Операция закоммичена до блокировки **или** отклонена `ACCOUNT_NOT_ACTIVE`; после коммита блокировки — ни одной новой операции по запрещённым ролям матрицы ([ledger-model.md §11.4](../architecture/ledger-model.md#114-статусы-счетов-и-допустимые-операции)) |
| S-10 | **Суточный лимит**: 100 000 ₽, два параллельных перевода по 60 000 ₽ с **разных** счетов одного пользователя | L4 | Общий счётчик `(user, currency, DAILY_OUTGOING, day)` под `FOR UPDATE` | Один проходит, второй — `LIMIT_EXCEEDED` |
| S-11 | **Суточный лимит при первом запросе дня** (строки счётчика ещё нет) | L4 | `INSERT … ON CONFLICT DO NOTHING` — второй ждёт вставку первого; затем `FOR UPDATE` | Как S-10 |
| S-12 | **Повтор запроса с тем же ключом** (одновременно и последовательно) | L1 | §4.3, случаи 1, 6 | Одна операция, ответы идентичны |
| S-13 | **Тот же ключ, другое тело** | L1 | `request_hash` | `409 IDEMPOTENCY_KEY_REUSED`, второго эффекта нет |
| S-14 | **Ответ на COMMIT потерян** | L1 | §4.3, случаи 6–7 | Повтор возвращает исходный результат |
| S-15 | **Падение процесса до COMMIT** | — | Откат транзакции; ключ исчезает | Повтор выполняет операцию ровно один раз |
| S-16 | **Падение relay** между захватом и подтверждением | outbox | Lease истекает, другой экземпляр перехватывает | Событие опубликовано ≥ 1 раза; эффект у потребителя ровно один (inbox) |
| S-17 | **Два экземпляра relay** | outbox | `FOR UPDATE SKIP LOCKED` при захвате + `lease_owner` | Пачки не пересекаются; порядок внутри агрегата сохранён |
| S-18 | **Повторная доставка события** потребителю | inbox | PK `(consumer, message_id)` | Эффект ровно один |
| S-19 | **Падение scheduler** во время истечения резервов | L2 | Каждый резерв — своя транзакция; кандидаты выбираются заново | Каждый резерв освобождён ровно один раз |
| S-20 | **Повышенная нагрузка на доход 4200**: много одновременных capture разных магазинов | L6 | Увеличение — в один бакет по `hash(journal_id)` | Нет сериализации на одной строке; `SUM` бакетов = пересчёт по проводкам |
| S-21 | **Выплата одновременно с пополнениями** (1100 уменьшается и растёт) | L5, L6 | Пополнение держит один бакет 1100; выплата берёт все бакеты по возрастанию `bucket_no` и ждёт; цикла нет, потому что пополнение после бакета ничего не ждёт | Нет deadlock; каждый бакет ≥ 0; сумма 1100 = пересчёт |
| S-22 | **Смена тарифа во время capture** | — | Тариф неизменяем; выбирается по `now()` в транзакции capture; `fee_tariff_id` фиксирует версию; при `fee ≥ amount` capture отклоняется до вызова `post_journal` | Комиссия по одной конкретной версии **или** `422 FEE_EXCEEDS_PAYMENT`: резерв `ACTIVE`, платёж `AUTHORIZED`, журнала нет ([D-27](../architecture/decisions.md#d-27)) |
| S-23 | **Решение на устаревшем остатке** (реплика) | — | Проверки только на primary под L5 | Невозможно по построению |
| S-24 | **Закрытие счёта при активном резерве или остатке** | L3, L5, L8 | Закрытие: `FOR NO KEY UPDATE` счёта (L3) + чтение проекции под L5, затем `UPDATE accounts` (L8) | `409 ACCOUNT_NOT_EMPTY` ([D-28](../architecture/decisions.md#d-28)); закрытие не конфликтует с `FOR KEY SHARE` вставок с FK |
| S-25 | **Блокировка покупателя одновременно с void или истечением резерва** | L2, L3, L5, L7 | Void и истечение берут L3 `FOR SHARE` только счёта покупателя; `HOLD_RELEASE` для `BLOCKED` разрешён матрицей | Резерв освобождён ровно один раз при любом порядке коммитов; `reserved` уменьшается, `available` растёт |
| S-26 | **Блокировка покупателя одновременно с возвратом** | L2, L3, L5, L6 | Возврат берёт L3 обоих счетов; матрица: кредит `BLOCKED`-покупателя разрешён, дебет магазина — только `ACTIVE` | `SUCCEEDED` при любом порядке; при `BLOCKED`-магазине — `FAILED (ACCOUNT_NOT_ACTIVE)`, журнала нет |
| S-27 | **Блокировка магазина одновременно с capture** | L2, L3 | Capture требует `ACTIVE` у обоих счетов под L3 | Capture закоммичен до блокировки **или** `422 ACCOUNT_NOT_ACTIVE`; платёж остаётся `AUTHORIZED`, резерв можно освободить void или истечением |
| S-28 | **Прямой вызов `post_journal` в обход use case** для `BLOCKED`/`CLOSED`-счёта | — | Проверка статуса в функции под `FOR SHARE NOWAIT` | `PC403` для запрещённой комбинации; `PC460`, если вызывающий не взял L3 и строку держит смена статуса |

После MVP: «отмена перевода `ON_HOLD` одновременно с одобрением» (L2 — перевод), падение между вызовами внешнего провайдера ([money-flows.md §12](../architecture/money-flows.md#12-после-mvp-внешний-провайдер-review-асинхронные-выплаты)).

## 10. Таймауты и настройки сессии

| Параметр | Значение по умолчанию | Зачем |
|---|---|---|
| `lock_timeout` (SET LOCAL) | 3 с для API, 10 с для фоновых задач | Не висеть за чужой блокировкой; основа ответа `REQUEST_IN_PROGRESS` |
| `statement_timeout` (SET LOCAL) | 5 с для API | Защита от плохого плана |
| `idle_in_transaction_session_timeout` (роль `paycore_app`) | 30 с | Завершение зависших транзакций (§4.3, случай 9) |
| TCP keepalive (pgx) | 15 с | Обнаружение разрыва |
| `default_transaction_isolation` (роль) | `read committed` | Независимость от настроек сервера |
| Дедлайн запроса (`context`) | 5 с | Согласован с таймаутами выше |
| Lease outbox / webhook | 30 с; таймаут публикации 10 с | Таймаут меньше lease |

Значения уточняются нагрузочными тестами (k6, ТЗ §7) с учётом цели p95 ≤ 200 мс (ТЗ §8).
