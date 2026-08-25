# NanoASR

Оффлайн-сервер распознавания речи на [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx):
Go, CPU, длинные файлы, **word-level тайминги**, OpenAI-совместимый API и встроенный
тестовый интерфейс.

> **Статус: v1 собрана, вехи M0–M5 закрыты.** Работает всё: файл → декод →
> 16 кГц → VAD → распознавание → пословные тайминги → постобработка →
> диаризация → оба диалекта API, плюс очередь с SSE, каталог моделей с
> горячей заменой и встроенный UI. Источник правды — [docs/SPEC.md](docs/SPEC.md),
> включая открытые вопросы (§19).

## Что это

| | |
|---|---|
| Профиль нагрузки | файлы 1–10 минут (лимит 30), телефонийные 8 кГц, моно и стерео |
| Результат | текст, сегменты, **пословные тайминги**, зоны тишины, спикеры, пунктуация |
| Инференс | CPU, оффлайн, без стриминга |
| API | OpenAI (`/v1/audio/transcriptions`) + нативный (`/api/v1`), новый диалект — один файл |
| Модели | реестр с автозагрузкой, LRU-резидентность, hot swap без разрыва запросов |
| Постобработка | пунктуация, ITN(ru), hotwords — всё опционально, по умолчанию выключено |
| UI | SPA внутри бинаря, отключается конфигом или `-tags noui` |

## Быстрый старт

```bash
make web build                                  # собрать SPA и сервер
./dist/nanoasr models catalog                   # что доступно
./dist/nanoasr models pull gigaam-v3-ctc-punct-ru silero-vad-v5
./dist/nanoasr serve -config configs/nanoasr.dev.yaml
```

Или одной командой: `make models` — скачает весь каталог (~1 ГБ).

Модель по умолчанию — `gigaam-v3-ctc-punct-ru`: она сама расставляет знаки
препинания и заглавные буквы. Отдельной модели пунктуации для русского не
существует, и это не обход, а лучший доступный ответ (SPEC §5.6).

В боевом конфиге нужен ключ: `auth.mode: apikey` без ключей — это ошибка старта,
а не сервер, отвечающий 401 на всё.

```bash
curl -s localhost:8080/v1/audio/transcriptions \
  -H 'Authorization: Bearer sk-...' \
  -F file=@call.wav -F response_format=verbose_json | jq '.text, .words[:3]'
```

### Добавить свою модель в каталог

```bash
nanoasr models inspect ./my-model --probe sample.wav
```

Определит семейство и словарь, вытащит метаданные из `.onnx` и напечатает
черновик манифеста. Флаг `--probe` распознаёт клип с каждым кандидатом
`features.dim` в отдельном процессе — потому что у некоторых моделей неверное
значение не портит текст, а **аварийно завершает процесс**.

Dev-конфиг слушает `127.0.0.1:8080` без аутентификации — открытый режим разрешён
**только** на loopback, на любом другом адресе сервер откажется стартовать.

### Что включается по запросу

Постобработка и диаризация по умолчанию выключены (SPEC решение №14) и
включаются параметрами запроса:

```bash
curl -s localhost:8080/api/v1/transcribe \
  -F file=@call.wav -F diarize=true -F itn=true \
  | jq '.speakers, .segments[0].speaker'

# Два канала телефонии распознаются раздельно и мержатся по времени.
curl -s localhost:8080/api/v1/transcribe \
  -F file=@call.wav -F channel_mode=split | jq '[.segments[].channel] | unique'
```

Диаризации нужны две модели на сервере (`diarization.enabled` плюс
`segmentation_model` и `embedding_model`); если их нет, ответ придёт с
варнингом, называющим недостающую настройку, а не молча без спикеров.

Через Docker:

```bash
docker compose -f deploy/docker-compose.yml up --build
```

## Сборка и поставка

Важная деталь: `sherpa-onnx-go` поставляется **только динамическими** библиотеками
(`libonnxruntime.so` 26 МБ + `libsherpa-onnx-c-api.so` 5 МБ), и cgo зашивает в бинарь
RUNPATH, указывающий в кэш Go-модулей. «Скопировать один бинарь» не сработает.

Поэтому два артефакта:

