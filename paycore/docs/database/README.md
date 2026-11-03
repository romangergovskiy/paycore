# PayCore — модель данных PostgreSQL

Статус: **проектирование**. Миграций пока нет. SQL-фрагменты в этих документах иллюстративны.

| Документ | Содержание |
|---|---|
| [er-diagram.md](er-diagram.md) | ER-диаграммы (Mermaid) по модулям; межмодульные связи |
| [database-schema.md](database-schema.md) | Соглашения, таблицы и колонки, типы, ограничения по таблицам |
| [data-integrity.md](data-integrity.md) | CHECK, FK (включая составные по валюте), уникальность, триггеры неизменяемости и баланса, индексы, роли и права, партиционирование |
| [concurrency.md](concurrency.md) | Уровни изоляции, порядок блокировок, идемпотентность, шаблоны транзакций, retry, outbox/inbox, внешние вызовы, каталог конкурентных сценариев S-01…S-20, таймауты |

Финансовая модель, на которую опирается схема, — [../architecture/ledger-model.md](../architecture/ledger-model.md). Открытые вопросы — [../architecture/open-questions.md](../architecture/open-questions.md).
