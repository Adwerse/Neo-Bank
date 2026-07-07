# Neo-Bank

Мини-необанк на микросервисной архитектуре.

## Структура
- `gateway/` — единая точка входа (API Gateway)
- `services/` — микросервисы: `auth-svc`, `accounts-svc`, `ledger-svc`, `transfers-svc`, `fraud-svc`, `notifications-svc`
- `proto/` — общие protobuf-контракты между сервисами
- `.github/workflows/` — CI-пайплайны

## Инфраструктура (dev)
Postgres, Redis и Kafka добавлены в `docker-compose.yml` для использования сервисами в следующих спринтах — пока ни один сервис к ним не подключается.

Креды Postgres в `docker-compose.yml` — только для локальной разработки, не для продакшена.

## Статус
На этом шаге описана только структура репозитория и `docker-compose.yml`.
Следующие шаги добавят Go-код сервисов, интеграцию с инфраструктурой (Postgres/Redis/Kafka) и CI.
