# PayCore — модель данных PostgreSQL

Статус: **принято для MVP**. Миграций пока нет (появятся на этапе 3.2, Goose — [D-38](../architecture/decisions.md#d-38)). SQL-фрагменты в этих документах иллюстративны.

| Документ | Содержание |
|---|---|
| [er-diagram.md](er-diagram.md) | ER-диаграммы (Mermaid) по модулям; что изменилось после этапа 1; модели fraud и сверки после MVP; межмодульные связи |
| [database-schema.md](database-schema.md) | Соглашения, таблицы и колонки, типы, ограничения по таблицам, включая бакеты, checkout, лимиты, симулятор, выплаты |
| [data-integrity.md](data-integrity.md) | CHECK, FK (включая составные по валюте), уникальность, функции-шлюзы ledger, триггеры, индексы, роли и права |
| [concurrency.md](concurrency.md) | Уровни изоляции, порядок блокировок L1…L8, идемпотентность (SQL и 12 случаев), шаблоны транзакций, retry, outbox с lease и inbox, каталог сценариев S-01…S-28, таймауты |

Финансовая модель — [../architecture/ledger-model.md](../architecture/ledger-model.md). Решения — [../architecture/decisions.md](../architecture/decisions.md), открытые вопросы — [../architecture/open-questions.md](../architecture/open-questions.md).
