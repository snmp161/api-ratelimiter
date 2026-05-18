# ТЗ: api-ratelimiter

**Версия:** 2.11  
**Статус:** Draft

---

## 1. Назначение

**api-ratelimiter** — сервис ограничения частоты запросов (rate limiter) для API-сервера с несколькими версиями протокола. Работает как вспомогательный HTTP-сервис, к которому nginx/Angie обращается через механизм `auth_request` по unix-сокету или TCP. Не занимается парсингом протоколов, не формирует бизнес-ответы — только принимает решение «пускать / не пускать».

---

## 2. Место в архитектуре

```
Клиент
  │
  ▼
nginx/Angie
  │
  ├─► auth_request → unix/tcp → api-ratelimiter (Go)
  │                                    │
  │                                 Redis
  │                           ┌──────────────────────────────────┐
  │                           │ redisDB1: лимиты по api-key      │
  │                           │ redisDB2: нарушители по api-key  │
  │                           │ redisDB3: нарушители по IP       │
  │                           └──────────────────────────────────┘
  │
  ├─► [200 от api-ratelimiter] → PHP upstream (запрос передаётся без изменений)
  │
  ├─► [403 от api-ratelimiter] → error_page в nginx/Angie → ответ 200 с телом по протоколу
  │                           (формат задаётся в location)
  │
  └─► [default location — без лимита] → PHP upstream (запрос без изменений)
```

**Разделение ответственности:**

- **nginx/Angie** — извлечение api_key/token из query-параметров (приоритет api_key), маршрутизация, формирование кастомных ответов при блокировке через `error_page 403 = @...`. Rate limiting подключается только на явно указанных location'ах.
- **api-ratelimiter** — только логика подсчёта и проверки лимитов. Возвращает `200 OK` или `403 Forbidden` с пустым телом. **403 (а не 429) намеренно** — модуль `auth_request` в nginx/Angie пропускает в parent только 2xx / 401 / 403, всё остальное превращается в 500. Перерендер 403 → 200 с протокол-специфичным телом — задача nginx/Angie через `error_page`.
- **PHP upstream** — единственный бэкенд, вся бизнес-логика. Получает запрос без изменений.
- **Админка сайта** — прямая работа с redisDB1.

---

## 3. Логика принятия решения

На каждый запрос: определяем принадлежность к redisDB1, инкрементируем нужный счётчик, сравниваем с применимым лимитом.

**Важно:** перед каждым сравнением `WindowCount` неявно выполняется проверка смены слота — если текущий слот изменился, счётчик сбрасывается и при необходимости инкрементируется `AbuseHits`/`ViolationHits`. Детали — в разделе 4.

```
Входящий запрос
    │
    ├─ Есть api_key / token?
    │       │
    │      ДА
    │       │
    │       ├─ Есть ключ в redisDB1?
    │       │       │
    │       │      ДА ──► счётчик в KnownCounters[api_key]
    │       │       │     лимит = Лимит из redisDB1
    │       │       │     WindowCount > Лимит из redisDB1 + burst?
    │       │       │         НЕТ → BurstHits++ если в burst-зоне → 200 OK
    │       │       │         ДА  → 403
    │       │       │
    │       │      НЕТ ──► счётчик в UnknownCounters[api_key]
    │       │              лимит = --global-limit
    │       │              WindowCount > --global-limit + burst?
    │       │                  НЕТ → BurstHits++ если в burst-зоне → 200 OK
    │       │                  ДА  → 403
    │       │
    │      (WindowCount и Total инкрементируются в обоих случаях)
    │
    └─ Нет api_key/token
            │
            └─► счётчик в UnknownCounters[ip]
                лимит = --global-limit
                WindowCount > --global-limit + burst?
                    НЕТ → BurstHits++ если в burst-зоне → 200 OK
                    ДА  → 403

Redis недоступен (redisDB1 недоступен) → api_key считается не в redisDB1,
    попадает в UnknownCounters с --global-limit, счётчики продолжают работать
```

Запись в redisDB2/redisDB3 происходит **не в момент блокировки запроса**, а в цикле cleanup. См. раздел 5.

---

## 4. Счётчики в памяти

Счётчики хранятся в памяти Go-процесса в **двух отдельных map**. При рестарте сбрасываются — приемлемо. Данные в Redis при этом сохраняются.

**Важно:** счётчик заводится и ведётся для каждого входящего запроса без исключений. redisDB1 используется только как источник значения лимита и для маршрутизации в нужную map — сам подсчёт всегда в памяти.

### KnownCounters — для api-key из redisDB1

```go
// map[api_key]*KnownCounter
type KnownCounter struct {
    FirstRequest   time.Time  // время первого запроса за всё время жизни счётчика
    LastRequest    time.Time  // время последнего запроса (используется для чистки)
    Total          int64      // суммарно запросов за всё время жизни счётчика
    WindowCount    int64      // счётчик текущего слота
    Slot           int64      // номер текущего слота: floor(now.Unix() / windowSeconds)
    BurstHits      int64      // запросов через burst (Лимит из redisDB1 < req <= Лимит из redisDB1+burst)
    ViolationHits  int64      // слотов, в которых WindowCount превысил Лимит из redisDB1
}
```

`ViolationHits` инкрементируется при **смене слота** если `WindowCount > Лимит из redisDB1`. Слот вычисляется из абсолютного времени — запрос всегда попадает в правильный слот независимо от истории:

```
На каждый запрос (KnownCounter):
    currentSlot = floor(now.Unix() / windowSeconds)

    если currentSlot != counter.Slot:
        если counter.WindowCount > Лимит из redisDB1:
            counter.ViolationHits++
        counter.Slot = currentSlot
        counter.WindowCount = 0    ← сброс перед инкрементом

    counter.WindowCount++          ← запрос всегда в правильном слоте
```

Счётчики из `KnownCounters` **никогда не попадают в redisDB2**. Их нарушения видны в метриках и на странице `/limits` в веб-админке.

### UnknownCounters — для остальных api-key и IP

Ключи в map используют namespace-префикс во избежание коллизий между api_key и IP-адресами:

```
"key:{api_key}"      — для запросов с api_key/token, не найденным в redisDB1
"ip:{ip_address}"    — для запросов без api_key/token
```

Пример: `"key:abc123"`, `"ip:192.168.1.1"`. Гарантирует отсутствие коллизий даже если api_key выглядит как IP-адрес.

```go
// map[api_key|ip]*UnknownCounter
type UnknownCounter struct {
    FirstRequest  time.Time  // время первого запроса за всё время жизни счётчика
    LastRequest   time.Time  // время последнего запроса (используется для чистки)
    Total         int64      // суммарно запросов за всё время жизни счётчика
    WindowCount   int64      // счётчик текущего слота
    Slot          int64      // номер текущего слота: floor(now.Unix() / windowSeconds)
    BurstHits     int64      // запросов через burst (global_limit < req <= global_limit+burst)
    AbuseHits     int64      // слотов, в которых WindowCount > global_limit * abuse_multiplier
}
```

`AbuseHits` инкрементируется при **смене слота**. Burst включён в WindowCount, порог считается от `global_limit` без burst:

```
На каждый запрос (UnknownCounter):
    currentSlot = floor(now.Unix() / windowSeconds)

    если currentSlot != counter.Slot:
        если counter.WindowCount > global_limit * abuse_multiplier:
            counter.AbuseHits++
        counter.Slot = currentSlot
        counter.WindowCount = 0    ← сброс перед инкрементом

    counter.WindowCount++          ← запрос всегда в правильном слоте
```