- **Docker-образ** — библиотеки и ffmpeg внутри образа (основной путь);
- **tar.gz** — `make dist` собирает с `CGO_LDFLAGS="-Wl,-rpath,\$ORIGIN/lib"` и кладёт
  `.so` рядом с бинарём. Проверено: `$ORIGIN/lib` резолвится первым, архив переносим,
  ~33 МБ.

## Развёртывание на Linux

Пошагово, для Ubuntu Server 22.04/24.04 и любого systemd-дистрибутива. Всё, кроме
весов моделей, лежит в одном архиве.

### 1. Собрать архив

```bash
make web dist          # nanoasr-<version>-linux-amd64.tar.gz, ~21 МБ
```

Собирать нужно на машине с Go 1.24+ и gcc — на целевом сервере компилятор не нужен.
Архив переносим: `RUNPATH` бинаря начинается с `$ORIGIN/lib`, поэтому библиотеки
берутся из самого архива, а не с машины сборки.

### 2. Разложить

```bash
sudo useradd -r -s /usr/sbin/nologin nanoasr
sudo mkdir -p /opt/nanoasr /var/lib/nanoasr
sudo tar -xzf nanoasr-*-linux-amd64.tar.gz -C /opt/nanoasr
sudo chown -R nanoasr:nanoasr /opt/nanoasr /var/lib/nanoasr
```

`/var/lib/nanoasr` — единственный каталог, в который сервису разрешено писать
(юнит ставит `ProtectSystem=strict`): туда лягут веса моделей, база задач и спул.

### 3. Поставить ffmpeg — опционально, но обычно нужен

```bash
sudo apt install -y ffmpeg
```

Без него работают WAV/PCM, включая телефонийные a-law и µ-law; mp3, opus, m4a и
прочее ответят `415`. В архив ffmpeg не входит намеренно — это внешняя зависимость
с собственным циклом обновлений.

### 4. Задать ключ

`/opt/nanoasr/nanoasr.yaml` приезжает с `auth.mode: apikey` и пустым списком ключей.
**Сервер откажется стартовать, пока ключа нет** — это не баг: сервер, отвечающий 401
на всё, хуже, чем сервер, который честно не поднялся.

```bash
KEY="sk-$(head -c 24 /dev/urandom | base64 | tr -d '/+=')"
echo "$KEY"                        # запишите, второй раз он не покажется
printf '%s' "$KEY" | sha256sum     # хеш для конфига
```

В `auth.keys` кладите **хеш**, а не сам ключ — тогда секрет не лежит в конфиге:

```yaml
auth:
  mode: apikey
  keys:
    - name: app
      key: "sha256:<хеш из команды выше>"
      rps: 10          # 0 — без ограничения
```

Ключ должен быть не короче 16 символов. Открытый режим (`auth.mode: open`)
разрешён **только** на loopback: на любом другом адресе сервер откажется стартовать.

### 5. Скачать модели

Веса в архив не входят (~1 ГБ на весь каталог) и качаются на сервере:

```bash
sudo -u nanoasr /opt/nanoasr/nanoasr models pull \
  -config /opt/nanoasr/nanoasr.yaml \
  gigaam-v3-ctc-punct-ru silero-vad-v5
```

Минимум — модель распознавания и VAD. Пропишите её в конфиге, иначе каждый запрос
должен будет называть модель сам:

```yaml
asr:
  default_model: gigaam-v3-ctc-punct-ru
```

Для диаризации нужны ещё две модели и включённый блок:

```bash
sudo -u nanoasr /opt/nanoasr/nanoasr models pull \
  -config /opt/nanoasr/nanoasr.yaml \
  pyannote-segmentation-3 campplus-sv-zh-en
```

```yaml
diarization:
  enabled: true
  segmentation_model: pyannote-segmentation-3
  embedding_model: campplus-sv-zh-en
```

### 6. Запустить как сервис

```bash
sudo cp /opt/nanoasr/nanoasr.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now nanoasr
sudo systemctl status nanoasr
```

### 7. Проверить

```bash
curl -s localhost:8080/healthz                       # без ключа
curl -s localhost:8080/readyz                        # + глубина очереди
curl -s localhost:8080/api/v1/models -H "Authorization: Bearer $KEY"
curl -s localhost:8080/v1/audio/transcriptions -H "Authorization: Bearer $KEY" \
  -F file=@call.wav -F response_format=verbose_json
```

