# api-ratelimiter

*Языки: [English](README.md) · **Русский***

Сервис ограничения частоты запросов для API-сервера. Работает как
вспомогательный HTTP-сервис, к которому nginx/Angie обращается через
`auth_request`. Не парсит протокол, не формирует бизнес-ответы — отвечает
`200 OK` или `403 Forbidden` с пустым телом (403, а не 429, потому что
`auth_request` пропускает в parent только 2xx / 401 / 403, остальное
конвертит в 500). Кастомизация ответа делается на стороне nginx/Angie
через `error_page`.

Полное ТЗ — [`docs/specification.md`](docs/specification.md).

## Что внутри

- HTTP endpoint `GET /check` — единственная точка для nginx/Angie. Принимает
  `X-Api-Key` и `X-Real-IP`, возвращает 200 / 403.
- in-memory счётчики с fixed-window алгоритмом и поддержкой burst.
- Redis (3 БД): индивидуальные лимиты по api-key, нарушители по api-key,
  нарушители по IP.
- веб-админка (просмотр) и Prometheus `/metrics` — оба читают из одного
  registry в памяти процесса.
- graceful shutdown с финальным flush'ем нарушителей в Redis.

## Архитектура

```
Клиент ─► nginx/Angie ─auth_request─► api-ratelimiter ─► Redis (DB1/2/3)
                  │
                  ├─[200]─► PHP upstream
                  └─[403]─► error_page → 200 с кастомным телом
```

Подробнее — раздел 2 `docs/specification.md`.

## Сборка

```bash
make build              # бинарь ./api-ratelimiter (LDFLAGS: -s -w, версия из git tag)
make test               # юнит-тесты
make test-cover         # отчёт coverage.html
make lint               # golangci-lint (требует установки golangci-lint)
```

Требования: Go **1.21+**, Redis **6.0+**.

## Запуск (dev)

```bash
make run
```

Поднимает сервис на:

- `unix:/tmp/ratelimit.sock` — `/check` для nginx/Angie
- `127.0.0.1:8080` — веб-админка
- `127.0.0.1:9091/metrics` — Prometheus
- ожидает Redis на `127.0.0.1:6379`

## Параметры запуска

Все флаги — `--flag value` (`pflag`).

| Флаг                   | Default                    | Назначение                                                |
|------------------------|----------------------------|-----------------------------------------------------------|
| `--listen`             | `unix:/run/ratelimit.sock` | Адрес `/check` (`unix:/path` или `host:port`)             |
| `--admin-listen`       | `127.0.0.1:8080`           | Веб-админка                                               |
| `--metrics-listen`     | `127.0.0.1:9091`           | Prometheus `/metrics`                                     |
| `--redis-addr`         | `127.0.0.1:6379`           | Адрес Redis                                               |
| `--redis-password`     | `""`                       | Пароль Redis                                              |
| `--log-level`          | `info`                     | `debug`, `info`, `warn`, `error`                          |
| `--log-format`         | `json`                     | `json` (продакшен) или `text` (dev)                       |
| `--global-limit`       | `100`                      | Лимит запросов в окне (для ключей не из redisDB1 и по IP) |
| `--burst`              | `0`                        | Доп. запросы сверх лимита в одном слоте                   |
| `--window`             | `second`                   | Единица окна: `second` или `minute`                       |
| `--cleanup-interval`   | `15`                       | Интервал cleanup, минуты                                  |
| `--abuse-ttl`          | `15`                       | TTL записей в redisDB2/redisDB3, минуты                   |
| `--abuse-multiplier`   | `10`                       | Порог `AbuseHits` = `global_limit * multiplier`           |
| `--abuse-transfer-threshold` | `3`                  | Минимум `AbuseHits` для переноса в Redis                  |
| `--socket-mode`        | `0666`                     | Права на unix-сокет из `--listen` (octal). Для TCP игнорируется |

При старте проверяется инвариант `--burst < --global-limit * --abuse-multiplier`,
иначе сервис завершается с ошибкой.

## Redis — структуры данных

Redis должен быть **выделенным**. Хардкод номеров БД:

| DB    | SELECT | Содержимое                              |
|-------|--------|-----------------------------------------|
| `DB1` | 1      | Индивидуальные лимиты по api-key        |
| `DB2` | 2      | Нарушители по api-key                   |
| `DB3` | 3      | Нарушители по IP                        |

Пример индивидуального лимита:

```
HSET rate:limit:abc123 created_at 1717000000 limit 500
```