Счётчики из `UnknownCounters` с `AbuseHits >= --abuse-transfer-threshold` переносятся в redisDB2/redisDB3 в цикле cleanup.

---

## 5. Цикл cleanup

Запускается раз в `--cleanup-interval` минут. Перед началом цикла проверяет `store.IsHealthy()` — если Redis на текущий момент известен как недоступный, **upsert'ы в redisDB2/redisDB3 пропускаются полностью** (счётчики остаются в памяти до восстановления Redis или до их естественной деактивации). GC inactive-счётчиков из памяти продолжает работать независимо от состояния Redis. Обрабатывает обе map независимо. Счётчик считается **неактивным** если выполнены **оба условия**:
1. Текущий слот отличается от слота счётчика (в текущем окне запросов не было).
2. С момента `LastRequest` прошло **не менее двух окон** (`2 × --window`).

Удвоенный зазор намеренно: при `window=minute` клиент со «struttering»-трафиком ~раз в 30-45 сек попадал бы каждый cleanup в категорию неактивных, и счётчик пересоздавался бы с нуля. 2×window сглаживает короткие паузы, цена — счётчик висит в памяти ещё до 2×window после последнего запроса.

### KnownCounters — чистка без переноса

```
Для каждого счётчика в KnownCounters:
    │
    ├─ Ключ существует в redisDB1?
    │       │
    │      НЕТ → ключ удалён из redisDB1 (warn-лог) → удалить из KnownCounters
    │       │    новые запросы уже создают счётчик в UnknownCounters
    │       │
    │      ДА
    │       └─ LastRequest старше одного слота? → удалить из KnownCounters
    │                                    иначе → оставить
```

Никакого переноса в redisDB2 — redisDB1-ключи не являются кандидатами в нарушители.

**Поведение при удалении ключа из redisDB1:** после удаления новые запросы этого api_key создают счётчик в `UnknownCounters`. Старый `KnownCounter` остаётся в памяти до следующего цикла cleanup (максимум `--cleanup-interval` минут), после чего удаляется. Двойное существование счётчиков в этот период не влияет на корректность rate limiting — решения принимаются по актуальному счётчику для каждого запроса.

### UnknownCounters — чистка с переносом в redisDB2/redisDB3

```
Для каждого счётчика в UnknownCounters:
    │
    ├─ LastRequest старше одного слота? (неактивный)
    │       │
    │      ДА
    │       ├─ AbuseHits >= --abuse-transfer-threshold → upsert в redisDB2/redisDB3, удалить из памяти
    │       └─ AbuseHits <  --abuse-transfer-threshold → просто удалить из памяти
    │
    └─ НЕТ (активный)
            ├─ AbuseHits >= --abuse-transfer-threshold → upsert в redisDB2/redisDB3, оставить в памяти
            └─ AbuseHits <  --abuse-transfer-threshold → оставить в памяти
```

**Upsert в redisDB2/redisDB3** — перезаписывает запись целиком:
- `first_seen`     ← `UnknownCounter.FirstRequest`
- `last_seen`      ← `UnknownCounter.LastRequest`
- `total_requests` ← `UnknownCounter.Total`
- `burst_hits`     ← `UnknownCounter.BurstHits`
- `abuse_hits`     ← `UnknownCounter.AbuseHits`
- TTL              ← скользящий `--abuse-ttl` минут (обновляется при каждом upsert)

---

## 6. Алгоритм rate limiting

**Fixed window** с поддержкой burst. Окна **абсолютные** — привязаны к астрономическому времени, не к моменту первого запроса клиента.

- Номер слота: `slot = floor(now.Unix() / windowSeconds)` — одинаков для всех клиентов в одну и ту же секунду (или минуту)
- Лимит в слоте: `--global-limit` (для ключей без записи в redisDB1) или значение из redisDB1
- Burst: `--burst` дополнительных запросов поверх лимита в одном слоте
- Эффективный лимит слота = `limit + burst`
- При смене слота: сначала оцениваем AbuseHits/ViolationHits завершившегося слота, затем сбрасываем WindowCount и инкрементируем для нового слота

**Блокировки:** один `sync.RWMutex` на каждую map (`KnownCounters` и `UnknownCounters` независимо). При росте нагрузки — переход на sharded mutex (256 шардов по hash ключа) без изменения логики.

---

## 7. Структуры данных в Redis

Redis должен быть **выделенным** — не делится с другими сервисами. Это обязательное требование: исключает пересечение ключей и обеспечивает полный контроль доступа.

Номера баз хардкодированы:

| Константа | SELECT | Назначение |
|-----------|--------|------------|
| `redisDB1` | `1` | Индивидуальные лимиты по api-key |
| `redisDB2` | `2` | Нарушители по api-key |
| `redisDB3` | `3` | Нарушители по IP |

База `0` не используется в текущей версии — зарезервирована под Redis-счётчики при горизонтальном масштабировании (v2). Для ручной диагностики через `redis-cli` без явного SELECT также доступна.

### redisDB1 (SELECT 1) — индивидуальные лимиты по api-key

```
Ключ:   rate:limit:{api_key}
Тип:    Hash
Поля:   created_at      — unix timestamp создания записи (метаданные)
        limit           — максимум запросов в окне
TTL:    нет (запись действует по факту наличия)
```

Пример:
```
HSET rate:limit:abc123 created_at 1717000000 limit 500
```

### redisDB2 (SELECT 2) — нарушители по api-key

```
Ключ:   rate:abuse:key:{api_key}
Тип:    Hash
Поля:   first_seen      — unix timestamp первого запроса (UnknownCounter.FirstRequest)
        last_seen       — unix timestamp последнего запроса (UnknownCounter.LastRequest)
        total_requests  — суммарно запросов (UnknownCounter.Total)
        burst_hits      — суммарно запросов через burst (UnknownCounter.BurstHits)
        abuse_hits      — окон с превышением порога (UnknownCounter.AbuseHits)
TTL:    --abuse-ttl минут, обновляется при каждом upsert из cleanup
```

### redisDB3 (SELECT 3) — нарушители по IP

```
Ключ:   rate:abuse:ip:{ip_address}
Тип:    Hash
Поля:   first_seen      — unix timestamp первого запроса
        last_seen       — unix timestamp последнего запроса
        total_requests  — суммарно запросов
        burst_hits      — суммарно запросов через burst (UnknownCounter.BurstHits)
        abuse_hits      — окон с превышением порога
TTL:    --abuse-ttl минут, обновляется при каждом upsert из cleanup
```

---

## 8. Параметры запуска

Формат флагов: `--flag value`. Библиотека: `github.com/spf13/pflag`.