Готовность проверяйте по `/readyz`, а не по факту запуска процесса: старт занимает
секунды на загрузку весов, и `/readyz` отдаёт `503 saturated` при полной очереди —
именно тогда балансировщику и следует перестать сюда слать.

UI — на `http://<адрес>:8080/ui/`. Ключ он спросит сам, поймав первый `401`.

### Обновление

```bash
sudo systemctl stop nanoasr
sudo tar -xzf nanoasr-<new>-linux-amd64.tar.gz -C /opt/nanoasr
sudo chown -R nanoasr:nanoasr /opt/nanoasr
sudo systemctl start nanoasr
```

Конфиг архив не перезаписывает вслепую — файл называется `nanoasr.yaml` и приезжает
как есть, так что свой держите в стороне или сравнивайте перед распаковкой. Веса в
`/var/lib/nanoasr` переживают обновление.

### Если не поднимается

Сервер объясняет отказ и выходит, а не стартует в нерабочем виде — так что первым
делом читайте причину:

```bash
sudo journalctl -u nanoasr -n 50 --no-pager
```

| Сообщение | Что делать |
|---|---|
| `auth.mode=apikey but no keys are configured` | шаг 4 |
| `key must be at least 16 characters` | ключ короче 16 символов |
| `auth.mode=open requires a loopback listen address` | либо `addr: "127.0.0.1:8080"`, либо `mode: apikey` |
| `model ... is not present in ...` | шаг 5 |
| `error while loading shared libraries` | `lib/` не рядом с бинарём — распакуйте архив целиком |
| `diarization.enabled is true but ... is empty` | назовите обе модели диаризации |

### Слушать не только localhost

`addr: ":8080"` в конфиге слушает все интерфейсы. Перед тем как открывать наружу:
ключ обязателен (шаг 4), а `jobs.webhook_allow_private` должен остаться `false` —
иначе `webhook_url` с приватным адресом превращает сервер в SSRF-прокси внутрь вашей
сети. TLS у сервера своего нет: ставьте его за nginx/Caddy или за туннелем.

## Требования

- Go 1.24+, gcc/g++ (cgo обязателен — sherpa-onnx это C++)
- Node 22+ для сборки SPA
- ffmpeg — **опционально**: без него работают WAV/PCM (включая a-law/µ-law),
  остальные форматы отвечают `415`

## Структура

```
cmd/nanoasr        точка входа: serve | models | version
internal/core      доменные типы и контракт сервиса — без HTTP
internal/audio     сниффинг, WAV/PCM, ffmpeg, ресемплинг
internal/vad       нарезка речи и зоны тишины
internal/asr       граница с sherpa-onnx + реестр семейств моделей
internal/words     токены → слова с таймингами
internal/postproc  пунктуация, ITN за реестром локалей, спаны склеек
internal/diarize   pyannote + эмбеддинги, привязка слов, резка сегментов
internal/pool      LRU, refcount, hot swap, варианты, CPU-губернатор
internal/pipeline  сквозной конвейер, реализует core.Service
internal/registry  манифесты, каталог, скачивание с проверкой sha256
internal/job       очередь, воркеры, SSE
internal/api       adapter + диалекты openai и native
internal/ui        embed.FS со сборкой SPA
web/               Vite + React + TanStack Router
docs/SPEC.md       спецификация
```

## Разработка

```bash
make lint             # go vet + gofmt + eslint + tsc
make test-race        # пул, очередь и губернатор корректны только под -race
make models testdata  # веса (~1 ГБ) и аудио для сквозных тестов
make test-integration # реальное распознавание + отчёты M1 и M5
make load             # 100 параллельных файлов, 30 минут, против живого сервера
cd web && npm run dev
```

Фронтенд намеренно жёсткий: маршруты не верстают себя сами, отступы приходят только
из примитивов `Page`/`Section`/`Stack`, тосты — только через `@/lib/toast`, анимации
живут в одном файле. Всё это — правила ESLint уровня `error`, а не договорённости.

## Лицензия

MIT — см. [LICENSE](LICENSE) и [NOTICE](NOTICE). Веса моделей в репозиторий не входят
и имеют собственные лицензии, включая non-commercial.
