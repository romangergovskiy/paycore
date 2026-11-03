# Транзакции, конкурентный доступ и защита от двойного списания

Статус: **проект**. SQL — иллюстративный псевдокод шагов, а не реализация. Связанные документы: [data-integrity.md](data-integrity.md), [../architecture/money-flows.md](../architecture/money-flows.md), [../architecture/financial-invariants-testing.md](../architecture/financial-invariants-testing.md).

## 1. Цели

1. Ни при какой интерлинии (interleaving) запросов доступный остаток не уходит ниже нуля (ТЗ §2.5: два перевода по 8 000 ₽ при 10 000 ₽).
2. Один бизнес-шаг даёт ровно один финансовый эффект при любых повторах: клиента, сервера, Kafka или воркера.
3. Отсутствие deadlock по построению (единый порядок блокировок), а при их случайном появлении — безопасный повтор.
4. Короткие транзакции: внутри транзакции нет сетевых вызовов во внешние системы.

## 2. Уровни изоляции ([D-04](../architecture/open-questions.md#d-04))

| Операция | Уровень | Механизм |
|---|---|---|
| Денежные операции (перевод, авторизация, capture, void, возврат, пополнение, выплата, корректировка) | **READ COMMITTED** | Явные `SELECT … FOR UPDATE / FOR SHARE` в фиксированном порядке и условные `UPDATE … WHERE status = …`. CHECK `>= 0` — последний рубеж |
| Изменение статуса счёта | READ COMMITTED | `UPDATE accounts` берёт блокировку строки, конфликтующую с `FOR SHARE` из денежных операций |
| Проверка инвариантов, пробный баланс, отчёты за период | **REPEATABLE READ, READ ONLY** | Один согласованный снимок на все запросы отчёта |
| Сверка проекции с ledger (I-03) | REPEATABLE READ, READ ONLY | Снимок; ложные расхождения на «живых» счетах исключаются сравнением по `account_version` |
| Лимиты на пользователя по нескольким счетам ([Q-12](../architecture/open-questions.md#q-12)) | READ COMMITTED + блокировка строки-счётчика лимита **или** SERIALIZABLE для этого use case | См. сценарий S-20 |

Почему не SERIALIZABLE везде. Он корректен «из коробки», но при конкуренции за один счёт даёт много откатов `40001`. Кроме того, поведение зависит от планов запросов (SIRead-блокировки на индексах), и под нагрузкой его трудно предсказать. Явные блокировки в READ COMMITTED дают ожидание вместо отката и прозрачны для разбора на собеседовании. Retry на `40001` всё равно реализуется (§6): он нужен для REPEATABLE READ-участков и для перехода на SERIALIZABLE, если его выберут.

Почему FOR UPDATE, а не только optimistic `version`. Оптимистичная блокировка при конкуренции на одном счёте превращается в шторм повторов. `version` сохраняется: она нужна для `account_version` в проводках ([D-16](../architecture/open-questions.md#d-16)) и для ETag в API.

Чтения для принятия решений (достаточность средств, статус) выполняются **только на primary и только под блокировкой**. Реплика годится для выписок и отчётов ([R-15](../architecture/spec-analysis.md#3-архитектурные-риски)).

## 3. Единый порядок захвата блокировок

Все use case берут блокировки строго в этом порядке. Внутри уровня — по возрастанию `id`.

| Уровень | Объект | Режим |
|---|---|---|
| L1 | `idempotency_keys` (вставка) | неявная блокировка уникального индекса |
| L2 | Бизнес-агрегат: `payments` / `transfers` / `manual_adjustments` / `payouts` | `FOR UPDATE` |
| L3 | `holds` | `FOR UPDATE` |
| L4 | `accounts` (все участники) | `FOR SHARE`, по возрастанию `id` |
| L5 | `account_balances` (все участники) | `FOR UPDATE`, по возрастанию `account_id` |
| L6 | Вставки: журнал, проводки, история, outbox, аудит; `UPDATE idempotency_keys` | — |

Системные ledger-счета не блокируются: у них нет проекции ([D-02](../architecture/open-questions.md#d-02)).

Следствия:

- Встречные переводы A→B и B→A блокируют `account_balances` в одинаковом порядке (min(id), затем max(id)), поэтому deadlock невозможен (S-07).
- Фоновые задачи (истечение резервов) **не** начинают с блокировки `holds`. Сначала они выбирают кандидатов без блокировки, затем в отдельной транзакции блокируют L2 (платёж), потом L3 (hold) и перепроверяют статус (S-05).
- Порядок проверяется code review и конкурентными тестами. В Go все блокировки берёт один компонент — Unit of Work сервиса проведения: он получает набор `account_id` и сам их сортирует.

## 4. Идемпотентность

Решение — [D-07](../architecture/open-questions.md#d-07). Два уровня:

1. **HTTP-уровень** — `platform.idempotency_keys`, заголовок `Idempotency-Key` (ТЗ §5). Обязателен для всех `POST`, которые двигают деньги или создают денежные объекты.
2. **Бизнес-уровень** — уникальные ограничения в доменных таблицах и в ledger ([data-integrity.md §3](data-integrity.md#3-уникальность-защита-от-повторов)). Работает даже при ошибке в HTTP-слое и для внутренних вызовов (воркеры, scheduler, Kafka).

### 4.1. Протокол HTTP-идемпотентности для однотранзакционных операций

Ключ записывается **в той же транзакции**, что и бизнес-эффект:

```text
BEGIN;
  INSERT INTO platform.idempotency_keys (id, scope, endpoint, key, request_hash, status, expires_at)
  VALUES (…, 'IN_PROGRESS', now() + TTL)
  ON CONFLICT (scope, endpoint, key) DO NOTHING
  RETURNING id;
  -- (a) строка вставлена → выполняем бизнес-операцию (§5)
  --     … UPDATE idempotency_keys SET status='COMPLETED', response_code, response_body, resource_id
  -- (b) конфликт → SELECT … FROM idempotency_keys WHERE (scope, endpoint, key) = (…):
  --     request_hash ≠ текущему → ROLLBACK; 422 IDEMPOTENCY_KEY_MISMATCH (ТЗ §5: «конфликт», [Q-10])
  --     status = COMPLETED     → ROLLBACK; вернуть сохранённый ответ (тот же код и тело)
  --     status = IN_PROGRESS   → ROLLBACK; 409 REQUEST_IN_PROGRESS + Retry-After
COMMIT;
```

Свойства:

- Два параллельных запроса с одним ключом: второй `INSERT` **ждёт** на уникальном индексе, пока первая транзакция не завершится. Если первая закоммитилась, второй получает конфликт и видит `COMPLETED` → повтор ответа. Если откатилась, второй вставляет ключ и выполняет операцию сам. Двойной эффект невозможен (S-02).
- Статус `IN_PROGRESS` виден снаружи только в **многотранзакционных** сценариях (внешний провайдер, §7.3). Там ключ коммитится первым шагом, а `locked_until` позволяет перехватить зависший запрос после таймаута.
- Сохраняются и бизнес-отказы (`FAILED`, `422 INSUFFICIENT_FUNDS`): повтор с тем же ключом вернёт тот же отказ, даже если средства уже появились. Технические ошибки (`5xx`, откат транзакции) не сохраняются: ключ откатывается вместе с транзакцией, и повтор выполнит операцию заново.
- `request_hash` — SHA-256 канонического JSON тела плюс значимые параметры пути. Заголовки в хеш не входят.
- `scope`: для клиента — `user:<id>`, для мерчанта — `merchant:<id>` (или `api_key:<id>`, [Q-10](../architecture/open-questions.md#q-10)).

### 4.2. Бизнес-идемпотентность внутренних шагов

- `Ledger.Post` при конфликте `UNIQUE (reference_type, reference_id, operation, operation_seq)` возвращает существующий журнал. Повторный вызов capture из воркера или саги безопасен.
- Переходы статуса — условные: `UPDATE payments SET status = 'CAPTURED', version = version + 1 WHERE id = $1 AND status = 'AUTHORIZED'`. 0 строк означает, что переход уже сделан или запрещён; решение принимается по текущему статусу.

## 5. Шаблоны транзакций

Каждая транзакция задаёт `SET LOCAL lock_timeout`, `SET LOCAL statement_timeout` (§10). Для всех шаблонов действует общая обёртка повторов (§6).

### 5.1. Перевод

```text
BEGIN ISOLATION LEVEL READ COMMITTED;
 L1  idempotency (§4.1)
 L4  SELECT id, status, currency, user_id FROM accounts
       WHERE id IN ($sender, $receiver) ORDER BY id FOR SHARE;
     проверки: владелец sender = инициатор; статусы/ограничения; валюты = currency
     лимиты (Q-12); фрод-решение (вычислено до транзакции, сохраняется здесь); комиссия по тарифу
 L5  SELECT account_id, available_amount, reserved_amount, version FROM account_balances
       WHERE account_id IN ($sender, $receiver) ORDER BY account_id FOR UPDATE;
     IF available(sender) < amount + fee → INSERT transfers(FAILED, INSUFFICIENT_FUNDS); goto finish
     ALLOW:
       UPDATE account_balances SET available_amount = available_amount - (amount+fee), version = version+1 … sender RETURNING version, available_amount;
       UPDATE account_balances SET available_amount = available_amount + amount, version = version+1 … receiver RETURNING …;
       INSERT transfers(COMPLETED), journal(TRANSFER, reference=transfer.id), 3 entries (+ account_version, balance_after)
     REVIEW:
       UPDATE sender: available -= amount+fee, reserved += amount+fee, version+1
       INSERT holds(ACTIVE), transfers(ON_HOLD, hold_id), journal(HOLD_PLACE), 2 entries, fraud_cases
 L6  INSERT transfer_status_history, outbox_events('transfer.completed' | 'transfer.on_hold' | 'transfer.failed'), audit (если требуется)
finish:
     UPDATE idempotency_keys SET status='COMPLETED', response…
COMMIT;
```

Почему защита работает (S-01): второй перевод ждёт на `FOR UPDATE` строки `account_balances` отправителя. После коммита первого он читает **актуальный** `available_amount = 196 000` (10 000 ₽ − 8 000 ₽ − комиссия 40 ₽) (в READ COMMITTED `FOR UPDATE` возвращает последнюю закоммиченную версию строки) и получает отказ. Даже если проверка в коде ошибочна, `CHECK (available_amount >= 0)` отклонит `UPDATE`.

Фрод-проверка выполняется **до** транзакции: она читает счётчики скорости и историю и не должна удерживать блокировки. Её решение — вход для транзакции. Небольшая неточность velocity-правил при гонке допустима. Если недопустима — [Q-16](../architecture/open-questions.md#q-16).

### 5.2. Авторизация платежа (внутренний счёт)

```text
BEGIN RC;
 L1  idempotency
 L2  SELECT … FROM payments WHERE id = $p FOR UPDATE;  status = CREATED, не истёк, валюта
 L4  SELECT … FROM accounts WHERE id IN ($customer_acc, $merchant_acc) ORDER BY id FOR SHARE; владелец, статусы
     фрод-решение, комиссия (фиксируется в платеже)
 L5  SELECT … FROM account_balances WHERE account_id = $customer_acc FOR UPDATE;
     IF available < amount → payments: CREATED→PENDING→FAILED (INSUFFICIENT_FUNDS), attempt FAILED
     ELSE UPDATE balance: available -= amount, reserved += amount;
          INSERT holds(ACTIVE, expires_at), journal(HOLD_PLACE), entries;
          UPDATE payments SET status='AUTHORIZED', customer_account_id, hold_id, fee_amount, fee_tariff_id, version+1
 L6  history (две смены статуса), payment_attempts, outbox('payment.authorized' | 'payment.failed')
     idempotency COMPLETED
COMMIT;
```

### 5.3. Capture

```text
BEGIN RC;
 L1 idempotency
 L2 SELECT … FROM payments WHERE id=$p FOR UPDATE;   status = AUTHORIZED, иначе 409 / идемпотентный ответ
 L3 SELECT … FROM holds WHERE id = payment.hold_id FOR UPDATE;  status = ACTIVE и expires_at > now(), иначе 409 AUTH_EXPIRED
 L4 accounts FOR SHARE (customer, merchant) по id; мерчант может принимать зачисления
 L5 account_balances FOR UPDATE (customer, merchant) по account_id
    customer: reserved -= amount; merchant: available += amount - fee
    journal(HOLD_CAPTURE, reference=hold.id), 3 entries (Дт A:2110, Кт MA:2200, Кт 4200)
    holds → CAPTURED; payments → CAPTURED, captured_amount = amount
 L6 history, outbox('payment.captured'), idempotency
COMMIT;
```

### 5.4. Void и истечение авторизации

Void — как capture до L3, затем L5 только для счёта клиента: `reserved -= amount, available += amount`; журнал `HOLD_RELEASE`; hold → `RELEASED`; платёж → `CANCELLED`.

Истечение (scheduler):

```text
-- без транзакции/блокировок: кандидаты
SELECT id, reference_type, reference_id FROM holds WHERE status='ACTIVE' AND expires_at < now() LIMIT 100;
-- для каждого — отдельная транзакция в порядке L2 → L3:
BEGIN RC;
  SELECT … FROM payments WHERE id = $ref FOR UPDATE;          -- L2
  SELECT … FROM holds WHERE id = $hold FOR UPDATE;            -- L3
  IF hold.status <> 'ACTIVE' OR hold.expires_at >= now() → COMMIT (ничего не делать)
  … как void, hold → EXPIRED, payment → CANCELLED(AUTH_EXPIRED)
COMMIT;
```

Несколько экземпляров scheduler безопасны: второй экземпляр ждёт на L2, затем видит `status <> 'ACTIVE'` и выходит. Если важна производительность, кандидатов можно выбирать с `FOR UPDATE SKIP LOCKED` в **отдельной короткой** транзакции, которая только «бронирует» пачку (колонка `processing_until`), а не держит блокировку.

### 5.5. Возврат (частичный и полный)

```text
BEGIN RC;
 L1 idempotency (scope мерчанта)
 L2 SELECT … FROM payments WHERE id=$p FOR UPDATE;     -- сериализует ВСЕ возвраты одного платежа
    status ∈ {CAPTURED, PARTIALLY_REFUNDED}; refunded_amount + $amount ≤ captured_amount
    иначе INSERT refunds(FAILED, REFUND_EXCEEDS_CAPTURED | INVALID_STATE)
 L4 accounts FOR SHARE (customer, merchant)
 L5 account_balances FOR UPDATE (customer, merchant) по account_id
    merchant.available < amount → refunds(FAILED, MERCHANT_INSUFFICIENT_FUNDS)  (D-10 A)
    иначе merchant.available -= amount; customer.available += amount;
          INSERT refunds(SUCCEEDED), journal(REFUND, reference=refund.id), entries
          UPDATE payments SET refunded_amount += amount, status = (= captured ? REFUNDED : PARTIALLY_REFUNDED), version+1
 L6 history, outbox('refund.succeeded'), idempotency
COMMIT;
```

`CHECK (refunded_amount <= captured_amount)` — страховка на случай, если проверку в коде обойдут (S-06).

### 5.6. Блокировка счёта

`UPDATE accounts SET status='BLOCKED' WHERE id=$a AND status='ACTIVE'` + `account_status_history` + audit + outbox. Этот `UPDATE` ждёт завершения всех транзакций, которые держат `FOR SHARE` на этом счёте, а новые денежные транзакции после него увидят `BLOCKED` (S-08). Что делать с активными резервами заблокированного счёта (capture разрешён?) — [Q-14](../architecture/open-questions.md#q-14).

## 6. Повторы (retry)

| SQLSTATE | Причина | Действие |
|---|---|---|
| `40001` serialization_failure | REPEATABLE READ / SERIALIZABLE | Повтор **всей** транзакции |
| `40P01` deadlock_detected | Нарушение порядка блокировок (баг) или внешняя причина | Повтор + метрика + алерт (deadlock — сигнал бага в порядке) |
| `55P03` lock_not_available | Сработал `lock_timeout` | Ограниченный повтор, затем `503` с `Retry-After` |
| `57014` query_canceled | `statement_timeout` | Без повтора, `503` |
| `23505` unique_violation | Идемпотентность / бизнес-дубль | **Не** повтор: прочитать существующий объект, вернуть идемпотентный ответ или `409` |
| `23514` check_violation | Например `available >= 0` | Не повтор: доменная ошибка по имени ограничения |
| `P0001`, `P0002` | Триггеры ledger | Не повтор: это баг. `500` + алерт |
| Обрыв соединения на `COMMIT` | Исход неизвестен | Сервер не повторяет. Клиент повторяет с тем же `Idempotency-Key` (S-10) |

Правила обёртки:

- Повторяется **функция-транзакция целиком** (closure над `pgx.Tx`), а не отдельный запрос.
- До 3–5 попыток, экспоненциальная задержка (например, 10 → 20 → 40 мс) с jitter, в пределах дедлайна `context`.
- Внутри функции нет побочных эффектов вне БД: ни HTTP, ни Kafka, ни отправки email. Всё внешнее — только через outbox.
- Повтор безопасен, потому что откат транзакции откатывает и ключ идемпотентности, и все эффекты.
- Метрики: число повторов по SQLSTATE и use case, время ожидания блокировок.

## 7. Внешние вызовы и transactional outbox

### 7.1. Outbox (ТЗ §4.7, §8)

- Событие вставляется в `outbox_events` **в той же транзакции**, что и бизнес-изменение. Нет коммита — нет события, есть коммит — событие точно будет опубликовано (at-least-once).
- Relay (воркер):

```text
BEGIN;
  SELECT … FROM outbox_events WHERE published_at IS NULL
   ORDER BY created_at LIMIT 100 FOR UPDATE SKIP LOCKED;
  -- публикация в Kafka: idempotent producer, acks=all, key = aggregate_id
  UPDATE outbox_events SET published_at = now() WHERE id = ANY($ids);
COMMIT;
```

- Kafka недоступна: публикация падает, транзакция откатывается, события остаются в outbox (ТЗ §10, S-12). Растёт метрика «возраст старейшего неопубликованного события».
- Воркер упал после публикации, но до коммита: событие будет опубликовано повторно. Дубли устраняет inbox на стороне потребителя (ТЗ §10 «Перезапуск Worker»).
- Порядок. `SKIP LOCKED` с несколькими relay может переставить события одного агрегата. Варианты: один relay; шардирование relay по `hash(aggregate_id) % N`; потребители, устойчивые к перестановке (версия агрегата в событии). Рекомендация — один relay в MVP, `aggregate_version` в payload ([Q-26](../architecture/open-questions.md#q-26)).
- До подключения Kafka (по roadmap — месяц 6) outbox всё равно пишется. Временный relay может вызывать обработчики (уведомления, webhook) напрямую из БД.

### 7.2. Inbox

```text
BEGIN;
  INSERT INTO inbox_messages (consumer, message_id) VALUES ($c, $event_id) ON CONFLICT DO NOTHING;
  -- 0 строк → уже обработано → COMMIT, ack
  -- 1 строка → обработка (свои таблицы) в этой же транзакции
COMMIT;  -- затем commit offset в Kafka
```

### 7.3. Вызов внешнего провайдера (если подтвердится [Q-03](../architecture/open-questions.md#q-03))

Шаблон «намерение → вызов → применение»:

1. **ТХ1:** idempotency (`IN_PROGRESS`), `payment_attempts(STARTED)`, платёж → `PENDING`. COMMIT.
2. Вызов провайдера **вне транзакции**, с idempotency key = `payment_attempts.id` и таймаутом.
3. **ТХ2:** L2 платёж `FOR UPDATE`; попытка всё ещё `STARTED`; применить результат (проводки, статусы), idempotency → `COMPLETED`. COMMIT.
4. Таймаут или обрыв на шаге 2: попытка → `UNKNOWN`. Задача восстановления запрашивает у провайдера статус по `attempts.id` и выполняет ТХ2. Повторный вызов провайдера с **новым** ключом запрещён.
5. Расхождения, оставшиеся после восстановления, ловит Reconciliation.

## 8. Горячие точки и масштабирование

| Точка | Решение в MVP | Развитие |
|---|---|---|
| Системные счета (доход, банк) | Без синхронной проекции — без блокировок ([D-02](../architecture/open-questions.md#d-02)) | Снапшоты по дням |
| Баланс крупного мерчанта | Блокировка строки на время транзакции (миллисекунды). Порядок L5 сохраняется | Субсчета-шарды мерчанта (N строк, зачисление в случайный шард, сумма — при чтении) или асинхронное зачисление через внутреннюю очередь |
| `account_version` на мерчанте | Требует строгой сериализации зачислений — следствие D-16 | При шардировании версия — на каждый шард |
| Пул соединений | `pgxpool`; размер ≈ (ядра БД × 2–4); транзакции короткие | PgBouncer в transaction mode (без session-фич, без `SET` вне `SET LOCAL`) |

## 9. Каталог конкурентных сценариев

Каждый сценарий — обязательный конкурентный тест ([financial-invariants-testing.md §5](../architecture/financial-invariants-testing.md#5-конкурентные-тесты)).

| ID | Сценарий | Защита | Ожидаемый результат |
|---|---|---|---|
| S-01 | Баланс 10 000 ₽, два параллельных перевода по 8 000 ₽ (ТЗ) | L5 `FOR UPDATE` + проверка + `CHECK >= 0` | Ровно один `COMPLETED`, один `FAILED (INSUFFICIENT_FUNDS)`; остаток 196 000 коп. (комиссия 0,5 % = 4 000) |
| S-02 | Два одинаковых запроса с одним `Idempotency-Key` одновременно | Уникальный индекс `idempotency_keys` + ключ в той же ТХ | Один перевод; оба ответа идентичны |
| S-03 | Тот же ключ, другое тело | `request_hash` | `422`/`409`, второго эффекта нет |
| S-04 | Два параллельных capture одного платежа (разные ключи) | L2 `FOR UPDATE` + условный переход + `UNIQUE` журнала | Один `HOLD_CAPTURE`; второй — `409 INVALID_STATE` |
| S-05 | Capture одновременно с void или с истечением | L2 → L3, перепроверка статуса hold | Выигрывает первый, второй — `409`; ровно один журнал capture или release |
| S-06 | Два параллельных частичных возврата 3 000 + 3 000 по платежу 5 000 | L2 `FOR UPDATE` + `CHECK refunded ≤ captured` | Один `SUCCEEDED`, один `FAILED`; `refunded_amount = 300 000` |
| S-07 | Встречные переводы A→B и B→A | Сортировка L5 | Нет deadlock; оба выполнены (если средств хватает) |
| S-08 | Блокировка счёта во время перевода | `FOR SHARE` (L4) против `UPDATE accounts` | Перевод либо завершён до блокировки, либо отклонён `ACCOUNT_BLOCKED` |
| S-09 | Перевод и авторизация платежа одновременно с одного счёта | Одна строка L5 | Сумма списаний и резервов ≤ остатку |
| S-10 | Ответ на `COMMIT` потерян, клиент повторяет | HTTP-идемпотентность | Повтор возвращает исходный результат, второго списания нет |
| S-11 | Падение процесса между ТХ1 и ТХ2 (провайдер) | `attempts(STARTED/UNKNOWN)` + recovery | Платёж доведён до финального статуса один раз |
| S-12 | Kafka недоступна | Outbox | События копятся, после восстановления публикуются |
| S-13 | Два relay одновременно | `FOR UPDATE SKIP LOCKED` | Каждое событие опубликовано (≥ 1 раз), без взаимной блокировки |
| S-14 | Повторная доставка события потребителю | Inbox PK | Эффект ровно один |
| S-15 | Смена тарифа во время операции | Снимок `fee_tariff_id`; тариф неизменяем | Используется тариф, действовавший на момент фиксации |
| S-16 | Решение на устаревшем балансе (реплика) | Проверки только на primary под L5 | Невозможно по построению |
| S-17 | Два `POST /payments` мерчанта с одним ключом | `UNIQUE (merchant_id, idempotency_key)` + HTTP-слой | Один платёж |
| S-18 | Отмена перевода `ON_HOLD` клиентом одновременно с одобрением аналитиком | L2 `FOR UPDATE` transfers + L3 hold | Ровно один из `HOLD_CAPTURE` / `HOLD_RELEASE` |
| S-19 | Закрытие счёта при активном резерве или ненулевом остатке | L4/L5 + проверка в use case закрытия | Отказ ([Q-13](../architecture/open-questions.md#q-13)) |
| S-20 | Суточный лимит 100 000 ₽, два параллельных перевода по 60 000 ₽ с разных счетов одного пользователя | Блокировка строки счётчика лимита пользователя (до L4) или SERIALIZABLE | Один проходит, второй — `LIMIT_EXCEEDED` |

Если блокировка счётчика лимита (S-20) принята, это уровень **L1.5** — между idempotency и бизнес-агрегатом. Тогда порядок в §3 нужно дополнить.

## 10. Таймауты и настройки сессии

| Параметр | Предложение | Зачем |
|---|---|---|
| `lock_timeout` (SET LOCAL) | 2–3 с для API, 10 с для фоновых задач | Не висеть бесконечно за чужой блокировкой |
| `statement_timeout` (SET LOCAL) | 5 с для API | Защита от плохого плана |
| `idle_in_transaction_session_timeout` (роль) | 10–30 с | Утечка транзакции в коде |
| `default_transaction_isolation` (роль) | `read committed` (явно) | Не зависеть от настроек сервера |
| Дедлайн запроса (Go `context`) | ≈ p99-бюджет (например, 2 с) | Согласован с `lock_timeout` |

Значения нужно уточнить нагрузочными тестами (k6, ТЗ §7) с учётом цели p95 ≤ 200 мс (ТЗ §8).
