# Neo-Bank

Мини-необанк на микросервисной архитектуре.

## Структура
- `gateway/` — единая точка входа (API Gateway)
- `services/` — микросервисы: `auth-svc`, `accounts-svc`, `ledger-svc`, `transfers-svc`, `fraud-svc`, `notifications-svc`
- `proto/` — общие protobuf-контракты между сервисами
- `frontend/` — SPA (Vite + React + TypeScript), см. «Фронтенд» ниже
- `.github/workflows/` — CI-пайплайны

## Инфраструктура (dev)
Postgres, Redis и Kafka подняты в `docker-compose.yml`. Postgres использует каждый сервис, у которого есть своя схема (все, кроме gateway). Redis — только auth-svc (сессии/токены). Kafka — auth-svc, accounts-svc и transfers-svc как продюсеры, accounts-svc и notifications-svc как консьюмеры (см. «События (Kafka)» ниже); notifications-svc дополнительно публикует в `transfer.events.dlq` (см. «notifications-svc: устойчивость консьюмера»), так что технически он теперь и продюсер тоже, но только для собственного dead letter topic, не для доменных событий.

Креды Postgres в `docker-compose.yml` — только для локальной разработки, не для продакшена.

## События (Kafka)
`auth-svc` публикует `UserActivated` в топик `user.events`, `accounts-svc` — `AccountCreated` в `account.events`, `transfers-svc` — `TransferCompleted`/`TransferFailed`/`TransferRejected` в `transfer.events`. Контракты — `proto/events/v1/{user,account,transfer}_events.proto`, сериализация бинарным protobuf. Ключ сообщения — `user_id` для `UserActivated`/`AccountCreated`, `sender_account_id` для Transfer*-событий: гарантирует, что все события одного пользователя/счёта попадают в одну партицию и обрабатываются по порядку. `event_id` — случайный UUIDv4 (`outbox.GenerateEventID`, см. ниже), используется консьюмерами (accounts-svc, notifications-svc) для дедупликации при повторной доставке (см. «Идемпотентность» ниже и «notifications-svc» дальше).

Кроме ключа и тела, каждое сообщение несёт **Kafka-заголовок `event_type`** (`outbox.HeaderEventType`) со значением из колонки `event_type` outbox-строки — дословно `TransferCompleted`, `UserActivated` и т.д. Это часть wire-контракта, а не отладочная метка: protobuf не самоописателен, и на топике с несколькими типами сообщений (`transfer.events`) консьюмеру больше не на что опереться — подробности в секции про письма о переводах.

`accounts-svc` — consumer топика `user.events` (consumer group `accounts-svc`): на `UserActivated` создаёт строку в `accounts` со сгенерированным номером счёта и `status = 'active'`, а **сразу после этого** — вызывает `ledger-svc` `CreateLedgerAccount(account_id)` по gRPC, чтобы у нового счёта появился ledger-аккаунт (адрес ledger — env `LEDGER_GRPC_ADDR`, дефолт `ledger-svc:8083`). Порядок фиксации важен: если вызов ledger упал, offset события **не** коммитится — Kafka передоставит сообщение, а идемпотентность (consumer'а и самого `CreateLedgerAccount`) делает повтор безопасным. Это ровно тот случай, ради которого строились at-least-once + идемпотентность.

Авто-создание топиков брокером включено (`KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE: "true"` задано в `docker-compose.yml` явно, хотя это и так дефолт Kafka), но полагаться на него мы перестали: одноразовый сервис `kafka-init` создаёт все три топика с явной политикой retention до старта notifications-svc — `compact` для `user.events`/`account.events`, `delete` для `transfer.events`. Почему разные — см. «Kafka: offset reset и retention» и «`transfer.events` — `delete`, а не `compact`» дальше. Ни auth-svc, ни transfers-svc не блокируют старт на доступности Kafka: продюсер (`segmentio/kafka-go`) подключается лениво при первой записи и переподключается сам, как и клиенты Postgres/Redis.

### Outbox: как публикация переживает недоступность Kafka
И auth-svc, и transfers-svc публикуют события через транзакционный outbox, а не напрямую в момент запроса — общая реализация (таблица + релей) вынесена в `pkg/outbox` (`neobank/pkg/outbox`), подключается через `require`/`replace` так же, как `pkg/health` и `proto/gen/go`.

Механика одинакова для обоих сервисов:
1. Событие пишется в outbox-таблицу **в той же Postgres-транзакции**, что и бизнес-изменение, которое оно описывает (`outbox.InsertEvent`, вызывается с уже открытым `pgx.Tx`) — либо оба пишутся, либо ни один: событие не может «потеряться» из-за краша между коммитом бизнес-строки и публикацией в Kafka, и не может появиться для отката, которого не было.
2. Отдельный воркер-релей (`outbox.RunRelay`, тикер раз в секунду) забирает необработанные строки (`published_at IS NULL`) через `SELECT ... FOR UPDATE SKIP LOCKED LIMIT 100`, публикует их в Kafka и помечает `published_at = now()` — публикация идёт **до** отметки, так что краш между ними даёт дубль (безопасно: консьюмер дедуплицирует по `event_id`), а не молчаливую потерю. `SKIP LOCKED` — на случай нескольких инстансов одного сервиса: каждый берёт свою пачку, не блокируясь на чужой.
3. Опубликованные строки не удаляются сразу — `outbox.RunCleanupWorker` (раз в час) удаляет только те, что старше `OUTBOX_RETENTION` (дефолт 7 дней), оставляя недавнюю историю доступной для отладки.

Таблицы называются по-разному в двух сервисах (`outbox` в transfers-svc, `auth_outbox` в auth-svc) — оба живут в одной физической базе `neobank`, и одинаковое имя пересеклось бы.

`auth-svc` исторически публиковал `UserActivated` напрямую из HTTP-хендлера сразу после коммита (см. TODO, который был в `services/auth-svc/kafka.go`) — это было осознанным ограничением MVP с известной дырой (краш между коммитом и публикацией терял событие молча). **Мигрирован на outbox**: `verifyEmailCode` пишет событие в `auth_outbox` в той же транзакции, что переводит `users.status` в `active`; сама публикация теперь асинхронна, через тот же релей, что и у transfers-svc. Заодно у auth-svc впервые появился настоящий раннер миграций (`services/auth-svc/migrate.go`, `MigrationsTable: "schema_migrations_auth_svc"`) — раньше `users`/`verification_codes` создавались `migrate` CLI вручную, без отслеживания в коде сервиса.

### Проверка вручную
```bash
docker compose exec kafka kafka-topics.sh --bootstrap-server localhost:9092 --list
docker compose exec kafka kafka-console-consumer.sh \
  --bootstrap-server localhost:9092 \
  --topic user.events \
  --from-beginning \
  --property print.key=true \
  --timeout-ms 10000
```
`key` выводится читаемым текстом (это `user_id`), `value` — бинарный protobuf и в консоли будет нечитаемым — это ожидаемо, не баг.

### Идемпотентность

`accounts-svc` — at-least-once consumer (сначала пишет в БД, потом коммитит оффсет; если упасть между этими двумя шагами, Kafka передоставит то же сообщение после рестарта). Повторная доставка `UserActivated` обрабатывается на двух независимых, дополняющих друг друга уровнях (`handleUserActivated` в `services/accounts-svc/kafka.go`):

1. **`accounts.user_id UNIQUE`** — INSERT использует `ON CONFLICT (user_id) DO NOTHING`. Если строка для этого `user_id` уже есть, повторная доставка не создаёт вторую и не падает — логируется («already exists... not recreating») и оффсет коммитится как обычно. Это единственный уровень, который *обязателен*: он один гарантирует отсутствие дублей в любом случае, даже если ниже что-то пойдёт не так.
2. **`processed_events`** (миграция `000002`, `event_id UUID PRIMARY KEY, processed_at TIMESTAMPTZ`) — быстрый путь для уже обработанных событий: перед обработкой consumer проверяет, есть ли `event_id` в таблице, и если да — пропускает работу целиком, даже не трогая `accounts`. Запись в `processed_events` делается **последним** шагом, строго после того, как строка в `accounts` подтверждённо существует (создана только что или уже была). Это осознанно: если бы событие помечалось обработанным *до* реальной обработки, а обработка затем упала бы по-настоящему (не из-за дубля, а по другой причине), оффсет не закоммитился бы, Kafka передоставила бы сообщение — но `processed_events` уже говорила бы «готово», и повтор был бы ложно пропущен, а пользователь остался бы без счёта навсегда. Запись последним шагом закрывает эту дыру: любой сбой до неё оставляет `processed_events` пустой, и повтор всегда по-настоящему переобрабатывается.

Оба INSERT'а (`accounts`, затем `processed_events`) сознательно не обёрнуты в одну транзакцию: consumer однопоточный и последовательный (`FetchMessage` вызывается строго по одному сообщению за раз, без конкурентной обработки внутри процесса), гонок между сообщениями нет — а уровень 1 сам по себе делает пересоздание строки безопасным, даже если запись в `processed_events` не успела произойти или потерялась.

Между созданием счёта и записью в `processed_events` вклинивается ещё один шаг — вызов `ledger-svc` `CreateLedgerAccount(account_id)` (см. выше). `processed_events` по-прежнему пишется **последним**, строго после того, как и строка `accounts`, и ledger-аккаунт подтверждённо существуют. Если ledger-вызов падает (сервис недоступен, сетевой сбой), обработчик возвращает ошибку, offset не коммитится, Kafka передоставляет — а идемпотентность самого `CreateLedgerAccount` (`ON CONFLICT (account_id)` → возвращает существующий) делает повтор безопасным. Кросс-сервисный RPC в одну SQL-транзакцию с локальными записями обернуть нельзя в принципе — за корректность повтора отвечает именно идемпотентность на каждом уровне, а не общая транзакция.

### Проверка идемпотентности вручную

Самый практичный способ воспроизвести повторную доставку без ручной сборки protobuf-сообщений — сбросить закоммиченный оффсет consumer-группы `accounts-svc` назад, заставив её перечитать уже обработанное сообщение:

```bash
# 1. Остановить accounts-svc — сброс оффсета требует неактивной группы
#    (Kafka считает группу активной ещё некоторое время после остановки
#    контейнера, из-за session timeout; проверить состояние можно через
#    --describe, дождавшись "has no active members"):
docker compose stop accounts-svc
docker compose exec kafka kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 --describe --group accounts-svc

# 2. Сдвинуть оффсет топика user.events на 1 сообщение назад
#    (к последнему обработанному UserActivated):
docker compose exec kafka kafka-consumer-groups.sh \
  --bootstrap-server localhost:9092 \
  --group accounts-svc --topic user.events \
  --reset-offsets --shift-by -1 --execute

# 3. Запустить accounts-svc заново — она перечитает то же сообщение:
docker compose start accounts-svc
docker compose logs -f accounts-svc
```

Проверено вручную на этом стеке (`bitnamilegacy/kafka:3.7.1`): после шага 3 в логах появляется `accounts-svc: event <event_id> already processed, skipping (redelivery)`, а `SELECT count(*) FROM accounts WHERE user_id = '<user_id>'` остаётся `1`. Дополнительно проверен и уровень 1 отдельно: если вручную удалить строку из `processed_events` (`DELETE FROM processed_events WHERE event_id = '<event_id>'`) и повторить шаги 1–3, лог показывает уже другую ветку — `account for user <user_id> already exists (redelivery of event <event_id>), not recreating` — то есть дедупликация срабатывает и без `processed_events`, только на `ON CONFLICT (user_id)`; при этом строка в `processed_events` восстанавливается (самолечение), а счёт по-прежнему один. Оффсет консьюмера в обоих случаях в итоге закоммичен (`kafka-consumer-groups.sh --describe` показывает `LAG 0`), т.е. дубль не оставляет группу «застрявшей».

## notifications-svc: проекция `user_contacts` из событий

Прежде чем отправлять письма (это следующая секция), `notifications-svc` строит и поддерживает свою локальную проекцию `user_id`/`account_id` → `email` (`user_contacts`), полностью из Kafka-событий, без единого синхронного вызова в auth-svc или accounts-svc. Это осознанный архитектурный выбор: сервис, специально вынесенный из критического пути (отправка писем не должна блокировать регистрацию или переводы), не должен обзаводиться зависимостью от аптайма другого сервиса ради того, чтобы просто узнать чей-то email — каждый сервис владеет своими данными, а notifications-svc держит собственную, независимую копию того, что ему нужно.

Два consumer'а (одна consumer-группа `notifications-svc`, два ридера — `kafka-go`'s `Reader` подписывается ровно на один топик, поэтому один ридер на топик, не один на группу):
- `user.events` → `UserActivated` → `upsertUserContactEmail` создаёт/обновляет строку `(user_id, email)`, `account_id` не трогает.
- `account.events` (новый топик, публикует accounts-svc через тот же outbox-подход, что transfers-svc/auth-svc — см. `services/accounts-svc/accounts.go`, `tryCreateAccount`) → `AccountCreated` → `updateUserContactAccountLink` дозаполняет `account_id` и `account_number` в уже существующую строку.

`AccountCreated` причинно всегда следует за `UserActivated` (accounts-svc создаёт счёт только в ответ на `UserActivated`), но у двух топиков независимые ридеры без гарантии взаимного порядка обработки внутри notifications-svc. Поэтому `updateUserContactAccountLink` — намеренно `UPDATE`, не `UPSERT`: если строки `user_contacts` ещё нет (обработчик `user.events` не успел), `RowsAffected = 0`. `email TEXT NOT NULL` в схеме исключает противоположную стратегию (upsert с пустым email).

Ждём мы при этом **внутри процесса** (`contactWaitAttempts` × `contactWaitDelay` = 15 × 200 мс), а не «вернуть ошибку и положиться на переспрашивание». У `Reader` из `kafka-go` **нет** per-message redelivery внутри работающего процесса: `FetchMessage` всегда отдаёт следующее сообщение независимо от того, закоммичен ли предыдущий оффсет. «Не коммитить» помогает, только если процесс перезапустится раньше, чем закоммитится любой более поздний оффсет на той же партиции; как только это произошло, пропущенное сообщение потеряно. Ограничение цикла нужно, чтобы по-настоящему застрявший случай не блокировал горутину навсегда.