Подробнее — раздел 7 `docs/specification.md`.

## Логика

- На каждый запрос определяется ключ: `api_key` (если есть и в DB1) → лимит
  из DB1 (карта `KnownCounters`); `api_key` не в DB1 → `--global-limit`
  (карта `UnknownCounters` с префиксом `key:`); пустой `api_key` → `IP`
  (`UnknownCounters` с префиксом `ip:`).
- В каждом слоте `WindowCount` инкрементируется без условий. Решение:
  `WindowCount > limit + burst` → 429, иначе 200 (с инкрементом `BurstHits`
  в burst-зоне `limit < WindowCount ≤ limit+burst`).
- При смене слота: если предыдущий `WindowCount > limit` →
  `ViolationHits++` (Known) или если `> limit*multiplier` → `AbuseHits++`
  (Unknown). Затем `WindowCount` сбрасывается.
- Перенос в DB2/DB3 происходит **только в цикле cleanup**, не на горячем
  пути. Из `KnownCounters` ничего не переносится — нарушения видны на
  странице `/limits` и в метриках.
- Redis недоступен → `api_key` считается «не в DB1», уходит в
  `UnknownCounters` с глобальным лимитом. Reconnect — автоматический
  через connection pool `go-redis`.
- При панике в `/check` → `200 OK` (fail open).

## Веб-админка

`http://<admin-listen>/`:

- `/` — статус, флаги, метрики (тот же набор что на `/metrics`)
- `/limits` — индивидуальные лимиты из DB1, с колонками-счётчиками из
  `KnownCounters` (in-memory)
- `/abuse/keys` — нарушители из DB2
- `/abuse/ips` — нарушители из DB3

Авторизации нет — предполагается проксирование через nginx/Angie.

## Метрики

Все метрики — в Prometheus registry, читаются и через `/metrics`, и через
веб-админку.

```
ratelimit_requests_total{result="allowed|blocked_individual|blocked_global"}
# всего блокировок = sum(rate(ratelimit_requests_total{result=~"blocked_.*"}[5m]))
ratelimit_counters_known_active
ratelimit_counters_unknown_active
ratelimit_memory_bytes
ratelimit_cleanup_runs_total
ratelimit_cleanup_deleted_total
ratelimit_cleanup_transferred_total
ratelimit_cleanup_last_duration_seconds
ratelimit_redis_errors_total
ratelimit_redis_db{1,2,3}_keys
ratelimit_check_duration_seconds  # histogram
```

Counter-значения — кумулятивные с момента старта. Для динамики
используйте `rate()`/`increase()`.

## Деплой

systemd-unit и пример конфига nginx/Angie — в разделах 12, 13 `docs/specification.md`.
Готовый к использованию конфиг nginx/Angie лежит в [`configs/nginx.example.conf`](configs/nginx.example.conf).
Бинарь устанавливается:

```bash
sudo make install      # → /usr/local/bin/api-ratelimiter
```

Версия бинаря берётся из git tag (`git describe --tags --always --dirty`).
При сборке вне репозитория — `dev`.

## Структура проекта

```
api-ratelimiter/
├── cmd/api-ratelimiter/main.go      # точка входа
├── internal/
│   ├── config/                  # флаги, валидация
│   ├── counter/                 # KnownCounters, UnknownCounters
│   ├── limiter/                 # маршрутизация и решение
│   ├── handler/                 # HTTP /check
│   ├── cleanup/                 # цикл чистки и переноса в DB2/DB3
│   ├── store/                   # Redis (3 БД)
│   ├── admin/                   # веб-админка + html-шаблоны
│   └── metrics/                 # Prometheus registry
├── configs/                     # пример конфига nginx/Angie + systemd-unit
├── packaging/                   # nfpm.yaml + скрипты установки .deb
├── docs/specification.md        # полное ТЗ
├── .github/workflows/           # CI / release пайплайны (GitHub)
├── .gitlab-ci.yml               # CI / release пайплайн (GitLab)
└── Makefile
```

## Ограничения

- При рестарте in-memory счётчики и Prometheus Counter'ы сбрасываются.
  Данные в Redis сохраняются.
- Запись нарушителей в DB2/DB3 — раз в `--cleanup-interval`. Осознанный
  компромисс: в базу попадают только систематические нарушители.
- Горизонтальное масштабирование без общего стейта: счётчики независимы
  на каждом инстансе. Под Redis-счётчики зарезервирована БД `0`.

## Лицензия

Apache License 2.0 — см. [LICENSE](LICENSE).