| Флаг                   | Тип    | Default                    | Описание                                                        |
|------------------------|--------|----------------------------|-----------------------------------------------------------------|
| `--listen`             | string | `unix:/run/ratelimit.sock` | Адрес для auth_request: `unix:/path/sock` или `host:port`       |
| `--socket-mode`        | string | `0666`                     | Права на unix-сокет из `--listen` (octal). Для TCP игнорируется. |
| `--admin-listen`       | string | `127.0.0.1:8080`           | Адрес и порт веб-админки                                        |
| `--metrics-listen`     | string | `127.0.0.1:9091`           | Адрес и порт Prometheus-метрик (`/metrics`)                     |
| `--redis-addr`         | string | `127.0.0.1:6379`           | Адрес Redis                                                     |
| `--redis-password`     | string | `""`                       | Пароль Redis                                                    |
| `--log-level`          | string | `info`                     | Уровень логирования: `debug`, `info`, `warn`, `error`           |
| `--log-format`         | string | `json`                     | Формат логов: `json` (продакшен) или `text` (разработка)        |
| `--global-limit`       | int    | `100`                      | Глобальный лимит запросов в окне                                |
| `--burst`              | int    | `0`                        | Дополнительные запросы сверх лимита (burst)                     |
| `--window`             | string | `second`                   | Единица окна: `second` или `minute`                             |
| `--cleanup-interval`   | int    | `15`                       | Интервал цикла cleanup, в минутах                               |
| `--abuse-ttl`          | int    | `15`                       | TTL записей в redisDB2/redisDB3, в минутах                                |
| `--abuse-multiplier`   | int    | `10`                       | Множитель global-limit для счёта AbuseHits                      |
| `--abuse-transfer-threshold` | int    | `3`                        | Минимальный AbuseHits для переноса счётчика в redisDB2/redisDB3           |

### Валидация при запуске

Сервис проверяет корректность параметров до начала работы и завершается с ошибкой если:

```
--burst >= --global-limit * --abuse-multiplier
```

Это гарантирует что burst-зона не перекрывает порог abuse — иначе разрешённые burst-запросы автоматически триггерили бы AbuseHits.

Также проверяется что `--socket-mode` парсится как корректное octal-значение в диапазоне `0..0777`. Допустимы обе формы записи: с ведущим нулём (`0666`) и без (`666`) — парсер использует base 8.

### Примеры запуска

```bash
# Unix socket (рекомендуется для single-instance)
api-ratelimiter \
  --listen unix:/run/ratelimit.sock \
  --socket-mode 0666 \
  --admin-listen 127.0.0.1:8080 \
  --metrics-listen 127.0.0.1:9091 \
  --redis-addr 127.0.0.1:6379 \
  --log-level info \
  --log-format json \
  --global-limit 100 \
  --burst 20 \
  --window second \
  --cleanup-interval 15 \
  --abuse-ttl 15 \
  --abuse-multiplier 10 \
  --abuse-transfer-threshold 3

# TCP (для горизонтального масштабирования)
api-ratelimiter \
  --listen 127.0.0.1:9090 \
  --admin-listen 127.0.0.1:8080 \
  --metrics-listen 127.0.0.1:9091 \
  --redis-addr 127.0.0.1:6379 \
  --log-level info \
  --log-format json \
  --global-limit 100 \
  --burst 20 \
  --window second
```

---

## 9. HTTP API сервиса

### `GET /check` — основной endpoint (вызывается nginx/Angie через auth_request)

**Источники api_key / token.** Сервис извлекает ключ из трёх мест в
порядке приоритета (первое непустое выигрывает):

1. Заголовок `X-Api-Key` — nginx/Angie ставит его явно через `proxy_set_header`.
2. Query-параметры `?api_key=...` или `?token=...` на самом `/check` —
   удобно когда nginx/Angie может извлечь значение и положить в URL через
   `proxy_pass http://api-ratelimiter/check?api_key=$client_key`.
