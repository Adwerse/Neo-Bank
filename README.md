# Neo-Bank

Мини-необанк на микросервисной архитектуре.

## Структура
- `gateway/` — единая точка входа (API Gateway)
- `services/` — микросервисы: `auth-svc`, `accounts-svc`, `ledger-svc`, `transfers-svc`, `fraud-svc`, `notifications-svc`
- `proto/` — общие protobuf-контракты между сервисами
- `.github/workflows/` — CI-пайплайны

## Инфраструктура (dev)
Postgres, Redis и Kafka подняты в `docker-compose.yml`. `auth-svc` использует все три (Postgres и Redis — с первого спринта, Kafka — как продюсер событий, см. ниже); остальные сервисы пока не подключены.

Креды Postgres в `docker-compose.yml` — только для локальной разработки, не для продакшена.

## События (Kafka)
`auth-svc` публикует событие `UserActivated` в топик `user.events` сразу после успешного `POST /verify-email` (в момент, когда `users.status` переходит в `active`). Контракт — `proto/events/v1/user_events.proto` (`events.v1.UserActivated`: `user_id`, `email`, `occurred_at`, `event_id`), сериализация бинарным protobuf. Ключ сообщения — `user_id`: это гарантирует, что все события одного пользователя попадают в одну партицию и обрабатываются по порядку. `event_id` — случайный UUIDv4, генерируется в auth-svc на каждую публикацию (`generateEventID` в `services/auth-svc/kafka.go`) и используется accounts-svc для дедупликации при повторной доставке (см. «Идемпотентность» ниже).

`accounts-svc` — consumer этого топика (consumer group `accounts-svc`): на `UserActivated` создаёт строку в `accounts` со сгенерированным номером счёта и `status = 'active'`.

Топик создаётся автоматически брокером при первой публикации (`KAFKA_CFG_AUTO_CREATE_TOPICS_ENABLE: "true"` задано явно в `docker-compose.yml`, хотя это и так поведение Kafka по умолчанию) — отдельного шага инициализации топика нет. auth-svc не блокирует старт на доступности Kafka: продюсер (`segmentio/kafka-go`) подключается лениво при первой записи и переподключается сам, как и клиенты Postgres/Redis.

Публикация в Kafka не входит в ту же транзакцию, что и обновление статуса в Postgres — это известное и осознанное ограничение MVP (см. TODO в `services/auth-svc/kafka.go`), по-настоящему решается паттерном outbox в будущем.

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

## Статус
На этом шаге описана только структура репозитория и `docker-compose.yml`.
Следующие шаги добавят Go-код сервисов, интеграцию с инфраструктурой (Postgres/Redis/Kafka) и CI.
