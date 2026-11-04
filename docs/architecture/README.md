# PayCore — архитектура финансового ядра

Статус: **проект принят для MVP; идёт этап 3 (реализация)**. В коде — каркас сервиса ([D-38](decisions.md#d-38), [D-39](decisions.md#d-39)), схема `platform` (этап 3.2), DDL таблиц `accounts`/`ledger` (этап 3.3), шлюзовые функции `ledger.post_journal` / `open_ledger_accounts` / `lock_account_balances` (этап 3.4, [D-40](decisions.md#d-40)), типизированный Go-вызов `post_journal` (этап 3.5), domain-валидация `Command` без БД (этап 3.6) и контракт Go Poster ↔ PostgreSQL (этап 3.7). HTTP, Payments и Transfers ещё не реализованы. Ревью SQL-схемы Ledger — [ledger-schema-review.md](ledger-schema-review.md). Первоисточник требований — [../PROJECT_SPEC.md](../PROJECT_SPEC.md).

MVP работает с виртуальными деньгами: источник средств — симулятор, реальных карт и внешних провайдеров нет. Соответствие PCI DSS, банковским требованиям и KYC/AML не заявляется ([D-20](decisions.md#d-20)).

## Документы

| Документ | Содержание |
|---|---|
| [decisions.md](decisions.md) | Реестр принятых решений (D-xx): выбранный вариант, альтернативы, обоснование; соответствие закрытых вопросов этапа 1 |
| [open-questions.md](open-questions.md) | Только нерешённые вопросы (Q-xx) |
| [spec-analysis.md](spec-analysis.md) | Анализ ТЗ: противоречия, недостающие требования, риски — со статусом разрешения; противоречия в документах, устранённые на этапе 2 |
| [ledger-model.md](ledger-model.md) | Финансовая модель: double-entry, журналы и проводки, правила дебета и кредита, балансы и бакеты, резервы, комиссии, неизменяемость, контракт `post_journal`, инварианты |
| [chart-of-accounts.md](chart-of-accounts.md) | План счетов; по каждому системному счёту — знак остатка, контроль, проекция, горячая строка |
| [money-flows.md](money-flows.md) | Машины состояний; симулятор; переводы; checkout → авторизация → capture; отмена и истечение; возвраты с числовыми примерами; сквозной сценарий; выплаты; сторно |
| [financial-invariants-testing.md](financial-invariants-testing.md) | План тестирования: unit, property-based, права и функции, идемпотентность, outbox, конкурентные S-01…S-28, fault injection |
| [ledger-schema-review.md](ledger-schema-review.md) | Ревью окончательной SQL-схемы Ledger после этапа 3.2: вердикт, расхождения с кодом, план грантов по таблице |

Модель данных — в [../database/README.md](../database/README.md).

## Ключевые решения (кратко)

1. **Ledger — первоисточник.** Журнал из ≥ 2 проводок, в каждой валюте \(\sum D = \sum C\), записи append-only, исправления — только сторно и корректировки (maker-checker) — [D-06](decisions.md#d-06), [D-31](decisions.md#d-31).
2. **Деньги — `BIGINT` в минорных единицах**, в MVP только RUB (модель мультивалютная), округление только в Commission Engine (`HALF_UP`, ставки в bp) — [D-17](decisions.md#d-17), [D-30](decisions.md#d-30).
3. **Бизнес-счёт ↔ два ledger-счёта** (доступно и резерв). Резерв — проводка — [D-01](decisions.md#d-01).
4. **Синхронная проекция у всех счетов:** `account_balances` у клиентов и магазинов, бакеты у системных счетов; CHECK ≥ 0 ловит отрицательный остаток в момент записи — [D-02](decisions.md#d-02).
5. **Единственный путь записи в ledger — функции `SECURITY DEFINER`** и права ролей; триггеры — вторая линия — [D-03](decisions.md#d-03).
6. **Платёж через checkout:** магазин не выбирает счёт покупателя; `PENDING` в MVP не хранится; комиссия удерживается при capture и не возвращается; возврат требует остатка магазина — [D-21](decisions.md#d-21), [D-22](decisions.md#d-22), [D-08](decisions.md#d-08)…[D-10](decisions.md#d-10).
7. **Конкурентность:** READ COMMITTED + единый порядок блокировок L1…L8 — [D-04](decisions.md#d-04).
8. **Идемпотентность в два уровня:** HTTP-ключ в той же транзакции и бизнес-`UNIQUE` — [D-07](decisions.md#d-07).
9. **Outbox с lease, публикация вне транзакции, inbox у потребителей;** Kafka в MVP не обязательна — [D-23](decisions.md#d-23).