Дедупликация — тот же паттерн idempotent-consumer, что у accounts-svc: проверка перед обработкой, запись после. Своя таблица `notifications_processed_events`, не `processed_events` — та уже занята accounts-svc в той же физической базе `neobank` (тот же класс коллизии, что уже заставил переименовать outbox-таблицы в `auth_outbox`/`accounts_outbox`, см. выше). Все типы событий пишутся в одну таблицу — `event_id` глобально уникален независимо от типа. Для `UserActivated`/`AccountCreated` статус всегда `skipped`: они кормят проекцию и писем не порождают (о регистрации auth-svc пишет пользователю сам). `processing`/`sent` появляются на событиях переводов — см. следующую секцию.

### Kafka: offset reset и retention

`UserActivated` публикуется с спринта 2, а `notifications-svc` подключается только сейчас — новая consumer-группа без явных настроек могла бы начать читать `user.events` с конца топика, и все пользователи, зарегистрированные раньше, остались бы без email в проекции навсегда. Решение, оба пункта обязательны вместе, ни один не достаточен в одиночку:

1. **`StartOffset: kafka.FirstOffset`** в `newKafkaReader` (`services/notifications-svc/kafka.go`) — явно, а не полагаясь на дефолт `kafka-go`. Действует только один раз: пока у группы `notifications-svc` нет закоммиченного оффсета на партиции. После первого коммита оффсета это значение больше не читается — чтение всегда продолжается с закоммиченного места, так что параметр безопасно оставить в коде навсегда, а не убирать после первого деплоя.
2. **`cleanup.policy=compact`** на топиках `user.events` и `account.events` (не `delete`, дефолт брокера) — иначе `FirstOffset` не помог бы, если старые сообщения уже физически удалены по retention до того, как notifications-svc впервые подключился. Компакция вместо этого хранит последнее сообщение на каждый ключ (`user_id`) бессрочно — ровно то, что нужно для построения проекции состояния, и естественно для топика, где почти всегда один `UserActivated` на пользователя. Применяется через одноразовый сервис `kafka-init` в `docker-compose.yml` (`kafka-topics.sh --create --if-not-exists ... --config cleanup.policy=compact` + `kafka-configs.sh --alter --add-config` — второе идемпотентно и покрывает случай, когда топик уже существовал с дефолтной политикой до этого изменения). `notifications-svc` зависит от `kafka-init` (`condition: service_completed_successfully`) — топики гарантированно сконфигурированы до первого чтения.

Проверено вручную: остановленные `notifications-svc`/`postgres` volume, существующие в топике `UserActivated` от пользователей, зарегистрированных до появления этого сервиса, — после `docker compose up` (миграции применяются, ридер стартует с `FirstOffset`) все они появляются в `user_contacts` без создания новых пользователей и без обращения к auth-svc.

### Устойчивость к недоступности auth-svc

`notifications-svc` никогда не вызывает auth-svc (ни HTTP, ни gRPC) — вся связь только через Kafka и собственную БД. Проверено: `docker compose stop auth-svc`, затем обычный поток (перевод, либо ранее необработанное `UserActivated` через сдвиг оффсета) — `notifications-svc` продолжает читать и обрабатывать события без ошибок, `/healthz` остаётся `200`.

## ledger-svc: внутренний gRPC API

`ledger-svc` считает и хранит балансы (`account_balances` — кэш поверх лога `entries`, всегда пересчитываемый из него), исполняет атомарные переводы между двумя счетами и отдаёт историю проводок. У него **нет** публичного HTTP API и **нет** маршрута в `gateway` — это осознанно: единственный клиент ledger-svc — `transfers-svc` (со спринта 5), который сам отвечает за аутентификацию и авторизацию перевода до вызова ledger. Здесь нет ни `X-User-Id`, ни какой-либо другой клиентской идентичности — это внутренний, service-to-service контракт.

Протокол — gRPC, а не HTTP: это вызов между сервисами внутри кластера, а не браузерный запрос, и `buf.gen.yaml` в репозитории уже настроен на генерацию grpc-стабов (`protoc-gen-go-grpc`), так что добавить контракт стоило дёшево.

Контракт — `proto/ledger/v1/ledger.proto` (`ledger.v1.LedgerService`):
- `GetBalance(account_id) → balance` — O(1) чтение из `account_balances`.
- `ExecuteTransfer(from_account_id, to_account_id, amount) → transaction_id` — атомарный перевод; бизнес-ошибки («недостаточно средств», «аккаунт не найден» — отдельно для `from`/`to`, «невалидная сумма») возвращаются как grpc-статусы (`FailedPrecondition`, `NotFound`, `InvalidArgument`), а не как поле в успешном ответе — это gRPC-идиоматичный эквивалент HTTP-кода + JSON `{"error": ...}` в остальных сервисах репозитория.
- `GetHistory(account_id, limit, offset) → entries[]` — постранично, новые сверху (`ORDER BY created_at DESC, id DESC`; `id` — tie-breaker, потому что обе проводки одного перевода получают одинаковый `created_at`: `now()` внутри одной транзакции Postgres фиксирован на её начало).

Генерация Go-кода из `.proto`: `buf generate` из корня репозитория (нужны локально `buf`, `protoc-gen-go`, `protoc-gen-go-grpc`).

Сервер дополнительно регистрирует стандартный grpc health-check (`grpc.health.v1.Health`) вместо HTTP `/healthz` и grpc reflection — для internal-only сервиса без внешних потребителей компромисс «reflection раскрывает контракт» не действует, а reflection избавляет от необходимости раздавать `.proto`-файлы, чтобы дёргать сервис через `grpcurl`.

### Проверка вручную
```bash
grpcurl -plaintext localhost:8083 list

grpcurl -plaintext -d '{"account_id": "<uuid>"}' \
  localhost:8083 ledger.v1.LedgerService/GetBalance

grpcurl -plaintext -d '{"from_account_id": "<uuid>", "to_account_id": "<uuid>", "amount": 1000}' \
  localhost:8083 ledger.v1.LedgerService/ExecuteTransfer

grpcurl -plaintext -d '{"account_id": "<uuid>", "limit": 10, "offset": 0}' \
  localhost:8083 ledger.v1.LedgerService/GetHistory
```

### Конкурентность: перевод не может увести счёт в минус

`executeTransfer` — единственный писатель в `entries`/`account_balances`, и он обязан отклонять перевод, если баланса не хватает. Опасность — классическая read-then-write гонка: два одновременных перевода с одного счёта оба читают один и тот же (ещё не списанный) баланс, оба видят «средств достаточно» и оба проходят — счёт уходит в минус, хотя каждая проверка по отдельности была «корректной».

**Выбран `SELECT ... FOR UPDATE`, а не `SERIALIZABLE`.** Обе стороны перевода (`ledger_accounts` строки `from` и `to`) блокируются `FOR UPDATE` внутри одной транзакции, **в детерминированном порядке — по возрастанию `account_id`**, а не в порядке `from`→`to`. Без этого два встречных перевода (A→B и B→A одновременно) могли бы захватить блокировки в противоположном порядке и словить дедлок; сортировка по `account_id` гарантирует, что обе транзакции всегда пытаются заблокировать один и тот же счёт первым — вторая просто ждёт, дедлок невозможен. `SERIALIZABLE` тоже решил бы гонку, но потребовал бы retry-цикла на `40001 serialization_failure` — такого паттерна в репозитории нигде больше нет, и вносить его ради одной функции означало бы новый, ничем не подкреплённый класс ошибок. `FOR UPDATE` вместо этого просто блокирует вторую транзакцию до коммита первой — тот же приём, что уже используется в `accounts-svc` (`updateAccountStatus`) и `auth-svc`, только тут блокируются два счёта, а не один.

**Тест, который это доказывает** — `TestExecuteTransfer_ConcurrentOverdraftPrevention` (`services/ledger-svc/ledger_test.go`): счёт с балансом 10000, 20 горутин одновременно пытаются списать по 1000 (суммарно 20000 — вдвое больше, чем есть). Ожидаемо: ровно 10 успехов, 10 `insufficient funds`, итоговый баланс ровно 0 (никогда отрицательный), и `SUM(entries)` по всем счетам, задействованным в тесте, равен 0.

Это не тест логики «по одной проверке за раз» — он реально запускает 20 горутин параллельно, так что гонка (если она есть) успевает проявиться. Проверено вручную: если временно убрать `FOR UPDATE` из `lockLedgerAccount`, тест падает стабильно (10 из 10 прогонов) — все 20 переводов проходят, баланс уходит на −10000. С `FOR UPDATE` тест стабильно зелёный (прогонялся `-count=15` подряд). Запустить самостоятельно:
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./... -run TestExecuteTransfer_ConcurrentOverdraftPrevention -count=20 -v
```
(`-race` здесь не годится — он ловит гонки по памяти Go, а не гонки по блокировкам строк в Postgres, которые как раз и проверяются; сама горутина в тесте не имеет разделяемого мутируемого состояния — каждая пишет только в свой индекс среза.)

## accounts-svc: баланс в `GET /accounts/me`

`GET /accounts/me` (через Gateway, с валидным токеном) возвращает счёт пользователя **вместе с балансом**. Баланс — авторитетный, живёт в `ledger-svc`; accounts-svc получает его вызовом `GetBalance(account_id)` по gRPC (`account_id` = `accounts.id` = `ledger_accounts.account_id`).

Формат: `balance` — целое число в **минимальных единицах** (центах), плюс отдельное поле `currency` (сейчас всегда `"EUR"` — в ledger нет измерения валюты, а форматирование `"123.45 €"` — работа фронта, не API):
```json
{ "id": "...", "user_id": "...", "account_number": "NB...", "status": "active",
  "created_at": "...", "updated_at": "...", "balance": 50000, "currency": "EUR" }
