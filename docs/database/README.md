# PayCore — модель данных PostgreSQL

Статус: **принято для MVP**. SQL-фрагменты в этих документах иллюстративны. В коде: `migrations/00001_platform_schema.sql` (схема `platform`), `00002_accounts.sql`, `00003_ledger.sql` (таблицы счетов и ledger), `00004_ledger_functions.sql` (шлюзовые `SECURITY DEFINER`-функции, [D-40](../architecture/decisions.md#d-40)), `00005_auth.sql` (`ALTER auth.users` и таблицы identity, без DROP заглушки). Роли создаёт `cmd/migrate bootstrap` под суперпользователем; Goose идёт от `paycore_owner` ([D-33](../architecture/decisions.md#d-33), [D-38](../architecture/decisions.md#d-38)).

| Документ | Содержание |
|---|---|
| [er-diagram.md](er-diagram.md) | ER-диаграммы (Mermaid) по модулям; что изменилось после этапа 1; модели fraud и сверки после MVP; межмодульные связи |
| [database-schema.md](database-schema.md) | Соглашения, таблицы и колонки, типы, ограничения по таблицам, включая бакеты, checkout, лимиты, симулятор, выплаты |
| [data-integrity.md](data-integrity.md) | CHECK, FK (включая составные по валюте), уникальность, функции-шлюзы ledger, триггеры, индексы, роли и права |
| [concurrency.md](concurrency.md) | Уровни изоляции, порядок блокировок L1…L8, идемпотентность (SQL и 12 случаев), шаблоны транзакций, retry, outbox с lease и inbox, каталог сценариев S-01…S-28, таймауты |

Ревью готовности схемы к реализации — [../architecture/ledger-schema-review.md](../architecture/ledger-schema-review.md).

Финансовая модель — [../architecture/ledger-model.md](../architecture/ledger-model.md). Identity/Auth (таблицы `auth.*`, токены, RBAC) — [../architecture/identity-auth.md](../architecture/identity-auth.md); DDL — `00005_auth.sql`. Решения — [../architecture/decisions.md](../architecture/decisions.md), открытые вопросы — [../architecture/open-questions.md](../architecture/open-questions.md).
