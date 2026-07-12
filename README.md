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
`auth-svc` публикует событие `UserActivated` в топик `user.events` сразу после успешного `POST /verify-email` (в момент, когда `users.status` переходит в `active`). Контракт — `proto/events/v1/user_events.proto` (`events.v1.UserActivated`), сериализация бинарным protobuf. Ключ сообщения — `user_id`: это гарантирует, что все события одного пользователя попадают в одну партицию и обрабатываются по порядку.

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

## Статус
На этом шаге описана только структура репозитория и `docker-compose.yml`.
Следующие шаги добавят Go-код сервисов, интеграцию с инфраструктурой (Postgres/Redis/Kafka) и CI.