```
У нового пользователя ledger-аккаунт уже создан (через Kafka-обработчик выше), проводок нет → `balance: 0`.

Если `ledger-svc` временно недоступен (`Unavailable`/`DeadlineExceeded`), эндпоинт возвращает **503**, а не `200` с нулевым балансом: показать фейковый ноль вместо настоящего баланса в банке хуже, чем честно сказать «сервис недоступен».

## Dev-инструменты

> Только для локальной разработки. Не путь для прода.

- `services/ledger-svc/cmd/seed` — наполняет локальную БД примерными ledger-данными (genesis + два счёта, см. заголовок файла).
- `services/ledger-svc/cmd/devtopup` — **пополнение счёта пользователя** до появления Stripe (спринт 9). Переводит `--amount` центов с genesis-аккаунта на `--account-id` (это `accounts.id`) через **обычный `ExecuteTransfer`** ledger-svc — тот же реальный путь (локи, проверка баланса, обновление кэша), что и у продового перевода. Единственное, что не может пройти через `ExecuteTransfer` — эмиссия денег (источник ушёл бы в минус, а `ExecuteTransfer` это запрещает): поэтому, когда у genesis не хватает средств, инструмент **чеканит** деньги в genesis прямой сбалансированной вставкой в БД (external → genesis, чтобы `SUM(entries)=0` сохранялся), и только потом делает настоящий перевод. Именно эта прямая эмиссия — причина, почему это dev-инструмент, а не HTTP-эндпоинт.

  ```bash
  # из services/ledger-svc, ledger-svc должен быть запущен (docker compose up ledger-svc)
  DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  LEDGER_GRPC_ADDR="localhost:8083" \
    go run ./cmd/devtopup --account-id <accounts.id> --amount 50000
  ```
  После этого `GET /accounts/me` для того же пользователя показывает `"balance": 50000`.

## Перевод денег через ledger (transfers-svc)

`transfers-svc` создаёт запись `transfers` в статусе `pending` (валидация: сумма положительна, получатель резолвится через `accounts-svc.ResolveAccountByNumber`, получатель ≠ отправитель, получатель не `closed`, отправитель `active` — баланс здесь **не** проверяется, это сделает `ledger-svc` атомарно), а затем вызывает `ledger-svc.ExecuteTransfer(sender_account_id, recipient_account_id, amount)` и по результату обновляет запись:
- успех → `status = 'completed'`, `ledger_transaction_id` — id проводки из ответа ledger.
- `ledger` вернул «недостаточно средств» (`FailedPrecondition`) → `status = 'failed'`, `failure_reason = 'insufficient_funds'`.
- `ledger` вернул «аккаунт не найден» (`NotFound`) или невалидную сумму (`InvalidArgument`, на практике недостижимо — `transfers-svc` уже проверил сумму раньше) или свою внутреннюю ошибку (`Internal`) → `status = 'failed'` с соответствующей `failure_reason` (`account_not_found` / `invalid_amount` / `ledger_internal_error`) — каждая доменная ошибка размечена явно, а не свалена в один общий `'error'`.

### Честная граница: неопределённый исход

`ledger-svc.ExecuteTransfer` (`services/ledger-svc/ledger.go`) оборачивает свою работу в одну Postgres-транзакцию, которая либо целиком коммитится, либо целиком откатывается (`defer tx.Rollback(ctx)`, `tx.Commit(ctx)` — только на успехе) **до того**, как уйдёт какой-либо gRPC-ответ. Это значит: любой полноценный ответ — успех или один из явных `status.Error(...)`, которые `ledger-svc` возвращает сам (`FailedPrecondition`, `NotFound`, `InvalidArgument`, `Internal`) — говорит совершенно точно, ушли деньги или нет.

`codes.Unavailable`, `codes.DeadlineExceeded` и `codes.Unknown` — принципиально другой случай: `ledger-svc` их сам никогда не возвращает, они возникают только на уровне транспорта (не достучались, либо не дождались ответа за `ledgerCallTimeout` = 5 секунд). Здесь `transfers-svc` **не знает**, исполнился перевод или нет: запрос мог не дойти, а мог дойти, исполниться, и уже ответ потеряться на обратном пути. Пометить `failed` в этом случае — соврать, если деньги реально ушли; пометить `completed` без `transaction_id` — соврать в другую сторону. Поэтому запись остаётся `pending` как есть (никакой записи в БД не делается вообще), а клиенту возвращается `202 Accepted` с телом `{"status": "pending", "message": "transfer status unknown, still processing"}`.

Разрешается эта неопределённость автоматически — см. «Reconciliation: закрываем pending переводы» ниже. То же самое «зависшее pending» применимо и к неопределённому исходу fraud-проверки (см. «fraud-check перед ledger») — тот же класс проблемы в двух местах потока, закрывается той же reconciliation-задачей, а не отдельно для каждого случая.

### Проверка вручную
```bash
# через Gateway: sender резолвится из X-User-Id, который Gateway кладёт
# сам из JWT после /auth/login — см. "API-клиент"/JWT-мидлварь выше
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_account_number":"NB...","amount":1000}'
```
Успешный перевод — `201` и `"status":"completed"` с заполненным `ledger_transaction_id`; сумма больше баланса — `201` и `"status":"failed","failure_reason":"insufficient_funds"`; оба случая проверяются balance-diff через `GET /accounts/me` обеих сторон (см. `cmd/devtopup` выше — им удобно завести отправителю стартовый баланс).

## Stripe-фондированные депозиты (transfers-svc, ledger-svc)

Stripe-депозиты в transfers-svc: две новые таблицы, `ledger-svc.Deposit`,
`POST /deposits` (создаёт Stripe `PaymentIntent`) и
`POST /webhooks/stripe` (подтверждает исход платежа по подписанному
событию от Stripe). Депозит доходит только до `deposits.status =
'succeeded'` — фактическое зачисление в ledger через
`ledger-svc.Deposit` из вебхука, и фронтенд, которому нужен
`client_secret`, — отдельные следующие шаги; `POST /deposits` пока
внутренний, наружу через Gateway тоже отдельным шагом.

### Почему депозиты живут в transfers-svc, а не в новом payments-svc

Депозит — тот же класс задачи, что и перевод: движение денег с
неопределённостью на стороне внешней системы (там — `ledger-svc`, здесь —
Stripe), которую нужно уметь дожидаться и сверять, а не гарантированно
знать синхронно в момент HTTP-ответа. transfers-svc уже несёт всю нужную
для этого инфраструктуру — клиент `ledger-svc`, outbox для надёжной
публикации событий, воркер reconciliation для зависших состояний (см.
разделы выше и ниже). Заводить отдельный payments-svc означало бы
дублировать всё это заново ради разделения ответственности, которое для
MVP не окупается; если/когда депозиты обрастут собственной сложностью
(возвраты, несколько провайдеров), выделение в отдельный сервис — разумный
следующий шаг, но не сейчас.

### Схема (`services/transfers-svc/migrations/000006`, `000007`)

`deposits` — одна строка на попытку депозита; `stripe_payment_intent_id`
уникален — естественная защита от двух записей на один и тот же
PaymentIntent. Статусы `succeeded` и `credited` — **сознательно разные**:
`succeeded` значит «Stripe подтвердил списание карты», `credited` — «мы
провели проводку в ledger». Это два факта в двух разных системах;
схлопнуть их в один статус означало бы потерять восстановимое состояние
«Stripe уже взял деньги, а мы их ещё не зачислили» — ровно то место, где
будущему обработчику вебхука будет что чинить.

`processed_stripe_events` — идемпотентность на уровне `event_id` Stripe
(`evt_...`): `PRIMARY KEY` на `event_id` надёжнее, чем полагаться на то,
что Stripe никогда не пришлёт вебхук дважды.

### `ledger-svc.Deposit` — genesis → счёт пользователя

`Deposit(account_id, amount, reference)` — новый gRPC-метод
(`proto/ledger/v1/ledger.proto`): genesis-проводка, атомарно списывающая
`amount` с системного genesis-счёта и зачисляющая его на `account_id` той
же механикой, что и `ExecuteTransfer` (одна Postgres-транзакция,
сбалансированная пара `entries` с общим `transaction_id`, инкрементальное
обновление `account_balances`) — но **отдельной функцией** (`deposit` в
`ledger.go`), а не веткой внутри `executeTransfer`. Единственное
поведенческое отличие: здесь **нет проверки достаточности средств** на
стороне genesis — он по определению обязан уметь уходить в минус (это и
есть представление денег, входящих в систему извне), тогда как
`executeTransfer` обязан делать эту проверку для каждого обычного счёта.
Встраивать genesis-исключение в `executeTransfer` означало бы добавить
особый случай в функцию, от которой зависит каждый обычный перевод, —
отдельная функция оставляет `executeTransfer` полностью нетронутым.

`reference` — необязательный будущий `deposits.id` (UUID), та же роль, что
и `ExecuteTransferRequest.reference` у переводов (см. «Reconciliation»
ниже): позволяет будущему обработчику вебхука спросить
`GetTransactionByReference`, не была ли эта проводка уже проведена, вместо
повторного зачисления. `Deposit`, как и `ExecuteTransfer`, **не
идемпотентен сам по себе** — два вызова с одним и тем же `reference` дают
две отдельные проводки; идемпотентность — ответственность вызывающей
стороны, как и раньше.

Системный genesis-счёт (`00000000-0000-0000-0000-000000000001`) теперь
детерминированно создаётся миграцией
`services/ledger-svc/migrations/000005_create_genesis_ledger_account` — до
этого шага он существовал только как побочный эффект первого запуска
`cmd/seed`/`cmd/devtopup`. Оба dev-инструмента не тронуты: их собственные
идемпотентные upsert'ы того же genesis-счёта остаются безвредными
no-op'ами, раз миграция уже создала строку первой.

### Секрет Stripe: только через переменную окружения

`STRIPE_SECRET_KEY` (`sk_test_...`) читается один раз при старте
transfers-svc (`main.go`) через `os.Getenv`, тем же паттерном, что и
`JWT_SECRET` у auth-svc — процесс останавливается (`log.Fatal`), если
переменная не задана, до подключения к Postgres и до миграций. В отличие
от `JWT_SECRET`, чьё dev-значение — просто захардкоженная в
`docker-compose.yml` строка (безопасно для внутреннего секрета), реальный
Stripe-ключ — куда более чувствительный секрет от третьей стороны:
`docker-compose.yml` берёт его из `${STRIPE_SECRET_KEY}` (docker compose
сам подставляет значения из `.env` в корне репозитория, рядом с
`docker-compose.yml`, — механизм работает без единой строчки кода), а
`.env` — в `.gitignore`; в репозитории лежит только `.env.example` c
плейсхолдером. Publishable-ключ (`pk_test_...`) сюда не входит — он
фронтенд-only и отложен до соответствующего шага.

Клиент — `github.com/stripe/stripe-go/v86`, создаётся один раз в `main()`
вызовом `stripe.NewClient(stripeSecretKey)` и хранится в пакетной (не
локальной) `var stripeClient *stripe.Client`. `stripe.NewClient` ничего не
делает по сети — как и остальные клиенты этого `main()` (`grpc.NewClient`
к accounts-svc/fraud-svc/ledger-svc), он ленивый: если ключ окажется
невалидным, это проявится только на первом реальном вызове Stripe API.

### `POST /deposits` — создание PaymentIntent

Как устроен платёж: `PaymentIntent` — объект на стороне Stripe,
представляющий намерение списать деньги. transfers-svc создаёт его и
получает `client_secret`; этот секрет уходит на фронт, где Stripe.js
подтверждает платёж **напрямую со Stripe** — данные карты никогда не
проходят через backend. Это не деталь реализации, а осознанная граница
PCI-scope: сервис никогда не хранит, не логирует и не видит номера карт.
Результат придёт вебхуком (следующий шаг), не в ответе на этот запрос.

`createDepositHandler` (`services/transfers-svc/http.go`), зарегистрирован
как `POST /deposits`:
- `account_id` берётся из `X-User-Id` (тот же `resolveSenderAccountID`, что
  и у переводов) — никогда из тела запроса, чтобы клиент не мог задепозитить
  на чужой счёт.
- `amount` проверяется на `[depositMinAmount, depositMaxAmount]`
  (`services/transfers-svc/deposit.go`): нижняя граница — 50 (минимум
  Stripe для EUR, €0.50 — иначе Stripe сам отклонит запрос, но менее
  информативно), верхняя — 1 000 000 центов (€10 000) — не бизнес-лимит, а
  защита от опечатки в лишние нули.
- счёт должен быть `active` — на `frozen`/`closed` не зачисляем.

Порядок операций в `createDeposit`: сначала `INSERT INTO deposits`
(`status='pending'`), потом Stripe `PaymentIntent`, потом `UPDATE ...
SET stripe_payment_intent_id`. Эти два шага принципиально нельзя завернуть
в одну транзакцию — между ними живой вызов Stripe API, который не умеет
участвовать в транзакции Postgres. Если процесс упадёт между шагами,
останется `pending`-депозит без `stripe_payment_intent_id`: это безопасно
(деньги не двигались, Stripe в большинстве таких сбоев даже не получил
запрос) — заброшенная попытка, а не что-то, что нужно чинить прямо здесь.

`Metadata: {deposit_id, account_id}` на самом `PaymentIntent` — то, что
свяжет будущий вебхук с этой записью: у Stripe нет понятия о наших
первичных ключах, кроме того, что мы сами туда положим.
`IdempotencyKey = deposit_id` (через `stripe-go`'s `Params.IdempotencyKey`)
привязывает Stripe-side защиту от дублей к конкретной попытке: повторный
вызов `Create` с тем же `deposit_id` (например, наш собственный ретрай
после сетевого сбоя) вернёт тот же `PaymentIntent`, а не создаст второй.

Ответ — только `{deposit_id, client_secret}`. Секретный ключ Stripe не
покидает `main()` ни при каких обстоятельствах, а из самого объекта
`PaymentIntent` наружу уходит единственное поле — `client_secret`.

### Тесты `POST /deposits`

`services/transfers-svc/deposit_test.go`, через `fakePaymentIntentCreator`
(тот же fake-по-функции паттерн, что и `fakeLedgerClient`/
`fakeAccountsClient` — реальные вызовы к Stripe в тестах не идут):
`TestCreateDeposit_Success` (в т.ч. `IdempotencyKey`/`metadata` доходят до
Stripe правильно), `TestCreateDeposit_InvalidAmount` (обе границы),
`TestCreateDeposit_AccountNotActive`,
`TestCreateDeposit_StripeErrorLeavesRowPending` (проверяет именно то
безопасное состояние, которое описано выше).
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -run TestCreateDeposit -v
```
Ручная проверка (нужен настоящий `STRIPE_SECRET_KEY` в `.env` —
см. выше):
```bash
curl -s -X POST http://localhost:8084/deposits \
  -H "X-User-Id: <account's user id>" \
  -H "Content-Type: application/json" \
  -d '{"amount": 5000}'
```
`201` с `{"deposit_id": "...", "client_secret": "pi_..._secret_..."}`;
в Stripe-дашборде (тестовый режим) — созданный `PaymentIntent` с
`metadata.deposit_id`/`metadata.account_id`.

### Проверка вручную

```bash
grpcurl -plaintext -d '{"account_id": "<uuid>", "amount": 1000}' \
  localhost:8083 ledger.v1.LedgerService/Deposit
```
Баланс `account_id` (`GET /accounts/me` через Gateway) вырастет ровно на
`amount`; genesis (`00000000-0000-0000-0000-000000000001`) уйдёт в минус
ровно на ту же сумму — так и задумано.

`STRIPE_SECRET_KEY` действительно обязателен: `docker compose up
transfers-svc` без `.env` (или с закомментированной переменной) —
контейнер падает сразу с логом `transfers-svc: STRIPE_SECRET_KEY
environment variable is required`, не долетая даже до подключения к
Postgres.

### Тесты `ledger.Deposit`

`services/ledger-svc/ledger_test.go`: `TestDeposit_Success` (целевой счёт
растёт на `amount`, genesis падает на `amount`, `SUM(entries)` по
транзакции равен 0), `TestDeposit_InvalidAmount`,
`TestDeposit_AccountNotFound`, `TestDeposit_WithReference` (`reference`
находится через `getTransactionByReference` — тот же путь, которым в
будущем воспользуется сверка Stripe-депозитов),
`TestDeposit_ReferenceIsNotIdempotencyKey` (два вызова с одним `reference`
— две разные проводки, то же поведение, что и у `ExecuteTransfer`,
специально не переизобретается).
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/ledger-svc/... -run TestDeposit -v
```

### `POST /webhooks/stripe` — подтверждение платежа

Ответ фронта после оплаты ничего не гарантирует: пользователь может
закрыть вкладку сразу после того, как деньги списались, и до backend это
никогда не долетит. Единственный надёжный источник исхода — вебхук от
Stripe, который приходит асинхронно и независимо от того, жив ли ещё
клиент. `POST /webhooks/stripe` (`services/transfers-svc/webhook.go`) —
публичный эндпоинт: у Stripe нет нашего JWT, поэтому проверка подписи —
не опция, а единственное, что отличает настоящий вебхук от любого, кто
угадал URL и прислал `{"type":"payment_intent.succeeded",...}` от себя,
чтобы бесплатно зачислить деньги.

**Подпись.** `webhook.ConstructEvent(payload, header, webhookSecret)` из
`stripe-go` — секрет для этого (`whsec_...`) отдельный от
`STRIPE_SECRET_KEY`, специально не тот же самый: компрометация одного не
должна автоматически компрометировать другой. Критично — подпись
считается по **сырым байтам тела**, поэтому `io.ReadAll(r.Body)`
происходит первым делом, до любого JSON-парсинга: если тело сначала
распарсить, а потом пересериализовать (другой порядок полей, другие
пробелы — не важно), подпись перестанет совпадать. Невалидная подпись —
`400`, без какой-либо обработки, только лог попытки.

**Идемпотентность.** Stripe доставляет вебхуки at-least-once и ретраит
недоставленные — один и тот же `evt_...` может прийти несколько раз.
Дедупликация — `INSERT event_id` в `processed_stripe_events`; на
unique-violation — это дубль, `200` без повторной обработки. Проверяющий
арбитр — сам constraint БД, а не `SELECT exists(...)` в коде: два
вебхука с одним `event_id` могут прийти параллельно, и только constraint
корректно разруливает эту гонку (проверка-потом-вставка в коде её не
ловит).

Важный нюанс, которого нет в буквальной формулировке задачи, но который
проявляется на практике: `INSERT` в `processed_stripe_events` и апдейт
`deposits` выполняются **в одной Postgres-транзакции**
(`processStripeEvent`). Если бы событие помечалось обработанным ДО того,
как реально применился эффект (апдейт статуса), временный сбой между
этими двумя шагами означал бы: ответ `500` (проси Stripe повторить), но
при этом полноценный повтор той же доставки уже никогда не сработает —
он попадёт в ветку «дубль, игнорируем», а нужный апдейт так и не
произойдёт. Одна транзакция на оба шага — единственный способ, которым
`400`/дубль/сбой ведут себя ровно так, как описано в DoD: сбой в
обработке откатывает и вставку `processed_stripe_events`, так что
следующая доставка того же `event_id` — это честная первая попытка, а не
проигнорированный дубль.

**События.** Три типа обрабатываются: `payment_intent.succeeded` →
`deposits.status = 'succeeded'`; `payment_intent.payment_failed` →
`status = 'failed'` + `failure_reason` (код ошибки Stripe, например
`card_declined`); `charge.refunded` → **не меняет status** (у `deposits`
нет отдельного статуса `refunded` — полноценный reversal, включая
возможное расширение схемы, откладывается до соответствующего шага), а
пишет `failure_reason = 'refunded'` как метку для будущей сверки —
переиспользование поля по контексту, тот же приём, что и у `Transfer`
(`FailureReason` уже несёт разный смысл в зависимости от `Status`, см.
выше). Любой другой тип события — `200` и игнорирование без обработки:
Stripe шлёт десятки типов событий, и падение на незнакомом заставило бы
его ретраить бесконечно без всякой пользы.

**Скорость.** Обработчик умышленно ничего не делает, кроме проверки
подписи, дедупликации и одного `UPDATE` — зачисление в `ledger-svc`
(более медленный, кросс-сервисный вызов) сюда не входит и переезжает в
отдельный шаг. У Stripe есть таймаут на ответ вебхука; долгая
синхронная обработка здесь означала бы ложные ретраи от Stripe поверх
уже случившегося платежа.

### Локальная разработка вебхуков: Stripe CLI

Stripe не может достучаться до `localhost` напрямую. Локально это решает
[Stripe CLI](https://docs.stripe.com/stripe-cli):

```bash
stripe login
stripe listen --forward-to localhost:8084/webhooks/stripe
```

`stripe listen` держит туннель открытым и печатает **локальный** webhook
signing secret (`whsec_...`, отдельный от продового/дашбордного) — его и
нужно положить в `.env` как `STRIPE_WEBHOOK_SECRET`. Пока `stripe listen`
не перезапущен, значение остаётся тем же.

С запущенными `transfers-svc` и `stripe listen` в соседнем терминале:
```bash
stripe trigger payment_intent.succeeded
```
шлёт настоящее подписанное событие на форвардящийся URL — в логе
`stripe listen` виден код ответа (`200`), а в БД — обновлённый
`deposits.status`.

### Проверка вручную

`STRIPE_WEBHOOK_SECRET` обязателен так же, как `STRIPE_SECRET_KEY`:
`docker compose up transfers-svc` без него в `.env` — контейнер падает с
логом `transfers-svc: STRIPE_WEBHOOK_SECRET environment variable is
required`, ещё до подключения к Postgres.

Поддельная подпись:
```bash
curl -s -o /dev/null -w "%{http_code}\n" -X POST http://localhost:8084/webhooks/stripe \
  -H "Stripe-Signature: t=0,v1=deadbeef" \
  -d '{"type":"payment_intent.succeeded"}'
