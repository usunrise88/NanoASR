# NanoASR

Оффлайн-сервер распознавания речи на [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx):
Go, CPU, длинные файлы, **word-level тайминги**, OpenAI-совместимый API и встроенный
тестовый интерфейс.

> **Статус: M1 закрыта.** Сквозной путь работает: файл → декод → 16 кГц → VAD →
> распознавание → пословные тайминги → `/v1/audio/transcriptions`. Очередь задач,
> реестр со скачиванием, UI и постобработка — вехи M2–M5, каждая заглушка помечена
> `TODO(Mn)`. Источник правды — [docs/SPEC.md](docs/SPEC.md), включая открытые
> вопросы (§19).

## Что это

| | |
|---|---|
| Профиль нагрузки | файлы 1–10 минут (лимит 30), телефонийные 8 кГц, моно |
| Результат | текст, сегменты, **пословные тайминги**, зоны тишины, опционально спикеры |
| Инференс | CPU, оффлайн, без стриминга |
| API | OpenAI (`/v1/audio/transcriptions`) + нативный (`/api/v1`), новый диалект — один файл |
| Модели | реестр с автозагрузкой, LRU-резидентность, hot swap без разрыва запросов |
| UI | SPA внутри бинаря, отключается конфигом или `-tags noui` |

## Быстрый старт

```bash
make models               # GigaAM v2 CTC (ru) + Silero VAD в ./.models
make web build            # собрать SPA и сервер
./dist/nanoasr models list -config configs/nanoasr.dev.yaml
./dist/nanoasr serve -config configs/nanoasr.dev.yaml
```

Распознать файл:

```bash
curl -s localhost:8080/v1/audio/transcriptions \
  -F file=@call.wav -F response_format=verbose_json \
  | jq '.text, .timestamp_source, .words[:3]'
```

Dev-конфиг слушает `127.0.0.1:8080` без аутентификации — открытый режим разрешён
**только** на loopback, на любом другом адресе сервер откажется стартовать.

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
internal/pool      LRU, refcount, hot swap, CPU-губернатор
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
make models testdata  # веса и аудио для сквозных тестов
make test-integration # реальное распознавание + отчёт по RTF и таймингам
cd web && npm run dev
```

Фронтенд намеренно жёсткий: маршруты не верстают себя сами, отступы приходят только
из примитивов `Page`/`Section`/`Stack`, тосты — только через `@/lib/toast`, анимации
живут в одном файле. Всё это — правила ESLint уровня `error`, а не договорённости.

## Лицензия

MIT — см. [LICENSE](LICENSE) и [NOTICE](NOTICE). Веса моделей в репозиторий не входят
и имеют собственные лицензии, включая non-commercial.
