# Neo-Bank

Мини-необанк на микросервисной архитектуре.

## Структура
- `gateway/` — единая точка входа (API Gateway)
- `services/` — микросервисы: `auth-svc`, `accounts-svc`, `ledger-svc`, `transfers-svc`, `fraud-svc`, `notifications-svc`
- `proto/` — общие protobuf-контракты между сервисами
- `.github/workflows/` — CI-пайплайны

## Статус
На этом шаге описана только структура репозитория и `docker-compose.yml`.
Следующие шаги добавят Go-код сервисов, инфраструктуру (Postgres/Redis/Kafka) и CI.