```
`400`, запись в `processed_stripe_events` не появляется.

### Тесты `POST /webhooks/stripe`

`services/transfers-svc/webhook_test.go` — через
`webhook.GenerateTestSignedPayload` (из `stripe-go/webhook`), то есть с
настоящим вычислением HMAC-подписи, без единого реального обращения к
Stripe: `TestStripeWebhookHandler_PaymentIntentSucceeded`,
`TestStripeWebhookHandler_PaymentIntentPaymentFailed`,
`TestStripeWebhookHandler_ChargeRefunded`,
`TestStripeWebhookHandler_UnknownEventTypeIsIgnored`,
`TestStripeWebhookHandler_InvalidSignature` (проверяет и `400`, и
отсутствие записи в `processed_stripe_events`),
`TestStripeWebhookHandler_DuplicateDeliveryIsNotReprocessed` (вторая
доставка того же `event_id` — `200`, но `deposits.updated_at` не
меняется и `processed_stripe_events` содержит ровно одну строку),
`TestProcessStripeEvent_ProcessingFailureDoesNotRecordEvent` (доказывает
именно то поведение с одной транзакцией на оба шага, что описано выше).
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -run "TestStripeWebhook|TestProcessStripeEvent" -v
```

## fraud-svc: подключение к Postgres, схема

`fraud-svc` получил Postgres-соединение и схему (`pgx/v5` + `golang-migrate`, тот же паттерн, что и у `ledger-svc`/`accounts-svc`/`transfers-svc`: миграции встроены через `//go:embed migrations/*.sql` и накатываются автоматически при старте, таблица версий миграций — namespaced `schema_migrations_fraud_svc`, т.к. все сервисы делят одну физическую БД `neobank`). Логика проверок появилась следующим шагом (см. «fraud-svc: rule-based скоринг переводов» ниже), а интеграция в поток перевода — ещё одним шагом после (см. «fraud-check перед ledger» в самом низу).

Две таблицы:
- **`fraud_rules`** — конфигурация правил (`rule_type` — `amount_threshold` / `velocity_count` / `velocity_sum`, уникален; `enabled`; `threshold_value` — сумма в центах или количество, в зависимости от типа правила; `window_seconds` — окно для velocity-правил, `NULL` для порога по разовой сумме). Мутируемая таблица, редактируется вручную/будущим admin-API.
- **`fraud_checks`** — лог всех проверок, append-only (`transfer_id`, `account_id`, `amount`, `decision` — `approve`/`reject`, `triggered_rule`, `details` JSONB с посчитанными значениями). Индекс `idx_fraud_checks_account_id_created_at` на `(account_id, created_at)` — под него заточены velocity-правила (посчитать переводы/сумму по счёту за последние N секунд). Лог нужен для аудируемости: когда пользователь спросит «почему мой перевод заблокировали», ответ должен быть в данных, а не в догадках; та же таблица — будущие данные для ML-скоринга (за скоупом этого шага).

Три дефолтных правила засеяны миграцией `000003_seed_default_fraud_rules` (значения — отправная точка для MVP, меняются через саму таблицу `fraud_rules`, без новой миграции):
| `rule_type` | `threshold_value` | `window_seconds` | Смысл |
|---|---|---|---|
| `amount_threshold` | 500000 (€5,000 в центах) | `NULL` | разовый перевод необычно большой суммы |
| `velocity_count` | 5 | 300 (5 мин) | больше 5 переводов за 5 минут — похоже на скомпрометированную сессию/скрипт |
| `velocity_sum` | 1000000 (€10,000 в центах) | 3600 (1 час) | вывод счёта серией переводов, каждый из которых по отдельности ниже порога разовой суммы |

`GET /healthz` теперь проверяет реальное соединение с Postgres (`SELECT 1` с таймаутом 2с, `503` при ошибке) — как и у остальных Postgres-сервисов, вместо DB-less обработчика из `pkg/health`.

## fraud-svc: rule-based скоринг переводов (`CheckTransfer`)

`fraud-svc` теперь считает решения, а не только хранит схему. gRPC-контракт — `proto/fraud/v1/fraud.proto` (`fraud.v1.FraudService.CheckTransfer(transfer_id, account_id, amount) → {decision, triggered_rule, reason}`), сервер поднят на отдельном порту (`GRPC_PORT`, дефолт `9085`) параллельно с уже существующим HTTP (`8085`) — тот же паттерн, что у `accounts-svc` (HTTP и gRPC на разных портах в одном процессе, `grpc.NewServer()` без опций, стандартный `grpc.health.v1.Health` + `reflection.Register`). `transfers-svc` теперь его вызывает — см. «fraud-check перед ledger (transfers-svc → fraud-svc)» в конце этого раздела.

### Принцип: rule-based, не ML

Правила из `fraud_rules` проверяются в фиксированном порядке — `amount_threshold` → `velocity_count` → `velocity_sum` — и **первое сработавшее правило сразу даёт `reject`**, дальше проверка не идёт. Это делает решение объяснимым: `triggered_rule` всегда называет ровно одно правило, а не «какую-то комбинацию» нескольких. Отключённые правила (`enabled = false`) пропускаются целиком.

- **`amount_threshold`**: `amount > threshold_value` → reject. Разовый перевод выше порога.
- **`velocity_count`**: количество одобренных проверок этого `account_id` за `window_seconds`, **плюс сама эта проверка** (т.е. «а если одобрить и её»), превышает `threshold_value` → reject.
- **`velocity_sum`**: то же самое, но сумма одобренных переводов за окно (плюс сумма текущего) вместо количества.

Источник данных для обоих velocity-правил — **собственная** таблица `fraud_checks` (`WHERE decision = 'approve'`), не БД `transfers-svc`: каждый сервис владеет своими данными и считает по ним, чужая БД не источник для чужой логики.

Наблюдаемое значение для каждого фактически проверенного правила (включая то, которое сработало) кладётся в `details` (JSONB) вместе с порогом и окном — постфактум видно не только что заблокировано, но и что именно было насчитано.

### Fail-closed, а не fail-open

Если `fraud-svc` не может посчитать решение (ошибка Postgres на любом шаге — чтение правил, подсчёт velocity, запись лога), `checkTransfer` возвращает ошибку, а RPC — grpc-статус `codes.Internal`, а **не** молчаливый `approve`. Чтение правил, velocity-подсчёты и запись в `fraud_checks` обёрнуты в одну транзакцию: либо решение целиком посчитано и залогировано, либо не сделано ничего — частичной записи без итогового решения не бывает. Что делать при недоступном fraud-svc (пропустить перевод, отклонить, поставить в очередь) — решает вызывающий (`transfers-svc`), не сам fraud-svc.

### Каждая проверка — строка в `fraud_checks`

И `approve`, и `reject` пишутся в лог, не только отклонённые — это то, что вообще делает возможным подсчёт velocity-правил (им нужна полная история одобренных проверок), и то же самое, что нужно для аудируемости из предыдущего шага.

### Проверка вручную
```bash
grpcurl -plaintext localhost:9085 list

grpcurl -plaintext -d '{"transfer_id": "<uuid>", "account_id": "<uuid>", "amount": 1000}' \
  localhost:9085 fraud.v1.FraudService/CheckTransfer
# {"decision": "approve", "reason": "no rule triggered"}

grpcurl -plaintext -d '{"transfer_id": "<uuid>", "account_id": "<uuid>", "amount": 600000}' \
  localhost:9085 fraud.v1.FraudService/CheckTransfer
# {"decision": "reject", "triggeredRule": "amount_threshold", "reason": "amount_threshold: observed 600000 exceeds threshold 500000"}
```
Проверено вручную (через образ `fullstorydev/grpcurl` в docker-сети `neo-bank_default`, поскольку локально `grpcurl` не установлен): оба вызова выше отработали как описано, и `SELECT * FROM fraud_checks WHERE account_id = '<uuid>'` показал обе строки — `approve` и `reject` — с ожидаемыми `decision`/`triggered_rule`.

### Тесты

`services/fraud-svc/fraud_test.go` — юнит-тесты на реальном Postgres (конвенция репозитория: `DATABASE_URL`, тест скипается, если переменная не задана), по одному сценарию на каждое правило: перевод выше порога → `reject`/`amount_threshold`; 6-й перевод в пятиминутном окне → `reject`/`velocity_count`; сумма выше лимита в часовом окне → `reject`/`velocity_sum`; обычный перевод → `approve`; отключённое правило пропускается; отменённый контекст (симуляция сбоя БД без реального отключения Postgres) → ошибка и **ни одной** записи в `fraud_checks` (fail-closed, без частичной записи).
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/fraud-svc/... -v
```

## fraud-check перед ledger (transfers-svc → fraud-svc)

`transfers-svc` теперь вызывает `fraud-svc.CheckTransfer` как часть создания перевода, **после** того как pending-запись уже вставлена, но **до** вызова `ledger-svc.ExecuteTransfer`:

```
createTransfer() → pending-запись создана
        ↓
checkTransferFraud() → approve   → settleTransfer() как раньше (completed/failed/uncertain)
                      → reject   → status='rejected', ledger вообще НЕ вызывается
                      → uncertain (fraud-svc недоступен) → запись остаётся pending, ledger НЕ вызывается
```

Порядок специально такой, а не «сначала fraud, потом создать запись»: у отклонённого перевода остаётся строка с причиной (пользователь видит в истории, что было и почему заблокировано), но при этом деньги гарантированно не двигались, потому что до вызова ledger дело просто не доходит. Это же делает reject-путь compensation-free — откатывать нечего, потому что трогать было нечего; правильная saga экономит на компенсации, ставя рискованный шаг (ledger) последним.

### `rejected` — новый статус, отдельно от `failed`

`failed` — техническая неудача или недостаток средств (уровень `ledger-svc`); `rejected` — заблокировано fraud-проверкой. Это разные вещи и для пользователя, и для аналитики, поэтому не схлопнуты в один статус: миграция `000003_add_rejected_transfer_status` добавляет `'rejected'` в CHECK на `transfers.status`. Отдельной колонки под причину не заведено — `failure_reason` переиспользован: он и раньше хранил «почему не completed» (коды `ledger-svc`), теперь при `rejected` там лежит `triggered_rule` от fraud (например, `"amount_threshold"`). Колонка `status` сама по себе всегда однозначно говорит, какой словарь смотреть — заводить `rejection_reason` ради чисто косметического разделения означало бы трогать все `RETURNING`/`SELECT` в файле ради нулевой смысловой пользы.

### Fail-closed vs fail-open — осознанный выбор

Если `fraud-svc` недоступен или сам вернул ошибку (`codes.Internal` — единственный код, который он вообще возвращает, других деловых кодов у него, в отличие от `ledger-svc`, нет), у `transfers-svc` есть буквально два варианта:

- **fail-open** — пропустить перевод без проверки. Доступнее (перевод не встаёт колом, если fraud-svc упал), но это дыра: злоумышленник, зная, что можно уронить fraud-svc (или просто попасть в окно реального сбоя), проводит перевод вообще без всякой проверки.
- **fail-closed** — не пропускать. Безопаснее, но перевод не завершается, пока fraud-svc не ответит.

Выбран **fail-closed**: перевод остаётся `pending` (никакой записи не делается — та же логика, что и у неопределённого исхода `ledger-svc`), клиенту — `202` с `"message": "fraud check unavailable, transfer still pending"`. Деньги в этом случае не двигались вообще: `ledger-svc` ещё не вызывался, поэтому даже откатывать нечего. Настоящий банк выберет именно так: «недоступна проверка на мошенничество» — это состояние, в котором деньги должны стоять на месте, а не состояние, которое молча пропускают ради доступности. Та же причина, по которой `checkTransferFraud` не делает по кодам ошибок разбор наподобие `settleTransfer` (там `ledger-svc` кодирует бизнес-исходы через grpc-статусы, здесь у `fraud-svc` такого нет — любая ошибка равнозначна «не смог посчитать», и это ровно то, что должно приводить к fail-closed, а не к угадыванию).

### Идемпотентность

Fraud-проверка вызывается **строго после** того, как переключатель исходов `createTransfer` в `http.go` уже обработал все ранние `return` (включая `createTransferReplayed`) — то есть ровно там же, где сегодня уже стоит вызов `settleTransfer`. Повтор с тем же `Idempotency-Key` короткоживущим путём возвращает текущее состояние существующей записи и не доходит до вызова fraud вообще — тем же механизмом, что уже не даёт повтору вызвать `ledger` дважды. Если запись всё ещё `pending` из-за неопределённого исхода fraud-проверки, повтор так и будет возвращать тот же `pending`-снимок, не пытаясь перепроверить fraud — но зависшей она не останется: reconciliation-воркер (см. «Reconciliation: закрываем pending переводы» ниже) разрешает и этот случай тоже, проверяя `ledger-svc` напрямую, независимо от того, что вызвало неопределённость.

### Проверка вручную
```bash
# легитимный перевод — approve, деньги переходят
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_account_number":"NB...","amount":1000}'
# {"status":"completed","ledger_transaction_id":"..."}

