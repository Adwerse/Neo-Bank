# Neo-Bank

Мини-необанк на микросервисной архитектуре.

## Структура
- `gateway/` — единая точка входа (API Gateway)
- `services/` — микросервисы: `auth-svc`, `accounts-svc`, `ledger-svc`, `transfers-svc`, `fraud-svc`, `notifications-svc`
- `proto/` — общие protobuf-контракты между сервисами
- `frontend/` — SPA (Vite + React + TypeScript), см. «Фронтенд» ниже
- `.github/workflows/` — CI-пайплайны

## Инфраструктура (dev)
Postgres, Redis и Kafka подняты в `docker-compose.yml`. `auth-svc` использует все три (Postgres и Redis — с первого спринта, Kafka — как продюсер событий, см. ниже); остальные сервисы пока не подключены.

Креды Postgres в `docker-compose.yml` — только для локальной разработки, не для продакшена.

## События (Kafka)
`auth-svc` публикует событие `UserActivated` в топик `user.events` сразу после успешного `POST /verify-email` (в момент, когда `users.status` переходит в `active`). Контракт — `proto/events/v1/user_events.proto` (`events.v1.UserActivated`: `user_id`, `email`, `occurred_at`, `event_id`), сериализация бинарным protobuf. Ключ сообщения — `user_id`: это гарантирует, что все события одного пользователя попадают в одну партицию и обрабатываются по порядку. `event_id` — случайный UUIDv4, генерируется в auth-svc на каждую публикацию (`generateEventID` в `services/auth-svc/kafka.go`) и используется accounts-svc для дедупликации при повторной доставке (см. «Идемпотентность» ниже).

`accounts-svc` — consumer этого топика (consumer group `accounts-svc`): на `UserActivated` создаёт строку в `accounts` со сгенерированным номером счёта и `status = 'active'`, а **сразу после этого** — вызывает `ledger-svc` `CreateLedgerAccount(account_id)` по gRPC, чтобы у нового счёта появился ledger-аккаунт (адрес ledger — env `LEDGER_GRPC_ADDR`, дефолт `ledger-svc:8083`). Порядок фиксации важен: если вызов ledger упал, offset события **не** коммитится — Kafka передоставит сообщение, а идемпотентность (consumer'а и самого `CreateLedgerAccount`) делает повтор безопасным. Это ровно тот случай, ради которого строились at-least-once + идемпотентность.

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

Как и консьюмер Kafka в `accounts-svc`, воркер живёт всё время жизни процесса без graceful shutdown (`context.Background()` из `main()`) — тот же паттерн, что и везде в этом репозитории.

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
