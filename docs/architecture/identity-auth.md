# Identity и Auth — архитектура и контракты

Статус: **принято для MVP**. Контракт — этап 3.19; DDL — `migrations/00005_auth.sql` (этап 3.20); domain и application — `internal/auth` (этап 3.21); семантика транзакций — `Decision` Commit/Rollback (этап 3.21.1); PostgreSQL-адаптеры портов — `internal/platform/postgres` (этап 3.22); сериализация Grant/Revoke ADMIN — `pg_advisory_xact_lock(0x5041590000000001)` (этап 3.23); Lock/Unlock/DisableUser и last-ADMIN lockout — этап 3.24; Argon2id / JWT HS256 / opaque refresh / UTC clock и `auth.user_status_history` — этап 3.25. HTTP Auth — этап 3.26 (`internal/auth/http`, Bearer, `app.Run`); PostgreSQL HTTP — 3.26.6. Bootstrap ADMIN нет. Решения — [D-41](decisions.md#d-41), [D-42](decisions.md#d-42), [D-43](decisions.md#d-43). Ошибки API — [D-39](decisions.md#d-39). Скоуп и отказ от PCI/KYC — [D-20](decisions.md#d-20). Стек без Redis — [D-38](decisions.md#d-38).

Связанные документы: [../PROJECT_SPEC.md](../PROJECT_SPEC.md) §2.1, §5, §8; [decisions.md](decisions.md); [../database/database-schema.md](../database/database-schema.md) §12; [../database/er-diagram.md](../database/er-diagram.md) §4; [../database/data-integrity.md](../database/data-integrity.md).

Текущий код: `00002` создаёт заглушку `auth.users (id)` ради FK `accounts.user_id`; `00005` расширяет её колонками и таблицами §7 **без DROP**; `00006` добавляет `auth.user_status_history`. Domain и application — `internal/auth/{domain,application}` (без pgx, HTTP, JWT, Argon2). Инфраструктура Hasher/AccessSigner/RefreshIssuer/Clock — `internal/platform/authcrypto`. PostgreSQL-адаптеры портов и Auth UoW — `internal/platform/postgres` (один `pgx.Tx` на `Do`, `Decision` решает Commit). Grant/Revoke роли ADMIN и мутации статуса, которые меняют число ACTIVE ADMIN: `RoleStore.LockAdminRoleMutations` → `pg_advisory_xact_lock(0x5041590000000001)` в той же Tx **до** `GetForUpdate` строки user, затем re-check инициатора ACTIVE+ADMIN, затем count/status. HTTP — `internal/platform/httpserver` (health, JSON-ошибки D-39, Bearer/RequireRole) и `internal/auth/http` (register/login/refresh/logout). Auth собирается в `app.Run` при ненулевом пуле. Accounts принимает `ChangedBy uuid.UUID` как есть и не читает auth-контекст. Первый ADMIN — будущая Go-команда migrate/bootstrap, не SQL-сид.

---

## 1. Границы

**Модуль `auth` владеет:** идентичностью пользователя, паролем, сессией, токенами, прикладными ролями, попытками входа, счётчиками rate limit, аудитом auth-событий.

**Модуль не владеет и не вызывает:** Ledger, `post_journal`, балансы, выписку, Open/Block/Close, Payments, Transfers, симулятор, комиссии, лимиты денежных операций, HTTP money. Деньги по-прежнему двигает только ledger-шлюз ([D-03](decisions.md#d-03), [D-40](decisions.md#d-40)).

**Не путать роли.**

| Слой | Примеры | Документ |
|---|---|---|
| Роли БД | `paycore_app`, `paycore_ledger`, `paycore_owner` | [D-33](decisions.md#d-33) |
| Прикладные роли | `CUSTOMER`, `ADMIN`, … | [D-42](decisions.md#d-42), §6 |
| Ledger `owner_type` | `CUSTOMER` / `MERCHANT` / `SYSTEM` | [ledger-model.md](ledger-model.md) |

Прикладной `ADMIN` работает через `paycore_app`, как все остальные. Он не получает роль `paycore_owner` и не обходит `post_journal`.

**Интеграция с Accounts (контракт, без кода здесь).** Когда появится HTTP счетов, `ChangedBy` / владелец — `Principal.UserID` из auth-контекста. Сейчас Open/Status по-прежнему принимают UUID в команде. FK `accounts.user_id → auth.users(id)` уже есть; `changed_by` остаётся UUID без FK (как в 00002).

**Мерчанты.** Таблица `merchants.merchants` — заглушка PK. Роль `MERCHANT` выдаёт будущий модуль merchants при создании магазина, не регистрация. `merchant_members` в этом этапе не реализуются.

---

## 2. Сущности

| Сущность | Таблица | Смысл |
|---|---|---|
| User | `auth.users` | Учётная запись. Email канонический (lowercase). Пароль — только Argon2id. |
| Role | `auth.roles` | Справочник кодов из ТЗ §2.1. Сид, не самообслуживание. |
| UserRole | `auth.user_roles` | Назначение роли. Несколько ролей на пользователя допустимы. |
| Session | `auth.sessions` | Факт входа с устройства. Отзыв сессии сразу делает access и refresh недействительными. |
| RefreshToken | `auth.refresh_tokens` | Одноразовый opaque-токен, в БД только SHA-256. Ротация через `replaced_by`. |
| LoginAttempt | `auth.login_attempts` | Журнал попыток (успех и отказ). Append-only для разбора. |
| RateLimitCounter | `auth.rate_limit_counters` | Окна лимитов в PostgreSQL. Redis нет ([D-38](decisions.md#d-38)). |
| ActionConfirmation | `auth.action_confirmations` | Заготовка под коды подтверждения. В MVP не используется (нет уведомлений). |

Access token — не строка таблицы: это JWT, проверяемый по подписи и по живой сессии ([D-41](decisions.md#d-41)).

---

## 3. Жизненные циклы

### 3.1. Пользователь

```
регистрация → ACTIVE
ACTIVE → LOCKED     (ADMIN или автоматическая блокировка после 10 неуспешных входов по этому user_id)
LOCKED → ACTIVE     (только ADMIN, Unlock)
ACTIVE → DISABLED   (только ADMIN)
DISABLED → ACTIVE   (только ADMIN)
```

`DISABLED` и `LOCKED` не входят. Регистрация не создаёт `PENDING_VERIFICATION`: подтверждения email в MVP нет.

Последний ACTIVE ADMIN нельзя перевести в LOCKED или DISABLED: `LockUser` / `DisableUser` → `ErrLastAdmin`, Rollback, статус не меняется. Авто-lockout после 10 неуспехов того же пользователя тоже не ставит LOCKED, если это последний ACTIVE ADMIN (fail count и attempt коммитятся, ответ login — `INVALID_CREDENTIALS`). `UnlockUser` — только `LOCKED` → `ACTIVE` (сброс `failed_login_count`); `DISABLED` → `ACTIVE` этим use case не выполняется.

Удаления пользователей нет: статус, не `DELETE`. FK со счетов без каскада.

### 3.2. Сессия

```
login (успех) → сессия жива, issued refresh
refresh       → тот же session_id, новый refresh, старый replaced_by
logout / revoke session / reuse refresh / DISABLE|LOCK user → revoked_at = now()
```

Живая сессия: `revoked_at IS NULL` и `user.status = 'ACTIVE'`. Истёкший refresh не отзывает сессию сам по себе: access действует до `exp` **или** до отзыва сессии (что раньше). `last_seen_at` обновляется при успешном refresh; на каждый access не пишем (лишняя нагрузка).

### 3.3. Refresh

Состояния строки: issued → used (replaced) | revoked | expired.

- Успешный refresh: `used_at = now()`, вставка нового токена, `replaced_by` на новый id.
- Повтор used/revoked токена: вся сессия отзывается, все её refresh получают `revoked_at`. Ответ `TOKEN_REUSED`.
- Истёкший неиспользованный: `TOKEN_EXPIRED`, сессия не отзывается.

### 3.4. Роль

Регистрация всегда вставляет ровно одну строку `user_roles (CUSTOMER)` с `granted_by IS NULL`. Дальше только `GrantRole` / `RevokeRole` от ADMIN, не от самого пользователя. Снять себе `ADMIN` нельзя. Снять последнюю ACTIVE `ADMIN` нельзя (проверка count после lock). Lock/Disable последней ACTIVE `ADMIN` — та же ошибка. Два параллельных Revoke/Lock/Disable/lockout ADMIN сериализуются advisory-блокировкой §10, не `FOR UPDATE` на `auth.roles`.

---

## 4. Токены (одна стратегия MVP)

Выбрано [D-41](decisions.md#d-41): **короткий JWT access + opaque refresh**. Других стратегий в MVP нет.

### 4.1. Access — JWT HS256

| Поле | Значение |
|---|---|
| Алгоритм | HS256 |
| Секрет | `PAYCORE_AUTH_JWT_SECRET`, ≥ 32 байт; пустой секрет — отказ старта (код 2) |
| TTL | **15 минут** |
| Заголовок | `Authorization: Bearer <jwt>` |

Claims:

| Claim | Содержание |
|---|---|
| `iss` | `paycore` |
| `sub` | `user_id` (UUID) |
| `sid` | `session_id` (UUID) |
| `roles` | кэш на TTL access (15 мин). Источник истины — `user_roles`, читаемый `ValidateSession` |
| `iat`, `nbf`, `exp` | UTC |
| `jti` | UUID токена; в БД не хранится |

Проверка на защищённом маршруте:

1. Подпись и `exp`/`nbf`/`iss`.
2. `SELECT` сессии по `sid`: не найдена или `revoked_at IS NOT NULL` → `UNAUTHORIZED` / `INVALID_TOKEN`.
3. Пользователь `ACTIVE`. Иначе `INVALID_TOKEN` (не светить `ACCOUNT_LOCKED` на каждом API-вызове).
4. `ValidateSession` проверяет живую сессию и `ACTIVE` пользователя и **читает роли из `user_roles`**, не из JWT claims (вариант A). В контекст кладётся `Principal{UserID, SessionID, Roles}` с текущими ролями. JWT `roles` — кэш не дольше 15 минут. **Grant/Revoke не отзывают сессии:** новые права видны на следующем `ValidateSession` или после refresh. HTTP middleware (следующий этап) после проверки подписи вызывает `ValidateSession`.

Logout эффективен сразу: шаг 2 отсекает JWT до истечения `exp`. Отдельного denylist `jti` нет.

### 4.2. Refresh — opaque

- 32 криптослучайных байта, клиенту — base64url **один раз**.
- В БД: `token_hash bytea UNIQUE` = SHA-256 от сырых байт (не от base64-строки).
- TTL **30 суток** с `issued` (`expires_at`).
- Передача: JSON-тело `POST /api/v1/auth/refresh` `{ "refresh_token": "..." }`, не cookie. Cookie-сессии в MVP нет.
- Старый токен после ротации хранится (чтобы поймать reuse), не удаляется.

Клиент хранит оба токена сам. Refresh не кладётся в access-логи и в `audit_logs.after_state`.

---

## 5. Пароли — Argon2id

Хранится только PHC-строка. CHECK: `password_hash LIKE '$argon2id$%'`.

| Параметр | Значение |
|---|---|
| Алгоритм | Argon2id |
| Память | 19 MiB (`m=19456`) |
| Итерации | 2 |
| Параллелизм | 1 |
| Соль | 16 байт |
| Tag | 32 байта |

Политика пароля (до хеша): длина 12…128, UTF-8. Состав (цифры/регистр) не требуем. Пароль длиннее 128 — `INVALID_REQUEST`, не хешируем (защита CPU).

Сравнение: `argon2.IDKey` / постоянное время библиотеки. Если пользователя нет — сравнение с заранее посчитанным dummy-хешем, затем всё равно `INVALID_CREDENTIALS`.

Смена пароля (аутентифицированный `POST /api/v1/auth/password`): текущий пароль + новый. Успех отзывает **все** сессии пользователя, включая текущую; клиент логинится заново. Сброс по email в MVP нет ([open-questions.md Q-31](open-questions.md#q-31)).

---

## 6. RBAC

Источник ролей — ТЗ §2.1. Назначение — [D-42](decisions.md#d-42).

| Роль | Что можно | Чего нельзя |
|---|---|---|
| `CUSTOMER` | Свои счета, свои переводы, свой checkout, своё тестовое пополнение | Чужие ресурсы; админ-API |
| `MERCHANT` | Операции своего магазина (платежи, выплата, ключи) — **когда появится модуль merchants** | Счета чужих магазинов; перевод «как клиент», пока нет роли CUSTOMER |
| `SUPPORT` | Чтение ограниченного профиля и статусов (будущие обращения) | Движение денег; grant ролей |
| `OPERATOR` | Создать заявку `manual_adjustments` ([D-31](decisions.md#d-31)); операционные очереди платежей | Утвердить корректировку; провести журнал напрямую; переводы |
| `RISK_ANALYST` | Чтение подозрительных операций и аудита | Движение денег; grant |
| `ADMIN` | Пользователи (lock/unlock/disable), grant/revoke ролей, утверждение корректировки, block/unblock/close счёта по админ-API, чтение аудита | **Самостоятельно двигать деньги:** transfer, capture, refund, payout, sim-topup, прямой DML баланса. Утвердить свою же заявку (`approved_by <> created_by`) |

Проверка всегда **роль + владение ресурсом**. `ADMIN` не обходит владение для денежных use case: этих use case у роли просто нет. Если у одного пользователя `CUSTOMER` и `ADMIN`, перевод проходит только как CUSTOMER по **своим** счетам — это не «бесконтрольное» перемещение.

Регистрация:

- тело без `roles`; поле `roles` → `INVALID_REQUEST`;
- сервер не читает заголовок «выдай мне ADMIN»;
- единственная вставляемая роль — `CUSTOMER`.

Первый ADMIN — будущая Go-команда migrate/bootstrap (`PAYCORE_BOOTSTRAP_ADMIN_PASSWORD` только там, не в SQL и не в API-процессе). `00005` не вставляет пользователей. Публичного «стань ADMIN» нет.

---

## 7. SQL-схема

Существующий `CREATE TABLE auth.users (id UUID PRIMARY KEY)` **не дропается**: на него уже ссылаются счета и тесты. Миграция `00005_auth.sql` делает `ALTER TABLE` и создаёт остальные таблицы. Goose Down отказывается, если в `users` / `user_roles` / `sessions` / `refresh_tokens` / `login_attempts` / `rate_limit_counters` / `action_confirmations` есть строки; каталог `roles` на Down удаляется вместе с таблицей. После появления боевых пользователей Down не применять.

### 7.1. `auth.users`

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK, UUIDv7 из Go ([D-14](decisions.md#d-14)) |
| email | TEXT NN | канонический lowercase; `CHECK (email = lower(email))`; UNIQUE INDEX `uq_users_email` ON `(email)` |
| password_hash | TEXT NN | `CHECK (password_hash LIKE '$argon2id$%')` |
| first_name, last_name | TEXT | длина 1…100, если не NULL |
| status | TEXT NN | `ACTIVE`, `LOCKED`, `DISABLED` |
| failed_login_count | INT NN | `>= 0`; сбрасывается на успешном login |
| locked_at | TIMESTAMPTZ | NOT NULL ⇔ `LOCKED` |
| created_at, updated_at | TIMESTAMPTZ NN | UTC ([D-29](decisions.md#d-29)) |

`GRANT SELECT, INSERT, UPDATE` → `paycore_app`; `SELECT` → `paycore_readonly`. `DELETE` нет. `GRANT USAGE ON SCHEMA auth` → `paycore_app`, `paycore_readonly` (`paycore_ledger` USAGE не получает). Фикстуры тестов заполняют `email`, `password_hash` (`$argon2id$…`) и `status`; CHECK не ослабляется.

### 7.2. `auth.roles`

`code TEXT PK` (`CUSTOMER`, `MERCHANT`, `SUPPORT`, `OPERATOR`, `RISK_ANALYST`, `ADMIN`), `description TEXT NN`. Сид шести строк. `paycore_app`: `SELECT` only.

### 7.3. `auth.user_roles`

PK `(user_id, role_code)`; FK → users, roles; `granted_by UUID` FK users NULL; `granted_at TIMESTAMPTZ NN`. `CHECK (granted_by IS DISTINCT FROM user_id OR role_code = 'CUSTOMER')` **не** ставим: ADMIN может выдать CUSTOMER себе же; самовыдачу привилегий запрещает приложение, не CHECK. `paycore_app`: `SELECT, INSERT, DELETE` (revoke = DELETE строки назначения; аудит пишется отдельно).

### 7.4. `auth.sessions`

`id UUID PK`, `user_id FK NN`, `user_agent TEXT`, `ip INET`, `created_at`, `last_seen_at`, `revoked_at`, `revoke_reason TEXT` (`LOGOUT`, `LOGOUT_ALL`, `PASSWORD_CHANGED`, `TOKEN_REUSE`, `USER_LOCKED`, `USER_DISABLED`, `ADMIN_REVOKE`). Живая: `revoked_at IS NULL`. Индекс `(user_id) WHERE revoked_at IS NULL`.

### 7.5. `auth.refresh_tokens`

| Колонка | Тип | Ограничения |
|---|---|---|
| id | UUID | PK |
| session_id | UUID NN | FK sessions |
| token_hash | BYTEA NN | UNIQUE; длина 32 (SHA-256) |
| replaced_by | UUID | FK refresh_tokens(id), NULL пока не использован |
| expires_at | TIMESTAMPTZ NN | `> created_at` |
| used_at, revoked_at, created_at | TIMESTAMPTZ | used ⇔ replaced_by NOT NULL |

Индекс `(session_id)`. Триггер/CHECK: не обновлять `token_hash`. Очистка истекших used-токенов старше 30 дней — отдельная задача, `DELETE` только на эту таблицу (как outbox), не на users.

### 7.6. `auth.login_attempts`

Append-only: `id`, `email TEXT NN` (как введено, для разбора), `user_id UUID` FK NULL, `ip INET`, `success BOOLEAN NN`, `failure_reason TEXT` (`INVALID_CREDENTIALS`, `RATE_LIMITED`, `LOCKED`, `DISABLED`, `INVALID_REQUEST`), `attempted_at`. Индексы: `(email, attempted_at DESC)`, `(ip, attempted_at DESC)` — уже заложены в [data-integrity.md §5.4](../database/data-integrity.md#54-инфраструктура). `paycore_app`: `SELECT, INSERT`. Триггер запрета UPDATE/DELETE — как audit.

### 7.7. `auth.rate_limit_counters`

PK `(bucket, counter_key, window_start)`; `count INT NN CHECK >= 0`; `updated_at`.

`bucket TEXT`: `LOGIN_EMAIL`, `LOGIN_IP`, `REGISTER_IP`, `REFRESH_IP`.

`window_start` — `date_bin('15 minutes', now(), TIMESTAMPTZ '1970-01-01')` (UTC). UPSERT: `count = count + 1`.

### 7.8. `auth.action_confirmations`

Как в ER: `user_id`, `action_type`, `code_hash BYTEA`, `attempts_left`, `expires_at`, `confirmed_at`. В MVP таблица может быть создана пустой и не вызываться. Не блокирует регистрацию/login.

### 7.9. Аудит

Проектный `audit.audit_logs` **ещё не создан** (миграций схемы `audit` нет). События `user.registered`, `user.login_succeeded` (без пароля и токенов), `user.login_failed` (без различия «нет такого email» в payload), `session.revoked`, `role.granted`, `role.revoked` остаются на будущий модуль audit. Хеш-цепочка — [Q-20](open-questions.md#q-20). Смена статуса пользователя в 3.25 пишется в §7.10, не в audit ([D-43](decisions.md#d-43)).

### 7.10. `auth.user_status_history`

Append-only: `id`, `user_id` FK, `changed_by` FK, `previous_status`, `new_status`, `reason` (1…500), `created_at`. CHECK: статусы из `{ACTIVE,LOCKED,DISABLED}`, `previous_status <> new_status`. Индекс `(user_id, created_at DESC)`. `paycore_app`: `SELECT, INSERT`. Триггер запрета UPDATE/DELETE/TRUNCATE. Пишет только `LockUser` / `UnlockUser` / `DisableUser` в той же Tx; no-op и `ErrLastAdmin` — без строки. Login lockout сюда не пишет.

Auth **не** использует L1…L8 [concurrency.md](../database/concurrency.md). Короткие транзакции, блокировки только строк auth (`users` / `refresh_tokens` / счётчик). Захватывать `account_balances` (L5) из auth запрещено.

---

## 8. Application-контракты

Пакет `internal/auth/{domain,application}` (имя схемы `auth`, [D-38](decisions.md#d-38)). Не импортирует `pgx` и `net/http`. Пустых пакетов «на будущее» в этом этапе нет.

```text
type Principal struct {
    UserID    uuid.UUID
    SessionID uuid.UUID
    Roles     []Role
}

func (p Principal) Has(Role) bool
func (p Principal) IsActiveCustomer() bool // Has(CUSTOMER) — для денежных use case позже
```

Команды (идея; реализация не здесь):

| Use case | Вход | Успех | Ошибки |
|---|---|---|---|
| Register | Email, Password, FirstName, LastName, IP | User (без hash) | `INVALID_REQUEST`, `EMAIL_TAKEN`, `AUTH_RATE_LIMITED` |
| Login | Email, Password, IP, UserAgent | SessionID, AccessJWT, RefreshPlain, Expires | `INVALID_CREDENTIALS`, `AUTH_RATE_LIMITED` |
| Refresh | RefreshPlain, IP | новые Access+Refresh | `INVALID_TOKEN`, `TOKEN_EXPIRED`, `TOKEN_REUSED`, `AUTH_RATE_LIMITED` |
| Logout | Principal | void | `INVALID_TOKEN` |
| LogoutAll | Principal | void | |
| ChangePassword | Principal, Old, New | void (все сессии отозваны) | `INVALID_CREDENTIALS`, `INVALID_REQUEST` |
| GrantRole | Actor Principal, UserID, Role | void | `FORBIDDEN`, `NOT_FOUND`, `INVALID_REQUEST` |
| RevokeRole | как Grant | void | плюс «последний ADMIN» |
| Lock/Unlock/DisableUser | Actor ADMIN, UserID, Reason | void | `FORBIDDEN`, `NOT_FOUND`, `INVALID_REQUEST`; last ACTIVE ADMIN — `ErrLastAdmin`; lock/disable отзывают сессии |

`Idempotency-Key` на auth-POST **не требуется** ([D-07](decisions.md#d-07) — денежные объекты). Дубль email — `EMAIL_TAKEN`.

Порты (интерфейсы application, без pgx): `UserStore`, `SessionStore`, `TokenStore`, `AttemptStore`, `RateLimiter`, `StatusHistoryStore`, `Clock`, `Hasher`, `AccessSigner`, `RefreshIssuer`. Hasher, Signer, Issuer и Clock — инфраструктура (`internal/platform/authcrypto`); домен не знает PHC и JWT. JWT `roles` — кэш TTL; источник истины ролей — PostgreSQL + `ValidateSession`. Refresh plaintext в БД не хранится.

---

## 9. HTTP

База `/api/v1`, как в комментарии `httpserver.Handle`. ТЗ §5 пишет `/auth/register` без префикса — для API это `/api/v1/auth/...`.

| Метод | Путь | Authn | Назначение |
|---|---|---|---|
| POST | `/api/v1/auth/register` | нет | регистрация |
| POST | `/api/v1/auth/login` | нет | вход |
| POST | `/api/v1/auth/refresh` | нет (refresh в теле) | ротация |
| POST | `/api/v1/auth/logout` | Bearer | текущая сессия |
| POST | `/api/v1/auth/logout-all` | Bearer | все сессии |
| POST | `/api/v1/auth/password` | Bearer | смена пароля |
| GET | `/api/v1/auth/sessions` | Bearer | свои сессии |
| POST | `/api/v1/auth/sessions/{id}/revoke` | Bearer | своя сессия |
| GET | `/api/v1/auth/me` | Bearer | профиль + роли |
| GET | `/api/v1/admin/users` | Bearer + ADMIN | список |
| POST | `/api/v1/admin/users/{id}/roles` | Bearer + ADMIN | grant |
| POST | `/api/v1/admin/users/{id}/lock` (и unlock/disable) | Bearer + ADMIN | статус |

Тела JSON, суммы не участвуют. Ответ login/refresh:

```json
{
  "access_token": "<jwt>",
  "token_type": "Bearer",
  "expires_in": 900,
  "refresh_token": "<opaque>",
  "user": { "id": "<uuid>", "email": "a@b.c", "roles": ["CUSTOMER"] }
}
```

`refresh_token` только здесь. Повторно не отдаётся.

Ошибки — конверт [D-39](decisions.md#d-39). Клиент ветвится по `code` и HTTP-статусу.

| code | Kind | HTTP | Когда |
|---|---|---|---|
| `INVALID_REQUEST` | invalid | 400 | валидация email/пароля/ролей в теле |
| `INVALID_CREDENTIALS` | unauthorized | 401 | login: неизвестный email, неверный пароль, LOCKED, DISABLED (одно сообщение) |
| `INVALID_TOKEN` | unauthorized | 401 | нет/битый Bearer, отозванная сессия, неизвестный refresh |
| `TOKEN_EXPIRED` | unauthorized | 401 | refresh просрочен |
| `TOKEN_REUSED` | unauthorized | 401 | reuse refresh; сессия уже отозвана |
| `FORBIDDEN` | forbidden | 403 | роль не позволяет |
| `NOT_FOUND` | not_found | 404 | admin: нет пользователя |
| `EMAIL_TAKEN` | conflict | 409 | email занят |
| `AUTH_RATE_LIMITED` | too_many_requests | 429 | лимит окна |
| `INTERNAL_ERROR` | internal | 500 | как D-39 |

`INVALID_CREDENTIALS` на login **не** различает «нет пользователя» / «плохой пароль» / «LOCKED». `ACCOUNT_LOCKED` в API нет — enumeration. `AUTH_RATE_LIMITED` можно отдать и для существующего, и для неизвестного email (сработал IP- или email-ключ).

Kind `too_many_requests` → HTTP 429: `apperror.KindTooManyRequests` (этап 3.26.1). Формат тела D-39 не меняется. Ответы 401 (когда появятся защищённые маршруты) несут `WWW-Authenticate: Bearer` без причины. HTTP Auth — [auth-http-integration-plan.md](auth-http-integration-plan.md).

`POST /api/v1/auth/logout` (этап 3.26.5): Bearer, тело не используется, успех **204** без JSON. Только текущая сессия Principal. Application `Logout` на уже отозванной сессии — успех; повторный HTTP с тем же JWT после revoke — 401 `INVALID_TOKEN` (ValidateSession в BearerAuth). Logout All не входит в 3.26.

`GET /livez` и `GET /readyz` без Bearer.

---

## 10. Транзакции

READ COMMITTED, как приложение ([D-04](decisions.md#d-04)). Одна короткая транзакция на use case. Begin делает инфраструктура auth (по аналогии с Accounts UoW), use case транзакцию не открывает сам.

Use case возвращает явное `Decision` (`Commit` / `Rollback`). Адаптер **не** выбирает commit по тексту или коду ошибки. `Commit` при успехе и при бизнес-отказе, чьи побочные записи должны остаться (лимит, attempt, lockout, reuse). `Rollback` — инфраструктура, отмена context, валидация без security-записей.

**Register:** rate limit REGISTER_IP → insert users → insert user_roles CUSTOMER → audit. `EMAIL_TAKEN` и `AUTH_RATE_LIMITED` → `Commit` счётчика, без второй строки user. Ошибка hasher/store → `Rollback`.

**Login:** UPSERT LOGIN_IP; найти user; dummy или verify; при неуспехе UPSERT LOGIN_EMAIL. Если у найденного пользователя есть роль ADMIN — сначала `LockAdminRoleMutations`, потом `GetForUpdate`. Если роли ADMIN нет — `GetForUpdate` без advisory; если после row lock роль уже ADMIN (параллельный Grant), Tx **откатывается** и `LoginIn` повторяет вход с advisory **до** `FOR UPDATE` (не наоборот: deadlock с Grant). На ACTIVE: `failed_login_count+1`; при `>= 10` → `LOCKED` и revoke сессий (`USER_LOCKED`), **кроме** последнего ACTIVE ADMIN (строка остаётся ACTIVE, count и attempt всё равно пишутся). Уже не ACTIVE — повторный `Update` не делается. Insert attempt, `Commit`, `INVALID_CREDENTIALS`. При успехе: сброс `failed_login_count`, insert session + refresh, attempt success, audit, `Commit`. Ошибка Append attempt — `Rollback` (ошибка не глотается). Успешный login CUSTOMER advisory не берёт.

**Refresh:** `SELECT … FOR UPDATE` строки refresh по `token_hash` до конца TX; два параллельных вызова не должны оба увидеть unused. Reuse → revoke session + все refresh сессии, `Commit`, `TOKEN_REUSED`. Истёкший unused → `TOKEN_EXPIRED`, сессия жива, `Commit`. Неизвестный токен → `INVALID_TOKEN`, чужие сессии не трогать, `Commit` счётчика. Unit-тест сериализует фейковую TX и **не** заменяет проверку изоляции PostgreSQL.

**Rate limit:** инкремент входит в ту же TX и коммитится вместе с `Decision=Commit` на бизнес-отказе. Иначе лимит не держится.

**ADMIN Grant/Revoke и статус:** в той же Tx сначала `SELECT pg_advisory_xact_lock(0x5041590000000001)` (`RoleStore.LockAdminRoleMutations`). Ключ стабильный: namespace `0x504159` («PAY») + объект `1` (ADMIN mutations). Это **transaction** lock (`pg_advisory_xact_lock`), не `pg_advisory_lock` (session) и не mutex процесса. `paycore_app` имеет на `auth.roles` только `SELECT`, поэтому не `FOR UPDATE` каталога. Порядок: advisory → `SELECT … FOR UPDATE` на `auth.users` → остальные записи. Grant/Revoke **роли ADMIN** берут user `FOR UPDATE` после advisory. После lock — re-read инициатора: не ACTIVE+ADMIN → `FORBIDDEN`. CountActiveWithRole(ADMIN) только после lock. Другие роли этот lock не берут. Login CUSTOMER без роли ADMIN — тоже не берёт, пока re-check после `FOR UPDATE` не увидит ADMIN.

**Lock/Unlock/DisableUser:** actor ADMIN; reason не пустой (до 500 рун). Advisory **всегда** (не смотреть `Has(ADMIN)` до lock: параллельный Grant не должен обойти сериализацию), re-check инициатора, затем `GetForUpdate` target. Last ACTIVE ADMIN: Lock/Disable → `ErrLastAdmin`, Rollback, без строки истории. Already LOCKED / already DISABLED / already ACTIVE (Unlock) — Commit, без смены статуса и без истории. Unlock `DISABLED` — `INVALID_REQUEST` (не перескок машины статусов). Unlock `LOCKED` → `ACTIVE` сбрасывает `failed_login_count` и `locked_at`. Lock и Disable отзывают сессии (`USER_LOCKED` / `USER_DISABLED`) и refresh в той же Tx. Реальная смена статуса вставляет `auth.user_status_history` (`reason`) до Commit; ошибка вставки — Rollback всей операции.

Прямой `UserStore.Update` статуса и прямой `RoleStore.Grant/Revoke` в обход use case (сид/будущий bootstrap) по-прежнему могут обойти инвариант; адаптер это не запрещает. SQL CHECK «≥1 ADMIN» нет (ломает пустую БД до bootstrap). Второй advisory-ключ не вводим.

Не брать блокировки финансовых таблиц. Не вызывать `post_journal`, `open_ledger_accounts`, `lock_account_balances`.

---

## 11. Угрозы и ответы

| Угроза | Ответ |
|---|---|
| Кража пароля из БД | Argon2id, не bcrypt/sha; CHECK формата |
| Перебор пароля | лимиты §13; dummy verify; одинаковый 401 |
| Enumeration email | login всегда `INVALID_CREDENTIALS`; register — `EMAIL_TAKEN` (сознательная утечка, иначе нельзя понять успех регистрации) |
| Кража access JWT | TTL 15 мин; logout отзывает сессию |
| Кража refresh | ротация; reuse отзывает сессию |
| Самоназначение ADMIN | Register игнорирует роли; grant только ADMIN |
| ADMIN двигает деньги | нет денежных permission у роли; maker-checker [D-31](decisions.md#d-31); ledger только шлюз |
| SQL-фрагмент в токене | JWT + opaque hash, не строка SQL |
| Подпись JWT слабым секретом | отказ старта, если секрет < 32 байт |
| CSRF | Bearer в заголовке, не cookie |
| XSS крадёт токен | контракт API; хранение на клиенте — не этот модуль |
| PCI / KYC | не заявляется ([D-20](decisions.md#d-20)); нет карт, нет документов личности |
| Redis как SPOF для лимитов | нет Redis; счётчики в PostgreSQL |

Логи: email, user_id, session_id, ip, request_id. Не логировать пароль, PHC, JWT, refresh plaintext, `Authorization`.

---

## 12. Тесты (контракт на реализацию)

Unit (без БД):

1. Register: только CUSTOMER; `roles` в команде — ошибка.
2. Политика пароля: 11 и 129 символов — ошибка; 12 — ок.
3. Hasher пишет `$argon2id$`.
4. Login неизвестный email / неверный пароль — один и тот же code.
5. JWT: подпись, `exp`, чужой `iss`.
6. Refresh reuse — сессия revoked, второй вызов `TOKEN_REUSED`.
7. Refresh expired — `TOKEN_EXPIRED`, сессия жива.
8. Principal.Has: ADMIN не проходит проверку «денежный CUSTOMER» без роли CUSTOMER.
9. GrantRole не-ADMIN — `FORBIDDEN`.
10. Revoke последнего ADMIN — ошибка.
11. Access после Logout — `INVALID_TOKEN`.
12. ChangePassword отзывает все сессии.

PostgreSQL (testcontainers, `paycore_app`, не только in-memory):

13. `UNIQUE (email)` — второй Register `EMAIL_TAKEN`.
14. `password_hash` не `$argon2id$` — отказ INSERT (CHECK).
15. Полный walk login → 2× refresh → logout → refresh старым и новым.
16. Параллельный refresh одного токена: один успех, второй reuse или `INVALID_TOKEN`; не две живые цепочки.
17. Rate limit: 6-й неуспешный login того же email в окне — `AUTH_RATE_LIMITED`; счётчик в таблице ≥ 6.
18. 11-й неуспешный login существующего пользователя → `LOCKED` в строке user; сессии revoked; ответ login всё равно `INVALID_CREDENTIALS`.
19. Auth use case не пишет в `ledger.*` и не открывает L5.

Запрет: `t.Skip` «нет Docker». Как у Accounts: собирать PG-тесты всегда; CI гоняет testcontainers.

---

## 13. Лимиты MVP (явные)

Окно везде **15 минут UTC**, одно ведро `date_bin`. Счётчик в PostgreSQL.

| bucket | Ключ | Лимит на окно | Эффект при превышении |
|---|---|---|---|
| `LOGIN_EMAIL` | `email` (lowercase) | **5** неуспешных | `AUTH_RATE_LIMITED` |
| `LOGIN_IP` | `host(ip)` | **20** попыток (успех+неуспех) | `AUTH_RATE_LIMITED` |
| `REGISTER_IP` | `host(ip)` | **10** | `AUTH_RATE_LIMITED` |
| `REFRESH_IP` | `host(ip)` | **60** | `AUTH_RATE_LIMITED` |

Отдельно от окна: **10** последовательных неуспешных login по существующему `user_id` → `LOCKED` (сброс счётчика только успешным login или Unlock).

Access TTL 15 мин, refresh TTL 30 суток — §4. Max сессий на пользователя в MVP не режем (logout-all есть).

Не входит в MVP: email verify, reset по почте, MFA, OAuth, JWT refresh, Redis, cookie, PCI, KYC, fraud `REVIEW`, API-ключи магазинов, подтверждение действий кодом.

---

## 14. Порядок реализации

1. **Документы** — этап 3.19.
2. **DDL** — этап 3.20, `00005_auth.sql`: `ALTER auth.users`, таблицы, сид `roles`, гранты. Не менять `00002`. Bootstrap ADMIN — не этот этап.
3. **domain + application** — команды §8, без HTTP и без pgx.
4. **postgres adapter** — UPSERT лимитов, ротация, интеграционные тесты §12. **Сделано** (3.22–3.24).
5. **Инфраструктура токенов и пароля** — Argon2id Hasher, JWT HS256 AccessSigner (+ Parse для middleware), opaque RefreshIssuer, UTC Clock; `00006` history. **Сделано** (3.25).
6. **HTTP** — маршруты §9, Bearer middleware, Kind 429, `PAYCORE_AUTH_JWT_SECRET`, вызов `ValidateSession`. **Сделано** (3.26.1–3.26.6). `/me`, admin, bootstrap ADMIN — не этот этап.
7. **Связка с Accounts** — отдельный этап: Principal → `ChangedBy` / владелец. Open/Balance/Statement 3.10–3.18 не переписывать здесь.
8. **Bootstrap ADMIN** — будущая Go-команда, не SQL. Нужен Hasher из 3.25.