# перевод выше порога — reject, ledger не вызывается
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_account_number":"NB...","amount":600000}'
# {"status":"rejected","failure_reason":"amount_threshold"}

# fraud-svc недоступен
docker compose stop fraud-svc
curl -s -X POST http://localhost:8080/transfers/ \
  -H "Authorization: Bearer <access_token>" -H "Content-Type: application/json" \
  -H "Idempotency-Key: <uuid>" \
  -d '{"recipient_account_number":"NB...","amount":1000}'
# 202 {"status":"pending","message":"fraud check unavailable, transfer still pending"}
docker compose start fraud-svc
```
Проверено вручную на полном стеке (два реальных пользователя через `/auth/register` → `/auth/verify-email` → `/auth/login`, отправитель профинансирован через `cmd/devtopup`): легитимный перевод — `completed`, в `fraud_checks` строка `approve`; перевод на 600000 — `rejected`/`amount_threshold`, балансы обеих сторон не изменились, `ledger_transaction_id` пуст; шесть быстрых мелких переводов подряд — 6-й `rejected`/`velocity_count`; остановленный `fraud-svc` — `202 pending`, баланс не изменился, `fraud_checks` для этого перевода пуст; повтор того же `Idempotency-Key` (и после reject, и после fraud-недоступности) — тот же ответ, счётчик строк `fraud_checks` для этого `transfer_id` не увеличился.

### Тесты

`services/transfers-svc/transfer_test.go` — `fakeFraudClient` (тот же паттерн, что `fakeAccountsClient`/`fakeLedgerClient`: встраивает реальный интерфейс как nil, переопределяет только `CheckTransfer`). `TestCheckTransferFraud_Approved`/`_Rejected`/`_Uncertain` (таблично по `codes.Internal`/`codes.Unavailable`, доказывая, что fail-closed срабатывает на **любой** ошибке, не только на документированной)/`_UnexpectedDecision` (незнакомое значение `decision` — тоже fail-closed, а не молчаливый approve). Обработчик HTTP отдельно не тестируется — в репозитории вообще нет тестов на уровне HTTP-хендлера/gRPC-сервера (только на уровень бизнес-логики ниже), и гарантия «повтор не вызывает fraud дважды» здесь структурная (ранний `return` до места вызова), а не отдельный тест — то же самое, что уже верно для `settleTransfer`.
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/transfers-svc/... -v
```

## Reconciliation: закрываем pending переводы (transfers-svc)

Это настоящая saga-проблема потока перевода — не reject от fraud (там ledger вообще не трогается, компенсировать нечего), а тот самый обрыв связи после вызова `ledger-svc.ExecuteTransfer`, помеченный TODO ещё в разделе «Честная граница» выше: `transfers-svc` вызвал `ledger`, `ledger` провёл проводку и закоммитил её, а ответ не дошёл (таймаут, сеть, сам `transfers-svc` упал между вызовом и записью `completed`). Запись висит в `pending` навсегда — деньги реально переведены, а система об этом не знает.

### Почему это не «откатить»

Компенсация в saga — не откат БД. Проводка в `entries` append-only и физически не удаляется. Компенсировать можно только двумя способами: **подтвердить** (если проводка реально прошла — записать `completed`, догнав реальность) или **провести обратную проводку** (если решено отменить нечто, что состоялось). Здесь нужен только первый способ — а если проводки не было вообще, компенсировать вообще нечего, деньги никуда не уходили. Обратные проводки (reversal) сюда не входят: они понадобятся в спринте 9 для Stripe-возвратов, где отменяется уже состоявшийся перевод, а не выясняется его судьба.

### `ledger-svc.GetTransactionByReference` — источник истины, а не догадка

Чтобы спросить «а провёл ли ledger вообще перевод с таким id», сначала нужно этот id туда донести. `ExecuteTransferRequest` получил необязательное поле `reference` (`proto/ledger/v1/ledger.proto`) — `transfers-svc` передаёт туда `transfer.ID` (`settleTransfer` в `transfer.go`), `ledger-svc` сохраняет его на обеих проводках (`entries.reference UUID`, миграция `000004_add_reference_to_entries`, индекс `idx_entries_reference`). Значение необязательное и по умолчанию `NULL` — `cmd/devtopup`/`cmd/seed` его не передают, и это ничего не меняет в их поведении.

`GetTransactionByReference(reference) → {found, transaction_id}` — `found = false` такой же полноценный, ожидаемый ответ, как и `found = true`, а не ошибка: reference мог никогда не использоваться, или перевод с ним никогда не выполнялся. Оба случая исчерпывают то, что может быть верно про зависший `pending`.

### Воркер: `runReconciliationWorker` (`services/transfers-svc/reconcile.go`)

Тикер раз в 30 секунд (`reconcileInterval`, константа — тюнить нечего) ищет переводы в `pending` старше настраиваемого порога (`getStalePendingTransfers`, `transfer.go`) — порог задаётся через `RECONCILE_STALE_AFTER` (`time.ParseDuration`, дефолт `2m`, не установлен в `docker-compose.yml` — только через код). Для каждого — `GetTransactionByReference(transfer_id)`:
- **найдена** → перевод реально прошёл: `status = 'completed'`, `ledger_transaction_id` заполняется. Компенсация не нужна — это просто «догнать реальность».
- **не найдена** → `ledger` её не проводил: `status = 'failed'`, `failure_reason = 'timeout_unresolved'`. Денег не двигалось, компенсировать нечего.
- ошибка транспорта (`ledger-svc` сам недоступен) → ничего не пишется, лог, следующий тик попробует снова — тот же fail-closed принцип, что и у `checkTransferFraud`/`settleTransfer`: не знаешь — не пиши.

Как и консьюмер Kafka в `accounts-svc`, воркер живёт всё время жизни процесса без graceful shutdown (`context.Background()` из `main()`) — тот же паттерн, что и у большинства фоновых циклов в этом репозитории. Консьюмеры `notifications-svc` — исключение, см. «notifications-svc: устойчивость консьюмера» ниже.

### Гонка с обычным обработчиком запроса

Между тем, как воркер прочитал список зависших `pending` (`getStalePendingTransfers`), и тем, как он решит его записать, тот же самый перевод может успеть разрешиться по обычному пути — например, клиент повторил запрос с тем же `Idempotency-Key`, и на этот раз `settleTransfer` действительно достучался до `ledger-svc`. Поэтому оба писателя воркера — `markTransferCompletedIfPending`/`markTransferFailedIfPending` (`transfer.go`) — это те же `UPDATE`, что и `markTransferCompleted`/`markTransferFailed`, но с добавленным `AND status = 'pending'`: если строка уже не `pending` к моменту записи, `UPDATE` не совпадает ни с одной строкой (`RowsAffected() == 0`), и воркер просто не резолвит её повторно — уже зафиксированный результат (какой бы он ни был) остаётся как есть, а не перезаписывается устаревшим представлением воркера.

### Логи

Каждое разрешение зависшего перевода логируется явно (`reconcileTransfer`, `transfer.go`) — с `transfer_id`, итоговым статусом и (для `completed`) `ledger_transaction_id`:
```
transfers-svc: reconcile: transfer 85abb5f7-... resolved to completed (ledger_transaction_id=0f34b0e1-...) — ledger-svc had already executed it, the original response was never received
transfers-svc: reconcile: transfer d88ce967-... resolved to failed (reason=timeout_unresolved) — ledger-svc never executed it, no money moved
```
Тики без единого разрешения ничего не логируют — иначе лог захламлялся бы каждые 30 секунд без всякой пользы.

### Как симулировался обрыв для проверки

Ни `docker network disconnect`, ни `iptables` не дают надёжно оборвать именно **ответ**, оставив сам вызов и коммит в `ledger-svc` нетронутыми — слишком тонкое по времени состояние гонки, чтобы воспроизводить его через реальную сеть. Вместо этого — временный `os.Getenv("SIMULATE_CRASH_AFTER_LEDGER_CALL") == "true"` прямо в `settleTransfer`, сразу после успешного `ExecuteTransfer` и до `markTransferCompleted`: `log.Fatalf(...)`, честно убивающий процесс в тот самый момент, когда `ledger-svc` уже закоммитил, а `transfers-svc` — ещё нет. Добавлено, использовано для проверки ниже, затем убрано целиком — это одноразовый инструмент для этой проверки, не постоянная часть кода (в отличие от `cmd/devtopup`, которым реально пользуются повторно).

**Проверено вручную на полном стеке:**
1. `SIMULATE_CRASH_AFTER_LEDGER_CALL=true`, `RECONCILE_STALE_AFTER=5s` (временно, только на время проверки) → перезапуск `transfers-svc`.
2. Обычный перевод через Gateway → клиент получает `502` (соединение оборвалось вместе с процессом), контейнер `transfers-svc` — `Exited (1)`, в логе `SIMULATED CRASH after ledger call for transfer <id>`.
3. `SELECT status FROM transfers WHERE id = '<id>'` → `pending`; `SELECT * FROM entries WHERE reference = '<id>'` → обе проводки уже на месте (реальные деньги реально перешли).
4. Убрать `SIMULATE_CRASH_AFTER_LEDGER_CALL`, перезапустить `transfers-svc` — воркер снова работает. Через один тик (≤35 c при пороге 5 c) перевод сам стал `completed` с правильным `ledger_transaction_id`; баланс отправителя (`GET /accounts/me`) уменьшился ровно на сумму перевода.
5. Обратный случай: вручную вставлена `pending`-запись без единой проводки в `ledger` (`INSERT INTO transfers (...) VALUES (..., 'pending', now() - interval '1 minute')`) — на следующем тике стала `failed`/`timeout_unresolved`.
6. `RECONCILE_STALE_AFTER` и код возвращены к дефолту (`2m`, без переменной в `docker-compose.yml`), `SIMULATE_CRASH_AFTER_LEDGER_CALL` полностью удалён из `transfer.go` — обычный перевод после отката по-прежнему `completed` с первого раза.

### Тесты

`services/ledger-svc/ledger_test.go`: `TestExecuteTransfer_WithReference` (reference сохраняется на обеих проводках, `getTransactionByReference` находит правильный `transaction_id`), `TestGetTransactionByReference_NotFound`, `TestExecuteTransfer_EmptyReferenceLeavesEntriesUnreferenced` (пустой reference → `NULL`, а не пустая строка — иначе все переводы без reference коллизировали бы на одном значении для поиска).

`services/transfers-svc/reconcile_test.go`: `TestGetStalePendingTransfers` (порог по возрасту), `TestReconcileTransfer_LedgerExecutedIt`/`_LedgerNeverExecutedIt`/`_TransportErrorLeavesRowUntouched` (три исхода через `fakeLedgerClient.getTransactionByReferenceFunc`), `TestMarkTransferCompletedIfPending_SkipsAlreadyResolved`/`TestMarkTransferFailedIfPending_SkipsAlreadyResolved` — доказывают саму гонку: заранее резолвят перевод в один терминальный статус, затем вызывают «противоположный» `*IfPending` и проверяют, что `RowsAffected = 0` и строка не тронута.
```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/ledger-svc/... ./services/transfers-svc/... -v
```

## notifications-svc: письма о переводах (`transfer.events` → Mailpit)

Третий консьюмер (`runTransferEventsConsumer`, та же группа `notifications-svc`, свой ридер на топик `transfer.events`) превращает три типа событий в четыре письма через Mailpit. Отправка — тот же подход, что в auth-svc: stdlib `net/smtp`, `Auth = nil`, тело собирается `fmt.Sprintf`, никакого шаблонизатора; env `SMTP_ADDR`/`SMTP_FROM` с теми же дефолтами (`mailpit:1025`, `noreply@neobank.local`), так что оба сервиса кладут почту в один ящик. Переход на Brevo/SES — смена этих значений и добавление `smtp.Auth`, а не переписывание логики выше.

### Кому и о чём: три события — четыре письма

У перевода две стороны, и они узнают о разном:

| Событие | Отправителю | Получателю |
|---|---|---|
| `TransferCompleted` | «transfer sent» | «transfer received» |
| `TransferFailed` | «transfer failed» | — |
| `TransferRejected` | «transfer declined» | — |

`TransferCompleted` — единственное событие, порождающее **два** письма: деньги ушли у одного и пришли к другому, оба факта адресату интересны. `TransferFailed` и `TransferRejected` означают, что денег не двигалось вовсе, — получателю не о чем знать, и его контакт даже не резолвится (лишний запрос плюс лишнее ожидание проекции ради адреса, который будет выброшен). Это же согласуется с решением спринта 7: получатель не видит чужие неуспешные переводы.

События несут только `sender_account_id`/`recipient_account_id` (UUID) — ни email, ни номера счёта. Адрес берётся из собственной проекции `user_contacts` по `account_id`; ради номера счёта в миграции `000003` добавлена колонка `account_number` (`AccountCreated` нёс её на wire всегда, но до этого спринта не персистилась).

