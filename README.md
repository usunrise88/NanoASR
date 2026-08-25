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
