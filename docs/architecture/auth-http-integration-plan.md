# Этап 3.26 — план HTTP Identity/Auth

Статус: **решения §8 зафиксированы (2026-11-08).** В коде: **3.26.1–3.26.6** (JWT + 429, runtime, public handlers, Bearer/RBAC, logout, PostgreSQL HTTP testcontainers). Этап 3.26 закрыт.

**Ворота.** Изучен `main` = `e9a8cb150fa88b2f37e1494059743ab31dcc5706` (merge PR **#32**, этап 3.25). Документ не опирается на чат: только файлы репозитория.

**Скоуп 3.26.** `POST /api/v1/auth/register`, `login`, `refresh`, `logout`; сборка Auth в `app.Run`; Bearer + `ValidateSession`; RBAC-middleware (готово к admin-маршрутам, сами admin/me/password **не** входят). Не входят: bootstrap ADMIN, Accounts HTTP, Payments, Transfers, Redis, Kafka, `audit.audit_logs`, LogoutAll / ChangePassword / список сессий.

Связанные документы (не противоречить): [identity-auth.md](identity-auth.md) §4, §8–§11, §13–§14; [D-39](decisions.md#d-39), [D-41](decisions.md#d-41), [D-42](decisions.md#d-42); [README.md](README.md) (индекс). ТЗ [PROJECT_SPEC.md](../PROJECT_SPEC.md) §5 даёт те же четыре маршрута без префикса `/api/v1` — для API префикс уже выбран в identity-auth §9.

---

## 1. Текущая архитектура (код на `e9a8cb1`)

### 1.1. Процесс и HTTP

| Компонент | Файл | Что есть |
|---|---|---|
| Вход | `cmd/api/main.go` | `app.Run`; код 2 на `config.ErrInvalid` |
| Сборка | `internal/app/app.go` | `config.Load` → slog → `postgres.Open` (пустой DSN = нет пула) → `httpserver.New` → `/livez` `/readyz`. **Auth не создаётся** |
| Конфиг | `internal/platform/config/config.go` | Только HTTP/Postgres/log. Неизвестные `PAYCORE_*` — отказ. `PAYCORE_AUTH_JWT_SECRET` нет (сейчас это «unknown variable») |
| Сервер | `internal/platform/httpserver` | `ServeMux`; `Handle(pattern, h)`; цепочка `withRequestID` → access log → recover → deadline (`HTTP_REQUEST_TIMEOUT`, default 5s) → `MaxBytesReader` (`HTTP_MAX_BODY_BYTES`, default 1 MiB) |
| Ошибки | `apperror` + `WriteError` | D-39 JSON `{error:{code,message,request_id}}`. Kind: invalid/401/403/404/405/409/413/422/503/500. **Нет `too_many_requests` / 429** |
| Health | `GET /livez`, `GET /readyz` | Без Bearer. `/readyz` ping пула, если он есть |

Маршрутов Auth нет. CORS нет. Cookie нет. CSRF-токен не нужен при Bearer (identity-auth §11).

### 1.2. Identity/Auth application

`internal/auth/{domain,application}` — без `net/http`, pgx, JWT, Argon2 (это ловят `imports_test.go`).

Use case, которые 3.26 вызывает:

| Метод | Обёртка UoW | Решение транзакции |
|---|---|---|
| `Register` | `RegisterIn` | Commit: успех, `EMAIL_TAKEN`, `AUTH_RATE_LIMITED`. Rollback: валидация, hasher/store/ctx |
| `Login` | `LoginIn` (до 2 попыток при `errAdminSerialRestart`) | Commit: успех, `INVALID_CREDENTIALS`, `AUTH_RATE_LIMITED`. Dummy Argon2 на неизвестный email |
| `Refresh` | `RefreshIn` | Commit: успех, `TOKEN_REUSED` (сессия отозвана), `TOKEN_EXPIRED` (сессия жива), `INVALID_TOKEN`. `GetForUpdate` по hash |
| `Logout` | `LogoutIn` | Commit: успех и повторный logout. Rollback: `INVALID_TOKEN` |
| `ValidateSession` | **без UoW** | Живая сессия + `ACTIVE` + роли из `user_roles`. JWT roles не читает |
| `GrantRole` / `RevokeRole` | `*In` | Нужны тестам «роли после Grant»; HTTP admin не в 3.26 |

`Credentials`: `SessionID`, `AccessToken`, `RefreshToken` (plaintext один раз), `TokenType=Bearer`, `ExpiresIn=900`, `PublicUser`. Пароль и PHC в ответе нет.

`AccessSigner` в application — только `Sign`. `Parse` живёт на `authcrypto.HS256`, не на порте.

`RateLimiter.Hit` — PostgreSQL `date_bin` 15 мин, не Redis. Ключ — строка из команды (`cmd.IP` / email). `ValidateIP` сегодня: «не пустая строка», не `net.ParseIP`.

Политика `EMAIL_TAKEN`: сознательная утечка (identity-auth §11). Login всегда `INVALID_CREDENTIALS` (нет пользователя / пароль / LOCKED / DISABLED).

### 1.3. Инфраструктура (уже есть, HTTP её только включает)

| Порт | Реализация | Заметки |
|---|---|---|
| Hasher | `authcrypto.Argon2idHasher` | PHC `$argon2id$v=19$m=19456,t=2,p=1$…`; dummy PHC на login |
| AccessSigner + Parse | `authcrypto.HS256` | `iss=paycore` **зашит**; секрет ≥ 32 байт; HS256 only; роли в JWT — кэш |
| RefreshIssuer | `authcrypto.OpaqueRefresh` | 32 байта → base64url; hash = SHA-256 **сырых байт**. `Hash` на мусор → `errInvalidRefresh` (не `domain.Error`) |
| Clock | `authcrypto.SystemClock` | `time.Now().UTC()` |
| Stores + UoW | `postgres.NewAuth*` + `NewAuthUoW` | Один `pgx.Tx` на `Do`; Commit по `Decision`, не по `err == nil`. Адаптеры принимают `execQueryer` (`*pgxpool.Pool` и `pgx.Tx` оба подходят) |

TTL: `domain.AccessTTL = 15m`, `RefreshTTL = 30×24h` — не из env.

### 1.4. Чего нет

- Auth в composition root.
- Kind 429.
- Транспортный пакет Auth.
- Middleware Bearer / RBAC / Principal в context.
- Запрет старта без JWT-секрета.
- HTTP-тестов Auth.

---

## 2. Целевой граф компонентов

Зависимости только вниз. Domain/application по-прежнему без HTTP и pgx.

```
cmd/api
  → app.Run
       → config (PAYCORE_* включая JWT secret)
       → postgres.Open (pool)
       → authcrypto (Hasher, HS256, OpaqueRefresh, Clock)
       → postgres.NewAuthUoW(pool)          // writes
       → postgres.NewAuthUsers/Sessions/Roles(pool)  // ValidateSession reads
       → application.Service (hasher/issuer/signer/clock; stores подставляет UoW)
       → httpserver.New
       → auth/http.Register(mux, deps)

httpserver (общие middleware)
  request_id → access_log → recover → deadline → max_body → mux

mux
  GET  /livez /readyz          // без Auth
  POST /api/v1/auth/register   // public
  POST /api/v1/auth/login      // public
  POST /api/v1/auth/refresh    // public, refresh в JSON
  POST /api/v1/auth/logout     // Bearer → Parse → ValidateSession → LogoutIn

Bearer middleware (logout и будущие protected)
  Authorization: Bearer <jwt>
  HS256.Parse (подпись, iss, exp, типы claims)
  application.ValidateSession(sid)     // источник ролей
  если JWT.sub ≠ Principal.UserID → INVALID_TOKEN
  context ← domain.Principal из ValidateSession, не из JWT.roles

RequireRole (RBAC)
  Principal из context; нет Principal → 401; нет роли → 403 FORBIDDEN
  JWT.roles не смотреть
```

**Не появляется в графе:** Redis, Kafka, Payments, Transfers, Accounts HTTP, ledger-шлюз, cookie store, denylist `jti`.

Пакет транспорта: `internal/auth/http` (первый `transport` модуля; accounts пока без HTTP). Общий Bearer/RBAC — `internal/platform/httpserver`, чтобы позже счета не тянули `internal/auth/http`. Handlers не содержат бизнес-правил: decode JSON → команда → `*In` / `ValidateSession` → encode.

---

## 3. Потоки запросов

Общее для всех POST: лимит тела 1 MiB → JSON; IP — host `RemoteAddr` (§8.4); пароль/JWT/refresh **не** пишутся в slog (access log уже не логирует заголовки). `Idempotency-Key` не требуется (D-07 — деньги).

Ошибки use case → таблица identity-auth §9 через маппер §4.2. `WriteError` не знает `domain.Error`: транспорт всегда оборачивает в `apperror`.

### 3.1. `POST /api/v1/auth/register` (без Bearer)

1. Decode `{email,password,first_name,last_name}`. Поле `roles` (даже `[]`) → `INVALID_REQUEST` 400, **не** вызывать Register (application тоже отвергает `len(Roles)>0`).
2. `RegisterCommand{Email,Password,FirstName,LastName,IP}`.
3. `RegisterIn(ctx, uow, svc, cmd)`: limiter `REGISTER_IP` → canonical email → GetByEmail → Argon2 Hash → user ACTIVE + `user_roles CUSTOMER`.
4. Успех: **201**, тело `user` без hash/токенов.
5. `EMAIL_TAKEN` → 409 (утечка «email занят» — контракт, не баг).
6. `AUTH_RATE_LIMITED` → 429.
7. Пароль 11/129 рун, пустой IP, битый email → 400, Rollback.

Сессии нет: регистрация не логинит.

### 3.2. `POST /api/v1/auth/login` (без Bearer)

1. Decode `{email,password}`. User-Agent из заголовка (может быть пустым — domain это допускает).
2. `LoginIn`: `LOGIN_IP` → dummy/real Argon2 → lockout/ADMIN advisory внутри application.
3. Успех **200**: `access_token`, `token_type`, `expires_in`, `refresh_token`, `user` (id, email, roles минимум).
4. Неизвестный email / неверный пароль / LOCKED / DISABLED → один `INVALID_CREDENTIALS` 401.
5. 6-й неуспех того же email в окне / 21-й с того же IP → `AUTH_RATE_LIMITED` 429 (счётчик закоммичен).
6. Не логировать password; не класть PHC в JSON.

### 3.3. `POST /api/v1/auth/refresh` (без Bearer, токен в теле)

Контракт D-41: `{ "refresh_token": "..." }`, **не cookie**. CSRF при cookie-less Bearer не применим. CORS в 3.26 нет.

1. Пустой токен → 400 `INVALID_REQUEST` (как application).
2. Мусорный base64/`Hash` error: транспорт мапит в `INVALID_TOKEN` 401 (не 500). Контракт application не менять.
3. `RefreshIn`: `REFRESH_IP` → `GetForUpdate(hash)`:
   - reuse → revoke session + все refresh сессии → `TOKEN_REUSED` 401;
   - expired unused → `TOKEN_EXPIRED` 401, сессия жива;
   - unknown → `INVALID_TOKEN` 401, чужие сессии не трогать;
   - user не ACTIVE → `INVALID_TOKEN`;
   - успех → новый access+refresh, тот же `session_id`, `last_seen_at` обновлён.
4. Два параллельных refresh одного plaintext: один Commit успеха, второй reuse или `INVALID_TOKEN`; две живые цепочки запрещены (уже UoW + `FOR UPDATE`; HTTP-тест это доказывает, не фейковый mutex).

### 3.4. `POST /api/v1/auth/logout` (Bearer)

1. Middleware: нет/не `Bearer` / битая подпись / чужой `iss` / просроченный access → `INVALID_TOKEN` 401 (**не** `TOKEN_EXPIRED`: этот code — про refresh, identity-auth §9).
2. `ValidateSession(sid)`: revoked / нет сессии / не ACTIVE → `INVALID_TOKEN` 401. Logout после revoke JWT до `exp` сразу бесполезен — это цель (нет denylist `jti`).
3. Principal в context из store-ролей.
4. `LogoutIn(ctx, uow, svc, principal)`: revoke session `LOGOUT` + refresh сессии. UserID и SessionID берутся из Principal в context (BearerAuth), не из тела и не из JWT claims в handler. Тело запроса не разбирается.
5. Успех: **204 No Content**, без JSON. Application `Logout`: повторный вызов по той же сессии — Commit без ошибки. HTTP: если BearerAuth ещё видит живую сессию (два запроса успели пройти ValidateSession), handler тоже отдаёт 204. Если сессия уже revoked — ValidateSession → **401** `INVALID_TOKEN` (это не Logout All и не смена контракта application).

Access после logout на любом protected маршруте: Parse OK (если JWT ещё не истёк) → ValidateSession → 401. Refresh старого opaque → `INVALID_TOKEN` или `TOKEN_REUSED` в зависимости от revoked vs used.

### 3.5. Роли после Grant/Revoke (без HTTP grant в 3.26)

Grant/Revoke сессии **не** отзывают. Следующий Bearer (тот же JWT) → ValidateSession читает `user_roles` → Principal с новыми ролями. JWT `roles` могут врать до 15 мин — **RBAC смотрит только Principal**. Тест: Login → GrantRoleIn в процессе → logout/dummy RequireRole(ADMIN) на тестовом маршруте **или** вызов ValidateSession через middleware test — без публичного admin API.

---

## 4. Контракты handler и middleware

### 4.1. JSON

**Register request**

```json
{ "email": "a@b.c", "password": "twelve chars", "first_name": "A", "last_name": "B" }
```

**Login / refresh success** — identity-auth §9:

```json
{
  "access_token": "<jwt>",
  "token_type": "Bearer",
  "expires_in": 900,
  "refresh_token": "<opaque>",
  "user": { "id": "<uuid>", "email": "a@b.c", "roles": ["CUSTOMER"] }
}
```

`refresh_token` только здесь. Повторно не отдавать. `session_id` в JSON нет (есть в application, нет в HTTP-контракте). `user` — только `id`, `email`, `roles`.

**Logout:** без тела. Успех **204**. Register успех **201**. Login/refresh **200**.

Decode: `encoding/json`, `DisallowUnknownFields`. Неизвестные поля, включая `roles` на register, → `INVALID_REQUEST` 400. Поля `details` нет (D-39).

### 4.2. Маппинг ошибок

Новый Kind `apperror.KindTooManyRequests` → HTTP 429. `statusFor` и `Kind.String()="too_many_requests"`. Формат тела D-39 не меняется. Код `AUTH_RATE_LIMITED`.

| Источник | Code | Kind | HTTP |
|---|---|---|---|
| валидация JSON / domain | `INVALID_REQUEST` | invalid | 400 |
| login | `INVALID_CREDENTIALS` | unauthorized | 401 |
| Bearer / refresh unknown / logout bad principal | `INVALID_TOKEN` | unauthorized | 401 |
| refresh TTL | `TOKEN_EXPIRED` | unauthorized | 401 |
| refresh reuse | `TOKEN_REUSED` | unauthorized | 401 |
| RequireRole | `FORBIDDEN` | forbidden | 403 |
| (admin, не 3.26) | `NOT_FOUND` | not_found | 404 |
| register | `EMAIL_TAKEN` | conflict | 409 |
| limiter | `AUTH_RATE_LIMITED` | too_many_requests | 429 |
| прочее | `INTERNAL_ERROR` | internal | 500 |

`postgres.wrapAuth` уже кладёт `*apperror.Error` с domain code: маппер должен понимать и `domain.Error`, и `apperror.Error` (`errors.As`). Инфраструктурные ошибки hasher/tx → 500, cause только в лог, не в клиент.

**401 vs 403.** Нет/битый/отозванный токен, не ACTIVE, mismatch sub/sid, истёкший access JWT → 401 `INVALID_TOKEN` (не `TOKEN_EXPIRED`). Аутентифицирован, роли не хватает → 403. Не использовать JWT.roles для 403.

Каждый **401** (с 3.26.4/3.26.5, не в 3.26.1): заголовок `WWW-Authenticate: Bearer` без `error`/`error_description` и без причины в теле сверх `code`/`message`.

### 4.3. Middleware

**`BearerAuth`** (platform):

```
вход: HS256 (Parse), SessionValidator = application.Service.ValidateSession
выход: context value Principal
ошибка: WriteError, цепочка дальше не идёт
```

Пустой Authorization, не схема Bearer, extra spaces — `INVALID_TOKEN`. Не логировать значение заголовка.

**`RequireRole(role)`:** читает Principal; `!Has(role)` → `FORBIDDEN`. Для 3.26 монтируется на logout **не** нужен (любая живая сессия). Нужен unit-тест + опциональный тестовый маршрут в `httpserver_test` / `auth/http` test. Публичные register/login/refresh **не** оборачивать.

Порядок на logout: mux → BearerAuth → handler (RBAC нет).

### 4.4. Handler

Сигнатура в духе:

```
type Handler struct {
    UoW application.UnitOfWork
    SVC application.Service // hasher/issuer/signer/clock; stores задаёт UoW / read adapters
}
```

Запрещено в handler: SQL, pgx, Argon2, проверка пароля, hit limiter, чтение JWT.roles для решения, бизнес-ветвление lockout. Разрешено: IP/UA, JSON, вызов `*In`, маппинг ошибок, `WriteJSON`.

Client IP: `host` из `RemoteAddr` (`net.SplitHostPort`). `X-Forwarded-For` и `X-Real-IP` не доверять. Значение должно быть валидным для `inet` в `auth.sessions`. CORS в 3.26 нет. Refresh — JSON-тело, не cookie.

Лимиты ввода: глобальный MaxBody; пароль 12…128 уже в domain (не хешировать 129). Дополнительный cap поля JSON не нужен, если тело ≤ 1 MiB.

---

## 5. Изменения `app.Run`

Порядок после успешного `config.Load` (ошибки конфига по-прежнему код 2):

1. **Секрет JWT.** `PAYCORE_AUTH_JWT_SECRET` ≥ 32 байт (как `NewHS256`). `config.Load` отвергает пустой/короткий секрет (`ErrInvalid`, код 2): секрет нужен, когда Auth включён; 3.26.1 проверяет его всегда, чтобы 3.26.2 мог включить Auth. Issuer **не** из env: `paycore`. TTL **не** из env: `domain.AccessTTL` / `RefreshTTL`.
2. Логгер как сейчас. Секрет в лог не писать.
3. `postgres.Open`. Пустой DSN: в local/test Auth-маршруты **не** монтировать (`/livez` жив). В `production` пустой DSN — `ErrInvalid` (3.26.2).
4. Если пул есть:
   - `signer := authcrypto.NewHS256([]byte(secret))`
   - `svc := application.Service{Hasher: NewArgon2idHasher(), Issuer: NewOpaqueRefresh(), Access: signer, Clock: NewSystemClock()}`
   - `uow := postgres.NewAuthUoW(pool)`
   - read: `Users/Sessions/Roles` на **pool** (не Tx) для `ValidateSession`
   - `readSvc := svc` с этими тремя портами
5. `httpserver.New` → `SetReadyCheck(pool.Ping)` как сейчас.
6. `authhttp.Mount(srv, deps)`: четыре маршрута.
7. `Serve` / shutdown без изменений. Пул `defer Close` после shutdown HTTP (сейчас Close сразу при выходе из Run — после Serve это уже так). Не закрывать пул, пока работают in-flight login/refresh.

`cmd/api` не знает про Auth. `cmd/migrate` / bootstrap ADMIN **не** трогать (этап после 3.26, нужен Hasher — он уже есть).

`.env.example`: строка `PAYCORE_AUTH_JWT_SECRET` без реального секрета (placeholder ≥ 32). Это конфиг-файл примера, не код загрузки — делать в подэтапе 3.26.1 вместе с `known` vars.

Тест `TestRunServesAndShutsDown`: передать валидный секрет в environ.

---

## 6. Таблица подэтапов

| ID | Название | Зачем отдельно |
|---|---|---|
| **3.26.1** | Конфиг JWT + Kind 429 | Fail-fast без маршрутов; не ломает Auth-тесты application |
| **3.26.2** | Composition root | Пул + authcrypto + UoW + read stores в `app.Run`; маршруты ещё заглушки или Mount no-op |
| **3.26.3** | Public handlers | register/login/refresh, DTOs, маппер ошибок; без Bearer |
| **3.26.4** | Bearer + RBAC | Parse, ValidateSession, Principal, RequireRole; тесты middleware |
| **3.26.5** | Logout | Сборка 3.26.3+3.26.4 |
| **3.26.6** | PG HTTP tests | testcontainers walk, reuse, race, rate limit, logout, Grant→роли |

Каждый подэтап — отдельный PR после согласия на весь план; дробить можно, реализовывать нельзя, пока план не утверждён.

---

## 7. Подэтапы: файлы, изменения, тесты, done

### 3.26.1 — конфиг и 429

**Файлы:** `internal/platform/config/{config.go,config_test.go}`, `.env.example`, `internal/platform/apperror/{apperror.go,apperror_test.go}`, `internal/platform/httpserver/{respond.go,httpserver_test.go}`.

**Изменения:** `Config.Auth` (или поле на корне) с `JWTSecret string`. `known` += `PAYCORE_AUTH_JWT_SECRET`. Пусто/короче 32 байт → `ErrInvalid` (считать байты секрета, не руны). Не добавлять issuer/TTL env. `KindTooManyRequests` + `statusFor` 429. Не монтировать Auth.

**Тесты:** Load без секрета / 31 байт / 32 байта; unknown `PAYCORE_AUTH_*` кроме secret по-прежнему ошибка; handler-stub 429 JSON D-39.

**Done:** процесс с коротким секретом не стартует (код 2); 429 в платформе; application/Auth SQL не тронуты.

### 3.26.2 — wiring

**Файлы:** `internal/app/app.go`, `internal/app/app_test.go`.

**Изменения:** создать hasher/signer/issuer/clock/UoW/read stores при ненулевом пуле. Если Mount ещё пустой — хотя бы не паниковать. Не логировать секрет и DSN plaintext (DSN уже `RedactDSN`).

**Тесты:** Run с секретом и без DSN — `/livez` 200; с невалидным секретом — `ErrInvalid`; не импортировать auth/http в domain.

**Done:** `app.Run` собирает Auth deps; HTTP Auth может подождать 3.26.3.

### 3.26.3 — public HTTP

**Файлы (новые):** `internal/auth/http/{handler.go,dto.go,errors.go,register.go,*_test.go}`. **Правка:** `app.go` вызывает `Mount`.

**Изменения:** три POST. Маппер §4.2. Register не вызывает login. RateLimiter — только через use case.

**Тесты (httptest, fake UoW как в application):** register CUSTOMER 201; `roles` в JSON → 400; EMAIL_TAKEN 409; login unknown = wrong password = 401 один code; rate-limit fake → 429; refresh empty 400; логи тестового буфера не содержат password/refresh/jwt; body > MaxBodyBytes → 413 как сейчас.

**Done:** три маршрута на mux; application не импортирует `net/http`.

### 3.26.4 — Bearer и RBAC

**Файлы:** `internal/platform/httpserver/auth.go` (имя по вкусу), тесты там же; `authcrypto` не менять (Parse уже есть).

**Изменения:** context key для `domain.Principal` (не экспортировать string key). Bearer → Parse → ValidateSession. RequireRole. Access `ErrTokenExpired` из Parse → `INVALID_TOKEN` на Bearer.

**Тесты:** нет заголовка; `Basic`; сломанный JWT; чужой iss; истекший JWT → `INVALID_TOKEN` + `WWW-Authenticate: Bearer` без причины; живой JWT + revoked session; JWT.roles=[ADMIN] но store CUSTOMER + RequireRole(ADMIN) → 403 не 200; после «Grant» в fake store → 200 на RequireRole(ADMIN) тем же JWT.

**Done:** JWT.roles не являются авторизацией; 401/403 как таблица.

### 3.26.5 — logout

**Файлы:** `internal/auth/http/handler.go` (`postLogout`, Mount через `BearerAuth`), `internal/app/app.go` (parser + read `ValidateSession`), тесты `internal/auth/http/logout_test.go`. PostgreSQL HTTP — 3.26.6.

**Изменения:** `POST /api/v1/auth/logout` → Principal из BearerAuth → `LogoutIn`. 204 без тела. Нет/битый Bearer / revoked session → 401 + `WWW-Authenticate: Bearer`. Register/login/refresh не менялись. Без Auth runtime (пустой DSN) маршрут не монтируется (404).

**Тесты (httptest, без `time.Sleep`, без PG):** 204 пустое тело; нет/битый Bearer 401; revoked 401; Logout получает UserID/SessionID Principal (тело игнорируется); повтор, пока ValidateSession пропускает — 204 как application; повтор после revoke — 401; чужая сессия того же user не отзывается; без parser/sessions — 404.

**Done:** четыре маршрута скоупа 3.26 закрыты на HTTP. Интеграция PG — 3.26.6.

### 3.26.6 — интеграция PostgreSQL

**Файлы:** `internal/platform/postgres/auth_http_pg_test.go` (testcontainers, `paycore_app`, без `t.Skip` «нет Docker»). Реальный `Argon2idHasher` + `HS256` + `OpaqueRefresh` + `NewAuthUoW`. Тестовый `GET /api/v1/__test/require-admin` только в этом файле, не в production Mount.

**Сценарии:**

1. register → login → refresh → logout → refresh старым и новым opaque.
2. Параллельный refresh одного токена: один 200, второй 401 `TOKEN_REUSED` или `INVALID_TOKEN`; в БД не две unused цепочки.
3. 6-й неуспешный login того же email → 429; строка `rate_limit_counters`.
4. Logout → Bearer на logout/тестовый protected → 401.
5. GrantRoleIn ADMIN (фикстура пользователя с ролью, не HTTP admin) → тот же access JWT проходит RequireRole(ADMIN) через ValidateSession.
6. Access log / slog dump теста не содержит plaintext password, PHC, JWT, refresh.
7. Нет записи в `ledger.*`.

**Done:** §12.15–12.16 identity-auth на HTTP; CI как остальные PG-тесты.

---

## 8. Зафиксированные решения (2026-11-08)

Не ADR D-xx; обязательны для 3.26. Менять только новым решением пользователя.

1. **JWT secret.** `PAYCORE_AUTH_JWT_SECRET` обязателен, когда Auth включён: ≥ 32 **байт** (не рун), как `NewHS256`. 3.26.1: `config.Load` отвергает пустой/короткий секрет всегда (`ErrInvalid`, код 2), чтобы 3.26.2 мог включить Auth. Тесты `app.Run` передают валидный секрет в environ.
2. **Пустой DSN.** local/test: Auth-маршруты не монтировать; `/livez` жив. `production`: нет DSN — ошибка старта. Монтирование — 3.26.2, не 3.26.1.
3. **Issuer и TTL.** Константы D-41: `iss=paycore`, access 15 мин, refresh 30 суток. Env только для секрета, не для issuer/TTL.
4. **Client IP.** Только host из `RemoteAddr`. Не доверять `X-Forwarded-For` / `X-Real-IP`.
5. **CORS.** В 3.26 нет.
6. **Refresh.** JSON-тело `{ "refresh_token": "..." }`, не cookie ([D-41](decisions.md#d-41)). CSRF для Bearer+JSON не нужен.
7. **HTTP status.** Register **201**, logout **204**, login/refresh **200**.
8. **User DTO.** Только `id`, `email`, `roles`. Нет `session_id`, `first_name`, `status`.
9. **JSON decode.** `DisallowUnknownFields`. Неизвестные поля, включая `roles` на register, → 400 `INVALID_REQUEST`.
10. **Истёкший access JWT.** `INVALID_TOKEN` 401, не `TOKEN_EXPIRED` (`TOKEN_EXPIRED` — только просроченный refresh).
11. **Мусорный refresh.** HTTP `INVALID_TOKEN` 401. Контракт application/`OpaqueRefresh.Hash` не менять.
12. **Скоуп 3.26.** Только `register`, `login`, `refresh`, `logout` (+ Bearer/RBAC middleware). Нет `me`, password, sessions, admin, bootstrap ADMIN.
13. **401 `WWW-Authenticate`.** Каждый 401 MUST слать `WWW-Authenticate: Bearer` **без** раскрытия причины (`error=`, description, внутренние детали). Не в 3.26.1 (нет Bearer).
14. **JSON `details`.** Нет. Тело ошибки — D-39: `code`, `message`, `request_id`.

Контракт application не менять под эти решения (в т.ч. пункт 11). Cookie, TTL env, CORS — смена этого списка, не «по ходу HTTP».

### Риски (не блокеры плана)

| Риск | Почему | Что делаем в 3.26 |
|---|---|---|
| Argon2 + timeout 5s | Login CPU на каждом запросе | Не менять hasher; не снижать dummy verify; не выносить hash из Tx без нового ADR |
| ValidateSession на каждый Bearer | Лишний SELECT сессии+user+roles | Это вариант A контракта; кэш ролей JWT не использовать для 403 |
| EMAIL_TAKEN | Enumeration | Оставить; login по-прежнему не различает email |
| Refresh `Hash` → 500 если забыть маппинг | `errInvalidRefresh` не domain | Пункт 11 |
| Access log 5xx `slog.Any("error", err)` | Теоретически утечка, если err содержит токен | Не класть токены в `fmt.Errorf`; ревью 3.26.3 |
| Параллельный refresh | Две цепочки | Уже `FOR UPDATE`; 3.26.6 доказывает по HTTP |
| Stale JWT.roles после Revoke ADMIN | 15 мин кэш | RBAC из ValidateSession; тест 3.26.4 |
| Нет ADMIN в пустой БД | Logout/register не требуют ADMIN | Не делать bootstrap в 3.26 |
| Пустой IP | `ValidateIP` + `inet` | Handler всегда ставит host; не передавать `""` |

Payments/Transfers/Redis/Kafka: **не нужны** для этих четырёх маршрутов; не тянуть «на будущее».

Контракт application **не** менять. Мусорный refresh мапит транспорт (пункт 11), не application.

---

## 9. Критерии готовности всего 3.26

Этап закрыт, когда одновременно:

1. Четыре маршрута отвечают identity-auth §9 + таблица §4.2; health без Bearer.
2. `app.Run` создаёт hasher, HS256, refresh issuer, clock, Auth UoW, PG adapters; без секрета ≥ 32 байт — код 2.
3. Handlers без бизнес-логики; domain/application без `net/http` и pgx.
4. Bearer: подпись + `ValidateSession`; JWT.roles не авторизация; 401 vs 403 как §4.2.
5. Logout отзывает сессию сразу; повторный logout успешен; access после logout — 401.
6. Refresh rotation/reuse/expiry/concurrency — поведение application сохранено и покрыто HTTP+PG.
7. Rate limit — существующий PG limiter, не Redis; 429 Kind новый.
8. `EMAIL_TAKEN` 409; login enumeration не появляется.
9. Refresh в JSON-теле, не cookie; CORS/CSRF не размывают D-41.
10. В логах тестов нет password, PHC, JWT, refresh, `Authorization`.
11. Нет миграций, нет plaintext пароля/refresh в БД, нет изменений ledger/accounts HTTP, нет bootstrap ADMIN, HTTP совпадает с §8.
12. `make check` (fmt, vet, race tests) зелёный, включая testcontainers Auth HTTP.

После 3.26 по-прежнему отдельно: bootstrap ADMIN, GET `/me`, admin users, связка Accounts `ChangedBy`, audit.

---

## Инварианты реализации (напоминание)

- Не менять `00005`/`00006` и сид `auth.roles`.
- Auth UoW коммитит по `Decision`.
- Advisory lock ADMIN — не трогать (logout/register/login CUSTOMER его не требуют, кроме lockout ADMIN внутри Login).
- Не вызывать `post_journal` / Open счёта из Auth HTTP.