### Чего в письме нет — и почему это не забывчивость

**В письме про заблокированный фродом перевод нет ни имени правила, ни порога.** `TransferRejected` несёт `triggered_rule`, обработчик его читает — и пишет только в лог. `buildTransferDeclinedEmail` **физически не принимает** такой параметр: назвать правило («velocity_count») или лимит («свыше 5 000.00 за один перевод») значит выдать инструкцию, как остаться под ним. Ровно та же логика, что в UI спринта 6 (`REJECTED_REASON_LABELS` в `TransferForm.tsx` — маппинг без fallback на сырую строку), только письмо ещё и пересылаемо и вечно. Отсутствие параметра — это способ сделать так, чтобы будущая правка не смогла раскрыть правило по невнимательности.

**В письме получателю нет ни email отправителя, ни чьего-либо баланса** — `buildTransferReceivedEmail` не получает ни того, ни другого. Получателю достаточно суммы, ID перевода и номера счёта, с которого пришли деньги.

**Коды ошибок ledger'а не показываются сырыми.** `failureReasonSentences` переводит `insufficient_funds` в «There were not enough funds in your account.»; неизвестный код — не строка `Reason: ledger_internal_error`, а отсутствие строки `Reason` вовсе.

### `event_type` в Kafka-заголовке: дискриминатор, которого нет в payload

`transfer.events` — первый топик в репозитории с несколькими типами сообщений, и распознать их по телу **невозможно**. `TransferCompleted`, `TransferFailed` и `TransferRejected` совпадают по номерам и типам полей 1–5, а поле 6 — `string` во всех трёх (`ledger_transaction_id` / `reason` / `triggered_rule`). Значит `proto.Unmarshal` любого из них в любой другой **проходит без ошибки**: `TransferFailed`, прочитанный как `TransferCompleted`, тихо кладёт `insufficient_funds` в `LedgerTransactionId`. Это не гипотетическая опасность — это то, что случилось бы при первом же наивном консьюмере.

Решение — заголовок, а не поле в proto: `pkg/outbox/relay.go` теперь ставит `Headers: [{event_type: <outbox.event_type>}]`. Колонка `event_type` в outbox-таблице существовала с самого начала и тратилась только на логи; относить её на wire в релее дёшево (три строки в общем пакете), не требует regen protobuf, не трогает ни одного продюсера и не может разойтись с тем, что записано в той же транзакции, что и бизнес-изменение. Заголовок аддитивен — консьюмеры `user.events`/`account.events` его просто не смотрят.

`notifications-svc` при этом **не импортирует `pkg/outbox`**: нужна ровно одна строка, а этот пакет — сторона *записи* (положить событие в outbox в одной транзакции с бизнес-изменением и отрелеить), тогда как notifications-svc никакой outbox-таблицей не владеет и ничего не публикует. Чистый консьюмер, зависящий от библиотеки публикации, перевернул бы слои. Литерал продублирован в `kafka.go` и запинен с обеих сторон (`TestHeaderEventType_IsWireContract` в `pkg/outbox`, `TestEventTypeHeader_MatchesProducer` в notifications-svc) — без этого переименование на стороне продюсера просто молча выключило бы письма.

Что делает консьюмер с каждым вариантом заголовка (актуально начиная с введения retry/DLQ ниже — раньше «обработчик вернул ошибку» и «обработчик успешен после ретрая» были одним и тем же «не коммитить» без границы попыток):

| Заголовок | Действие | Коммит оффсета |
|---|---|---|
| известный тип, обработчик успешен (сразу или после ретрая) | письмо(а) + `finishEvent` | да |
| известный тип, `proto.Unmarshal` падает на каждой попытке | `transferMaxAttempts` ретраев, затем DLQ | да — см. «Ограниченный retry и DLQ» |
| известный тип, обработчик падает на каждой попытке | `transferMaxAttempts` ретраев, затем DLQ | да — см. ниже |
| заголовка нет (`""`) | сразу поднимается как ошибка, идёт через тот же retry/DLQ путь (детерминированно неудачный — заголовок не появится ни на одной попытке) | да |
| неизвестное значение | тот же путь, что и выше | да |

У безголового сообщения соблазнительно вытащить `event_id`, распаковав его как `TransferCompleted` (поля 1–5 же совпадают) — не делаем: это ровно та случайная кросс-распаковка, ради устранения которой заголовок и вводился. В DLQ и в лог идут partition и offset, которыми оператор и так полез бы смотреть сообщение.

### Ограниченный retry и DLQ: один сломанный перевод не должен останавливать остальные

`handleTransferMessageWithRetry` (`kafka.go`) — граница, которая решает «попробовать ещё раз» или «сдаться и пойти дальше», и именно то, чего не хватало в описанном выше «не коммитить»: `Reader` из `kafka-go` не переспрашивает одно и то же сообщение внутри работающего процесса — `FetchMessage` всегда идёт вперёд независимо от того, закоммичен ли предыдущий оффсет. Раньше это означало, что сообщение N, за которым последовало успешно закоммиченное N+1, терялось молча, как только коммитился более поздний оффсет — не бесконечный retry, не блокировка партиции, а тихая потеря.

Теперь `processTransferMessage` (unmarshal + диспетчеризация + обработчик, одним куском) перевызывается для одного и того же сообщения до `transferMaxAttempts` раз (5) с экспоненциальной задержкой (`transferRetryBaseDelay` = 500 мс, удваивается, потолок `transferRetryMaxDelay` = 8 с). Повторный вызов безопасен благодаря `claimEvent`: попытка, дошедшая до захвата события, при неудаче оставляет строку барьера в `processing`, а следующая попытка реклеймит ту же строку, а не считает событие уже обработанным — та же политика «дубль важнее потери», что уже применяется к падению во время отправки, теперь покрывает и повторяемый транзиентный сбой.

Если все попытки исчерпаны — сообщение ядовитое: payload, который никогда не распарсится, адрес, который SMTP отказывается принимать навсегда, или зависимость, недоступная на протяжении всего окна ретраев. Вместо потери (как раньше при ошибке `proto.Unmarshal`) или зависания этой горутины на нём навсегда (блокируя все уведомления за ним), сообщение целиком — тот же ключ, то же значение, те же исходные заголовки — публикуется в `transfer.events.dlq` (`sendToDLQ`) с добавленными заголовками `dlq_reason`/`dlq_source_partition`/`dlq_source_offset`; если `event_id` успел определиться (unmarshal прошёл), строка барьера закрывается как `skipped`; оффсет коммитится. Ничего не теряется — DLQ хранит исходные байты для ручного разбора; `event_id` не всегда есть — у сообщения без заголовка или с нераспознанным типом его получить неоткуда без той самой кросс-распаковки, которую заголовок и должен предотвратить, так что для этих случаев барьерной строки не остаётся вовсе, только запись в DLQ и закоммиченный оффсет.

### Отсутствующий контакт — не ядовитое сообщение

`waitForContactByAccountID` при исчерпании своего ограниченного ожидания (~3 с, `contactWaitAttempts`×`contactWaitDelay`) возвращает `found = false` без ошибки — **не** ошибку, которая попала бы в `handleTransferMessageWithRetry`. Это осознанно: перевод, обогнавший проекцию (`AccountCreated` ещё не обработан), — повод подождать, а не повод считать сообщение битым. Постоянно неразрешимый `account_id` означает сломанную проекцию, а не испорченный payload, и DLQ, созданный для «оператор может починить и переиграть», — не то место, куда это стоит класть. Такое событие по-прежнему коммитится сразу, статус — `sent`/`skipped` в зависимости от того, нашлась ли хотя бы одна сторона (см. таблицу ниже). Битый UUID — другой случай: Postgres отвечает `22P02`, это уже ошибка обработчика, и она корректно уходит в retry/DLQ путь.

### Барьер идемпотентности: `processing` → отправка → `sent`

Kafka даёт at-least-once, и релей из outbox тоже (публикация идёт до отметки `published_at`) — одно событие может прийти дважды. Но **отправку письма нельзя внести в транзакцию БД**: внешний побочный эффект не откатывается. Exactly-once здесь недостижим в принципе; выбирается только направление ошибки.

`claimEvent` (`contacts.go`) — один атомарный оператор:

```sql
INSERT INTO notifications_processed_events (event_id, status)
VALUES ($1, 'processing')
ON CONFLICT (event_id) DO UPDATE SET status = notifications_processed_events.status
RETURNING status
```

`DO UPDATE` здесь — намеренный no-op: строке присваивается её же значение. Его единственная задача — заставить `RETURNING` сработать на ветке конфликта; `ON CONFLICT DO NOTHING` возвращает ноль строк и не может сообщить, что там уже лежало. Возвращается пред-существующий статус: `sent`/`skipped` → пропустить, `processing` → работать.

Почему одним оператором, а не парой `isEventProcessed` + `markEventProcessed`, как у проекционных обработчиков: там побочный эффект — upsert, и гонка между чтением и записью безвредна (сделать дважды = сделать один раз). Здесь побочный эффект — письмо, оно себя не дедуплицирует, поэтому проверка и захват обязаны быть одним оператором.

**`processing` означает «работать» — это выбор, а не запасной вариант.** Падение между возвратом из `smtp.SendMail` и `finishEvent` оставляет строку, которая честно говорит «неизвестно, ушло ли письмо». Повторить — риск дубля; пропустить — риск тишины. Для уведомлений о деньгах второе «вам поступило 25.00 EUR» — лёгкое раздражение, а пропавшее — клиент, который не знает, что деньги пришли. **Дубль важнее потери.**

Закрывает событие `finishEvent` — именно `UPDATE`, и подменять его на `markEventProcessed` нельзя: у того `ON CONFLICT DO NOTHING`, строка после `claimEvent` уже существует, и он бы тихо не сделал ничего, оставив `processing` навсегда и превратив каждую переигровку в новый комплект писем. Самый вероятный способ сломать это через полгода, поэтому две функции намеренно оставлены раздельными, а не «объединены».

Одного барьер не делает: **не сериализует конкурентные реплики.** Захват воркера A коммитится сразу, воркер B через миллисекунду видит `processing` и тоже идёт работать. Сегодня недостижимо (одна реплика, а Kafka в группе отдаёт партицию одному консьюмеру), но это свойство политики «`processing` → работать», а не SQL, и лучше знать о нём заранее.

### Одна строка барьера на два письма — и что это стоит

У `TransferCompleted` два побочных эффекта и **одна** строка в `notifications_processed_events`. Плюс: «обработано ли событие» — один недвусмысленный факт. Минус, честно: строка не отличает «отправлено оба» от «отправлено одно». Падение между двумя письмами оставляет `processing`, и переигровка отправит **оба** — отправитель получит дубль. Это то же направление («дубль важнее потери»), а альтернатива (строка барьера на каждого адресата) обменяла бы его на схему, где «одна строка на событие» уже не выполняется.

Отправителю пишем первым — он инициатор и ждёт подтверждения, так что если из двух писем успеет уйти одно, пусть это будет его.

### Порядок с оффсетом: одна попытка против всех пяти

Оффсет коммитится **после** обработки, не до, — иначе падение до отправки потеряло бы событие безвозвратно. Раньше (до retry/DLQ) «не коммитить» на деле означало почти ничего — `Reader` из `kafka-go` не переспрашивает одно сообщение внутри процесса, `FetchMessage` всегда идёт вперёд, так что непрокоммиченное сообщение просто терялось, как только коммитился более поздний оффсет. Теперь у каждого сообщения есть настоящая граница попыток — `transferMaxAttempts` (5) внутри `handleTransferMessageWithRetry`, — и коммит происходит либо когда одна из попыток дошла до `finishEvent`, либо когда все попытки исчерпаны и сообщение ушло в DLQ. Разница между «одна попытка» и «все пять» — вот что определяет письма и статус строки барьера:

| Ситуация | Письма | Строка барьера | Коммит |
|---|---|---|---|
| SMTP лежит на попытке №k, k < 5 | нет (пока) | `processing` | нет — следующая попытка через `transferRetryDelay(k)` |
| SMTP лежит на всех 5 попытках | нет | `skipped` (закрыта после DLQ) | да — событие в `transfer.events.dlq` |
| SMTP моргнул, какая-то попытка прошла | есть | `sent` | да |
| Письмо №1 ушло, №2 упало, retry исчерпан | одно (дубль возможен при последующем ретрае) | `skipped` (закрыта после DLQ) | да |
| Контакт не нашёлся за ~3 с | что резолвилось | `sent` / `skipped` | **да**, сразу — не ошибка, не входит в retry/DLQ (см. «Отсутствующий контакт» выше) |
| Ошибка Postgres на поиске контакта (битый UUID, `22P02`) | нет (пока) | `processing` | нет — тот же retry/DLQ путь, что и SMTP |
| `claimEvent` вернул `sent`/`skipped` | нет | не меняется | да — штатная ветка переигровки |

Ненайденный контакт коммитится осознанно, как и раньше: любой счёт в системе создаётся accounts-svc в ответ на `UserActivated`, поэтому вечно неразрешимый `account_id` означает сломанную проекцию, а не «внешний счёт», — и это ровно то, что «Отсутствующий контакт — не ядовитое сообщение» выше объясняет подробнее. Если из двух сторон нашлась одна — письмо уходит ей (у получателя просто исчезает строка *From account*), статус `sent`; если ни одной — писем нет, статус `skipped`, то есть «решили не отправлять», а не «не смогли».

### Почему `LastOffset`, а не `FirstOffset`

Самая дорогая ошибка этого спринта была бы в одной строке конфигурации ридера. Ридеры разделены по намерению:

- `newProjectionReader` (`user.events`, `account.events`) — `FirstOffset`. `user_contacts` — это состояние: переигровка компактного лога пересобирает его, повторный upsert ничего не стоит.
- `newNotificationReader` (`transfer.events`) — **`LastOffset`, и иначе нельзя.** Топик не компактится, живёт по обычному time-retention и копится с 5-го спринта. `FirstOffset` на новой группе переиграл бы всю историю, и барьер идемпотентности не остановил бы **ни одного** события: в `notifications_processed_events` нет строк на те `event_id`. Каждый пользователь получил бы письмо про каждый свой перевод за недели, по два на каждый успешный. Письмо — побочный эффект во внешнем мире, а не обновление состояния: «пересобрать» его нельзя, и история — ровно то, что переигрывать НЕ надо.