3. **Заголовок `X-Original-URI`** — nginx/Angie передаёт сюда `$request_uri`
   родительского запроса. Сервис парсит URI и достаёт `api_key` / `token`
   из его query-строки. Это **рекомендуемый способ**: `$request_uri` —
   одна из немногих встроенных переменных, которая сохраняется в
   auth_request-субзапросе (большинство `$arg_*` и `$args` в нём
   обнулены — известный nginx#761, унаследовано в Angie). Map-извлечение
   `$arg_api_key` в субзапросе на ряде сборок nginx/Angie возвращает пустоту,
   поэтому полагаться на него ненадёжно.

В каждом из вариантов: `api_key` приоритетнее `token`. Если ни один
источник не дал значения — ключом для лимитирования становится
`X-Real-IP` (rate-limit по IP).

**Входные заголовки:**
```
X-Api-Key:       {value}   — опц., прямой источник ключа (приоритет 1)
X-Original-URI:  {uri}     — опц., полный URI родителя; парсится на сервере (приоритет 3)
X-Real-IP:       {ip}      — IP-адрес клиента (IPv4 или IPv6), fallback при отсутствии ключа.
                              Значение валидируется через net.ParseIP; невалидные
                              заголовки трактуются как отсутствующий IP (defense in
                              depth на случай ошибки в конфиге nginx/Angie, где вместо
                              $remote_addr пробросили бы клиентский заголовок).
```

**Ответы:**

| Код | Условие |
|-----|---------|
| `200 OK` | Запрос разрешён |
| `403 Forbidden` | Лимит превышен (не `429` — `auth_request` пропускает в parent только 2xx / 401 / 403, прочие коды конвертит в `500`) |

Тело ответа всегда пустое. Кастомизацию ответа клиенту делает nginx/Angie через `error_page`.

**Гарантия fail open:** handler `/check` оборачивается в `defer/recover`. При любой внутренней панике или ошибке сервис возвращает `200 OK` — nginx/Angie всегда получает валидный ответ, никогда 5xx. Это исключает необходимость обработки ошибок auth_request на стороне nginx/Angie.

### `GET /healthz` и `GET /readyz` — проверки для оркестратора

Поднимаются на admin-порту (`--admin-listen`). Семантика:

| Endpoint | 200 OK | 503 Service Unavailable |
|----------|--------|--------------------------|
| `/healthz` (**liveness**) | Процесс жив | (никогда — пока процесс работает) |
| `/readyz` (**readiness**) | Готов обслуживать на полной функциональности | shutdown в процессе **или** Redis недоступен (`PING` с таймаутом 200ms) |

Хотя сервис fail-open и продолжает обвечать на `/check` при недоступности Redis (с глобальным лимитом для всех ключей), `/readyz` намеренно отдаёт 503 в этом случае — это сигнал оркестратору, что инстанс деградирован и трафик лучше увести на здоровый. Если все инстансы деградированы — fail-open всё равно сработает на уровне `/check`.

---

## 10. Метрики — единый источник данных

Все метрики хранятся **только** в Prometheus registry внутри Go-процесса. Веб-админка и `/metrics` endpoint читают из одного источника.

```
Prometheus registry (в памяти процесса)
    │
    ├─► --metrics-listen/metrics  — Prometheus text format (для scrape)
    │
    └─► --admin-listen/           — те же данные, рендер в HTML
```

### Список метрик

**Счётчики запросов** (Counter — монотонно растут с момента старта):
```
ratelimit_requests_total{result="allowed"}
ratelimit_requests_total{result="blocked_individual"}  # по лимиту из redisDB1
ratelimit_requests_total{result="blocked_global"}      # по глобальному лимиту
```

Общее число блокировок = `blocked_individual + blocked_global`. Отдельной
метки `blocked` нет — суммируйте подметки в PromQL:
`sum(rate(ratelimit_requests_total{result=~"blocked_.*"}[5m]))`.

**Состояние памяти** (Gauge — текущее значение):
```
ratelimit_counters_known_active   # активных счётчиков в KnownCounters сейчас
ratelimit_counters_unknown_active # активных счётчиков в UnknownCounters сейчас
ratelimit_memory_bytes            # объём памяти под оба счётчика
```

**Cleanup** (Counter + Gauge):
```
ratelimit_cleanup_runs_total           # сколько раз отработал cleanup
ratelimit_cleanup_deleted_total        # счётчиков удалено за всё время
ratelimit_cleanup_transferred_total    # записей перенесено в redisDB2/redisDB3 за всё время
ratelimit_cleanup_last_duration_seconds # длительность последнего cleanup
```

**Redis** (Gauge + Counter):
```
ratelimit_redis_errors_total   # ошибок соединения (Counter)
ratelimit_redis_db1_keys       # записей в redisDB1 (Gauge)
ratelimit_redis_db2_keys       # записей в redisDB2 (Gauge)
ratelimit_redis_db3_keys       # записей в redisDB3 (Gauge)
```

**Latency** (Histogram):
```
ratelimit_check_duration_seconds
```

---

## 11. Веб-админка

Доступна по `--admin-listen`. Авторизации нет на уровне сервиса —
предполагается проксирование через nginx/Angie с IP-allowlist'ом и/или
обфусцированным URL. Помимо просмотра, поддерживает деструктивные
действия над содержимым Redis (см. ниже).

### `/` — Статус и метрики

**Параметры запуска:** таблица всех флагов и текущих значений.

**Состояние сервиса:** Uptime, версия, статус Redis.

**Метрики** (из Prometheus registry, те же данные что на `/metrics`):

| Метрика | Тип | Описание |
|---------|-----|----------|
| `requests_total{allowed}` | Counter | Пропущено с момента старта |
| `requests_total{blocked_individual}` | Counter | По индивидуальному лимиту |
| `requests_total{blocked_global}` | Counter | По глобальному лимиту |
| `counters_known_active` | Gauge | Счётчиков в KnownCounters сейчас |
| `counters_unknown_active` | Gauge | Счётчиков в UnknownCounters сейчас |
| `memory_bytes` | Gauge | Память под счётчики сейчас |
| `cleanup_runs_total` | Counter | Запусков cleanup |
| `cleanup_deleted_total` | Counter | Удалено счётчиков за всё время |
| `cleanup_transferred_total` | Counter | Перенесено в redisDB2/redisDB3 за всё время |
| `cleanup_last_duration_seconds` | Gauge | Длительность последнего cleanup |
| `redis_errors_total` | Counter | Ошибок Redis |
| `redis_db1_keys` | Gauge | Записей в redisDB1 |
| `redis_db2_keys` | Gauge | Записей в redisDB2 |
| `redis_db3_keys` | Gauge | Записей в redisDB3 |
| `check_p50_ms` | Histogram | Медиана latency /check |
| `check_p95_ms` | Histogram | 95-й перцентиль |
| `check_p99_ms` | Histogram | 99-й перцентиль |

Counter-значения накапливаются с момента старта. Для анализа динамики — Prometheus + `rate()`/`increase()`.

### `/limits` — redisDB1

Страница состоит из двух разделов:

**Top-25 KnownCounters (in-memory)** — наверху. Колонки: `api_key` |
`Total` | `BurstHits` | `ViolationHits`. Сортировка по умолчанию — по
`ViolationHits`. Заголовки `Total` и `BurstHits` кликабельны и
переключают сортировку через query-параметр `?topsort=total|burst|violations`.
Активная колонка помечена «↓». При переключении сортировки `q` и `page`
основной таблицы сохраняются. Данные живые, при рестарте обнуляются.

**redisDB1 entries** — основная таблица. Колонки: `api_key` |
`лимит (req/окно)` | `создан` | `всего запросов` | `окон с нарушением` |
`burst запросов`. Колонки `всего запросов`, `окон с нарушением`,
`burst запросов` берутся из `KnownCounters` (in-memory). Если счётчик
неактивен или ещё не создавался — прочерк. Пагинация 25/стр, поиск по
api_key.

### `/abuse/keys` — redisDB2

**Top-25 UnknownCounters (in-memory, prefix `key:`)** — наверху.
Колонки: `api_key` | `Total` | `BurstHits` | `AbuseHits`. Сортировка по
умолчанию — `AbuseHits`. `?topsort=total|burst|abuse` для переключения.

**redisDB2 entries** — основная таблица. Колонки: `api_key` |
`первый запрос` | `последний запрос` | `всего запросов` | `burst запросов` |
`окон с превышением` | `TTL`. Пагинация, поиск.

### `/abuse/ips` — redisDB3

**Top-25 UnknownCounters (in-memory, prefix `ip:`)** — наверху.
Колонки: `IP` | `Total` | `BurstHits` | `AbuseHits`. Та же логика
сортировки, что и на `/abuse/keys`.

**redisDB3 entries** — основная таблица. Колонки: `IP` | `первый запрос` |
`последний запрос` | `всего запросов` | `burst запросов` |
`окон с превышением` | `TTL`. Пагинация, поиск.

### Действия Delete и Purge

На страницах `/limits`, `/abuse/keys`, `/abuse/ips` есть две кнопки —
**Delete** и **Purge** — справа от строки поиска. Каждая запись имеет
чекбокс в первой колонке.

**Delete selected** — удаляет только отмеченные чекбоксами записи.
Подтверждения не требует (скоупом ограничено выделением).

- Routes: `POST /limits/delete`, `POST /abuse/keys/delete`, `POST /abuse/ips/delete`.
- Body: `keys=k1&keys=k2&...` (одно значение на отмеченный чекбокс).
- Бэкенд выполняет `DEL key1 key2 ...` в соответствующей БД.
- **На клиенте кнопка заблокирована (`disabled`) пока не отмечен ни один чекбокс**, разблокируется при первом отмеченном и снова блокируется когда выделение снимают. Реализуется минимальным inline JS (см. ниже).

**Purge all** — полностью очищает соответствующую базу через `FLUSHDB`.

- Routes: `POST /limits/purge`, `POST /abuse/keys/purge`, `POST /abuse/ips/purge`.
- **На клиенте при нажатии открывается inline-баннер с двумя кнопками — `Yes, purge` и `No`.** `Yes` отправляет тот же POST с `confirm=yes` в теле, `No` скрывает баннер. Inline JS, без модалок и внешних библиотек.
- Без JS (degradation): первый POST возвращает страницу подтверждения с эквивалентными кнопками; второй POST с `confirm=yes` выполняет операцию. Серверная защита от случайного нажатия сохраняется в обоих режимах.

После любого действия — 303 на исходную страницу. Все операции
логируются на info-уровне (`admin delete` / `admin purge` с лейблом БД и
числом обработанных записей) — для аудита через journald.

### CSRF-защита

Каждый рендер страницы с формой включает скрытое поле
`<input type="hidden" name="csrf_token" value="...">`. Токен — 32 байта
из `crypto/rand`, формируется один раз на старте процесса и живёт до
рестарта. Все state-changing POST-ы (`*/delete`, `*/purge`) валидируют
поле через `subtle.ConstantTimeCompare` — несовпадение даёт `403`
и warn-лог с путём и `RemoteAddr`. Защищает от cross-origin POST-ов из
браузера легитимного админа на сторонний сайт: атакующий не может
прочитать тело страницы под cross-origin и не узнаёт токен.

### Ограничение размера тела запроса

Сервис **не** ограничивает размер тела POST-а самостоятельно — это
сделано на уровне nginx/Angie через `client_max_body_size` в admin-
location'е (см. `configs/nginx.example.conf`). Дефолт в примере конфига —
`256k`, что с запасом покрывает Delete/Purge-формы с CSRF-токеном и
десятками тысяч чекбоксов. Если админка проксируется иначе — лимит
обязательно настраивается там.

### Стек шаблонов и JS

Интерфейс: чистый HTML, `html/template` из stdlib, минимальный inline CSS.
JavaScript — только короткий inline-скрипт для UX-улучшений Delete/Purge
(`partials.html`, define `actions_script`). Внешних JS-зависимостей,
build pipeline'а или фреймворков нет. Скрипт подключается через
`{{template "actions_script" .}}` только на трёх страницах со списками,
страница `/` идёт без JS. При отключённом JavaScript админка остаётся
функциональной — Delete и Purge работают через server-side fallback.

---

## 12. Конфигурация nginx/Angie

Готовый файл — [`configs/nginx.example.conf`](../configs/nginx.example.conf). Ниже приведён
тот же конфиг по разделам с пояснениями.

### Передача api_key / token в api-ratelimiter

В auth_request-субзапросе `$args` и `$arg_*` обнулены (известный
nginx#761, унаследовано в Angie), поэтому map по `$arg_api_key` и
прямая подстановка `$arg_api_key` в `proxy_set_header` ненадёжны — на
ряде сборок nginx/Angie возвращают пустую строку. `$request_uri`, наоборот,
сохраняется в субзапросе.

Поэтому nginx/Angie не извлекает `api_key`/`token` руками, а просто передаёт
весь `$request_uri` родителя в заголовке `X-Original-URI`. Парсинг
строки и приоритет `api_key` > `token` — на стороне api-ratelimiter (см.
раздел 9).

### Отключение rate limiting (планово или per-request)

Поддерживаются два сценария «трафик идёт мимо лимитера»:

1. **Сервис остановлен / упал** (`systemctl stop api-ratelimiter`, рестарт, краш).
   В `upstream api-ratelimiter` рядом с реальным бэкендом прописан `backup`-сервер
   — это in-nginx-затычка, всегда отвечающая `200` на `/check`. При
   `max_fails=1 fail_timeout=2s` nginx за один неудачный коннект помечает
   primary как failed и шлёт subrequest'ы на backup. По возвращению сервиса
   primary восстанавливается автоматически. Reload nginx **не нужен**.

2. **Per-request exemption** (внутренний мониторинг, allow-list ключей/IP,
   служебный трафик). Через `map $ratelimit_upstream` оператор маршрутизирует
   выбранные запросы на отдельный upstream `api-ratelimiter_bypass`, который
   состоит **только** из затычки — лимитер вообще не дёргается. Дефолт — все
   запросы идут на `api-ratelimiter` (с обычным failover'ом).

Затычка — отдельный `server { listen unix:... return 200; }` в том же nginx,
не отдельный процесс.

### Server block

```nginx
# Variable in proxy_pass below requires a resolver declaration. Lookups
# always hit upstream{} blocks; DNS никогда не выполняется.
resolver 127.0.0.1 valid=300s ipv6=off;

upstream php_api {
    server 127.0.0.1:9000;  # уточнить при деплое
    keepalive 32;
}

# Real + backup stub. На стоп systemd-юнита nginx за один failed-connect
# переключается на backup.
upstream api-ratelimiter {
    server unix:/run/api-ratelimiter/ratelimit.sock max_fails=1 fail_timeout=2s;
    server unix:/run/api-ratelimiter-stub.sock backup;
    keepalive 32;
}

# Только затычка. Выбирается через $ratelimit_upstream для exempt-трафика.
upstream api-ratelimiter_bypass {
    server unix:/run/api-ratelimiter-stub.sock;
    keepalive 8;
}

# In-nginx stub.
server {
    listen unix:/run/api-ratelimiter-stub.sock;
    access_log /var/log/nginx/ratelimit-bypass.log;
    location / { return 200; }
}

# Switch — default api-ratelimiter, "api-ratelimiter_bypass" для exempt-условий.
# Шаблон — оператор подставляет реальные условия:
#   map $arg_api_key $ratelimit_upstream {
#       default            api-ratelimiter;
#       "internal-monitor" api-ratelimiter_bypass;
#   }
map $request_uri $ratelimit_upstream {
    default api-ratelimiter;
}

server {
    listen 80;
    server_name api.example.com;

    location = /_ratelimit {
        internal;
        proxy_pass                http://$ratelimit_upstream/check;
        proxy_next_upstream       error timeout;
        proxy_next_upstream_tries 2;
        proxy_pass_request_body   off;
        proxy_set_header          Content-Length    "";
        proxy_set_header          X-Original-URI    $request_uri;
        proxy_set_header          X-Real-IP         $remote_addr;
        proxy_connect_timeout     100ms;
        proxy_read_timeout        200ms;
    }

    # ── Лимитируемые эндпойнты ─────────────────────────────────────────

    location ~ ^/(control|premium)/get-number {
        auth_request  /_ratelimit;
        error_page 403 = @ratelimit_our;
        proxy_pass              http://php_api;
        proxy_pass_request_headers on;
    }

    location = /smshub/get-number {
        auth_request  /_ratelimit;
        error_page 403 = @ratelimit_smshub;
        proxy_pass              http://php_api;
        proxy_pass_request_headers on;
    }

    location = /stubs/handler_api.php {
        auth_request  /_ratelimit;
        error_page 403 = @ratelimit_stubs;
        proxy_pass              http://php_api;
        proxy_pass_request_headers on;
    }

    # ── Дефолтный location — без лимита ────────────────────────────────
    location / {
        proxy_pass              http://php_api;
        proxy_pass_request_headers on;
    }

    # ── Кастомные ответы при блокировке (403 → 200 с телом по протоколу)
    location @ratelimit_our {
        default_type application/json;
        return 200 '{"error_code":"no_numbers","error_id":0,"error_msg":"No numbers, try again..."}';
    }

    location @ratelimit_smshub {
        default_type application/json;
        return 200 '{"status":"NO NUMBERS"}';
    }

    location @ratelimit_stubs {
        default_type text/plain;
        return 200 'NO NUMBERS';
    }
}
```

---

## 13. Деплой

Поставка — `.deb`-пакет под `linux/amd64`, собираемый в CI (см. раздел 18).
Пакет кладёт бинарь в `/usr/bin/api-ratelimiter`, unit — в
`/lib/systemd/system/api-ratelimiter.service`. Сервис запускается под
`www-data` (пользователь существует на любой машине с PHP/nginx;
postinstall-скрипт создаёт его, если не нашёл, через `adduser --system`).
`www-data` при удалении пакета не удаляется, т.к. шарится с другими сервисами.

Альтернатива для ручной установки — `make install` (бинарь попадает
в `/usr/local/bin/api-ratelimiter`, unit нужно положить отдельно).

### Systemd unit

```ini
[Unit]
Description=api-ratelimiter
After=network.target redis-server.service

[Service]
Type=simple
User=www-data
Group=www-data
ExecStart=/usr/bin/api-ratelimiter \
    --listen unix:/run/api-ratelimiter/ratelimit.sock \
    --socket-mode 0666 \
    --admin-listen 127.0.0.1:8080 \
    --metrics-listen 127.0.0.1:9091 \
    --redis-addr 127.0.0.1:6379 \
    --log-level info \
    --log-format json \
    --global-limit 100 \
    --burst 20 \
    --window second \
    --cleanup-interval 15 \
    --abuse-ttl 15 \
    --abuse-multiplier 10 \
    --abuse-transfer-threshold 3
RuntimeDirectory=api-ratelimiter
RuntimeDirectoryMode=0755
Restart=always
RestartSec=3
TimeoutStopSec=30

NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true

[Install]
WantedBy=multi-user.target
```

Сокет лежит в `/run/api-ratelimiter/ratelimit.sock` (внутри `RuntimeDirectory`),
поэтому пути в `configs/nginx.example.conf` и в unit совпадают.

### Graceful shutdown

При получении `SIGTERM` или `SIGINT` сервис выполняет завершение в следующем порядке:

```
SIGTERM/SIGINT
    │
    ├─ 1. Прекратить приём новых соединений на /check
    │      В период drain'а новые запросы от nginx/Angie получат connection refused.
    │      Это неизбежно при рестарте — кратковременные 502 от nginx/Angie возможны.
    │
    ├─ 2. Дождаться завершения текущих in-flight запросов
    │      (таймаут: 10 секунд)
    │
    ├─ 3. Выполнить финальный cleanup:
    │      - перенести UnknownCounters с AbuseHits >= threshold в redisDB2/redisDB3
    │      - логировать итоги (info)
    │
    └─ 4. Закрыть соединения с Redis, завершить процесс
```

`TimeoutStopSec=30` в systemd даёт сервису 30 секунд на graceful shutdown перед SIGKILL. Финальный cleanup при типичном объёме данных занимает менее секунды.

---

## 14. Стек технологий

| Компонент | Выбор | Обоснование |
|-----------|-------|-------------|
| Язык | Go **1.21+** | `log/slog` доступен с 1.21; низкий overhead, нативная конкурентность |
| Redis | **6.0+** | Стабильный HSET multi-field, ACL, улучшенный RESP3 |
| Флаги | `github.com/spf13/pflag` | Поддержка формата `--flag` |
| Redis-клиент | `github.com/redis/go-redis/v9` | Зрелая библиотека, connection pool, multi-db, auto-reconnect |
| Метрики | `github.com/prometheus/client_golang` | Единый источник для /metrics и веб-админки |
| Логирование | `log/slog` (stdlib) | Structured logging, JSON/text, уровни — без внешних зависимостей |
| HTTP-сервер | `net/http` stdlib | Достаточно для /check, веб-админки и /metrics |
| Счётчики | in-memory map + `sync.RWMutex` | Нет сетевого overhead |
| Шаблоны | `html/template` stdlib | Простой HTML без зависимостей |

Внешних зависимостей: 3 — `pflag`, `go-redis`, `prometheus/client_golang`. `log/slog` входит в stdlib.

---

## 15. Логирование

Библиотека: `log/slog` (stdlib, Go 1.21+). Вывод в **stdout** — systemd/journald забирает и хранит.

### Уровни и события

| Уровень | Событие |
|---------|---------|
| `info` | Старт сервиса с текущим конфигом (без паролей); штатное завершение; итоги каждого cleanup (удалено N счётчиков, перенесено M записей, длительность); успешный reconnect к Redis |
| `warn` | Переход Redis в недоступное состояние (один warn на цикл «здоров→болен», далее тихо); ключ исчез из redisDB1 при cleanup (api_key, был в KnownCounters); невалидное значение поля в Redis-hash (`limit`, `total_requests`, ...) |
| `info` | Переход Redis обратно в доступное состояние (`redis recovered` с указанием операции, на которой это заметили) |
| `error` | Паника в горутине cleanup |
| `debug` | Каждое решение по запросу (allowed/blocked, api_key/ip, лимит, WindowCount) — очень verbose, только для отладки |

Индивидуальные заблокированные запросы **не логируются** на уровне info/warn — при 5k RPS это сделает лог нечитаемым. Статистика блокировок — через метрики.

**Шум при недоступном Redis.** Состояние «здоров/болен» отслеживается централизованно в `store.Store` через `atomic.Bool` + `observe()`. Переход логируется один раз — все промежуточные ошибки (на горячем пути `/check`, в cleanup, в админке) увеличивают `ratelimit_redis_errors_total` без дополнительных лог-записей. Когда Redis возвращается — лог `redis recovered` (info). Сделано чтобы при многочасовой недоступности Redis лог не превращался в гигабайт одинаковых warn'ов.

### Формат JSON (продакшен)

```json
{"time":"2024-05-01T12:00:00Z","level":"INFO","msg":"cleanup finished",
 "deleted":142,"transferred":3,"duration_ms":12}
```

### Формат text (разработка)

```
2024-05-01 12:00:00 INFO cleanup finished deleted=142 transferred=3 duration_ms=12
```

---

## 16. Структура Go-проекта

```
api-ratelimiter/
├── cmd/
│   └── api-ratelimiter/
│       └── main.go              # точка входа: парсинг флагов, инициализация, запуск
├── internal/
│   ├── config/
│   │   ├── config.go            # структура Config, валидация параметров
│   │   └── config_test.go
│   ├── counter/
│   │   ├── known.go             # KnownCounter, KnownCounters map + методы
│   │   ├── unknown.go           # UnknownCounter, UnknownCounters map + методы
│   │   └── counter_test.go
│   ├── limiter/
│   │   ├── limiter.go           # логика решения: пускать/блокировать
│   │   └── limiter_test.go
│   ├── handler/
│   │   ├── check.go             # HTTP handler GET /check
│   │   └── handler_test.go
│   ├── cleanup/
│   │   ├── cleanup.go           # цикл cleanup: чистка + перенос в Redis
│   │   └── cleanup_test.go
│   ├── store/
│   │   └── redis.go             # клиент Redis: redisDB1/2/3, reconnect
│   ├── admin/
│   │   ├── admin.go             # handlers веб-админки
│   │   └── templates/
│   │       ├── index.html       # статус и метрики
│   │       ├── limits.html      # redisDB1
│   │       ├── abuse_keys.html  # redisDB2
│   │       └── abuse_ips.html   # redisDB3
│   └── metrics/
│       └── metrics.go           # определение и регистрация Prometheus-метрик
├── configs/
│   ├── nginx.example.conf       # образец конфига nginx/Angie
│   └── api-ratelimiter.service      # systemd unit (используется и в .deb)
├── packaging/
│   ├── nfpm.yaml                # описание .deb пакета (nfpm)
│   └── scripts/
│       ├── postinstall.sh       # создание www-data, daemon-reload, enable
│       ├── preremove.sh         # stop + disable
│       └── postremove.sh        # daemon-reload (www-data НЕ удаляется)
├── docs/
│   └── specification.md         # это ТЗ
├── .github/workflows/
│   ├── ci.yml                   # lint+test+build на push/PR
│   └── release.yml              # сборка и публикация по тегу v*
├── Makefile
├── go.mod
└── go.sum
```

**Правила:**
- Весь код кроме `main.go` — в `internal/` (недоступен для импорта извне)
- Каждый пакет тестируется своим `*_test.go` файлом
- `main.go` — только склейка: создаёт зависимости, передаёт в компоненты, запускает серверы и ждёт сигнала завершения

---



## 17. Unit-тесты

Тесты обязательны. Покрывают критическую бизнес-логику — всё что связано с подсчётом, лимитами и cleanup. Используется стандартный `testing` пакет Go.

### Обязательное покрытие

**Счётчики и rate limiting (`counter_test.go`):**
- Инкремент `WindowCount` и `Total` на каждый запрос
- Смена слота: сброс `WindowCount`, сохранение `Total`
- Разрешение запроса при `WindowCount <= limit`
- Блокировка при `WindowCount > limit`
- Разрешение запроса в burst-зоне (`limit < WindowCount <= limit+burst`), инкремент `BurstHits`
- Блокировка при `WindowCount > limit+burst`
- Инкремент `ViolationHits` (KnownCounter) при смене слота если `WindowCount > Лимит из redisDB1`
- Инкремент `AbuseHits` (UnknownCounter) при смене слота если `WindowCount > global_limit * abuse_multiplier`
- Корректность `FirstRequest` / `LastRequest`

**Маршрутизация в счётчики (`limiter_test.go`):**
- api_key из redisDB1 → попадает в `KnownCounters`, применяется лимит из redisDB1
- api_key не из redisDB1 → попадает в `UnknownCounters`, применяется `--global-limit`
- запрос без api_key → попадает в `UnknownCounters` по IP, применяется `--global-limit`
- при недоступности Redis → api_key попадает в `UnknownCounters` с `--global-limit`

**Цикл cleanup (`cleanup_test.go`):**
- Неактивный счётчик (KnownCounters) удаляется из памяти
- Неактивный счётчик (UnknownCounters) с `AbuseHits < threshold` удаляется, в Redis не пишется
- Неактивный счётчик (UnknownCounters) с `AbuseHits >= threshold` переносится в Redis и удаляется из памяти
- Активный счётчик (UnknownCounters) с `AbuseHits >= threshold` переносится в Redis и остаётся в памяти
- Активный счётчик (KnownCounters) не переносится в Redis ни при каких условиях
- После upsert в Redis поля `first_seen`, `last_seen`, `total_requests`, `burst_hits`, `abuse_hits` соответствуют счётчику
- TTL устанавливается корректно

**Валидация параметров (`config_test.go`):**
- Старт завершается с ошибкой если `--burst >= --global-limit * --abuse-multiplier`
- Корректные параметры проходят валидацию

**Парсинг заголовков (`handler_test.go`):**
- Непустой `X-Api-Key` → используется как ключ
- Пустой `X-Api-Key` → rate limit по `X-Real-IP`
- Отсутствующий `X-Real-IP` при пустом ключе → запрос пропускается (fail open without key — лимитировать нечем)
- Валидация `X-Real-IP`: только IPv4/IPv6, остальное игнорируется

**Redis-операции (`store/redis_test.go`):**
- `LookupLimit` / `LimitExists` — найден / не найден / ошибка парсинга
- `UpsertAbuseKey` / `UpsertAbuseIP` — поля и TTL записаны корректно
- `ScanLimits` / `ScanAbuseKeys` / `ScanAbuseIPs` — обход через `SCAN`
- `DeleteLimits` / `DeleteAbuseKeys` / `DeleteAbuseIPs` — удаление с правильным префиксом, idempotent для отсутствующих ключей
- `PurgeLimits` / `PurgeAbuseKeys` / `PurgeAbuseIPs` — `FLUSHDB` соответствующей БД
- `DBSize` — корректный счётчик по трём БД
- `Ping` — успех при живом Redis, ошибка при недоступности

Используется `github.com/alicebob/miniredis/v2` — pure-Go in-process Redis, без Docker.

**Веб-админка (`admin/admin_test.go`):**
- `/`, `/limits`, `/abuse/keys`, `/abuse/ips` рендерятся в 200 с ожидаемым содержимым
- `/` рендерится и при недоступном Redis (баннер «redis err», но 200)
- CSRF: невалидный/отсутствующий токен на `*/delete` и `*/purge` → 403
- CSRF: валидный токен → действие выполняется
- `/limits/delete`: GET → 405, пустой выбор → 303 без операций
- `/limits/purge`: первый POST с валидным токеном → страница подтверждения с прокинутым токеном; второй POST с `confirm=yes` → `FLUSHDB`
- Top-25 счётчиков отрисовываются из in-memory (`KnownMap` / `UnknownMap`)
- Фильтрация `key:` / `ip:` префиксов в top-таблицах
- Несуществующий путь → 404

### Запуск тестов

```bash
make test          # все тесты
make test-verbose  # с выводом
make test-cover    # с покрытием (генерирует coverage.html)
```

---

## 18. Сборка и публикация

### Makefile

```makefile
BINARY   = api-ratelimiter
VERSION  = $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS  = -ldflags "-X main.Version=$(VERSION) -s -w"

.PHONY: build run test test-verbose test-cover clean install lint

build:
	go build $(LDFLAGS) -o $(BINARY) ./cmd/api-ratelimiter

run: build
	./$(BINARY) \
	  --listen unix:/tmp/ratelimit.sock \
	  --admin-listen 127.0.0.1:8080 \
	  --metrics-listen 127.0.0.1:9091 \
	  --redis-addr 127.0.0.1:6379 \
	  --log-level debug \
	  --log-format text \
	  --global-limit 100 \
	  --burst 20 \
	  --window second

test:
	go test ./...

test-verbose:
	go test -v ./...

test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY) coverage.out coverage.html

install: build
	install -m 755 $(BINARY) /usr/local/bin/$(BINARY)
```

Версия бинаря берётся из git tag — доступна в веб-админке и в логах. При сборке вне git-репозитория подставляется `dev`.

### CI / CD

CI/CD на GitHub Actions.

**Платформа:** только `linux/amd64`. Multi-arch и Docker-образ — не входят
в текущую поставку.

**Триггеры:**

| Событие              | Что делается                                              |
|----------------------|-----------------------------------------------------------|
| push в любую ветку, PR | `golangci-lint` → `go vet` → `go test -race` с покрытием → `go build` + smoke-тест |
| push git-тега `v*`   | то же + сборка артефактов и публикация GitHub Release     |

**Артефакты публикации (по тегу `v*`):**

| Артефакт                                                 | Где лежит после публикации |
|----------------------------------------------------------|----------------------------|
| `api-ratelimiter-vX.Y.Z-linux-amd64.tar.gz` (+ `.sha256`) | GitHub Releases            |
| `api-ratelimiter_X.Y.Z_amd64.deb`           (+ `.sha256`) | GitHub Releases            |

`.deb` собирается через `nfpm` (`packaging/nfpm.yaml`), включает unit и
post-/pre-install скрипты из `packaging/scripts/` (см. раздел 13).

**Файлы пайплайнов:**

```
.github/workflows/ci.yml         # lint+vet+test(+coverage)+build на push/PR
.github/workflows/release.yml    # lint+test → bin + .deb → GitHub Release
```

Аутентификация GitHub Releases — через автоматически предоставляемый
`GITHUB_TOKEN` (release-job декларирует `permissions: contents: write`),
дополнительной настройки секретов не требуется.

---

## 19. Ограничения и допущения

- При недоступности Redis: fail open (все запросы пропускаются, api_key уходит в UnknownCounters с --global-limit). Reconnect выполняется автоматически через connection pool `go-redis` — при восстановлении Redis сервис возобновляет нормальную работу без перезапуска. Успешный reconnect логируется на уровне `info`.
- При рестарте: in-memory счётчики и Prometheus Counter'ы сбрасываются; данные в Redis сохраняются.
- Запись в redisDB2/redisDB3 происходит **только в цикле cleanup**. Интервал попадания = `--cleanup-interval`. Осознанный компромисс: в базу попадают только систематические нарушители.
- Горизонтальное масштабирование: счётчики независимы на каждом инстансе; переход на Redis-счётчики — возможное улучшение v2. Под Redis-счётчики зарезервирована база `0` (SELECT 0). При переходе на Redis-счётчики целесообразно добавить in-memory TTL-кэш для redisDB1 (30 сек) чтобы исключить lookup на каждый запрос.
- Веб-админка поддерживает Delete (выборочное удаление записей) и Purge (FLUSHDB соответствующей базы) для DB1/DB2/DB3. Создание и редактирование записей в DB1 (`HSET rate:limit:...`) остаётся за основной админкой сайта.
- Rate limiting применяется только к явно перечисленным location'ам. Дефолтный `location /` пропускает всё без ограничений.
- `/stubs/handler_api.php` лимитируется целиком (все action). Фильтрация по `$arg_action` — при необходимости доработка на уровне nginx/Angie.
- Рост памяти между cleanup-циклами: все уникальные api_key и IP накапливаются в памяти в течение `--cleanup-interval`. При стабильном трафике с ограниченным числом уникальных ключей это несущественно. При аномальном трафике с большим числом уникальных IP — память может расти быстро до следующего cleanup.
- Counter-метрики в веб-админке — накопленные с момента старта. Для динамики — Prometheus + `rate()`/`increase()`.

---

## 20. Планы развития

Запланированные улучшения, не реализованные в текущей версии. Размещены здесь
вместе, чтобы было удобно планировать релизы.

### 20.1. Расчёт percentile в веб-админке (`check_p50_ms`, `check_p95_ms`, `check_p99_ms`)

**Что:** в админке на `/` сейчас для histogram'а `ratelimit_check_duration_seconds`
выводятся только `count` и `sum`. Раздел 11 требует отдельные строки `check_p50_ms`,
`check_p95_ms`, `check_p99_ms`. Их нужно вычислять прямо в Go-процессе из данных
histogram'а на момент рендера страницы.

**Зачем:** админка должна давать оперативный взгляд на latency без необходимости
ходить в Prometheus. Текущие `count`/`sum` дают только среднее, что бесполезно
для анализа хвостов.

**Где правка:** `internal/admin/admin.go`, функция `formatMetricValue` для веток
с `m.Histogram` и/или новая функция, которая возвращает три отдельные строки
для каждого histogram'а.

**Алгоритм** (тот же что в PromQL `histogram_quantile`):

```
1. Прочитать buckets:  []Bucket{UpperBound, CumulativeCount}
2. total  = последний CumulativeCount  (= SampleCount)
3. rank   = total * X / 100            (для p_X)
4. Найти i: первый bucket где CumulativeCount[i] >= rank
5. Линейная интерполяция внутри bucket'а:
     prev_bound = UpperBound[i-1]   (или 0 для i=0)
     prev_count = CumulativeCount[i-1]   (или 0 для i=0)
     result = prev_bound + (UpperBound[i] - prev_bound) *
              (rank - prev_count) / (CumulativeCount[i] - prev_count)
6. Перевести из секунд в миллисекунды для отображения.
```

**Краевые случаи:**
- `total == 0` (нет наблюдений) → отображать прочерк, не делить на ноль.
- Попадание в последний `+Inf` bucket → отдавать `UpperBound` предпоследнего
  bucket'а с пометкой «≥», т.к. внутри `+Inf` интерполировать нечем.
- bucket с нулевым диапазоном (`CumulativeCount[i] == prev_count`) → пропустить.

**Точность.** У нас `prometheus.ExponentialBuckets(0.0001, 2, 14)` —
14 buckets от 100µs до ~1.6s. Для ожидаемого диапазона `/check` (десятки µs —
единицы ms) разрешения хватает: на p99 интерполяция внутри bucket'а даёт
ошибку не больше ширины bucket'а (≤ ×2). Для админки этого достаточно;
точные перцентили — через Prometheus и PromQL `histogram_quantile`.

**Объём работ:** ~25–30 строк кода в `internal/admin/admin.go` плюс правка
`templates/index.html` для показа отдельных строк. Тесты — табличные
проверки на синтетическом histogram'е с известным распределением.

### 20.2. Уже упомянутые в других разделах

- **In-memory TTL-кэш для redisDB1** (раздел 19): сейчас lookup на каждый
  запрос. Нужен 30-секундный кэш чтобы убрать Redis с горячего пути и
  снизить latency. Особенно актуально при переходе на Redis-счётчики.
- **Sharded mutex** (раздел 6): один `sync.RWMutex` на map'у переходит на
  256 шардов по hash ключа. Делается при росте contention'а, без изменения
  внешней логики.
- **Redis-счётчики для горизонтального масштабирования** (раздел 19): база
  `0` уже зарезервирована. Переход — отдельный большой эпик.

### 20.3. Pipeline для `scanAbuse` (N+1 → N)

**Что:** `internal/store/redis.go`, функция `scanAbuse` для каждого ключа,
найденного через `SCAN`, делает два round-trip'а: `HGetAll` + `TTL`. При
10k abuse-записей это 20k команд на одно открытие `/abuse/keys` или
`/abuse/ips`.

**Как:** обернуть в `pipe := c.Pipeline(); pipe.HGetAll(...); pipe.TTL(...);
pipe.Exec(...)` — go-redis отправит обе команды одним I/O. Лучше — собирать
батч из N (e.g. 100) ключей за один SCAN-инкремент и пайплайнить весь батч.

**Объём работ:** ~30 строк в `scanAbuse`. Тесты — расширить
существующие `TestScanAbuseKeys`/`TestScanAbuseIPs` синтетикой на 1000+
ключей и убедиться что результат идентичен.

### 20.4. Серверная пагинация для админских scan-страниц

**Что:** сейчас `ScanLimits` / `ScanAbuseKeys` / `ScanAbuseIPs` всегда
обходят всю БД через SCAN и возвращают весь массив в Go, фильтрация по `q`
и пагинация — в памяти. При больших объёмах Redis это секунды CPU/память
на каждое открытие страницы оператором.

**Как:**
- Использовать `SCAN cursor` со страничным offset через cookie/query
  (cursor устойчив между запросами в рамках одной Redis-сессии — придётся
  его пробрасывать через URL).
- Альтернатива: кэшировать полный список в Go с TTL (e.g. 30 сек), отдавать
  пагинацию из кэша. Проще, но stale data на короткое окно.

**Объём работ:** средний — затрагивает три ручки админки и шаблоны.

### 20.5. Чанкование `DEL` в админских действиях

**Что:** `delByPrefix` шлёт один `DEL key1 key2 ... keyN`. Redis обрабатывает
`DEL` синхронно и O(N); если оператор поставит чекбоксы на 10k ключей —
Redis залочится на несколько сотен мс, влияя на `/check`. Форма Delete
не ограничивает выделение.

**Как:** разбивать `ids` на чанки по 100 ключей, выполнять последовательно.
Опционально показывать прогресс на клиенте.

**Объём работ:** ~10 строк в `internal/store/redis.go::delByPrefix` + опц.
UI-индикатор.

### 20.6. In-memory TTL-кэш для `LookupLimit` (детализация §20.2)

**Что:** `LookupLimit` сейчас идёт в Redis на каждый `/check`. При 5k RPS
это 5k HGET/сек по redisDB1. Лимиты в DB1 меняются редко (создание/правка
оператором), поэтому кэш с TTL 30 сек практически идеален.

**Структура:** `sync.Map[apiKey]struct{limit int64; foundAt time.Time}`.
На miss — Redis lookup, заполнение кэша. На hit с `time.Since(foundAt) <
30s` — отдать из кэша.

**Инвалидация:** не нужна, TTL разрешает stale 30 сек. При admin-delete
ключа лимит исчезнет из кэша максимум через 30 сек.

**Краевой случай:** «not found» тоже кэшируется (negative cache) — иначе
несуществующий api_key, который часто стучит, заваливает Redis запросами.

**Объём работ:** ~50 строк в `internal/store/redis.go` (новый тип
`cachedStore` или поля в `Store`) + тесты на TTL.

---
