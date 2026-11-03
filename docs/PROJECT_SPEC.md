Отлично. Тогда будем разрабатывать PayCore как большой инженерный проект уровня банковской платёжной инфраструктуры, а не как учебное приложение с несколькими CRUD-эндпоинтами.

Я предлагаю сразу относиться к нему как к продукту, который потенциально могла бы разрабатывать настоящая финтех-команда.

Наша цель — получить проект, в котором ты сможешь продемонстрировать навыки Go Backend Developer, необходимые для работы в банковском финтехе: от проектирования финансовой модели и PostgreSQL до конкурентной обработки платежей, Kafka, распределённых систем, безопасности и мониторинга.

Основной проект

Technical specification v1.0

# PayCore

Banking & Payment Processing Infrastructure

[Platform — Merchant Payment Operations Workspace | a55](https://images.openai.com/static-rsc-4/hMLKmkQS4PHv9dg-Z4Hb0D3VwNh5x7oyWoeQkJtSd5UTj3KXZCcqHjmoWesh4TK1xDgTJN22ujqJR0QjcM41dv4mPTx8Sr8Lk0rGT9maB6WvBQe7WVKQHCUQZCabVlhfc5red1ZaIfToPCfsxkJ9nBW_RPIsCpVOodF3SYlDIRw?purpose=inline)

[a55.tech](https://a55.tech/platform?utm_source=chatgpt.com)

Backend

Go

Frontend

React + TypeScript

Database

PostgreSQL

Architecture

Modular Monolith → Microservices

Главная идея

Платформа, которая позволяет пользователям хранить средства на внутренних счетах, переводить деньги, оплачивать покупки, принимать платежи как мерчант, делать возвраты и контролировать финансовые операции.

Все финансовые операции на первом этапе симулируются внутри проекта. Это не подключение к реальной банковской инфраструктуре.

## 1. Концепция и границы проекта

Сначала определим, что именно мы разрабатываем.

PayCore — это условная платёжная организация, предоставляющая инфраструктуру для денежных операций.

У платформы будет три основных интерфейса:

1. Customer Dashboard — личный кабинет обычного пользователя.
2. Merchant Dashboard — кабинет бизнеса, принимающего платежи.
3. Admin Dashboard — внутренняя панель управления платёжной инфраструктурой.

Также будет публичный API, через который сторонние приложения смогут создавать платежи и получать информацию об их статусе.

### Основные пользовательские сценарии

| Пользователь  | Что может делать                                       |
| ------------- | ------------------------------------------------------ |
| Клиент        | Создавать счета, переводить деньги, оплачивать покупки |
| Клиент        | Смотреть баланс, выписки и историю транзакций          |
| Мерчант       | Создавать платежи, получать оплаты, оформлять возвраты |
| Мерчант       | Смотреть оборот, комиссии, выплаты                     |
| Администратор | Управлять пользователями и счетами                     |
| Администратор | Проверять подозрительные операции                      |
| Администратор | Управлять комиссиями, лимитами и блокировками          |
| Оператор      | Разбирать ошибки, сверять платежи, просматривать аудит |

### Что будет отличать PayCore от обычного банковского приложения

Мы реализуем не просто таблицу транзакций.

В основе будет собственная финансовая система — Double-Entry Ledger (двойная бухгалтерская запись).

Это фундамент проекта.

Каждое движение денег будет представлено финансовыми проводками. Система должна уметь доказать, откуда пришли деньги, куда ушли и почему изменился баланс.

## 2. Функциональные требования (ТЗ)

Разделим функциональность на 12 бизнес-модулей.

### 2.1. Authentication & Authorization

Система аутентификации и разграничения прав.

Функции:

- Регистрация пользователей.
- Авторизация по email и паролю.
- Access Token и Refresh Token.
- Ротация Refresh Token.
- Выход из системы и отзыв сессий.
- RBAC — Role-Based Access Control.
- Управление пользовательскими сессиями.
- Подтверждение важных действий.
- Ограничение количества попыток авторизации.
- Аудит входов и подозрительных попыток.

Роли:

| Роль         | Права                                        |
| ------------ | -------------------------------------------- |
| CUSTOMER     | Операции со своими счетами                   |
| MERCHANT     | Управление собственным бизнесом              |
| SUPPORT      | Просмотр обращений и ограниченная информация |
| OPERATOR     | Обработка платежных инцидентов               |
| RISK_ANALYST | Проверка подозрительных операций             |
| ADMIN        | Администрирование системы                    |

Важно: роль администратора не должна автоматически давать возможность бесконтрольно перемещать деньги.

### 2.2. Account Management

Управление счетами.

Функции:

- Создание счетов.
- Поддержка нескольких валют.
- Просмотр баланса.
- Просмотр доступных и зарезервированных средств.
- Блокировка и разблокировка счетов.
- Ограничение операций.
- История изменений статуса.
- Получение выписки.

Пример:

| Счёт           | Валюта | Доступно | Зарезервировано |
| -------------- | ------ | -------- | --------------- |
| Основной       | RUB    | 85 000   | 5 000           |
| Дополнительный | USD    | 500      | 0               |
| Накопительный  | RUB    | 120 000  | 0               |

Для демонстрации можно использовать тестовые деньги. Никакого отношения к реальным банковским остаткам они иметь не будут.

### 2.3. Ledger Service — ядро проекта

Критически важный модуль

Ledger отвечает за учёт денежных средств.

Основные требования:

- Двойная запись каждой финансовой операции.
- Неизменяемость проведённых записей.
- Проверка сбалансированности проводок.
- Раздельный учёт валют.
- История всех движений средств.
- Поддержка резервирования.
- Корректировка только через новые компенсирующие проводки.
- Возможность восстановления баланса по истории операций.

Пример перевода 1 000 рублей:

| Счёт Ledger      | Дебет | Кредит |
| ---------------- | ----- | ------ |
| Счёт отправителя | 1 000 | 0      |
| Счёт получателя  | 0     | 1 000  |
| Итого            | 1 000 | 1 000  |

Правило:

\\[ \sum Debit = \sum Credit \\]

Никакая проведённая операция не должна менять историю задним числом.

Баланс можно хранить в отдельной проекции для быстрого чтения, но первоисточником финансовой истории остаётся Ledger.

### 2.4. Payment Processing

Основная система обработки платежей.

Возможности:

- Создание платежа.
- Авторизация платежа.
- Резервирование денег.
- Подтверждение платежа (Capture).
- Отмена авторизации (Void).
- Возврат средств (Refund).
- Частичный возврат.
- Отслеживание статуса.
- Комиссии.
- История попыток обработки.

Жизненный цикл:

\#chatgpt-mermaid-\_r_338\_{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;fill:rgb(237, 237, 237);}@keyframes edge-animation-frame{from{stroke-dashoffset:0;}}@keyframes dash{to{stroke-dashoffset:0;}}#chatgpt-mermaid-\_r_338\_ .edge-animation-slow{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 50s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_338\_ .edge-animation-fast{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 20s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_338\_ .error-icon{fill:rgb(48, 48, 48);}#chatgpt-mermaid-\_r_338\_ .error-text{fill:rgb(237, 237, 237);stroke:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_338\_ .edge-thickness-normal{stroke-width:1px;}#chatgpt-mermaid-\_r_338\_ .edge-thickness-thick{stroke-width:3.5px;}#chatgpt-mermaid-\_r_338\_ .edge-pattern-solid{stroke-dasharray:0;}#chatgpt-mermaid-\_r_338\_ .edge-thickness-invisible{stroke-width:0;fill:none;}#chatgpt-mermaid-\_r_338\_ .edge-pattern-dashed{stroke-dasharray:3;}#chatgpt-mermaid-\_r_338\_ .edge-pattern-dotted{stroke-dasharray:2;}#chatgpt-mermaid-\_r_338\_ .marker{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_338\_ .marker.cross{stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_338\_ svg{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;}#chatgpt-mermaid-\_r_338\_ p{margin:0;}#chatgpt-mermaid-\_r_338\_ defs [id$="-barbEnd"]{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_338\_ g.stateGroup text{fill:rgb(31, 78, 148);stroke:none;font-size:10px;}#chatgpt-mermaid-\_r_338\_ g.stateGroup text{fill:rgb(237, 237, 237);stroke:none;font-size:10px;}#chatgpt-mermaid-\_r_338\_ g.stateGroup .state-title{font-weight:bolder;fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_338\_ g.stateGroup rect{fill:rgb(9, 23, 44);stroke:rgb(31, 78, 148);}#chatgpt-mermaid-\_r_338\_ g.stateGroup line{stroke:rgb(175, 175, 175);stroke-width:1;}#chatgpt-mermaid-\_r_338\_ .transition{stroke:rgb(175, 175, 175);stroke-width:1;fill:none;}#chatgpt-mermaid-\_r_338\_ .stateGroup .composit{fill:transparent;border-bottom:1px;}#chatgpt-mermaid-\_r_338\_ .stateGroup .alt-composit{fill:#e0e0e0;border-bottom:1px;}#chatgpt-mermaid-\_r_338\_ .state-note{stroke:rgb(58, 132, 63);fill:rgb(48, 48, 48);}#chatgpt-mermaid-\_r_338\_ .state-note text{fill:rgb(237, 237, 237);stroke:none;font-size:10px;}#chatgpt-mermaid-\_r_338\_ .stateLabel .box{stroke:none;stroke-width:0;fill:rgb(9, 23, 44);opacity:0.5;}#chatgpt-mermaid-\_r_338\_ .edgeLabel .label rect{fill:rgb(9, 23, 44);opacity:0.5;}#chatgpt-mermaid-\_r_338\_ .edgeLabel{background-color:rgb(0, 0, 0);text-align:center;}#chatgpt-mermaid-\_r_338\_ .edgeLabel p{background-color:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_338\_ .edgeLabel rect{opacity:0.5;background-color:rgb(0, 0, 0);fill:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_338\_ .edgeLabel .label text{fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_338\_ .label div .edgeLabel{color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_338\_ .stateLabel text{fill:rgb(237, 237, 237);font-size:10px;font-weight:bold;}#chatgpt-mermaid-\_r_338\_ .node circle.state-start{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_338\_ .node .fork-join{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_338\_ .node circle.state-end{fill:rgb(31, 78, 148);stroke:transparent;stroke-width:1.5;}#chatgpt-mermaid-\_r_338\_ .end-state-inner{fill:transparent;stroke-width:1.5;}#chatgpt-mermaid-\_r_338\_ .node rect{fill:rgb(9, 23, 44);stroke:rgb(31, 78, 148);stroke-width:1px;}#chatgpt-mermaid-\_r_338\_ .node polygon{fill:rgb(9, 23, 44);stroke:rgb(31, 78, 148);stroke-width:1px;}#chatgpt-mermaid-\_r_338\_ [id$="-barbEnd"]{fill:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_338\_ .statediagram-cluster rect{fill:rgb(9, 23, 44);stroke:rgb(31, 78, 148);stroke-width:1px;}#chatgpt-mermaid-\_r_338\_ .cluster-label,#chatgpt-mermaid-\_r_338\_ .nodeLabel{color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_338\_ .statediagram-cluster rect.outer{rx:5px;ry:5px;}#chatgpt-mermaid-\_r_338\_ .statediagram-state .divider{stroke:rgb(31, 78, 148);}#chatgpt-mermaid-\_r_338\_ .statediagram-state .title-state{rx:5px;ry:5px;}#chatgpt-mermaid-\_r_338\_ .statediagram-cluster.statediagram-cluster .inner{fill:transparent;}#chatgpt-mermaid-\_r_338\_ .statediagram-cluster.statediagram-cluster-alt .inner{fill:rgb(48, 48, 48);}#chatgpt-mermaid-\_r_338\_ .statediagram-cluster .inner{rx:0;ry:0;}#chatgpt-mermaid-\_r_338\_ .statediagram-state rect.basic{rx:5px;ry:5px;}#chatgpt-mermaid-\_r_338\_ .statediagram-state rect.divider{stroke-dasharray:10,10;fill:rgb(48, 48, 48);}#chatgpt-mermaid-\_r_338\_ .note-edge{stroke-dasharray:5;}#chatgpt-mermaid-\_r_338\_ .statediagram-note rect{fill:rgb(48, 48, 48);stroke:rgb(58, 132, 63);stroke-width:1px;rx:0;ry:0;}#chatgpt-mermaid-\_r_338\_ .statediagram-note rect{fill:rgb(48, 48, 48);stroke:rgb(58, 132, 63);stroke-width:1px;rx:0;ry:0;}#chatgpt-mermaid-\_r_338\_ .statediagram-note text{fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_338\_ .statediagram-note .nodeLabel{color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_338\_ .statediagram .edgeLabel{color:red;}#chatgpt-mermaid-\_r_338\_ [id$="-dependencyStart"],#chatgpt-mermaid-\_r_338\_ [id$="-dependencyEnd"]{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);stroke-width:1;}#chatgpt-mermaid-\_r_338\_ .statediagramTitleText{text-anchor:middle;font-size:18px;fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].statediagram-cluster rect{fill:rgb(9, 23, 44);stroke:url(#chatgpt-mermaid-\_r_338\_-gradient);stroke-width:1;}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].statediagram-cluster rect.outer{rx:5px;ry:5px;filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_338\_ .node .neo-node{stroke:rgb(31, 78, 148);}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].node rect,#chatgpt-mermaid-\_r_338\_ [data-look="neo"].cluster rect,#chatgpt-mermaid-\_r_338\_ [data-look="neo"].node polygon{stroke:url(#chatgpt-mermaid-\_r_338\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].swimlane.cluster rect{filter:none;}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].node path{stroke:url(#chatgpt-mermaid-\_r_338\_-gradient);stroke-width:1px;}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].node .outer-path{filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].node .neo-line path{stroke:rgb(31, 78, 148);filter:none;}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].node circle{stroke:url(#chatgpt-mermaid-\_r_338\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].node circle .state-start{fill:#000000;}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].icon-shape .icon{fill:url(#chatgpt-mermaid-\_r_338\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_338\_ [data-look="neo"].icon-shape .icon-neo path{stroke:url(#chatgpt-mermaid-\_r_338\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_338\_ :root{--mermaid-font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";}CREATEDPENDINGAUTHORIZEDFAILEDCAPTUREDCANCELLEDPARTIALLY_REFUNDEDREFUNDED

Не все переходы разрешены. Например, нельзя вернуть средства по платежу, который никогда не был проведён.

### 2.5. Transfers

Переводы между пользователями.

Функции:

- Перевод между своими счетами.
- Перевод другому пользователю.
- Проверка лимитов.
- Проверка доступного баланса.
- Комиссии за перевод.
- Статус перевода.
- Отмена до момента проведения.
- История переводов.

Особое внимание уделим конкурентности.

Пример проблемы:

У пользователя 10 000 рублей. Одновременно поступают два запроса на перевод по 8 000 рублей.

Система не должна позволить потратить 16 000 рублей.

Мы будем решать это с помощью PostgreSQL-транзакций, блокировок и правильно спроектированного порядка выполнения операций.

### 2.6. Merchant Management

Мерчанты — организации, которые используют PayCore для приёма платежей.

Функции:

- Регистрация бизнеса.
- Создание магазинов.
- Выпуск API-ключей.
- Управление webhook.
- Просмотр платежей.
- Комиссии и тарифы.
- Выплаты.
- Статистика продаж.

Для демонстрации можно создать тестовый интернет-магазин, который будет принимать оплату через PayCore.

### 2.7. Commission Engine

Отдельный механизм расчёта комиссий.

Например:

- Комиссия за перевод — 0,5%.
- Комиссия за оплату — 1,5%.
- Минимальная комиссия — 10 рублей.
- Максимальная комиссия — 500 рублей.

Все тарифы должны иметь версии и периоды действия.

Расчёт должен использовать целочисленную арифметику с явно заданными правилами округления. Никаких `float64` для денежных значений.

### 2.8. Fraud Detection

Система обнаружения мошенничества.

На первом этапе — rule-based engine.

Примеры правил:

- Более 10 операций за минуту.
- Слишком крупная операция.
- Несколько неудачных попыток подряд.
- Превышение лимита.
- Подозрительная последовательность платежей.

Результат:

- Risk Score.
- Список сработавших правил.
- Решение: ALLOW, REVIEW или BLOCK.
- Создание инцидента для аналитика.

### 2.9. Notification Service

Уведомления:

- Успешный перевод.
- Успешная оплата.
- Возврат.
- Блокировка счёта.
- Подозрительная операция.
- Изменение настроек безопасности.

Каналы: email, in-app notifications, webhook для мерчантов.

### 2.10. Reconciliation

Финансовая сверка.

Это одна из самых интересных функций проекта.

Допустим, у нас есть внутренний реестр платежей и внешний симулятор платёжного провайдера.

Система должна:

1. Получить реестр операций провайдера.
2. Сопоставить операции с внутренними платежами.
3. Найти расхождения.
4. Сформировать отчёт.
5. Создать инциденты.
6. Позволить оператору разобрать проблему.

### 2.11. Reporting & Analytics

Отчётность:

- Оборот за период.
- Количество платежей.
- Успешные и неуспешные операции.
- Средний чек.
- Комиссионный доход.
- Возвраты.
- Активность пользователей.
- Распределение по валютам.

### 2.12. Audit & Monitoring

Журналирование всех критически важных действий:

- Авторизация.
- Создание счёта.
- Блокировка.
- Проведение платежа.
- Возврат.
- Изменение тарифов.
- Действия сотрудников.

Важное требование: audit log должен быть отдельным от обычных технических логов.

## 3. Архитектура системы

Теперь переходим к инженерной части.

Я предлагаю использовать Modular Monolith с последующим переходом к Microservices.

Почему не начинать сразу с микросервисов?

Потому что распределённые транзакции, сетевые ошибки и согласованность данных добавят огромную сложность ещё до того, как появится рабочая финансовая логика. Мы сначала сделаем надёжное ядро, а затем научимся разделять его на сервисы.

### 3.1. Общая архитектурная схема

\#chatgpt-mermaid-\_r_33f\_{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;fill:rgb(237, 237, 237);}@keyframes edge-animation-frame{from{stroke-dashoffset:0;}}@keyframes dash{to{stroke-dashoffset:0;}}#chatgpt-mermaid-\_r_33f\_ .edge-animation-slow{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 50s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_33f\_ .edge-animation-fast{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 20s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_33f\_ .error-icon{fill:rgb(48, 48, 48);}#chatgpt-mermaid-\_r_33f\_ .error-text{fill:rgb(237, 237, 237);stroke:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33f\_ .edge-thickness-normal{stroke-width:1px;}#chatgpt-mermaid-\_r_33f\_ .edge-thickness-thick{stroke-width:3.5px;}#chatgpt-mermaid-\_r_33f\_ .edge-pattern-solid{stroke-dasharray:0;}#chatgpt-mermaid-\_r_33f\_ .edge-thickness-invisible{stroke-width:0;fill:none;}#chatgpt-mermaid-\_r_33f\_ .edge-pattern-dashed{stroke-dasharray:3;}#chatgpt-mermaid-\_r_33f\_ .edge-pattern-dotted{stroke-dasharray:2;}#chatgpt-mermaid-\_r_33f\_ .marker{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_33f\_ .marker.cross{stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_33f\_ svg{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;}#chatgpt-mermaid-\_r_33f\_ p{margin:0;}#chatgpt-mermaid-\_r_33f\_ .label{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33f\_ .cluster-label text{fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33f\_ .cluster-label span{color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33f\_ .cluster-label span p{background-color:transparent;}#chatgpt-mermaid-\_r_33f\_ .label text,#chatgpt-mermaid-\_r_33f\_ span{fill:rgb(237, 237, 237);color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33f\_ .node rect,#chatgpt-mermaid-\_r_33f\_ .node circle,#chatgpt-mermaid-\_r_33f\_ .node ellipse,#chatgpt-mermaid-\_r_33f\_ .node polygon,#chatgpt-mermaid-\_r_33f\_ .node path{fill:rgb(9, 23, 44);stroke:rgb(31, 78, 148);stroke-width:1px;}#chatgpt-mermaid-\_r_33f\_ .rough-node .label text,#chatgpt-mermaid-\_r_33f\_ .node .label text,#chatgpt-mermaid-\_r_33f\_ .image-shape .label,#chatgpt-mermaid-\_r_33f\_ .icon-shape .label{text-anchor:middle;}#chatgpt-mermaid-\_r_33f\_ .node .katex path{fill:#000;stroke:#000;stroke-width:1px;}#chatgpt-mermaid-\_r_33f\_ .rough-node .label,#chatgpt-mermaid-\_r_33f\_ .node .label,#chatgpt-mermaid-\_r_33f\_ .image-shape .label,#chatgpt-mermaid-\_r_33f\_ .icon-shape .label{text-align:center;}#chatgpt-mermaid-\_r_33f\_ .node.clickable{cursor:pointer;}#chatgpt-mermaid-\_r_33f\_ .root .anchor path{fill:rgb(175, 175, 175)!important;stroke-width:0;stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_33f\_ .arrowheadPath{fill:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_33f\_ .edgePath .path{stroke:rgb(175, 175, 175);stroke-width:1px;}#chatgpt-mermaid-\_r_33f\_ .flowchart-link{stroke:rgb(175, 175, 175);fill:none;}#chatgpt-mermaid-\_r_33f\_ .edgeLabel{background-color:rgb(0, 0, 0);text-align:center;}#chatgpt-mermaid-\_r_33f\_ .edgeLabel p{background-color:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_33f\_ .edgeLabel rect{opacity:0.5;background-color:rgb(0, 0, 0);fill:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_33f\_ .labelBkg{background-color:rgba(0, 0, 0, 0.5);}#chatgpt-mermaid-\_r_33f\_ .cluster rect{fill:rgb(48, 48, 48);stroke:rgba(255, 255, 255, 0.15);stroke-width:1px;}#chatgpt-mermaid-\_r_33f\_ .cluster text{fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33f\_ .cluster span{color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33f\_ div.mermaidTooltip{position:absolute;text-align:center;max-width:200px;padding:2px;font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:12px;background:rgb(48, 48, 48);border:1px solid rgba(255, 255, 255, 0.15);border-radius:2px;pointer-events:none;z-index:100;}#chatgpt-mermaid-\_r_33f\_ .flowchartTitleText{text-anchor:middle;font-size:18px;fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33f\_ rect.text{fill:none;stroke-width:0;}#chatgpt-mermaid-\_r_33f\_ .icon-shape,#chatgpt-mermaid-\_r_33f\_ .image-shape{background-color:rgb(0, 0, 0);text-align:center;}#chatgpt-mermaid-\_r_33f\_ .icon-shape p,#chatgpt-mermaid-\_r_33f\_ .image-shape p{background-color:rgb(0, 0, 0);padding:2px;}#chatgpt-mermaid-\_r_33f\_ .icon-shape .label rect,#chatgpt-mermaid-\_r_33f\_ .image-shape .label rect{opacity:0.5;background-color:rgb(0, 0, 0);fill:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_33f\_ .label-icon{display:inline-block;height:1em;overflow:visible;vertical-align:-0.125em;}#chatgpt-mermaid-\_r_33f\_ .node .label-icon path{fill:currentColor;stroke:revert;stroke-width:revert;}#chatgpt-mermaid-\_r_33f\_ .node .neo-node{stroke:rgb(31, 78, 148);}#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].node rect,#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].cluster rect,#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].node polygon{stroke:url(#chatgpt-mermaid-\_r_33f\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].swimlane.cluster rect{filter:none;}#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].node path{stroke:url(#chatgpt-mermaid-\_r_33f\_-gradient);stroke-width:1px;}#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].node .outer-path{filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].node .neo-line path{stroke:rgb(31, 78, 148);filter:none;}#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].node circle{stroke:url(#chatgpt-mermaid-\_r_33f\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].node circle .state-start{fill:#000000;}#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].icon-shape .icon{fill:url(#chatgpt-mermaid-\_r_33f\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33f\_ [data-look="neo"].icon-shape .icon-neo path{stroke:url(#chatgpt-mermaid-\_r_33f\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33f\_ .node text{font-size:14px;font-weight:600;letter-spacing:normal;fill:rgb(153, 206, 255);}#chatgpt-mermaid-\_r_33f\_ .edgeLabels text{font-size:13px;font-weight:600;letter-spacing:-0.08px;fill:rgb(153, 206, 255);}#chatgpt-mermaid-\_r_33f\_ .node tspan[font-weight="normal"],#chatgpt-mermaid-\_r_33f\_ .edgeLabels tspan[font-weight="normal"]{font-weight:600;}#chatgpt-mermaid-\_r_33f\_ .edgeLabel .label rect{opacity:1;rx:13px;ry:13px;fill:rgb(0, 14, 26);stroke:rgb(26, 62, 95);stroke-width:1px;}#chatgpt-mermaid-\_r_33f\_ .node rect,#chatgpt-mermaid-\_r_33f\_ .node circle,#chatgpt-mermaid-\_r_33f\_ .node ellipse,#chatgpt-mermaid-\_r_33f\_ .node polygon,#chatgpt-mermaid-\_r_33f\_ .node path{fill:rgb(0, 40, 77);stroke:rgba(255, 255, 255, 0.1);stroke-width:1px;}#chatgpt-mermaid-\_r_33f\_ .node rect{rx:16px;ry:16px;}#chatgpt-mermaid-\_r_33f\_ .mermaid-decision .label-container{fill:rgb(0, 14, 26);stroke:rgb(26, 62, 95);stroke-dasharray:2,2;}#chatgpt-mermaid-\_r_33f\_ .edgePaths .flowchart-link{stroke:rgb(175, 175, 175);stroke-width:1px;stroke-linecap:round;stroke-linejoin:round;}#chatgpt-mermaid-\_r_33f\_ .marker{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_33f\_ :root{--mermaid-font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";}ObservabilityInfrastructureGo BackendКлиентские приложенияPrometheusGrafanaOpenTelemetryPostgreSQLRedisKafkaBackground WorkersObject StorageAPI Layer / MiddlewareAuthAccountsPaymentsLedgerTransfersMerchantsFraud EngineCommission EngineReconciliationNotificationsReportingReact + TypeScriptMerchant DashboardAdmin DashboardExternal API Clients

Это логическая схема. В первой версии все бизнес-модули будут находиться в одном Go-приложении и использовать одну PostgreSQL-базу.

### 3.2. Архитектура внутри Go

Будем использовать принципы Clean Architecture и разделение по бизнес-доменам.

Каждый модуль будет иметь примерно такую структуру:

```
internal/
└── payments/
    ├── domain/
    │   ├── payment.go
    │   ├── status.go
    │   ├── errors.go
    │   └── events.go
    │
    ├── application/
    │   ├── create_payment.go
    │   ├── authorize_payment.go
    │   ├── capture_payment.go
    │   └── refund_payment.go
    │
    ├── infrastructure/
    │   ├── postgres/
    │   │   └── repository.go
    │   ├── kafka/
    │   │   └── publisher.go
    │   └── provider/
    │       └── simulator.go
    │
    └── transport/
        ├── http/
        │   ├── handler.go
        │   ├── dto.go
        │   └── routes.go
        └── grpc/
```

Распределение ответственности:

- `domain` — бизнес-модель, правила, инварианты.
- `application` — сценарии использования.
- `infrastructure` — PostgreSQL, Redis, Kafka и внешние интеграции.
- `transport` — REST API и gRPC.

Главное правило: бизнес-логика не должна зависеть от конкретного HTTP-фреймворка или PostgreSQL.

Например, `CapturePayment` должен работать с интерфейсами репозиториев и Ledger, а не самостоятельно выполнять SQL-запросы из HTTP-обработчика.

### 3.3. Какие сервисы выделим впоследствии

После реализации модульного монолита:

| Сервис               | Ответственность            | Приоритет выделения |
| -------------------- | -------------------------- | ------------------- |
| Identity Service     | Авторизация и безопасность | Средний             |
| Account Service      | Счета и пользователи       | Средний             |
| Payment Service      | Жизненный цикл платежей    | Высокий             |
| Ledger Service       | Финансовый учёт            | Очень высокий       |
| Fraud Service        | Анализ рисков              | Высокий             |
| Notification Service | Уведомления                | Средний             |
| Reporting Service    | Аналитика                  | Средний             |

Важный архитектурный момент: при переходе к микросервисам каждый сервис должен владеть своими данными. Нельзя просто разделить Go-папки на отдельные контейнеры и считать это полноценной микросервисной архитектурой.

## 4. Проектирование базы данных PostgreSQL

Теперь самое важное для backend-разработчика — модель данных.

Я предлагаю начать с PostgreSQL и использовать `pgx/v5`, SQL-миграции и явные SQL-запросы.

Не будем прятать всю работу с базой за ORM. Нам нужно хорошо понимать, какие запросы выполняются, какие блокировки используются и почему.

### 4.1. Основные сущности

База будет разделена логически на несколько доменов.


\#chatgpt-mermaid-\_r_33r\_{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;fill:rgb(237, 237, 237);}@keyframes edge-animation-frame{from{stroke-dashoffset:0;}}@keyframes dash{to{stroke-dashoffset:0;}}#chatgpt-mermaid-\_r_33r\_ .edge-animation-slow{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 50s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_33r\_ .edge-animation-fast{stroke-dasharray:9,5!important;stroke-dashoffset:900;animation:dash 20s linear infinite;stroke-linecap:round;}#chatgpt-mermaid-\_r_33r\_ .error-icon{fill:rgb(48, 48, 48);}#chatgpt-mermaid-\_r_33r\_ .error-text{fill:rgb(237, 237, 237);stroke:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33r\_ .edge-thickness-normal{stroke-width:1px;}#chatgpt-mermaid-\_r_33r\_ .edge-thickness-thick{stroke-width:3.5px;}#chatgpt-mermaid-\_r_33r\_ .edge-pattern-solid{stroke-dasharray:0;}#chatgpt-mermaid-\_r_33r\_ .edge-thickness-invisible{stroke-width:0;fill:none;}#chatgpt-mermaid-\_r_33r\_ .edge-pattern-dashed{stroke-dasharray:3;}#chatgpt-mermaid-\_r_33r\_ .edge-pattern-dotted{stroke-dasharray:2;}#chatgpt-mermaid-\_r_33r\_ .marker{fill:rgb(175, 175, 175);stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_33r\_ .marker.cross{stroke:rgb(175, 175, 175);}#chatgpt-mermaid-\_r_33r\_ svg{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";font-size:16px;}#chatgpt-mermaid-\_r_33r\_ p{margin:0;}#chatgpt-mermaid-\_r_33r\_ .entityBox{fill:rgb(9, 23, 44);stroke:rgb(31, 78, 148);}#chatgpt-mermaid-\_r_33r\_ .relationshipLabelBox{fill:rgb(48, 48, 48);opacity:0.7;background-color:rgb(48, 48, 48);}#chatgpt-mermaid-\_r_33r\_ .relationshipLabelBox rect{opacity:0.5;}#chatgpt-mermaid-\_r_33r\_ .labelBkg{background-color:rgba(48, 48, 48, 0.5);}#chatgpt-mermaid-\_r_33r\_ .edgeLabel{background-color:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_33r\_ .edgeLabel .label rect{fill:rgb(0, 0, 0);}#chatgpt-mermaid-\_r_33r\_ .edgeLabel .label text{fill:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33r\_ .edgeLabel .label{fill:rgb(31, 78, 148);font-size:14px;}#chatgpt-mermaid-\_r_33r\_ .label{font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";color:rgb(237, 237, 237);}#chatgpt-mermaid-\_r_33r\_ .edge-pattern-dashed{stroke-dasharray:8,8;}#chatgpt-mermaid-\_r_33r\_ .node rect,#chatgpt-mermaid-\_r_33r\_ .node circle,#chatgpt-mermaid-\_r_33r\_ .node ellipse,#chatgpt-mermaid-\_r_33r\_ .node polygon{fill:rgb(9, 23, 44);stroke:rgb(31, 78, 148);stroke-width:1px;}#chatgpt-mermaid-\_r_33r\_ .relationshipLine{stroke:rgb(175, 175, 175);stroke-width:1px;fill:none;}#chatgpt-mermaid-\_r_33r\_ .marker{fill:none!important;stroke:rgb(175, 175, 175)!important;stroke-width:1;}#chatgpt-mermaid-\_r_33r\_ [data-look=neo].labelBkg{background-color:rgba(48, 48, 48, 0.5);}#chatgpt-mermaid-\_r_33r\_ .node .neo-node{stroke:rgb(31, 78, 148);}#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].node rect,#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].cluster rect,#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].node polygon{stroke:url(#chatgpt-mermaid-\_r_33r\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].swimlane.cluster rect{filter:none;}#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].node path{stroke:url(#chatgpt-mermaid-\_r_33r\_-gradient);stroke-width:1px;}#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].node .outer-path{filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].node .neo-line path{stroke:rgb(31, 78, 148);filter:none;}#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].node circle{stroke:url(#chatgpt-mermaid-\_r_33r\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].node circle .state-start{fill:#000000;}#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].icon-shape .icon{fill:url(#chatgpt-mermaid-\_r_33r\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33r\_ [data-look="neo"].icon-shape .icon-neo path{stroke:url(#chatgpt-mermaid-\_r_33r\_-gradient);filter:drop-shadow( 1px 2px 2px rgba(185,185,185,1));}#chatgpt-mermaid-\_r_33r\_ :root{--mermaid-font-family:-apple-system-body,ui-sans-serif,-apple-system,system-ui,"Segoe UI",Helvetica,"Apple Color Emoji",Arial,sans-serif,"Segoe UI Emoji","Segoe UI Symbol";}USERSUSER_ROLESROLESACCOUNTSSESSIONSMERCHANTSMERCHANT_MEMBERSPAYMENTSREFUNDSPAYMENT_ATTEMPTSLEDGER_ACCOUNTSLEDGER_ENTRIESJOURNAL_TRANSACTIONSTRANSFERSHOLDSAUDIT_LOGSWEBHOOK_ENDPOINTSOUTBOX_EVENTSWEBHOOK_DELIVERIEShasassignedownshashasbelongsreceivesfundshascontainsmapsrecordscontainsgeneratesgenerateshastriggersconfigurestriggers

Это концептуальная ER-модель, а не полный список всех внешних ключей.

### 4.2. Таблицы пользователей и доступа

users

| Поле          | Тип         | Назначение     |
| ------------- | ----------- | -------------- |
| id            | UUID        | Идентификатор  |
| email         | VARCHAR     | Email          |
| password_hash | TEXT        | Хеш пароля     |
| first_name    | VARCHAR     | Имя            |
| last_name     | VARCHAR     | Фамилия        |
| status        | VARCHAR     | Статус         |
| created_at    | TIMESTAMPTZ | Дата создания  |
| updated_at    | TIMESTAMPTZ | Дата изменения |

Ограничения:

- Email уникальный.
- Пароль хранится только в виде безопасного хеша.
- Статус ограничен допустимыми значениями.

Дополнительные таблицы:

```
roles
user_roles
sessions
refresh_tokens
login_attempts
```

### 4.3. Счета

accounts

| Поле           | Тип         | Назначение  |
| -------------- | ----------- | ----------- |
| id             | UUID        | ID счёта    |
| user_id        | UUID        | Владелец    |
| account_number | VARCHAR     | Номер счёта |
| currency       | CHAR(3)     | Валюта      |
| status         | VARCHAR     | Статус      |
| created_at     | TIMESTAMPTZ | Создание    |

Баланс будет храниться в отдельной таблице-проекции:

account_balances

| Поле             | Тип         |
| ---------------- | ----------- |
| account_id       | UUID        |
| currency         | CHAR(3)     |
| available_amount | BIGINT      |
| reserved_amount  | BIGINT      |
| version          | BIGINT      |
| updated_at       | TIMESTAMPTZ |

Суммы храним в минимальных денежных единицах: например, 100 рублей = 10 000 копеек.

Не используем `float32` и `float64` для финансовых расчётов.

Баланс-проекция нужна для быстрого чтения, но при проведении операции она должна обновляться согласованно с проводками в одной транзакции PostgreSQL.

### 4.4. Ledger — финансовое ядро

Здесь будут три основные таблицы.

ledger_accounts

| Поле         | Тип         | Назначение                   |
| ------------ | ----------- | ---------------------------- |
| id           | UUID        | ID                           |
| owner_type   | VARCHAR     | CUSTOMER / MERCHANT / SYSTEM |
| owner_id     | UUID        | Владелец                     |
| account_type | VARCHAR     | Тип счёта                    |
| currency     | CHAR(3)     | Валюта                       |
| created_at   | TIMESTAMPTZ | Создание                     |

journal_transactions

| Поле           | Тип         | Назначение         |
| -------------- | ----------- | ------------------ |
| id             | UUID        | ID операции        |
| reference_type | VARCHAR     | PAYMENT / TRANSFER |
| reference_id   | UUID        | ID бизнес-операции |
| status         | VARCHAR     | Статус             |
| created_at     | TIMESTAMPTZ | Время создания     |

ledger_entries

| Поле                   | Тип         | Назначение          |
| ---------------------- | ----------- | ------------------- |
| id                     | UUID        | ID проводки         |
| journal_transaction_id | UUID        | Финансовая операция |
| ledger_account_id      | UUID        | Счёт                |
| entry_type             | VARCHAR     | DEBIT / CREDIT      |
| amount                 | BIGINT      | Сумма               |
| currency               | CHAR(3)     | Валюта              |
| created_at             | TIMESTAMPTZ | Дата                |

Обязательные правила:

1. Сумма проводки всегда больше нуля.
2. Для каждой операции сумма дебета равна сумме кредита в каждой валюте.
3. Проведённые проводки нельзя редактировать и удалять.
4. Исправления делаются обратными или корректирующими проводками.
5. Нельзя проводить операцию по счёту в другой валюте без отдельного FX-сценария.

Для обеспечения целостности будем использовать ограничения, транзакции, а также отдельную процедуру или сервис проведения сбалансированных записей. Обычного `CHECK` на одной строке недостаточно для проверки суммы по нескольким строкам.

### 4.5. Платежи и переводы

payments

```
id
merchant_id
customer_account_id
amount
currency
fee_amount
status
idempotency_key
description
created_at
updated_at
```

payment_attempts

```
id
payment_id
attempt_number
provider_reference
status
error_code
started_at
finished_at
```

refunds

```
id
payment_id
amount
currency
status
reason
created_at
```

transfers

```
id
sender_account_id
receiver_account_id
amount
currency
fee_amount
status
created_at
```

Для частичных возвратов необходимо проверять, что сумма всех успешных возвратов не превышает сумму исходного платежа. Эту проверку будем делать транзакционно.

### 4.6. Инфраструктурные таблицы

Это таблицы, которые обеспечивают надёжность всей системы:

| Таблица              | Назначение                                   |
| -------------------- | -------------------------------------------- |
| idempotency_keys     | Защита от повторного выполнения запросов     |
| outbox_events        | События для последующей отправки в Kafka     |
| inbox_messages       | Учёт обработанных сообщений                  |
| audit_logs           | История действий пользователей и сотрудников |
| holds                | Резервирование средств                       |
| fraud_rules          | Правила обнаружения мошенничества            |
| fraud_cases          | Инциденты для проверки                       |
| webhook_endpoints    | Настройки webhook                            |
| webhook_deliveries   | Попытки доставки webhook                     |
| reconciliation_runs  | Запуски финансовой сверки                    |
| reconciliation_items | Результаты сопоставления операций            |

### 4.7. Индексы и оптимизация

Сразу предусмотрим индексы для наиболее частых запросов.

Например:

```
CREATE INDEX idx_payments_merchant_created
ON payments (merchant_id, created_at DESC);

CREATE INDEX idx_payments_status_created
ON payments (status, created_at DESC);

CREATE INDEX idx_transfers_sender_created
ON transfers (sender_account_id, created_at DESC);

CREATE INDEX idx_ledger_entries_transaction
ON ledger_entries (journal_transaction_id);
```

Также будем изучать:

- `EXPLAIN ANALYZE`;
- составные индексы;
- частичные индексы;
- уникальные индексы;
- блокировки строк;
- MVCC;
- уровни изоляции;
- connection pooling.

Важное решение: все денежные операции будут атомарными. Создание проводок, изменение баланса-проекции, изменение состояния платежа и запись Outbox-события должны происходить в одной транзакции, где это относится к одному локальному бизнес-процессу.

## 5. REST API и взаимодействие с системой

Публичное API будет версионированным:

`/api/v1/...`

### Основные endpoint'ы

Authentication

| Метод | Endpoint         | Назначение        |
| ----- | ---------------- | ----------------- |
| POST  | `/auth/register` | Регистрация       |
| POST  | `/auth/login`    | Авторизация       |
| POST  | `/auth/refresh`  | Обновление токена |
| POST  | `/auth/logout`   | Завершение сессии |

Accounts

| Метод | Endpoint                   | Назначение         |
| ----- | -------------------------- | ------------------ |
| POST  | `/accounts`                | Создание счёта     |
| GET   | `/accounts`                | Список счетов      |
| GET   | `/accounts/{id}`           | Информация о счёте |
| GET   | `/accounts/{id}/balance`   | Баланс             |
| GET   | `/accounts/{id}/statement` | Выписка            |

Transfers

| Метод | Endpoint          | Назначение            |
| ----- | ----------------- | --------------------- |
| POST  | `/transfers`      | Перевод               |
| GET   | `/transfers/{id}` | Информация о переводе |
| GET   | `/transfers`      | История               |

Payments

| Метод | Endpoint                   | Назначение       |
| ----- | -------------------------- | ---------------- |
| POST  | `/payments`                | Создание платежа |
| POST  | `/payments/{id}/authorize` | Авторизация      |
| POST  | `/payments/{id}/capture`   | Подтверждение    |
| POST  | `/payments/{id}/cancel`    | Отмена           |
| POST  | `/payments/{id}/refunds`   | Возврат          |
| GET   | `/payments/{id}`           | Информация       |

Merchants

| Метод | Endpoint              | Назначение        |
| ----- | --------------------- | ----------------- |
| POST  | `/merchants`          | Создание мерчанта |
| GET   | `/merchants/me`       | Профиль бизнеса   |
| POST  | `/merchants/api-keys` | Выпуск API-ключа  |
| POST  | `/merchants/webhooks` | Создание webhook  |

Admin

```
GET    /admin/users
GET    /admin/payments
GET    /admin/fraud/cases
POST   /admin/fraud/cases/{id}/resolve
POST   /admin/accounts/{id}/block
GET    /admin/audit-logs
GET    /admin/reconciliation/runs
```

Все защищённые маршруты должны проверять не только роль, но и принадлежность конкретного ресурса пользователю или мерчанту.

### Пример создания платежа

```
POST /api/v1/payments
Authorization: Bearer <token>
Idempotency-Key: order-123-payment-001
Content-Type: application/json
```

```
{
  "merchant_id": "uuid",
  "account_id": "uuid",
  "amount": 500000,
  "currency": "RUB",
  "description": "Order #123"
}
```

Ответ:

```
{
  "id": "uuid",
  "status": "CREATED",
  "amount": 500000,
  "currency": "RUB",
  "created_at": "2026-11-03T10:00:00Z"
}
```

В этом примере 500000 — это 5000 рублей в копейках.

Для повторного запроса с тем же ключом и тем же содержимым вернём результат первоначальной операции, а не создадим новый платёж. При несовпадении содержимого вернём конфликт.

## 6. Frontend — полноценный интерфейс

Я предлагаю сделать интерфейс современным, в стиле банковских финансовых платформ.

Технологии:

- React 19.
- TypeScript.
- Vite.
- React Router.
- TanStack Query.
- Zustand.
- Tailwind CSS.
- shadcn/ui.
- Recharts.
- React Hook Form + Zod.

### 6.1. Клиентский кабинет

[Financials Web UI Dark Theme by Filip  on Dribbble](https://images.openai.com/static-rsc-4/uxVDYS4aGnfPIvLCSciyju6c1dur6nTOmAclV_NkXIDzfQf3g9-olLV4wFkh2hJUtjKql9mi6qYeZjF-ganwWgmZfro7urDJnyWJuvrIgHH0z11U5x4MutMC4I4lSpF4nM9YQB8c_W6LodEoX99N4q81NwydzviK53b0F5ZeioI?purpose=inline)

[Fintech Mobile Banking Dashboard UI – Modern App Design :: Behance](https://images.openai.com/static-rsc-4/kEGmIckTTYW91hcWtHQtrQSg8k_fLnw_zjY5yKzKEoIhI-rGRsfbHdv4-YL6hxXCZrPsfKexKIrOX7HJ36tBPsqA7oWc-_A3-7tB_YLVKFLD-kRnGZfHMqSre6mXWdF5an567qzeOvtdx0zjFPUqpauGML0g9B2s-WkzRGc5lXA?purpose=inline)

[Finly App | Artkai 🚀](https://images.openai.com/static-rsc-4/0AlMRPrCGtfsUpDQgY6ybt8i2W-CbJI9ADMQYnmpmqdF10G5Dw9gzYH_9X_H4RhQaKJ2UH7E0Rt0pKFrCYvuJYVrVZ_WUBV57ybmtlBas7fu50IvAh8AMYirvLxzN9UjV-Yppld3LMWbn31OAg-YKngeoVE1sLSihCo1ecCNzOw?purpose=inline)

10

Страницы:

- Overview — общий обзор счетов.
- Accounts — управление счетами.
- Transfers — переводы.
- Payments — платежи.
- Transactions — история операций.
- Statements — выписки.
- Security — настройки безопасности.
- Profile — профиль.

### 6.2. Merchant Dashboard

[Fintech - B2B SaaS by Masum Parvej for Halal Lab on Dribbble](https://images.openai.com/static-rsc-4/U-6s1puofsFNOE9YIAc2CiRY77HgjOKrVA7qXXihfVHanDtpBGj_D__jwcxmscOfCisMWXJYOLzR0RlUDdE7HlZHKk8MaEQ26CAnTm_2p6lDJnmMJCS2ahR6TL61ihOKz2Vc8DeEBKIk6249DuSvSA9DiHxk2oC5y-iGpLMPYVY?purpose=inline)

[Enable Bank Downtime Alerts | Razorpay Docs](https://images.openai.com/static-rsc-4/qswm59gLJqWEOdmF5zMIEbrcB4RBLl3rGq6PkmgQ4LmDHiKAYFuAbOT0Uvmt_VZxBVSlwkUO5Sv4NV6GJUeN7oCPmhh3WWRkaVm5N9KGe3rAd4nyOx1YjCytbIKqw3bOZitR1WBkVNPIM1NWKo4221_yb9XMXF4nL-nd_fiCWyY?purpose=inline)

[ThirdPay. Make your money unstoppable.](https://images.openai.com/static-rsc-4/5KuyTqft_do2G8YISmpL2filFY-Br63H_R46ZJKDaZXQQIFSa7nRlzsC-So3KdEZMjhWzsKlI0aFJcnPJEplS0bbzYe7xTiHAGLXkHVLk0wXBiy0XRa6cDH2cu2ZrXXFT6uQXx5hJCIJaEjzYCb1RzBWxcdN0E1nDQVPSyZO-A0?purpose=inline)

11

Страницы:

- Dashboard.
- Payments.
- Refunds.
- Settlements.
- Analytics.
- API Keys.
- Webhooks.
- Developers.
- Settings.

Особенно интересна страница Developers: документация API, примеры запросов, тестовые ключи и журнал webhook-доставок.

### 6.3. Admin Dashboard

[Arkena – Financial KPI Dashboard by Arkan Tanriwa on Dribbble](https://images.openai.com/static-rsc-4/Z9wFHQ81rtIxXu30_BNok85t52LPcyH4n3DNqyXjXmyZnn33gYpQKuCZ6VN1XCqQqxjUEK7OOMzcVkTs2hYjTbrvLz7PYsEx8WpP5_M1SvyYRURUuvSC-ltEWS9OpGG9-IZ5i4QWoGvbmAi6Q-ZMfLOF25fYoXqBd61u7kGvS4A?purpose=inline)

[Orca Fraud - fight fraud in emerging markets](https://images.openai.com/static-rsc-4/o6MJMxd_DTfzqHKFDQxCFqnrvgz92XHuRF2vZrjXMZm7b1BOHLk8AJ2ZZH3QEZ2lbDPnR_NLO-RMt5gsu0BxGDeFzuMNFbkA8wDSJQ9ndVdvOS7HYYVwibaTJ6RBFzzM0E67YI-l5Y2PLfw5Rthy60BAadOgrMjGHgkZRf9_8vA?purpose=inline)

[OrbitPayout Product Redesign (DAR Design Case Study)](https://images.openai.com/static-rsc-4/5e1uD0vNOeygNKrLaGqIEMv7yAMSVUCGVLTVWlP_jg2xI7bu1UXVsGNd914aludFlE-Umq9CUmw9zAS3UxT8fTiRVjrrOlYzUIgD8352ksno2Wt4fi-qckN3ERDT50eXLxqKrF1a0LO6ZDNQPLSCBNIvTRZNWHkNHScoiuVR7fU?purpose=inline)

12

Страницы:

- System Overview.
- Users.
- Accounts.
- Payments.
- Fraud Monitoring.
- Reconciliation.
- Ledger Explorer.
- Audit Logs.
- System Health.
- Configuration.

Отдельно хочу сделать Ledger Explorer — интерфейс, в котором можно открыть любую финансовую операцию и посмотреть все её проводки, связанные платежи, события и изменения статусов.

Это будет одна из самых интересных демонстраций проекта.

### 6.4. Дизайн-система

Предлагаю визуально ориентироваться на современный enterprise fintech:

- Тёмная тема как основная.
- Графитовый фон.
- Тёмно-синие панели.
- Небольшие яркие акценты.
- Компактные таблицы.
- Финансовые графики.
- Фильтрация и поиск.
- Адаптивная вёрстка.

Важно, чтобы интерфейс не был просто красивой картинкой. Все таблицы, графики, фильтры и действия должны работать через настоящий API.

## 7. Полная структура Git-репозитория

Предлагаю один репозиторий — monorepo.

```
paycore/
│
├── cmd/
│   ├── api/
│   │   └── main.go
│   ├── worker/
│   │   └── main.go
│   └── scheduler/
│       └── main.go
│
├── internal/
│   ├── auth/
│   │   ├── domain/
│   │   ├── application/
│   │   ├── infrastructure/
│   │   └── transport/
│   │
│   ├── users/
│   ├── accounts/
│   ├── ledger/
│   ├── payments/
│   ├── transfers/
│   ├── merchants/
│   ├── commissions/
│   ├── fraud/
│   ├── notifications/
│   ├── reconciliation/
│   ├── reporting/
│   └── audit/
│
├── pkg/
│   ├── logger/
│   ├── validator/
│   ├── money/
│   ├── pagination/
│   ├── errors/
│   └── middleware/
│
├── api/
│   ├── openapi/
│   └── proto/
│
├── migrations/
│
├── frontend/
│   ├── src/
│   │   ├── app/
│   │   ├── components/
│   │   ├── features/
│   │   │   ├── auth/
│   │   │   ├── accounts/
│   │   │   ├── payments/
│   │   │   ├── transfers/
│   │   │   ├── merchant/
│   │   │   └── admin/
│   │   ├── hooks/
│   │   ├── lib/
│   │   ├── services/
│   │   ├── stores/
│   │   └── types/
│   └── package.json
│
├── deployments/
│   ├── docker/
│   └── kubernetes/
│
├── configs/
│
├── scripts/
│
├── tests/
│   ├── integration/
│   ├── e2e/
│   ├── load/
│   └── fixtures/
│
├── docs/
│   ├── architecture/
│   ├── database/
│   ├── api/
│   ├── adr/
│   ├── security/
│   └── diagrams/
│
├── .github/
│   └── workflows/
│
├── docker-compose.yml
├── Makefile
├── go.mod
├── go.sum
├── .env.example
└── README.md
```

Не все папки нужно создавать в первый день. Структура будет расширяться вместе с функциональностью.

### Дополнительные инженерные требования

Go:

- Go modules.
- Dependency injection через конструкторы.
- Интерфейсы на границах модулей.
- Контекст во всех операциях с I/O.
- Graceful shutdown.
- Корректная обработка ошибок.
- Структурированные логи через `log/slog`.

Инфраструктура:

- PostgreSQL.
- Redis.
- Kafka.
- Docker Compose.
- Миграции через golang-migrate или Goose.
- CI через GitHub Actions.

Качество:

- golangci-lint.
- go vet.
- race detector.
- Unit-тесты.
- Интеграционные тесты.
- Контрактные тесты.
- Нагрузочные тесты через k6.

## 8. Безопасность и надёжность

Для банковского направления этот раздел нельзя оставлять на потом.

| Требование                            | Реализация                                                     |
| ------------------------------------- | -------------------------------------------------------------- |
| Хранение паролей                      | Argon2id                                                       |
| Авторизация                           | Access + Refresh Tokens                                        |
| Доступ к API                          | RBAC и проверка владения ресурсами                             |
| Защита запросов                       | Rate Limiting                                                  |
| Защита платежей                       | Idempotency                                                    |
| Защита от повторной обработки событий | Inbox                                                          |
| Передача событий                      | Transactional Outbox                                           |
| Секреты                               | Переменные окружения в разработке, secret manager в production |
| Логи                                  | Structured Logging                                             |
| Аудит                                 | Отдельный Audit Log                                            |
| Сетевое взаимодействие                | TLS                                                            |
| Валидация                             | Проверка входных данных на сервере                             |

### Требования к надёжности

Установим измеримые цели для нашего проекта:

| Метрика                                           | Целевое значение |
| ------------------------------------------------- | ---------------- |
| Доступность в демонстрационном окружении          | 99,9%            |
| API latency p95 при заданной нагрузке             | до 200 мс        |
| Успешность корректных финансовых операций         | 100%             |
| Потеря подтверждённых проводок                    | 0                |
| Повторное выполнение одной идемпотентной операции | 0                |
| Несбалансированные проведённые операции           | 0                |

Это целевые показатели, а не обещание производительности. Мы будем измерять их нагрузочными и отказоустойчивыми тестами и фиксировать условия экспериментов.

## 9. Пошаговый план разработки на 6–12 месяцев

Я бы выстроил разработку так, чтобы после каждого этапа у тебя оставался работающий продукт.

## Roadmap PayCore

Ориентир — 6–12 месяцев индивидуальной разработки, в зависимости от свободного времени.

1. Месяц 1 — проектирование и фундамент
   - Архитектура и ADR.
   - ER-диаграмма.
   - Go-проект.
   - PostgreSQL и миграции.
   - Конфигурация.
   - HTTP-сервер и middleware.
   - CI и Docker Compose.
   Результат: готовый backend skeleton
2. Месяц 2 — Identity и Accounts
   - Регистрация и авторизация.
   - JWT и Refresh Token.
   - RBAC.
   - Пользователи.
   - Счета и валюты.
   - Баланс-проекция.
   Результат: рабочий личный кабинет
3. Месяцы 3–4 — Ledger и Transfers
   - Двойная бухгалтерская запись.
   - Проводки.
   - Транзакции PostgreSQL.
   - Блокировки.
   - Защита от overspending.
   - Идемпотентность.
   - Выписки.
   Результат: финансовое ядро
4. Месяц 5 — Payments и Merchants
   - Платёжный lifecycle.
   - Авторизация и Capture.
   - Возвраты.
   - Комиссии.
   - API-ключи.
   - Симулятор провайдера.
   Результат: полноценная платёжная система
5. Месяц 6 — Kafka и распределённая обработка
   - Kafka.
   - Outbox и Inbox.
   - Background Workers.
   - Retry и Dead Letter Queue.
   - Webhooks.
   - Saga.
   Результат: асинхронная инфраструктура
6. Месяц 7 — Fraud и Reconciliation
   - Fraud Rules.
   - Risk Score.
   - Fraud Cases.
   - Сверка платежей.
   - Разбор расхождений.
   Результат: банковские операционные процессы
7. Месяцы 8–9 — Frontend
   - Клиентский кабинет.
   - Merchant Dashboard.
   - Admin Dashboard.
   - Ledger Explorer.
   - Графики и аналитика.
   Результат: законченный пользовательский продукт
8. Месяцы 10–11 — Production Engineering
   - Prometheus и Grafana.
   - OpenTelemetry.
   - Jaeger.
   - CI/CD.
   - Kubernetes.
   - Нагрузочное тестирование.
   - Fault Injection.
   Результат: развёртываемая система
9. Месяц 12 — выделение сервисов и презентация
   - Выделение Ledger или Fraud в отдельный сервис.
   - gRPC.
   - Разделение владения данными.
   - Архитектурная документация.
   - Демонстрационные сценарии.
   - Финальная документация GitHub.
   Результат: портфолио-проект для собеседований

Не обязательно ждать 12 месяцев, чтобы начинать откликаться. После работающего Ledger, Transfers и Payments уже можно показывать проект на собеседованиях.

## 10. Как будем проверять, что проект действительно качественный

У каждого этапа будут критерии готовности. Не будем переходить к следующему модулю, пока предыдущий не работает и не покрыт тестами.

Вот примеры обязательных проверок.

| Сценарий                                              | Ожидаемый результат                           |
| ----------------------------------------------------- | --------------------------------------------- |
| Перевод 1000 ₽                                        | Списано 1000 ₽, зачислено 1000 ₽              |
| Перевод при недостатке средств                        | Операция отклонена                            |
| Два параллельных перевода по 8000 ₽ с баланса 10000 ₽ | Успешен максимум один                         |
| Повтор запроса с тем же ключом                        | Нет второго списания                          |
| Падение Kafka                                         | Событие остаётся в Outbox                     |
| Перезапуск Worker                                     | Незавершённое событие обрабатывается повторно |
| Возврат большей суммы, чем оплата                     | Операция отклонена                            |
| Изменение проведённой проводки                        | Запрещено                                     |
| Проверка Ledger                                       | Дебет равен кредиту                           |

Дополнительно будем делать тесты, проверяющие инварианты системы на случайных последовательностях финансовых операций.

### 10.1. Что важно понимать про настоящий банковский продукт

PayCore будет демонстрационной платёжной инфраструктурой, а не лицензированной банковской системой.

В реальном продукте дополнительно потребуются юридические и регуляторные процедуры, KYC/AML, требования к защите платёжных данных, управление ключами, резервирование инфраструктуры, контроль доступа и независимые аудиты. Мы сможем моделировать отдельные процессы, но не будем заявлять о соответствии PCI DSS или банковским требованиям без соответствующего аудита.

# 11. С чего начинаем разработку

Я предлагаю пока не писать Go-код.

Сначала нужно принять несколько фундаментальных архитектурных решений. Если сейчас неправильно спроектировать Ledger, потом придётся переделывать практически весь проект.

Вот пять решений, которые я предлагаю зафиксировать.

| Вопрос        | Моё предложение     |
| ------------- | ------------------- |
| Архитектура   | Modular Monolith    |
| Основная БД   | PostgreSQL          |
| Работа с БД   | pgx + SQL           |
| Денежный учёт | Double-Entry Ledger |
| Асинхронность | Kafka + Outbox      |

### Первые документы, которые создадим

В папке `docs/`:

```
docs/
├── architecture/
│   ├── system-design.md
│   ├── architecture-overview.md
│   └── module-boundaries.md
│
├── database/
│   ├── er-diagram.md
│   ├── database-schema.md
│   └── data-integrity.md
│
├── api/
│   └── api-specification.md
│
└── adr/
    ├── 001-modular-monolith.md
    ├── 002-postgresql.md
    ├── 003-double-entry-ledger.md
    └── 004-transactional-outbox.md
```

ADR — Architecture Decision Records. Это документы, в которых мы фиксируем не только принятое решение, но и альтернативы, причины выбора и компромиссы.

Такой подход будет особенно полезен на техническом собеседовании: ты сможешь объяснить не только то, что написал, но и почему спроектировал систему именно так.

## 12. Первый этап: детальное проектирование Ledger

Я предлагаю начать именно с Ledger, а не с регистрации пользователей.

Причина простая: авторизацию можно переписать относительно безболезненно, а ошибки в финансовой модели способны разрушить всю архитектуру.

В первом этапе мы должны подробно спроектировать:

1. Типы финансовых счетов и их назначение.
2. План счетов (Chart of Accounts).
3. Двойную бухгалтерскую запись.
4. Правила дебета и кредита.
5. Доступный и зарезервированный баланс.
6. Платёжные проводки.
7. Переводы между счетами.
8. Комиссии.
9. Возвраты и компенсационные операции.
10. PostgreSQL constraints и транзакции.
11. Конкурентный доступ и блокировки.
12. Тесты финансовых инвариантов.

Начинаем отсюда

# PayCore — Phase 1

Financial Core & Ledger Design

[Ledgers - Modern Treasury](https://images.openai.com/static-rsc-4/Jn857T0msnL_lEQBAN2jxTFqXZQErfYJw6dekvGInVaSwPh9ZaoGPEQ-QZHguXYI7Vd858AJDQ6WMhbHy33AZGRckNg5HlPMw-_gN3PR-eZHvJPYmcUNl7fkGbjweasurAcUS3ikT4z3Hy30mgXgDFzMXzZXmk2hGnbV2AGu0mQ?purpose=inline)

[moderntreasury.com](https://www.moderntreasury.com/products/ledgers?utm_source=chatgpt.com)

Наша первая цель — спроектировать финансовое ядро, которое гарантирует корректность денежных операций даже при параллельных запросах, сбоях и повторных попытках.

Мой совет: не торопиться с React, Kafka и Kubernetes. Сначала нужно добиться того, чтобы Go-приложение правильно проводило деньги и могло математически доказать корректность своего Ledger.

После этого остальная инфраструктура будет строиться вокруг уже надёжного ядра.

Следующий шаг — полное проектирование финансовой модели PayCore: Chart of Accounts, схема проводок, движение денег, балансы и все финансовые сценарии с конкретными примерами в рублях. Именно с него я бы начал нашу разработку.