`StartOffset` действует только пока у группы нет закоммиченного оффсета на партиции, так что после первого старта это ничего не стоит — краш, рестарт и ручной сброс оффсета одинаково продолжают с закоммиченного места. Цена — ровно один пропуск: перевод, завершившийся во время самого первого запуска, до первого коммита, письма не получит. Один раз, на одном деплое, в обмен на неразосланный спам по всей истории на том же деплое.

### `transfer.events` — `delete`, а не `compact`

`kafka-init` теперь создаёт и `transfer.events`, с `cleanup.policy=delete` — брокерским дефолтом, записанным явно, потому что альтернатива здесь активно неверна. Ключ этого топика — `sender_account_id`, и компакция хранила бы только **последнее** событие на отправителя, молча выбросив всю остальную его историю переводов. `user.events`/`account.events` компактятся именно потому, что они — снимки состояния по `user_id`; `transfer.events` — лог дискретных фактов, и компакция на нём была бы потерей данных, переодетой в политику хранения.

Предсоздавать, а не полагаться на авто-создание, нужно ещё и потому, что `depends_on: kafka-init` у notifications-svc до этого покрывал два топика из трёх: на свежем стеке, где ещё не было ни одного перевода, третий ридер упирался бы в несуществующий топик, а путь ошибки `FetchMessage` — цикл без паузы. Заодно добавлен `fetchErrorBackoff` (1 с) во все три цикла на остаточные случаи вроде рестарта брокера.

### Формат суммы

`formatMinorUnits` — зеркало `frontend/src/features/accounts/money.ts`: `abs/100` и `abs%100` целочисленно (никогда не `float64(minorUnits)/100`), ручная группировка тысяч, ровно два знака. `123456` → `1,234.56`. Одно отличие — **без символа валюты**: `" EUR"` приписывает `formatAmount`, потому что `€` не ASCII, а все письма ASCII-only — это и позволяет не ставить MIME charset-заголовок, ровно как в письмах auth-svc. Предпосылка проверяется тестом, а не подразумевается.

### Проверка вручную

Полный прогон DoD. Пересобрать нужно и transfers-svc — релей, ставящий заголовок, живёт в его процессе:

```bash
docker compose up -d --build
# зарегистрировать и подтвердить alice@example.com и bob@example.com,
# пополнить счёт alice (см. «Dev-инструменты»)

# 1. Успешный перевод между двумя пользователями → ДВА письма
curl -s -X POST http://localhost:8080/transfers \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"recipient_account_number":"<номер счёта bob>","amount":123456}'

curl -s http://localhost:8025/api/v1/messages \
  | jq -r '.total, (.messages[] | "\(.To[0].Address)  \(.Subject)")'
# 2
# bob@example.com    Neo-Bank: transfer received
# alice@example.com  Neo-Bank: transfer sent
#   в обоих телах — 1,234.56 EUR; у alice строка "To account", у bob — "From account"

# 2. Заблокированный фродом → ОДНО письмо отправителю, без раскрытия правила
curl -s -X DELETE http://localhost:8025/api/v1/messages
curl -s -X POST http://localhost:8080/transfers \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"recipient_account_number":"<номер счёта bob>","amount":600000}'
# {"status":"rejected","failure_reason":"amount_threshold"}  <- API говорит; письмо не должно

curl -s http://localhost:8025/api/v1/messages | jq -r '.total, (.messages[]|.To[0].Address)'
# 1
# alice@example.com          <- получателю не пришло ничего

curl -s "http://localhost:8025/api/v1/message/<ID>" | jq -r .Text \
  | grep -Ei 'amount_threshold|velocity|500000|threshold|limit'
# (пусто — ни имени правила, ни порога)
```

Тест redelivery — новых писем нет, строка одна и `sent`:

```bash
docker compose exec postgres psql -U neobank -d neobank \
  -c "SELECT event_id, status FROM notifications_processed_events ORDER BY processed_at DESC LIMIT 3;"
#  <uuid успешного перевода> | sent
#   ^ ОДНА строка, несмотря на ДВА отправленных письма

curl -s -X DELETE http://localhost:8025/api/v1/messages
docker compose stop notifications-svc          # группа должна быть неактивна для сброса
docker compose exec kafka kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group notifications-svc --topic transfer.events --reset-offsets --shift-by -2 --execute
docker compose start notifications-svc
docker compose logs -f notifications-svc
# notifications-svc: event <uuid> already handled, skipping (redelivery)

curl -s http://localhost:8025/api/v1/messages | jq .total
# 0                                            <- новых писем нет

docker compose exec postgres psql -U neobank -d neobank \
  -c "SELECT count(*) FROM notifications_processed_events WHERE event_id = '<uuid>';"
#  1
docker compose exec kafka kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --describe --group notifications-svc
#  LAG 0 на всех трёх топиках — переигровка закоммитилась, ничего не застряло
```

`--shift-by -2`, а не `--to-earliest`: у ридера `transfer.events` старт `LastOffset`, и сброс в начало переиграл бы всю историю топика — ровно то, чего этот старт и избегает.

Опционально, «SMTP лежит на протяжении всего окна ретраев» — теперь заканчивается в DLQ, а не зависает в `processing`:

```bash
docker compose stop mailpit
# сделать перевод → в логе: sendEmailWithRetry (3 попытки), затем 5 попыток
# handleTransferMessageWithRetry с растущей паузой (500мс, 1с, 2с, 4с),
# затем "giving up ... sending to transfer.events.dlq"
docker compose exec postgres psql -U neobank -d neobank \
  -c "SELECT event_id, status FROM notifications_processed_events WHERE status = 'skipped' ORDER BY processed_at DESC LIMIT 1;"
#  ^ не 'processing' — строка закрыта после DLQ, честно (мы не отправили, а не "не знаем")

docker compose exec kafka kafka-console-consumer.sh --bootstrap-server localhost:9092 \
  --topic transfer.events.dlq --from-beginning --max-messages 1 \
  --property print.headers=true
#  ^ event_type=TransferCompleted, dlq_reason=..., dlq_source_partition=..., dlq_source_offset=...

docker compose start mailpit
curl -s -X POST http://localhost:8080/transfers \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"recipient_account_number":"<номер счёта bob>","amount":1000}'
curl -s http://localhost:8025/api/v1/messages | jq .total
#  ^ следующий перевод дошёл нормально — партиция не заблокирована предыдущим,
#    ушедшим в DLQ
```

**Про dev-данные:** у контактов, слинкованных до миграции `000003`, `account_number` остаётся `NULL`, и письмо просто опускает строку со счётом. Сброс оффсета сам по себе не бэкфиллит: строки барьера на те `AccountCreated` уже есть, и каждое переигранное событие короткозамкнётся. Либо `docker compose down -v`, либо (dev-only, вручную — не кодом):

```bash
docker compose stop notifications-svc
docker compose exec postgres psql -U neobank -d neobank -c \
  "DELETE FROM notifications_processed_events WHERE event_id IN (SELECT event_id FROM accounts_outbox WHERE event_type = 'AccountCreated');"
docker compose exec kafka kafka-consumer-groups.sh --bootstrap-server localhost:9092 \
  --group notifications-svc --topic account.events --reset-offsets --to-earliest --execute
docker compose start notifications-svc
# account.events компактится, так что переигровка доставит последнее AccountCreated на пользователя
```

Проще всего демонстрировать на свежих пользователях. Кросс-сервисный `UPDATE user_contacts ... FROM accounts` технически возможен (одна физическая база) и отвергается осознанно: notifications-svc не читает чужие таблицы.

### Тесты

`services/notifications-svc/money_test.go` — таблица против семантики `money.ts` (`0` → `0.00`, `5` → `0.05`, `123456` → `1,234.56`, `100000000` → `1,000,000.00`, `-2550` → `-25.50`) плюс проверка ASCII, на которой держится отсутствие charset-заголовка.

`services/notifications-svc/email_test.go` — здесь требования спринта становятся исполняемыми: в теле declined-письма нет ни `amount_threshold`/`velocity_count`/`velocity_sum`, ни слов `threshold`/`limit`/`rule`, ни цифр порогов; в received-письме нет `@` (то есть ничьего email) и слова `balance`, есть номер счёта отправителя, и строка *From account* исчезает целиком при пустом номере; failed-письмо рендерит каждый известный код как фразу и опускает строку `Reason` для неизвестного, не показывая сырой токен; у зануленного `occurred_at` не появляется дата `1970`.

`services/notifications-svc/kafka_test.go` — `eventTypeOf` (есть / нет / пустое значение / неверный регистр / дубликаты ключей → первый), и пиннинг обоих наборов литералов wire-контракта.

`services/notifications-svc/dlq_test.go` — без БД: `transferRetryDelay` (рост и потолок), `sendToDLQ` (ключ/значение/оригинальные заголовки сохраняются, `dlq_reason`/`dlq_source_partition`/`dlq_source_offset` добавляются, через фейковый `kafkaMessageWriter`), и три ядовитых ветки `processTransferMessage` (нет заголовка, неизвестный тип, `proto.Unmarshal` падает) — все три не трогают `pool`, поэтому тестируются с `nil` без БД.

`services/notifications-svc/contacts_test.go` — под живой базой (`t.Skip` без `DATABASE_URL`, конвенция из `pkg/outbox`): жизненный цикл `claimEvent` — первый вызов `true`; **второй тоже `true`** на строке, оставленной в `processing` (политика восстановления после падения проверяется, а не предполагается); после `finishEvent(sent)` и `finishEvent(skipped)` — `false`; три захвата подряд + `finishEvent` оставляют ровно одну строку. Плюс `getContactByAccountID`: hit, miss, и `account_number IS NULL` → `""`.

`pkg/outbox/relay_test.go` — `TestRelayBatch_StampsEventTypeHeader` (заголовок доезжает до сообщения, ровно один, со значением из колонки) и `TestHeaderEventType_IsWireContract`.

```bash
DATABASE_URL="postgres://neobank:neobank_dev_password@localhost:5432/neobank?sslmode=disable" \
  go test ./services/notifications-svc/... ./pkg/outbox/... -v
```

## notifications-svc: устойчивость консьюмера

Retry/DLQ (выше) отвечает на «одно сообщение не должно останавливать остальные». Здесь — три смежных вопроса: видно ли снаружи, что консьюмер отстаёт или упал; не теряет ли рестарт сообщение, которое как раз обрабатывалось; и говорит ли `/healthz` правду.

### Лаг консьюмеров

`monitorConsumers` (`kafka.go`) раз в `consumerLagLogInterval` (30 с) логирует лаг каждого из трёх ридеров — `reader.Stats().Lag`, посчитанный самим `kafka-go` из high water mark партиции на каждом fetch. Не `Reader.Lag()` (тот возвращает `-1` в режиме consumer-group, а у всех трёх ридеров здесь есть `GroupID`) — именно `Stats().Lag`, которая по коду `kafka-go` обновляется независимо от режима. Без этого «письма не приходят» неотличимо снаружи от «никто не переводил деньги»: оба выглядят как тишина в Mailpit. Лаг — то, что их различает.

Те же числа продублированы в `/healthz` (`consumer_lag`, ключ — имя топика), а не только в логе — обе формы годятся по условиям задачи («логов и простой метрики» достаточно, Prometheus/Grafana — вне скоупа), и раз число уже посчитано для лога, отдать его же в JSON почти бесплатно. `consumerHealth` в `/healthz` (следующий раздел) обновляется отдельным циклом, не этим — `Stats()` оказалась неподходящим сигналом для здоровья, хотя для лага она справляется отлично.

```bash
docker compose logs notifications-svc | grep 'consumer lag'
# notifications-svc: consumer lag: topic=user.events lag=0 offset=42
# notifications-svc: consumer lag: topic=account.events lag=0 offset=17
# notifications-svc: consumer lag: topic=transfer.events lag=0 offset=9

curl -s http://localhost:8086/healthz | jq .consumer_lag
# {"user.events": 0, "account.events": 0, "transfer.events": 0}
```

### Graceful shutdown: SIGTERM дообрабатывает текущее сообщение

До этого шага ни один Kafka-консьюмер в репозитории не завершался иначе, чем убийством процесса — `context.Background()` из `main()`, без обработки сигналов, паттерн, явно описанный (и здесь впервые нарушенный) в разделе Reconciliation выше. `notifications-svc` теперь первое исключение.

`main()` берёт `ctx` из `signal.NotifyContext(..., syscall.SIGINT, syscall.SIGTERM)` и передаёт его каждому консьюмеру как `fetchCtx` — контекст, на котором блокируется `FetchMessage`. Отмена контекста расталкивает ридер, если тот простаивает в ожидании следующего сообщения, и горутина завершается, не начиная новую работу. Но сообщение, уже полученное из `FetchMessage` в момент отмены, обрабатывается на **отдельном** `context.Background()`, а не на `fetchCtx` — иначе SIGTERM мог бы оборвать `sendEmailWithRetry`/`claimEvent` на середине, и сообщение осталось бы ни закоммиченным, ни по-настоящему обработанным. Это и значит «дообработать текущее сообщение»: граница — по сообщению, не по горутине. В `runTransferEventsConsumer` та же логика вдобавок покрывает всю цепочку ретраев из `handleTransferMessageWithRetry` — SIGTERM посреди третьей из пяти попыток не обрывает её, а даёт довести до конца (успеха, следующей попытки или DLQ).

HTTP-сервер (`http.Server` вместо голого `http.ListenAndServe`) останавливается тем же сигналом через `srv.Shutdown` с отдельным таймаутом (`shutdownTimeout`, 10 с) — это не связано с Kafka-частью и ограничено по времени специально, в отличие от консьюмеров. `main()` дожидается всех трёх консьюмерных горутин через `sync.WaitGroup` **без** таймаута: обрезать это дедлайном воспроизвело бы ту самую проблему, которую graceful shutdown должен убрать.

Это создаёт зависимость от внешнего таймаута, который код не контролирует: если SMTP лежит на протяжении всей серии ретраев, дренаж одного сообщения может занять больше, чем стандартные 10 секунд, которые Docker/Kubernetes ждут между SIGTERM и SIGKILL. `docker-compose.yml` поэтому задаёт `stop_grace_period: 60s` для `notifications-svc` — с запасом относительно `transferMaxAttempts` попыток и пауз между ними. Если SIGKILL всё же случится раньше (проверено вручную ниже, с намеренно коротким `--timeout 30` при недоступном Mailpit) — это не потеря данных: сообщение остаётся незакоммиченным и `processing`, `claimEvent` реклеймит его на следующем старте — тем же путём, каким уже восстанавливается обычный краш. Щедрый grace period — это то, что превращает этот путь из «единственного» в «редкий, для внешнего SIGKILL», а не гарантия сама по себе.

```bash
docker compose stop mailpit                   # чтобы гарантированно поймать сообщение в процессе ретраев
curl -s -X POST http://localhost:8080/transfers \
  -H "Authorization: Bearer $ALICE_TOKEN" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"recipient_account_number":"<номер счёта bob>","amount":1000}'
docker compose stop --timeout 30 notifications-svc
docker compose logs notifications-svc | tail -20
# notifications-svc: shutdown signal received, draining in-flight work
# notifications-svc: transfer.events: attempt N/5 failed ...
# notifications-svc: waiting for consumers to finish their current message
# ... (ретраи или DLQ доводятся до конца, ЗАТЕМ)
# notifications-svc: shutdown complete
docker compose start mailpit notifications-svc
```

### `/healthz`: честная связь с Kafka, не только «процесс жив»

`pkg/health.Handler` (всё ещё используется у gateway) всегда отвечает `200` — он не проверяет вообще ничего, только то, что HTTP-сервер способен ответить. `notifications-svc` теперь, как и `auth-svc`/`accounts-svc`/`transfers-svc`/`fraud-svc`, использует собственный inline-обработчик `GET /healthz` вместо него — но, в отличие от них (там проверяется только `SELECT 1`), здесь ещё и Kafka: `consumerHealth` (`kafka.go`) — один `atomic.Bool` на весь брокер (не по одному на ридер — все три ридера подключаются к одному и тому же списку брокеров, так что «доступна ли Kafka» здесь один факт, а не три). `/healthz` возвращает `503`, если Postgres недоступен **или** брокер сейчас недоступен — раньше сервис бодро отвечал `200`, даже если Kafka была недоступна с самого старта.

**Флаг обновляет `monitorKafkaHealth` — независимый цикл, который сам раз в `kafkaHealthProbeInterval` (10 с) дозванивается до брокера (`kafka.DialContext`), а не что-то, выведенное из состояния трёх ридеров.** Через это пришлось пройти двумя более простыми путями, и оба не выдержали проверки `docker compose stop/start kafka`:

1. Обновлять флаг прямо в цикле `FetchMessage` (`true` на ошибку фетча, `false` на успешный) — честно в сторону отказа, но не в сторону восстановления: `FetchMessage` не возвращается вообще, пока сообщение не готово, так что у восстановившегося, но бездействующего (нет новых событий) ридера просто нет момента, в который флаг мог бы переключиться обратно. После `docker compose start kafka` `/healthz` оставался `503` сколько угодно долго.
2. Сравнивать `reader.Stats().Errors` между тиками `monitorConsumers`, в предположении, что `kafka-go` ретраит соединение в фоне независимо от того, заблокирован ли `FetchMessage`, и это должно инкрементить счётчик. Проверка показала обратное: при реальном отключении брокера `Stats().Errors` переставал расти после первых нескольких ошибок при старте, хотя `FetchMessage` продолжал явно логировать `failed to dial` каждые ~25 секунд — этот счётчик покрывает более узкий набор внутренних путей ретрая, чем path, по которому реально идут ошибки дозвона у consumer-group ридера.

Оба случая ловились одинаково: `docker compose stop kafka`, подождать, `docker compose start kafka`, подождать снова **не отправляя ни одного сообщения** — и смотреть, возвращается ли `kafka` к `true`. Прямой независимый пробник эту проблему обходит целиком: результат дозвона — это и есть сигнал, а не что-то, что нужно выводить из внутренностей ридера.

```bash
docker compose stop kafka
sleep 10   # один цикл kafkaHealthProbeInterval
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8086/healthz
# 503
curl -s http://localhost:8086/healthz | jq '{status, kafka, postgres}'
# {"status": "error", "kafka": false, "postgres": true}

docker compose start kafka
sleep 10   # снова один цикл — восстановление тоже не ждёт ни одного сообщения на топиках
curl -s http://localhost:8086/healthz | jq '{status, kafka, postgres}'
# {"status": "ok", "kafka": true, "postgres": true}
```

## Фронтенд

`frontend/` — SPA на Vite + React + TypeScript, обращается к бэкенду через Gateway (`http://localhost:8080`). Роутинг, структура проекта и типизированный API-слой уже на месте; настоящих форм и экранов пока нет — это следующие шаги.

### Запуск (dev)
```bash
cd frontend
npm install
npm run dev
```
Поднимает dev-сервер на `http://localhost:5173` (порт по умолчанию у Vite). Бэкенд (в первую очередь Gateway) поднимается отдельно, `docker compose up`.

### Структура — feature-based, не по типам файлов
```
frontend/src/
├── app/           — роутинг (react-router), провайдеры (react-query), layout-shell
├── features/
│   ├── auth/      — components/ (LoginPage, RegisterPage), api.ts (register/login/logout/...); hooks/ появятся вместе с реальными формами
│   └── accounts/  — components/ (DashboardPage), api.ts (getMe); hooks/ появятся вместе с реальными запросами в UI
└── shared/
    ├── ui/          — переиспользуемые примитивы: Button, Input, Card, tokens.css
    └── api-client/  — HTTP-слой: fetch-обёртка, токены, single-flight refresh, сгенерированные типы (см. «API-клиент» ниже)
```
Принцип: фича несёт свои компоненты, хуки и вызовы API рядом, а не разложена по `components/`, `hooks/`, `api/` на верхнем уровне репозитория. `shared/api-client/` — только инфраструктура (fetch, токены, retry-логика), а не место для конкретных вызовов конкретных эндпоинтов: те типизированы через сгенерированные типы, но живут в `api.ts` своей фичи.

Стили — CSS Modules (`*.module.css`), без отдельной библиотеки: работают у Vite из коробки, и классы уже естественно скопированы по компонентам — то же самое разбиение, что и у feature-based структуры. Общие токены (цвета, отступы, radius, шрифт) — `shared/ui/tokens.css`, CSS custom properties с поддержкой `prefers-color-scheme: dark`.

### Dev-прокси и CORS
У Gateway нет префикса `/api` — маршруты у него `/auth/*`, `/accounts/*` и т.д. напрямую (`gateway/proxy.go`). Фронт обращается к `/api/*`; dev-сервер Vite (`frontend/vite.config.ts`) перехватывает `/api/*`, снимает префикс `/api` и проксирует остаток на `http://localhost:8080`. Например, `GET /api/accounts/me` с фронта уходит на Gateway как `GET /accounts/me`.

Это полностью убирает проблему CORS в разработке: браузер видит только один origin (dev-сервер Vite), запрос к Gateway идёт со стороны самого dev-сервера, а не напрямую из браузера. **В продакшене так же работать не будет** — там нужно либо отдавать собранный статик (`npm run build` → `frontend/dist/`) через сам Gateway (тогда фронт и API снова на одном origin), либо явно выставить CORS-заголовки на Gateway, если фронт и бэкенд остаются на разных origin. Этот выбор — не часть текущего шага.

### API-клиент

**Выбран вариант с OpenAPI-спекой, а не ручными TS-типами.** Контракт Gateway (8 auth-эндпоинтов + `GET /accounts/me`) описан в `gateway/openapi.yaml`; `frontend/src/shared/api-client/schema.ts` генерируется из него командой `npm run gen:api` (обёртка над `openapi-typescript`, см. `frontend/package.json`) и **руками не редактируется**. В спеку осознанно не включены `GET /accounts/{id}` и `PATCH /accounts/{id}/status` — Gateway их проксирует, но фронт их не вызывает и не будет: это внутренняя/оперская поверхность accounts-svc, не часть контракта с браузером.

Причина выбора: спека — это ещё и единственное живое, проверяемое описание того, что Gateway на самом деле принимает и отдаёт (тело запроса, все коды ответа, какие пути требуют bearer-токен — это тоже в спеке, `security` по каждому эндпоинту списан прямо с `gateway/middleware.go`). Ручные типы работали бы не хуже день в день, но расходятся с бэкендом молча: ничто не заставляет вспомнить о них при следующем изменении хендлера. Цена — лишний шаг генерации при каждом изменении контракта; при таком маленьком числе эндпоинтов (9) она того стоит.

Сами типизированные HTTP-методы (`register`, `login`, `getMe`, ...) не генерируются — это обычные функции в `features/*/api.ts`, использующие типы `paths[...]` из сгенерированной схемы под каждый параметр и ответ. Осознанно не взят `openapi-fetch` (типизированный клиент поверх той же генерации): он берёт на себя разбор ответа и заворачивает результат в `{data, error}`, что плохо сочетается с тем, что должен делать `shared/api-client/client.ts` сам — единообразно бросать `ApiError` (со статусом и телом) и перехватывать 401 для refresh-and-retry. Взято от `openapi-typescript` только то, что действительно нужно — типы, — а вся управляющая логика написана руками.

```bash
# перегенерировать типы после любого изменения gateway/openapi.yaml
cd frontend
npm run gen:api
```

`npm audit` на этом шаге показывает 2 high (ReDoS в `js-yaml`, транзитивная зависимость `openapi-typescript` → `@redocly/openapi-core`). Это dev-only инструмент, парсящий только наш собственный `gateway/openapi.yaml`, а не недоверенный ввод — реальной экспозиции нет; `npm audit fix` пока недоступен из-за конфликта peer-зависимости `openapi-typescript` на TypeScript (заявлен `^5.x`, в репозитории уже `~6.0.2` — сам пакет от этого не ломается, конфликтует только резолвер).

### Хранение токенов — и его цена

- **Access-токен** (JWT, TTL 15 минут) — только в памяти, модульная переменная в `shared/api-client/tokenStore.ts`. Не переживает перезагрузку страницы.
- **Refresh-токен** (opaque, TTL 7 дней, одноразовый — ротируется на каждый `/auth/refresh`) — в `localStorage`, чтобы сессия переживала перезагрузку.

Это компромисс, не забывчивость. `localStorage` уязвим к XSS: любой инжектнутый в страницу JS может прочитать `localStorage` и увести refresh-токен, а с ним — возможность бесконечно перевыпускать сессию. По-настоящему правильное решение — `httpOnly`-cookie для refresh-токена: тогда JS (в том числе инжектнутый) физически не может его прочитать, только браузер молча прикладывает cookie к запросам на `/auth/refresh`. Это осознанно не сделано на этом шаге, потому что требует правки бэкенда (auth-svc должен отвечать на `/login`/`/refresh` через `Set-Cookie`, а не JSON-полем `refresh_token`, плюс `SameSite`/`Secure`-политика, плюс сам Gateway должен научиться читать cookie, а не только `Authorization`-заголовок) — то есть контракт `TokenPair` в `gateway/openapi.yaml` пришлось бы менять вместе с этим. Текущий вариант (`localStorage`) — сознательно принятый краткосрочный компромисс, а не то, как это должно остаться.

Держать access-токен вне `localStorage` (только в памяти) — это половина смягчения: даже успешный XSS не достаёт долгоживущий JWT напрямую, только 15-минутный, и то лишь пока вкладка открыта. Полностью проблему это не снимает (тот же XSS всё ещё может дёрнуть `/accounts/me` от имени пользователя, пока вкладка жива, и достать refresh-токен из `localStorage`), но сужает окно и цену компрометации.

### Автоматический refresh и single-flight

`shared/api-client/client.ts`: любой запрос, получивший `401`, автоматически вызывает `/auth/refresh` и повторяет исходный запрос с новым access-токеном; если сам refresh не проходит (отклонён бэкендом, а не просто сетевой сбой) — токены чистятся и `client.ts` делает `window.location.href = '/login'`. Это единственная функция, которая триггерит refresh: см. флаг `skipAuthRetry` в `RequestOptions` — им помечены все auth-эндпоинты, у которых собственный `401` (например, `/auth/login` с неверным паролем) значит совсем не «токен протух», а `/auth/logout` (единственный auth-путь, реально требующий сессии — см. `publicPaths` в `gateway/middleware.go`) от общей логики не освобождён.

Критично — **single-flight**: `refreshPromise` в `client.ts` — общий promise на модуль. Первый вызов, поймавший `401`, создаёт его и реально бьёт по `/auth/refresh`; все остальные конкурентные вызовы видят уже созданный promise и ждут его вместо того, чтобы стрелять своим запросом. Это не оптимизация, а необходимость: refresh-токен одноразовый (ротируется при каждом вызове, спринт 1) — без single-flight пять параллельных запросов на `/auth/refresh` означали бы, что только первый пройдёт, а остальные четыре попытаются погасить уже использованный токен и получат отказ, разлогинив пользователя на ровном месте. После завершения (успех или неудача) `refreshPromise` сбрасывается в `null` через `.finally()` — следующий, независимый протухший токен (например, 15 минут спустя) запускает новый цикл, а не переиспользует уже разрешившийся promise.

**Как проверено.** Ручной сценарий из постановки (открыть dashboard с несколькими параллельными запросами, посмотреть Network) пока недоступен буквально — экраны и data-fetching в UI появятся в следующих промптах, `DashboardPage` сейчас статичная заглушка. Вместо этого поведение проверено скриптом, гоняющим настоящий `client.ts`/`tokenStore.ts` под Node (`tsx`) с подменёнными `fetch`/`localStorage`: 5 параллельных запросов, каждый ловит `401`, и — ровно **один** вызов `/auth/refresh`, все 5 успешно повторились с новым токеном. Отдельно проверено, что после первого цикла `refreshPromise` не залипает: второй, независимый протухший токен запускает новый (второй) вызов `/auth/refresh`, а не переиспользует уже разрешившийся promise. Скрипт был временным (не закоммичен) — при появлении реального dashboard с несколькими запросами стоит повторить проверку буквально, через Network-вкладку.

### Маршруты
`/register`, `/login`, `/dashboard` — сейчас пустые страницы-заглушки (заголовок внутри `Card`), нужны только чтобы проверить, что роутинг работает. `/` редиректит на `/login`.

## Статус
На этом шаге описана только структура репозитория и `docker-compose.yml`.
Следующие шаги добавят Go-код сервисов, интеграцию с инфраструктурой (Postgres/Redis/Kafka) и CI.
