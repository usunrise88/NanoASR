# NanoASR — спецификация на разработку

**Статус:** черновик v0.1 — согласованы ключевые решения, часть вопросов открыта (раздел 19).
**Дата:** 2026-08-21
**Репозиторий:** `github.com/usunrise88/nanoasr` · лицензия MIT

---

## 1. Что это

Production-level HTTP-сервер офлайн-распознавания речи (ASR / speech-to-text) на базе
[sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx), написанный на Go, для CPU-инференса.

**Профиль основной нагрузки:**

| Параметр | Значение |
|---|---|
| Длительность файла | 1–10 минут (лимит 30 минут) |
| Качество | телефонийное, 8 кГц, узкая полоса |
| Каналы | моно, один говорящий (диаризация — опция) |
| Результат | текст + **word-level тайминги** |
| Железо | CPU, без GPU (провайдер `cuda` доступен в конфиге, но не поддерживается) |
| Стриминг | **не поддерживается**, только офлайн-инференс целого файла |

**Три обязательных свойства:**

1. **Много моделей** — реестр моделей, скачивание по требованию, hot swap без рестарта,
   выбор модели на каждый запрос.
2. **Лёгкий запуск** — один Docker-образ или один tar.gz; модели подтягиваются сами.
3. **OpenAI-совместимость** + дешёвое добавление собственных диалектов API.

### 1.1 Не-цели (v1)

- Стриминговое (real-time) распознавание — архитектурно исключено.
- GPU-инференс.
- Обучение и дообучение моделей.
- Горизонтальное масштабирование с общим состоянием (см. 19.3).
- Перевод (`/v1/audio/translations`) — эндпоинт есть, отвечает `501`.

---

## 2. Зафиксированные решения

Решения приняты по итогам согласования требований. Изменение любого из них — изменение
объёма работ; строка «последствие» показывает цену пересмотра.

| # | Решение | Последствие |
|---|---|---|
| 1 | **Sync + async job API** | синхронный OpenAI-путь и нативная очередь работают поверх одного ядра |
| 2 | **Гибридный декодер:** Go для WAV/PCM, ffmpeg для остального | ffmpeg — опциональная внешняя зависимость, без неё работают WAV/PCM |
| 3 | **Поставка: Docker (основное) + tar.gz с libs** | CI без C++-тулчейна; проверено на стенде (см. 3) |
| 4 | **Word-timestamps только у моделей, которые их отдают** | в манифесте модели `capabilities.word_timestamps`, в ответе `timestamp_source` |
| 5 | **SQLite (pure-Go) + файлы на диске** | одноузловая инсталляция; интерфейсы `JobStore`/`BlobStore` оставляют путь к Postgres |
| 6 | **Auth: API-ключи (Bearer), open-режим только на 127.0.0.1** | мультитенантности нет, но у ключа есть scope |
| 7 | **Реестр: вшитый каталог + скачивание с HF/GitHub releases** | нужен исходящий HTTPS или своё зеркало |
| 8 | **Семейства моделей v1: transducer + CTC** | zipformer/GigaAM-RNNT, zipformer-ctc, nemo-ctc, wenet-ctc, telespeech |
| 9 | **Кастомный API — компиляционные адаптеры** | 1 пакет = 1 диалект, регистрация через `init()`, пересборка при добавлении |
| 10 | **Диаризация — полная, опцией запроса** | pyannote segmentation + speaker embeddings + clustering, второй проход |
| 11 | **LRU-кэш моделей + глобальный CPU-семафор** | предсказуемая память и отсутствие oversubscription потоков |
| 12 | **UI: прогон + плеер, менеджер моделей, история задач** | SPA вшита в бинарь, отключается конфигом и build-тегом |
| 13 | **Все лимиты настраиваемые, авто-дефолты от `nproc`/RAM** | целевое железо не зафиксировано, нужен встроенный расчёт дефолтов |
| 14 | **Постобработка (пунктуация, ITN, hotwords) — опциональна** | по умолчанию выключена, включается параметром запроса |
| 15 | **Ретеншн: ничего не храним, кроме активной задачи** | аудио на сервер не сохраняется вообще, см. 2.1 |
| 16 | **Наблюдаемость — не в v1** | см. 2.2 — риск зафиксирован |
| 17 | **Мультиязычность с самого начала; UI: ru + en** | каталог сразу многоязычный, язык — параметр запроса |
| 18 | **Лимиты: 30 мин / 100 МБ / очередь 100** | значения дефолтные, все в конфиге |

Решения 19–26 приняты в M3, когда очередь перестала быть интерфейсом и стала кодом.

| # | Решение | Последствие |
|---|---|---|
| 19 | **Очередная задача — это активная задача**: аудио живёт от `202` до терминального статуса | уточнение решения №15, а не отмена. Очередь бессмысленна, если принятую работу нельзя выполнить |
| 20 | **Второй лимит — `jobs.max_queued_bytes`** (4 ГиБ) поверх `queue_size` | `queue_size: 100` при лимите 100 МБ — это 10 ГБ диска. Превышение → `429 queue_full` + `Retry-After`; синхронный путь бюджет не тратит |
| 21 | **`queued` переживают рестарт, `running` → `failed` (`server_restart`)** | правка §9: восстановить можно только то, о чём известно, что оно не начиналось |
| 22 | **Ключ видит только свои задачи; чужой id → `404`, не `403`** | `403` подтверждает существование задачи, превращая перебор id в перепись чужой активности. Правило — в `Store.List`, а не в обработчике |
| 23 | **Курсорная пагинация по `(created_at, id)`** | `OFFSET` при вставках пропускает и дублирует строки |
| 24 | **UI читает SSE через `fetch`, не `EventSource`** | `EventSource` не умеет `Authorization`, а токен в query попал бы в `AccessLog`. Ограничение для M4 |
| 25 | **Вебхуки: только https, только публичные адреса, без редиректов, HMAC с меткой времени** | иначе `webhook_url` — это SSRF-прокси внутрь сети сервера, а подпись без метки времени воспроизводима вечно |
| 26 | **Rate limit — token bucket в памяти, на ключ** | один процесс, десяток ключей; распределённый лимитер здесь был бы инфраструктурой ради инфраструктуры |

Решения 27–33 приняты в M4, когда UI впервые прошёл путь целиком.

| # | Решение | Последствие |
|---|---|---|
| 27 | **Приоритет — свойство ключа** (`auth.keys[].priority`), не запроса | Параметр запроса позволил бы любому клиенту объявить себя срочным, что равно отсутствию приоритетов. `PriorityInteractive` из M3 был недостижим: `Source=ui` не выставлял ни один путь |
| 28 | **SPA узнаёт про аутентификацию по `401`**, ключ — в `localStorage` | Флаг в конфиге пришлось бы отдавать отдельной публичной ручкой, и он мог бы разойтись с тем, что сервер на самом деле требует. `ui.require_auth` удалён |
| 29 | **UI всегда шлёт `POST /jobs`, никогда `/transcribe`** | Синхронный путь не даёт ни стадий, ни прогресса, ни отмены |
| 30 | **После перезагрузки `/result/$jobId` — транскрипт без плеера + дропзона** | Аудио не попадает на сервер (решение №15), значит перезагрузка его теряет. Страница говорит об этом и принимает файл заново |
| 31 | **Декодирование для волны — на главном потоке при 8 кГц, RMS — в Worker** | Планировалось наоборот, но `OfflineAudioContext` — интерфейс окна и в Worker'е не существует. Частота остаётся сутью: 10 минут 48 кГц стерео — это ~230 МБ Float32 во вкладке, а огибающая из 2000 столбцов разницы не видит |
| 32 | **SRT/VTT/TXT скачиваются с сервера** | `internal/api/subtitle` вынесен затем, чтобы тайм-код существовал в одном месте; вторая реализация на TypeScript — второе место, где его чинить |
| 33 | **`wordAt` имеет допуск 10 мс** | Перемотка на начало слова попадает на сетку сэмплов аудиоэлемента — на 48 наносекунд раньше начала. Точное сравнение считало это паузой перед словом и не подсвечивало ничего: ровно то взаимодействие, ради которого экран существует |

### 2.1 Разрешение конфликта: «ничего не храним» vs «плеер и история»

Требования «плеер с волной и кликабельным транскриптом» + «история задач» формально
противоречат решению №15. Разрешается так:

- **Аудио на сервер не кладётся ни в каком виде.** Загруженный файл живёт как временный
  файл только на время обработки задачи и удаляется в `defer`.
- **Плеер в UI играет локальный `File` из браузера** через `URL.createObjectURL()` —
  файл уже есть у клиента, повторно качать с сервера незачем.
- **Пики волны считаются на клиенте** (`AudioContext.decodeAudioData` + RMS-даунсемпл в
  Web Worker). Сервер отдаёт только **зоны тишины** — они приходят из VAD в составе ответа.
- **История задач — только метаданные и транскрипт** (модель, длительность, параметры,
  тайминги, текст). Переслушивание из истории невозможно; чтобы прослушать снова, нужно
  снова приложить файл. В UI это отражено явной подписью на карточке истории.
- Хранить аудио дольше задачи **нельзя**: настройки для этого нет. `internal/spool`
  удаляет файл при переходе в терминальный статус, и уборка при старте — тоже.
  Если её включить, появляется `GET /api/v1/jobs/{id}/audio` и переслушивание из истории.

### 2.2 Зафиксированный риск: наблюдаемости нет в v1

Решение №16 исключает Prometheus, OpenTelemetry, bench/eval-команды и pprof из v1.
Это осознанный выбор заказчика, работа ведётся по нему. Фиксирую цену:

- Деградацию (рост глубины очереди, падение RTF, вытеснение моделей из кэша, рост
  времени ожидания слота CPU) будет видно только по внешним симптомам — таймауты у клиента.
- Подбор `inference_slots` / `num_threads` / размера батча без замеров превращается в
  угадывание; на неизвестном железе (решение №13) это основной источник недонастройки.

**Что делается вместо этого, чтобы включение стоило часы, а не переписывание:**
интерфейс `core.Observer` с no-op реализацией вызывается во всех точках конвейера;
`GET /healthz` и `GET /readyz` есть всегда (нужны Docker healthcheck и k8s probes);
логирование — стандартный `log/slog` с уровнями. Prometheus/OTel/bench — вехи M6.

---

## 3. Проверенные факты (не предположения)

Проверено на стенде `linux/amd64`, Go 1.24.7, gcc 13.3:

```
sherpa-onnx-go v1.13.6  →  sherpa-onnx 1.13.6, onnxruntime 1.27.1  — собирается и работает
```

| Факт | Значение | Почему важно |
|---|---|---|
| Офлайн-семейств моделей в Go API | 18 | `transducer, paraformer, nemo_ctc, zipformer_ctc, wenet_ctc, whisper, sense_voice, moonshine, dolphin, canary, fire_red_asr(+ctc), funasr_nano, qwen3_asr, cohere_transcribe, omnilingual, med_asr, tdnn, telespeech` |
| `OfflineRecognizerResult` | `Text, Tokens[], Timestamps[], Durations[], YsLogProbs[], Lang, Emotion, Event` | word-тайминги и confidence строятся отсюда |
| Батч-декодирование | `DecodeStreams([]*OfflineStream)` | ключ к пропускной способности на длинных файлах |
| Смена параметров без перезагрузки | `OfflineRecognizer.SetConfig()` | смена `decoding_method`/hotwords без выгрузки весов |
| VAD | `SileroVadModelConfig` (+ TEN VAD), `MaxSpeechDuration` | нарезка длинного файла |
| Диаризация | `OfflineSpeakerDiarization.Process() → []{Start, End, Speaker}` | доступна прямо из Go |
| Speaker ID | `SpeakerEmbeddingManager` (Register/Search/Verify) | задел под идентификацию говорящих |
| Пунктуация | `OfflinePunctuation` (CT-Transformer) | постобработка без сторонних сервисов |
| Денойзер | `OfflineSpeechDenoiser` (GTCRN, DpdfNet) | опция для шумной телефонии |
| Ресемплинг | **внутри** `OfflineStream.AcceptWaveform(sampleRate, samples)` | 8 кГц → 16 кГц делает сам sherpa-onnx (Kaldi LinearResample) |
| Чтение файлов | только `ReadWave` / `ReadWaveMultiChannel` | mp3/opus/m4a обязаны декодироваться снаружи |
| Отдельного ресемплера в Go API **нет** | — | VAD и диаризация требуют 16 кГц на входе → ресемплим сами до конвейера |
| Линковка | только `.so`: `libonnxruntime.so` (26 МБ) + `libsherpa-onnx-c-api.so` (5 МБ) | «просто скопировать бинарь» не работает |
| RUNPATH по умолчанию | `/root/go/pkg/mod/.../lib/x86_64-unknown-linux-gnu` | абсолютный путь в кэш модулей — непригоден для поставки |

**Проверка релокации (это и есть механика поставки tar.gz):**

```bash
CGO_LDFLAGS="-Wl,-rpath,\$ORIGIN/lib" go build -o dist/nanoasr ./cmd/nanoasr
cp $(go env GOMODCACHE)/github.com/k2-fsa/sherpa-onnx-go-linux@v1.13.6/lib/x86_64-unknown-linux-gnu/*.so dist/lib/
# RUNPATH: [$ORIGIN/lib:/root/go/pkg/mod/...]   ← $ORIGIN/lib резолвится первым
# запуск при «спрятанном» modcache: OK
# итоговый размер dist: 33 МБ
```

Вывод: `$ORIGIN/lib` попадает в RUNPATH **первым**, бинарь переносим. `patchelf` не нужен.

### 3.1 Следствие решения №8 для мультиязычности

Решения №8 (только transducer + CTC) и №17 (мультиязычность сразу) совместимы, но с оговоркой:
мультиязычность обеспечивается **набором моноязычных моделей в каталоге**, а не одной
мультиязычной моделью. SenseVoice/Whisper/Paraformer (широкое языковое покрытие одной моделью)
в v1 не входят. Языки v1 — те, для которых есть transducer/CTC-экспорты: ru, en, zh, de, fr, es,
ja, ko, а также многоязычные NeMo-CTC. Добавление семейства = один файл-маппер (см. 7.2), ~120 строк.

---

## 4. Архитектура

```
                      ┌──────────────────────── HTTP :8080 ────────────────────────┐
                      │  middleware: requestid → recover → log → auth → rate limit │
                      └───────┬───────────────────┬──────────────────┬─────────────┘
                              │                   │                  │
                 ┌────────────▼───────┐ ┌─────────▼────────┐ ┌───────▼────────┐
                 │ adapter: openai    │ │ adapter: native  │ │ ui (embed.FS)  │
                 │ /v1/audio/...      │ │ /api/v1/...      │ │ /ui/*          │
                 └────────────┬───────┘ └─────────┬────────┘ └────────────────┘
                              └─────────┬─────────┘
                                        ▼
                          ┌─────────────────────────────┐
                          │   core.Service              │  ← единственная точка правды
                          │   Transcribe / Submit / Get │     адаптеры HTTP не протекают внутрь
                          └──────────────┬──────────────┘
                                         ▼
                          ┌─────────────────────────────┐
                          │   job.Queue  (prio, cancel) │──► job.Store (SQLite)
                          └──────────────┬──────────────┘
                                         ▼
   ┌─────────────────────────────── pipeline (worker) ────────────────────────────────┐
   │                                                                                  │
   │  audio.Decode ──► vad.Segment ──► pool.Lease(model) ──► asr.Recognize (batch)     │
   │   wav|ffmpeg      silero          LRU + refcount        DecodeStreams             │
   │        │              │                  │                     │                 │
   │        │              ▼                  │                     ▼                 │
   │        │        silence regions          │              words.Assemble            │
   │        │                                 │              (tokens → words)          │
   │        │                                 │                     │                 │
   │        │                                 │                     ▼                 │
   │        │                                 │        postproc: punct → ITN           │
   │        │                                 │                     │                 │
   │        └──────► diarize.Process ─────────┴─────────────────────┤                 │
   │                 (опция, 2-й проход)                            ▼                 │
   │                                                        core.Result               │
   └──────────────────────────────────────────────────────────────────────────────────┘
                                         ▲
                          ┌──────────────┴──────────────┐
                          │  registry: catalog + download│──► models_dir (диск)
                          │  pool: LRU / refcount / swap │
                          │  governor: CPU semaphore     │
                          └─────────────────────────────┘
```

**Принципы:**

1. `core.Service` — единственный вход в бизнес-логику. Не знает про `http.Request`,
   multipart, SSE и JSON-теги. Всё HTTP-специфичное — в адаптерах.
2. Конвейер — линейный, без обратных связей; каждая стадия принимает и возвращает
   явные значения (никаких «обогащений» общей структуры на месте).
3. Всё, что может исчерпать ресурсы (память, CPU, диск, дескрипторы), проходит через
   явный ограничитель: `max_upload_bytes`, `queue_size`, `inference_slots`,
   `max_resident_models`, `max_model_rss_mb`.
4. Отмена — сквозная: `context.Context` от HTTP-соединения до `DecodeStreams`,
   между сегментами проверяется `ctx.Err()`.

---

## 5. Конвейер обработки

### 5.1 Приём файла

- `multipart/form-data`, поле `file`; поток пишется во временный файл
  (`os.CreateTemp`) с жёстким лимитом `max_upload_bytes` (100 МБ) через `io.LimitReader`.
  Превышение → `413`.
- Временный файл удаляется в `defer` всегда, включая панику и отмену.
- Сниффинг формата по magic bytes (`RIFF/WAVE`, `ID3`/`0xFFFB`, `OggS`, `fLaC`, `ftyp*`,
  `#!AMR`, `0x1A45DFA3`), **не по расширению и не по `Content-Type`**.

### 5.2 Декодирование → канонический PCM

Цель: `mono float32 @ 16000 Hz` в `[]float32`, значения в `[-1, 1]`.

| Вход | Путь | Обоснование |
|---|---|---|
| WAV/PCM: s16le, s24le, s32le, f32le, a-law, µ-law | нативный Go-декодер | телефонийный кейс — большинство файлов уже такие; ноль процессов |
| mp3, ogg/opus/vorbis, flac, m4a/aac, amr, gsm, webm, mp4 | `ffmpeg -i pipe:0 -f f32le -ac 1 -ar 16000 pipe:1` | покрывает всё остальное |

- **ffmpeg не обязателен**: если бинарь не найден при старте, сервер логирует
  предупреждение и отвечает `415 unsupported_media_type` на не-WAV входы.
- Аргументы ffmpeg **фиксированы в коде**; пользовательские данные идут только через stdin.
  Имя файла в аргументы не подставляется никогда.
- На ffmpeg вешается `context` с таймаутом `min(audio.ffmpeg_timeout, ctx deadline)`,
  `Cancel` + `WaitDelay` для гарантированного убийства процесса.
- Ресемплинг 8→16 кГц: делает ffmpeg (`soxr`) на своём пути; на нативном пути —
  собственный полифазный ресемплер (windowed-sinc, Kaiser). Линейной интерполяции быть
  не должно: она даёт алиасинг и роняет качество распознавания на узкополосном сигнале.
- Каналы (`audio.channel_mode`): `downmix` (по умолчанию), `first`, `split`
  (каждый канал — отдельный проход, результаты мержатся по времени, `channel` в словах).
  `split` — это дешёвая альтернатива диаризации для двухканальной телефонии (A/B leg).

**Память:** 30 минут × 16000 × 4 байта = **115 МБ** на задачу в пике.
Это учитывается в `jobs.max_concurrent` (см. 12.3).

### 5.3 VAD и зоны тишины

- Silero VAD (`sherpa_onnx.VoiceActivityDetector`), вход строго 16 кГц.
- Параметры: `threshold` 0.5, `min_silence_ms` 300, `min_speech_ms` 250,
  `max_speech_sec` 20 (важно: без верхней границы длинный монолог уедет в один
  гигантский сегмент и убьёт латентность и память).
- Выход: `[]SpeechSegment{StartSample, Samples}` → сегменты речи.
- **Зоны тишины** — дополнение сегментов речи до `[0, duration]`, отфильтрованное по
  `min_silence_ms`. Идут в ответ (`silence[]`) и рисуются в плеере.
- Если VAD выключен (`vad.enabled: false`) — один сегмент на весь файл; word-тайминги
  всё ещё работают, но качество на 10-минутном файле падает, а память растёт.

### 5.4 Распознавание

- Сегменты группируются в батчи: `min(batch.max_size, batch.max_seconds)`.
  Дефолт: `max_size = 8`, `max_seconds = 60`.
- На батч: `NewOfflineStream` ×N → `AcceptWaveform` → `DecodeStreams` → `GetResult`.
- Между батчами — проверка `ctx.Err()` (отмена клиента не должна доезжать до конца файла).
- Слоты CPU берутся на **батч**, а не на файл, чтобы длинная задача не держала слот целиком.

### 5.5 Сборка слов из токенов

Вход: `Tokens[] []string`, `Timestamps[] []float32` (секунды от начала сегмента),
`Durations[] []float32`, `YsLogProbs[] []float32`, `modeling_unit` из манифеста.

```
для каждого токена t[i]:
  начало нового слова, если:
    modeling_unit == "bpe"          и t[i] начинается с "▁"      (U+2581)
    modeling_unit == "cjkchar"      и всегда (каждый иероглиф — слово)
    modeling_unit == "cjkchar+bpe"  и (t[i] — CJK) или t[i] начинается с "▁"
    modeling_unit == "char"         и t[i] == " "
word.start = Timestamps[first]
word.end   = Durations[last] > 0 ? Timestamps[last] + Durations[last]
           : i+1 существует       ? Timestamps[i+1]
           :                        segment.end
word.text  = concat(tokens) с удалением "▁" и trim
word.conf  = exp(mean(YsLogProbs[first..last]))      # если YsLogProbs непусты
```

Затем: смещение на `segment.start`, клэмп в границы сегмента, отсечение слов нулевой
длины, слияние пунктуации с предыдущим словом.

**Инварианты (проверяются юнит-тестами):**
`0 ≤ w.start < w.end ≤ duration`, `w[i].end ≤ w[i+1].start`, `join(words) == text`
после нормализации пробелов.

**Точность.** Токенные таймстемпы у transducer-моделей привязаны к фрейму энкодера
(обычно 40 мс), у CTC — к шагу 40 мс. Реалистичная погрешность границы слова
**±40–80 мс**; на 8 кГц телефонии после апсемплинга может быть хуже. Это надо
измерить на ваших данных до того, как обещать точность наружу (см. 19.1).

### 5.6 Постобработка (всё опционально, по умолчанию выключено)

| Стадия | Механизм | Влияние на тайминги |
|---|---|---|
| **Hotwords** | `HotwordsFile` + `HotwordsScore` (transducer), `HomophoneReplacerConfig` | до декодирования, тайминги не трогает |
| **Пунктуация** | `OfflinePunctuation` (CT-Transformer), отдельная модель в реестре | знаки приклеиваются к предыдущему слову, границы слов не сдвигаются |
| **Заглавные буквы** | из той же модели пунктуации либо правилами | посимвольная замена, тайминги неизменны |
| **ITN** | собственный слой правил (Go), пословные спаны | **переписывает текст**: N слов → 1 токен; тайминги нового токена = `[start(first), end(last)]`, в `words[].original` сохраняется исходная форма |

**Ключевое требование ко всей постобработке:** ни одна стадия не имеет права
разорвать связь «слово ↔ интервал времени». Контракт стадии — `[]Word → []Word`,
где каждое выходное слово ссылается на непрерывный диапазон входных.
ITN для русского в sherpa-onnx (`RuleFsts`) фактически отсутствует, поэтому слой правил
пишется свой: числа, даты, время, деньги, телефоны, проценты. Объём — см. 18, веха M5.

### 5.7 Диаризация (опция запроса `diarize=true`)

- Второй проход по **полному** аудио: `OfflineSpeakerDiarization.Process()`
  (pyannote segmentation → speaker embeddings → fast clustering).
- Требует двух дополнительных моделей в реестре (segmentation + embedding extractor).
- Параметры: `num_clusters` (если известно число говорящих) **или** `threshold`
  (если неизвестно), `min_duration_on/off`.
- Привязка к словам: для каждого слова берётся спикер с максимальным перекрытием
  интервала `[w.start, w.end]` по сегментам диаризации; при нулевом перекрытии —
  спикер ближайшего по времени сегмента, поле `speaker_confidence` понижается.
- Стоимость: дополнительно ~0.3–0.5× RTF сверх распознавания. Диаризация **не**
  запускается, если `channel_mode: split` — там спикеры уже разделены каналами.

---

## 6. Модель данных результата

Канонический результат (нативный диалект, `application/json`):

```jsonc
{
  "id": "job_01J...",
  "model": "gigaam-v2-rnnt-ru@2",
  "language": "ru",
  "duration": 187.44,                  // секунды исходного аудио
  "text": "полный текст одной строкой",
  "timestamp_source": "token",         // token | segment | aligned
  "segments": [
    {
      "id": 0,
      "start": 1.28, "end": 6.94,
      "text": "здравствуйте это компания ромашка",
      "channel": 0,
      "speaker": "spk_0",              // null, если диаризация выключена
      "avg_confidence": 0.83,
      "words": [
        { "word": "здравствуйте", "start": 1.28, "end": 1.96, "confidence": 0.91 },
        { "word": "это",          "start": 2.02, "end": 2.14, "confidence": 0.88 }
      ]
    }
  ],
  "silence": [                          // зоны тишины для плеера
    { "start": 0.0,  "end": 1.28 },
    { "start": 6.94, "end": 8.10 }
  ],
  "speakers": [                         // только при diarize=true
    { "id": "spk_0", "total_speech": 92.1, "segments": 14 }
  ],
  "stats": {
    "audio_duration": 187.44,
    "processing_ms": 21840,
    "rtf": 0.117,
    "stages_ms": { "decode": 640, "vad": 1180, "asr": 18320, "post": 90, "diarize": 0 },
    "segments_total": 31,
    "speech_ratio": 0.72
  },
  "warnings": [
    { "code": "word_timestamps_unavailable", "message": "..." }
  ]
}
```

**Правила:**

- `timestamp_source`: `token` — тайминги из модели; `segment` — только границы VAD-сегментов
  (модель без word-таймингов); `aligned` — зарезервировано под forced alignment (M6).
- `confidence` — **некалиброванная** величина `exp(mean(logprob))`. Годится для сортировки
  и подсветки сомнительных мест, **не годится** как вероятность. Это написано в API-доках.
- `speaker` — `null`, а не пустая строка, если диаризация не запускалась.
- `warnings[]` — не ошибки: запрос выполнен, но что-то деградировало (не было ffmpeg
  и файл оказался WAV с редким кодеком, модель не умеет word-тайминги, VAD не нашёл речи).

---

## 7. Модельный слой

### 7.1 Манифест модели

Лежит рядом с весами (`{models_dir}/{id}/model.yaml`), либо приходит из каталога.

```yaml
id: gigaam-v2-rnnt-ru
revision: "2"                    # часть ключа кэша; меняется → hot swap
family: transducer               # transducer | zipformer_ctc | nemo_ctc | wenet_ctc | telespeech
display_name: "GigaAM v2 RNNT (русский)"
languages: [ru]
sample_rate: 16000
modeling_unit: bpe               # bpe | cjkchar | char | cjkchar+bpe
files:
  encoder: encoder.onnx
  decoder: decoder.onnx
  joiner:  joiner.onnx
  tokens:  tokens.txt
  bpe_vocab: bpe.vocab           # опционально
capabilities:
  word_timestamps: true
  confidence: true
  language_detect: false
  punctuation_builtin: false
runtime:
  num_threads: null              # null → auto (см. 12.2)
  decoding_method: greedy_search # greedy_search | modified_beam_search
  max_active_paths: 4
  blank_penalty: 0.0
resources:
  approx_rss_mb: 1200            # используется LRU-планировщиком до загрузки
source:
  url: "https://github.com/k2-fsa/sherpa-onnx/releases/download/asr-models/..."
  sha256: "…"
  size_bytes: 412335104
license: "apache-2.0"            # показывается в UI; бывают non-commercial
notes: "8 кГц телефония: лучше всего с beam=4"
```

### 7.2 Добавление нового семейства моделей

Один файл `internal/asr/families/{family}.go`:

```go
func init() { asr.RegisterFamily("nemo_ctc", nemoCTC{}) }

type nemoCTC struct{}

func (nemoCTC) Configure(m *registry.Manifest, dir string, cfg *sherpa.OfflineModelConfig) error
func (nemoCTC) Capabilities() asr.Capabilities
```

Больше нигде ничего менять не нужно: реестр, пул, конвейер и API работают через манифест.

### 7.3 Реестр и скачивание

- **Каталог** — `catalog.yaml`, вшит в бинарь (`embed.FS`), может быть переопределён
  `registry.catalog_url` (своё зеркало / закрытый контур). Каждая запись перед
  добавлением скачивается, прогоняется через `nanoasr models inspect --probe` и
  проверяется реальным распознаванием; ничего не выводится из имени модели.
- **`commercial_use`** заполняет тот, кто добавляет запись, **после чтения лицензии**,
  а не сопоставлением строк вроде «nc» или «research». Догадка здесь давала бы
  гарантию, которой у каталога нет. `registry.strict_license` пропускает только явное
  `yes`, поэтому честное `unknown` — это смысл поля, а не пробел в нём.
- Скачивание: HTTP с докачкой (`Range`), проверка `sha256`, распаковка
  (`.tar.bz2`/`.tar.gz`/`.zip`) во временный каталог, затем **атомарное** `os.Rename`
  в `{models_dir}/{id}@{revision}`. Частично скачанная модель никогда не видна.
- Защита распаковки: запрет `..` и абсолютных путей в записях архива, лимит на суммарный
  распакованный размер и число файлов (защита от zip-bomb), запрет симлинков.
- Прогресс скачивания — SSE-поток `GET /api/v1/models/{id}/download` (для UI).
- `registry.allow_download: false` → air-gapped режим, только локальные каталоги.

### 7.4 Пул моделей, LRU и hot swap

Ключ экземпляра: `{id}@{revision}`.

```
absent ──download──► downloaded ──load──► ready ──swap/evict──► draining ──refcount=0──► unloaded
```

- **Аренда (lease):** запрос берёт `pool.Acquire(ctx, id)` → `*Instance` с refcount++,
  освобождает через `Release()`. Модель с refcount > 0 **не выгружается никогда**.
- **Вытеснение:** при превышении `max_resident_models` **или** `max_model_rss_mb`
  выбирается наименее недавно использованный экземпляр с refcount == 0. `pinned: true` —
  не вытесняется. Если вытеснять нечего — новый запрос ждёт до `pool.acquire_timeout`,
  затем `503` с `Retry-After`.
- **TTL простоя:** `pool.idle_ttl` (дефолт 15 мин) — фоновая выгрузка неиспользуемых.
- **Hot swap:** `POST /api/v1/models/{id}/reload?revision=3` → грузится новый экземпляр
  **рядом** со старым → после успешного прогрева указатель в реестре атомарно
  переключается (`atomic.Pointer`) → старый переходит в `draining` → выгружается,
  когда его отпустит последний in-flight запрос. Ни один запрос не прерывается,
  и ни один запрос не видит полузагруженную модель.
- **Прогрев:** после загрузки прогоняется 1 с тишины — прогревает аллокаторы
  onnxruntime и убирает «первый запрос в 3 раза медленнее».
- **Диагностика:** `GET /api/v1/models` отдаёт состояние, refcount, время последнего
  использования, оценку RSS и `pinned`.

---

## 8. HTTP API

Ошибки, лимиты, аутентификация и CORS — общие middleware; диалекты отличаются только
формой запроса/ответа.

### 8.1 Диалект `openai`

| Метод | Путь | Заметки |
|---|---|---|
| `POST` | `/v1/audio/transcriptions` | multipart: `file`, `model`, `language`, `prompt`, `response_format`, `temperature`, `timestamp_granularities[]` |
| `POST` | `/v1/audio/translations` | `501` — моделей перевода в v1 нет |
| `GET` | `/v1/models` | список моделей в формате OpenAI (`{object:"list", data:[…]}`) |
| `GET` | `/v1/models/{id}` | |

- `response_format`: `json` (только `{text}`), `verbose_json` (сегменты + слова),
  `text`, `srt`, `vtt`.
- `timestamp_granularities[]`: `segment` и/или `word`. Запрос `word` у модели без такой
  возможности → ответ отдаётся с `segment` + `warnings[]` (не ошибка), потому что клиенты
  OpenAI SDK не ожидают отказа. Строгий режим — заголовок `X-NanoASR-Strict: 1` → `422`.
- `prompt` — в v1 маппится на hotwords (список слов через запятую), а не на LM-подсказку.
  Расхождение с OpenAI задокументировано.
- `temperature` принимается и игнорируется (у греди/beam-декодера нет семплирования) —
  об этом варнинг в ответе.
- Ошибки — в формате OpenAI: `{"error":{"message","type","param","code"}}`.

### 8.2 Диалект `native` (`/api/v1`)

| Метод | Путь | Назначение |
|---|---|---|
| `POST` | `/transcribe` | синхронно, весь набор параметров NanoASR |
| `POST` | `/jobs` | `202` + `{id, status:"queued", position}` |
| `GET` | `/jobs` | история (пагинация, фильтры по модели/статусу/дате) |
| `GET` | `/jobs/{id}` | статус и результат |
| `GET` | `/jobs/{id}/events` | **SSE**: `queued → running(stage,percent) → done/failed` |
| `DELETE` | `/jobs/{id}` | отмена (в очереди — снятие, в работе — `context.Cancel`) |
| `GET` | `/models` | состояние пула + манифесты |
| `POST` | `/models/{id}/download` | SSE-прогресс скачивания |
| `POST` | `/models/{id}/load` · `/unload` · `/pin` · `/reload` | управление пулом, hot swap |
| `GET` | `/catalog` | доступные для скачивания модели |
| `GET` | `/config` | эффективная конфигурация (секреты вымараны) |
| `GET` | `/healthz` · `/readyz` | liveness / readiness; `/readyz` отдаёт `queue_depth`, `queue_size` и `queued_bytes` и становится `503 saturated` при полной очереди — именно тогда балансировщику и следует перестать сюда слать |

Параметры `/transcribe` и `/jobs`: `model`, `language`, `diarize`, `num_speakers`,
`punctuate`, `itn`, `hotwords[]`, `hotwords_score`, `channel_mode`, `vad` (override),
`decoding_method`, `max_active_paths`, `response_format`, `webhook_url`, `strict`.
Неизвестное **значение** параметра — `400` с именем поля в `param`: молча
проигнорировать `punctate=true` значит оставить вызывающего в уверенности, что
пунктуация включена.

`/models/*` кроме `GET` и `GET /config` требуют админского ключа. Чтение списка
моделей и каталога — нет: это не изменение состояния сервера.

**Управление задачами.** `GET /jobs` отдаёт `{data, next_cursor}`; курсор
непрозрачен и продолжает выборку с той же строки. `GET /jobs/{id}` принимает
`response_format` — завершённую задачу можно забрать сразу субтитрами.
`DELETE /jobs/{id}` возвращает задачу в том состоянии, в которое она **фактически**
пришла: успевшая завершиться задача остаётся `succeeded`, а не превращается в
`canceled` ради красивого ответа.

**SSE.** `GET /jobs/{id}/events` и `POST /models/{id}/download` стримят
`text/event-stream`. Отказ (`404`, `403`) всегда приходит **до** заголовков потока:
после `200` его негде сообщить так, чтобы клиент это заметил. Поток заканчивается
событием `done` — `EventSource` переподключается при закрытии соединения, и клиент,
которому не сказали, что работа кончилась, переподключался бы вечно. Каждое событие
несёт `id`, который клиент возвращает в `Last-Event-ID` (или `?last_event_id=`) при
переподключении. Догоняющий снапшот завершённой задачи `id` не несёт: он не переход,
и выдуманный номер давал бы неизменной задаче новый id при каждом переподключении.

Ошибки — RFC 9457 `application/problem+json`. `429` всегда с `Retry-After`.

### 8.3 Добавление своего диалекта

```go
// internal/api/mycorp/adapter.go
package mycorp

func init() { adapter.Register(&Adapter{}) }

type Adapter struct{}

func (a *Adapter) Name() string { return "mycorp" }
func (a *Adapter) Mount(mux *http.ServeMux, svc core.Service, deps adapter.Deps) {
    mux.HandleFunc("POST /mycorp/v2/stt", a.handleSTT(svc))
}
```

Включение — в конфиге: `api.dialects: [openai, native, mycorp]`. Ядро не меняется.
Адаптер обязан: (1) не обращаться к пулу/реестру напрямую, только через `core.Service`;
(2) не логировать тела запросов; (3) переводить `core` ошибки через `deps.ErrorMapper`.

### 8.4 Коды ошибок

| HTTP | Код | Когда |
|---|---|---|
| 400 | `invalid_request` | нет файла, неизвестный параметр, битый multipart |
| 401 | `unauthorized` | нет/неверный ключ |
| 403 | `model_forbidden` | ключ не имеет доступа к модели |
| 404 | `model_not_found` / `job_not_found` | |
| 413 | `file_too_large` / `duration_exceeded` | превышен лимит |
| 415 | `unsupported_media_type` | формат не распознан или нет ffmpeg |
| 422 | `capability_unavailable` | strict-режим и модель не умеет запрошенного |
| 429 | `queue_full` / `rate_limited` | очередь полна по числу задач или по `max_queued_bytes`; либо ключ превысил свой `rps`. Всегда с `Retry-After` |
| 499 | — | клиент отсоединился (только в логах) |
| 500 | `internal` | |
| 503 | `model_unavailable` / `draining` | нет слота, идёт выгрузка, шатдаун |
| 504 | `processing_timeout` | превышен `jobs.max_processing_time` |

---

## 9. Хранилище

SQLite (`modernc.org/sqlite`, pure-Go — не конфликтует с cgo от sherpa-onnx),
режим WAL, `busy_timeout=5000`.

```sql
CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,
  status        TEXT NOT NULL,        -- queued|running|succeeded|failed|canceled|expired
  priority      INTEGER NOT NULL DEFAULT 0,
  model_id      TEXT NOT NULL,
  model_rev     TEXT NOT NULL,
  params_json   TEXT NOT NULL,
  source        TEXT NOT NULL,        -- api|ui
  api_key_id    TEXT,
  filename      TEXT,
  audio_bytes   INTEGER,
  audio_seconds REAL,
  created_at    INTEGER NOT NULL,
  started_at    INTEGER,
  finished_at   INTEGER,
  error_code    TEXT,
  error_message TEXT,
  result_json   TEXT,                 -- полный core.Result
  stats_json    TEXT
);
CREATE INDEX jobs_status_created ON jobs(status, created_at DESC);
CREATE INDEX jobs_model          ON jobs(model_id, created_at DESC);

CREATE TABLE api_keys (
  id         TEXT PRIMARY KEY,
  name       TEXT NOT NULL,
  hash       TEXT NOT NULL,           -- argon2id, не сам ключ
  scopes     TEXT NOT NULL,           -- JSON: {"models":["*"],"admin":false,"rps":10}
                                      -- в v1 ключи живут в конфиге, а не здесь:
                                      -- admin и rps задаются в auth.keys[]
  created_at INTEGER NOT NULL,
  last_used  INTEGER,
  disabled   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
```

- Аудио в БД **не хранится**. `filename`/`audio_bytes` — только метаданные.
- **Аудио очередной задачи живёт на диске** в `storage.temp_dir` под именем
  `job-<id>.audio` (решение №19), от `202` до терминального статуса. Имя выводится
  из id, а не хранится в колонке: именно это позволяет после рестарта сопоставить
  строку с файлом, а файл без строки — опознать как сироту.
- Объём этого хранения ограничен `jobs.max_queued_bytes` (решение №20).
- **При старте, строго в этом порядке:**
  1. `queued` возвращаются в очередь — их аудио на месте, параметры в `params_json`;
  2. `Sweep` удаляет все `job-*.audio`, для которых нет живой задачи;
  3. `running` помечаются `failed` c `error_code=server_restart` — сколько работы уже
     сделано, неизвестно, и восстановить это нельзя.

  Порядок нельзя менять: `Sweep` до шага 1 удалит ровно те файлы, которых ждут
  возобновляемые задачи. Чужие файлы в `temp_dir` не трогаются — только `job-*.audio`.
- Ретеншн истории: `jobs.history_ttl` (дефолт 30 дней), фоновый GC раз в час.
- Пагинация истории — курсором по `(created_at, id)` (решение №23), не `OFFSET`.
- `api_key_id` — не справочное поле: `Store.List` фильтрует по нему всё, кроме
  запросов от админского ключа (решение №22).

---

## 10. Конфигурация

Приоритет: **флаги > переменные окружения (`NANOASR_*`) > YAML > дефолты**.
Все дефолты вычислимы: сервер стартует без конфига вообще.

```yaml
server:
  addr: ":8080"
  read_header_timeout: 10s
  max_upload_bytes: 104857600      # 100 МБ
  shutdown_grace: 30s

auth:
  mode: apikey                     # apikey | open  (open разрешён только на 127.0.0.1)
  keys: []                         # или через NANOASR_AUTH_KEYS / БД

api:
  dialects: [openai, native]

ui:
  enabled: true
  path: /ui

audio:
  ffmpeg_path: ffmpeg              # пусто → нативный путь только для WAV/PCM
  ffmpeg_timeout: 120s
  max_duration: 30m
  target_sample_rate: 16000
  channel_mode: downmix            # downmix | first | split

vad:
  enabled: true
  model: silero-vad-v5
  threshold: 0.5
  min_silence_ms: 300
  min_speech_ms: 250
  max_speech_sec: 20

asr:
  models_dir: /var/lib/nanoasr/models
  default_model: ""                # пусто → единственная загруженная или ошибка
  max_resident_models: 3
  max_model_rss_mb: 6144
  inference_slots: 0               # 0 → GOMAXPROCS
  idle_ttl: 15m
  acquire_timeout: 30s
  batch: { max_size: 8, max_seconds: 60 }

registry:
  allow_download: true
  catalog_url: ""                  # пусто → вшитый каталог
  mirrors: []
  download_concurrency: 2

jobs:
  queue_size: 100
  max_concurrent: 0                # 0 → auto (см. 12.3)
  max_queued_bytes: 4294967296     # 4 ГиБ аудио на диске; не меньше max_upload_bytes
  max_processing_time: 30m
  history_ttl: 720h
  webhook_secret: ""               # пусто → доставки без подписи, с предупреждением при старте
  webhook_allow_private: false     # true допускает webhook_url в приватные диапазоны; только на loopback

postproc:
  punctuation: { enabled: false, model: "" }
  itn:         { enabled: false, locale: ru }

diarization:
  enabled: false
  segmentation_model: ""
  embedding_model: ""
  clustering: { num_clusters: 0, threshold: 0.5 }

storage:
  db_path: /var/lib/nanoasr/nanoasr.db
  temp_dir: ""                     # пусто → os.TempDir()

log:
  level: info
  format: json                     # json | text
```

---

## 11. Безопасность

| Поверхность | Мера |
|---|---|
| API-ключи | хранятся как argon2id-хэш; сравнение в constant time; в логи не попадают |
| Open-режим | разрешён **только** при `addr` на `127.0.0.1`/`::1`; иначе — отказ старта |
| Загрузка файлов | `io.LimitReader` до чтения; temp-файл с `0600`; удаление в `defer` |
| ffmpeg | фиксированные аргументы, вход через stdin, таймаут + `WaitDelay`, отдельная рабочая директория |
| Распаковка моделей | запрет `..`, абсолютных путей, симлинков; лимит на размер и число файлов |
| Скачивание моделей | обязательная проверка `sha256` из каталога; только HTTPS; редиректы только на whitelist-хосты |
| SSRF | `webhook_url` — только HTTPS; проверяются **все** резолвнутые адреса, не первый (имя с одним публичным и одним приватным ответом — стандартный обход); редиректы не выполняются, иначе проверка адреса обходится ответом `302`. Доставка подписывается `X-NanoASR-Signature: sha256=<hmac>` по `"<timestamp>.<body>"` |
| Path traversal | `model_id` валидируется regex `^[a-zA-Z0-9._-]{1,64}$`, склейка путей только через `filepath.Join` + проверка префикса |
| UI | ассеты открыты (браузер не шлёт Bearer за `<script>`), API — нет; SPA узнаёт про ключ по `401`. Отключается `ui.enabled` и вырезается build-тегом `noui` |
| Логи | не логируются: тела запросов, транскрипты, имена файлов при `log.level > debug` |
| Заголовки | `X-Content-Type-Options`, `Referrer-Policy`, CSP для `/ui` |

Отдельно: **процессной изоляции инференса в v1 нет**. Падение в C++-коде sherpa-onnx
роняет весь сервер. Это принято сознательно (решение №11, вариант с отдельными
процессами отклонён). Митигация — рестарт-политика Docker/systemd; в roadmap — M6.

**Это не гипотетический риск, а измеренный.** Если `features.dim` в манифесте не
совпадает с тем, что объявляет модель, onnxruntime бросает C++-исключение, которое
доходит до `terminate()` и убивает процесс через SIGABRT. Проверено на
`zipformer-small-en`: значение 64 вместо 80 не даёт плохой транскрипт — оно
аварийно завершает сервер. Из этого следуют три вещи:

1. Прогрев модели при загрузке (§7.4) переносит падение с пользовательского
   запроса на момент загрузки. Процесс всё равно умирает, но предсказуемо.
2. `nanoasr models inspect --probe` запускает каждого кандидата в **отдельном
   процессе** — иначе инструмент умирал бы, отвечая на вопрос, ради которого написан.
3. Аргумент за изоляцию инференса в M6 усиливается: одна неверная строка в
   манифесте способна уронить сервер, и никакой Go-код этого не перехватит.

### 11.1 Отклонение от требования к хешированию ключей

Выше в этом разделе изначально был записан argon2id. При реализации он заменён на
**SHA-256 с constant-time сравнением**, и это осознанное отклонение: argon2 создан
против перебора низкоэнтропийных паролей и стоит десятки миллисекунд CPU на запрос.
На сервере, где CPU специально нормируется под инференс (§12.3), это самострел —
поток запросов с мусорными ключами отнимает ядра у распознавания. API-ключ это 256
бит случайности, его не перебирают.

---

## 12. Производительность и ёмкость

### 12.1 Сравнение семейств (измерено)

Одно и то же аудио, одна и та же акустическая модель GigaAM v2, два декодера,
4 ядра, `num_threads=2`:

| Модель | RTF | ASR | слов | confidence |
|---|---|---|---|---|
| `gigaam-v2-ctc-ru` | 0.245 | 2678 мс | 26 | нет |
| `gigaam-v2-rnnt-ru` | 0.871 | 3555 мс | 26 | да |

Расхождение текстов между ними — **3.8 % WER**. То есть RNNT примерно втрое дороже
по CPU при том же результате; его единственное преимущество — пословная уверенность,
которой CTC не даёт (`ys_log_probs` приходят пустыми — проверено). Это ответ на
вопрос §19.2 №5: **дефолтом разумно ставить CTC**, а RNNT включать там, где нужна
подсветка сомнительных мест.

### 12.2 Измерено на M1

4 ядра, `num_threads=2`, GigaAM v2 CTC int8 (237 МБ), клип 11.3 с, речь 92 %:

| Вход | RTF | decode | VAD | ASR |
|---|---|---|---|---|
| 16 кГц WAV | 0.238 | 5 мс | 58 мс | 2624 мс |
| 8 кГц A-law | 0.177 | 4 мс | 61 мс | 1934 мс |
| 8 кГц µ-law | 0.182 | 6 мс | 75 мс | 1977 мс |
| mp3 64k (ffmpeg) | 0.186 | 130 мс | 60 мс | 1910 мс |
| opus 24k (ffmpeg) | 0.190 | 109 мс | 64 мс | 1973 мс |

**Сдвиг границ слов относительно 16 кГц оригинала** (частичный ответ на §19.1 №2):

| Вход | медиана | среднее | p95 | макс |
|---|---|---|---|---|
| 8 кГц A-law | 8 мс | 16 мс | 32 мс | 48 мс |
| 8 кГц µ-law | 8 мс | 16 мс | 32 мс | 48 мс |
| mp3 64k | 8 мс | 12 мс | 32 мс | 48 мс |
| opus 24k | 0 мс | 6 мс | 32 мс | 40 мс |

То есть телефонийная полоса стоит меньше одного кадра энкодера. Абсолютную
погрешность это не измеряет — у публичного клипа нет эталонной пословной
разметки; нужны ваши записи (§19.1 №1).

Текст на всех пяти путях совпадает с точностью до первого слова: узкая полоса
съедает начальный слог («ничьих» → «чьих»). WER между путями < 10 %.

### 12.3 Ориентиры RTF для других моделей (проверить на своём железе)

| Модель | Потоков | Ожидаемый RTF | 10-мин файл |
|---|---|---|---|
| zipformer transducer (int8) | 4 | 0.04–0.10 | 24–60 с |
| zipformer transducer (fp32) | 4 | 0.10–0.20 | 60–120 с |
| GigaAM v2 RNNT | 4 | 0.10–0.25 | 60–150 с |
| CTC (zipformer-ctc / nemo-ctc) | 4 | 0.03–0.08 | 18–48 с |
| + диаризация | 4 | +0.3–0.5 | +180–300 с |

VAD снимает тишину — на телефонии `speech_ratio` обычно 0.5–0.8, то есть реальное время
пропорционально **речи**, а не длине файла.

### 12.4 CPU-губернатор

Проблема: onnxruntime создаёт свой пул потоков на экземпляр модели. Три модели по
4 потока на 4-ядерной машине = 12 потоков на 4 ядра, потери на переключение контекста.

Решение: глобальный **взвешенный семафор** ёмкостью `inference_slots` (дефолт `GOMAXPROCS`).
Каждый батч берёт `num_threads` слотов, освобождает после `DecodeStreams`.
`num_threads` модели по умолчанию = `clamp(GOMAXPROCS/2, 1, 8)`.

### 12.5 Авто-дефолты (решение №13)

```
inference_slots   = GOMAXPROCS
num_threads       = clamp(GOMAXPROCS/2, 1, 8)
jobs.max_concurrent = clamp(GOMAXPROCS / num_threads, 1, 8)
max_model_rss_mb  = clamp(totalRAM_MB / 2, 1024, 16384)
max_resident_models = clamp(max_model_rss_mb / 1500, 1, 6)
```

Проверка при старте: `max_concurrent × 115 МБ` (пиковый PCM) `+ max_model_rss_mb`
не должно превышать 80 % доступной памяти; иначе — предупреждение в лог с конкретными числами.

---

## 13. UI

### 13.1 Стек

| Слой | Выбор |
|---|---|
| Сборка | Vite + React 19 + TypeScript (strict) |
| Роутинг | TanStack Router, file-based, `defaultPreload: 'intent'` |
| Данные | TanStack Query |
| Примитивы | Base UI (`@base-ui/react`) |
| Компоненты | shadcn-подход: код в репозитории, не зависимость |
| Цвет | Radix Colors — **Slate** (нейтральная шкала) + один акцент |
| Иконки | Iconoir (`iconoir-react`) |
| Тосты | Sonner-подход поверх собственного стора |
| i18n | i18next + react-i18next, `ru` / `en` |
| Встраивание | `embed.FS` в Go-бинарь, отдельного Node-сервера нет |

### 13.2 Архитектурная «стена»

Требование — «consistency is a wall, not a guideline». Реализуется тремя уровнями:

**1. Единая оболочка.** Ровно один `src/routes/__root.tsx`: шапка, переключатели
языка и темы, история уведомлений, `<Outlet/>`. Страницы не рисуют собственные шапки.

**2. Типизированный контекст маршрута.** Каждый маршрут обязан экспортировать `pageMeta`:

```ts
export const Route = createFileRoute('/models')({
  component: ModelsPage,
  context: (): PageContext => ({
    page: { titleKey: 'models.title', descriptionKey: 'models.desc', actions: <NewModelButton/> },
  }),
})
```

Заголовок, отступы и хлебные крошки рисует оболочка из контекста — страница не может
поставить свой отступ, потому что ей неоткуда.

**3. Примитивы компоновки — единственный способ верстать.**
`<Page>`, `<Section>`, `<Stack gap="…">`, `<Grid>`, `<Inline>`. Все отступы — только
токены шкалы (`1 | 2 | 3 | 4 | 6 | 8`), произвольные значения не принимаются типами.

**Правила ESLint (`eslint.config.js`), включённые как `error`:**

| Правило | Что запрещает |
|---|---|
| `no-restricted-syntax` | `<div className="flex …">` / `grid` / `p-*` / `gap-*` внутри `src/routes/**` — верстать можно только примитивами |
| `no-restricted-syntax` | атрибут `style={{…}}` где угодно, кроме `components/player/**` (там canvas-геометрия) |
| `no-restricted-imports` | импорт `sonner` вне `src/lib/toast.ts` |
| `no-restricted-imports` | импорт `@base-ui/react` и подпути вне `src/components/ui/**` |
| `no-restricted-imports` | импорт `react-i18next` вне `src/lib/i18n.ts` и компонентов (только через `useT()`) |
| `no-restricted-syntax` | `transition`/`animation`/`animate-*` вне `src/styles/motion.css` |
| локальное правило `require-page-meta` | маршрут без экспорта `pageMeta` |
| `@typescript-eslint/no-restricted-types` | сырые строки там, где ожидается ключ перевода |

Плюс TypeScript: `strict`, `noUncheckedIndexedAccess`, `exactOptionalPropertyTypes`,
шкала отступов — литеральный union, ключи переводов — union из JSON ресурсов
(типобезопасный `t()`).

### 13.3 Анимации

Выключены по умолчанию: глобально `--motion-duration: 0ms`. Компонент, которому
движение действительно нужно (Dialog, Sheet, Popover, Toast), выставляет
`data-motion="enter"` и берёт длительность из `--motion-duration-surface` (120–180 мс).
Блок `@media (prefers-reduced-motion: reduce)` обнуляет всё. Никаких `transition:` в
компонентах — только в `styles/motion.css`.

### 13.4 Загрузка данных

- **Ничего до ~300 мс.** Хук `useDelayedPending(300)` — раньше не показывается ничего.
- **Скелетоны** — для данных известной формы (списки моделей, история). Приглушённый
  тон (`--slate-3`), мягкий пульс, отключаемый `prefers-reduced-motion`, **размеры
  совпадают с реальным контентом**, чтобы не было сдвига макета.
- **Инлайновые спиннеры** — только для действий/сабмитов, тонкие, монохромные, внутри
  кнопки на месте иконки.
- **Полноэкранных спиннеров нет.** Единственное исключение — первичная гидрация.
- Всё идёт через `<Skeleton>` и `<Spinner>`; собственных индикаторов в страницах нет
  (следит ESLint-правило на `animate-spin`).

### 13.5 Поверхности и уведомления

Порядок выбора — от наименее блокирующего:

```
инлайн  →  popover  →  side/bottom sheet  →  центральный модал  →  тост (постфактум)
```

- Центральный модал — только для настоящих прерываний: подтверждение удаления модели,
  обязательный выбор. Всё остальное — sheet.
- Тосты — стек с автозакрытием, **никогда** для критической информации.
- Анимации: тихий fade/scale, мягкая (не чёрная) подложка `slate-a8`.
- Всё — через единые `<Dialog>`, `<Sheet>`, `<Popover>`, `toast()` поверх Base UI.
- **История уведомлений**: каждый `toast()` пишется в стор (`lib/notifications.ts`,
  до 100 записей, persist в `localStorage`); в шапке — кнопка-колокол с бейджем
  непрочитанных и dropdown со списком: иконка уровня, заголовок, относительное время,
  действие. Кнопка «Очистить».

### 13.6 Плеер и транскрипт (ключевой экран)

```
┌──────────────────────────────────────────────────────────────────┐
│  ▁▂▅▇▅▂▁▁▁▂▇█▇▅▂▁▁▁▁▁▂▅▇▅▂▁▁▁▁▂▅▇▇▅▂▁   ← canvas waveform        │
│  ░░░        ░░░░░              ░░░       ← зоны тишины (slate-4)  │
│  │                                                               │
│  ▶  0:42 / 3:07   ─────●──────────────   1.0×   [⤓ SRT ▾]         │
├──────────────────────────────────────────────────────────────────┤
│  здравствуйте это компания ромашка чем могу помочь               │
│  ^^^^^^^^^^^^                                                    │
│  каждое слово — кликабельный span, клик → seek(word.start)        │
└──────────────────────────────────────────────────────────────────┘
```

- **Источник аудио — локальный `File`** (`URL.createObjectURL`), сервер аудио не отдаёт.
- **Пики**: `decodeAudioData` в `OfflineAudioContext` на 8 кГц — на главном потоке,
  потому что `OfflineAudioContext` в Worker'е не существует (решение №31); RMS по
  окнам в Worker'е → `Float32Array` (~2000 точек). Сэмплы передаются, а не копируются.
- **Зоны тишины** — из `silence[]` ответа, рисуются подложкой под волной.
- **Клик по слову** → `audio.currentTime = word.start`, playhead прыгает.
- **Подсветка текущего слова** — `requestAnimationFrame` + бинарный поиск по плоскому
  массиву слов; ререндера React на каждый кадр нет, меняется только CSS-класс через ref.
- Клавиатура: `Space` play/pause, `←/→` ±5 с, `Shift+←/→` — предыдущее/следующее слово,
  `F` — поиск по транскрипту. Всё доступно с клавиатуры и озвучивается скринридером.
- Слова с низким `confidence` подчёркиваются пунктиром (порог настраивается).
- При `diarize=true` — цветовая метка спикера на сегменте и фильтр «показать только spk_N».
- Экспорт: JSON — на клиенте (он уже в руках), SRT/VTT/TXT — с сервера через
  `?response_format=` (решение №32): один рендер субтитров на весь продукт.

> Собственный canvas выбран вместо wavesurfer.js осознанно: нужны одновременно
> зоны тишины, пословная подсветка и seek-по-слову — интеграция чужого плагина регионов
> выйдет дороже 150 строк своего рендерера, плюс минус одна зависимость в бандле.

### 13.7 Экраны

| Маршрут | Содержание |
|---|---|
| `/` | Новый прогон: dropzone, выбор модели (с автозагрузкой из каталога), параметры (язык, диаризация, пунктуация, ITN, hotwords), кнопка «Распознать» |
| `/result/$jobId` | Плеер + транскрипт + статистика (RTF, стадии, число сегментов) + экспорт |
| `/models` | Пул и каталог: статус, RSS, refcount, pinned, скачать / загрузить / выгрузить / hot swap; лицензия модели |
| `/jobs` | История и очередь: фильтры, живой статус через SSE, повтор на другой модели |
| `/settings` | Язык, тема, ключ API, порог подсветки по `confidence` |

---

## 14. Сборка и поставка

### 14.1 Артефакты

1. **Docker-образ** (основной): multi-stage — `node:22` (сборка SPA) → `golang:1.24`
   (сборка cgo) → `debian:bookworm-slim` + `ffmpeg` + `.so` + бинарь.
   Модели — на volume `/var/lib/nanoasr/models`.
2. **tar.gz** для systemd: `bin/nanoasr`, `lib/*.so`, `configs/nanoasr.yaml`,
   `nanoasr.service`, `README`. Собирается с `CGO_LDFLAGS="-Wl,-rpath,\$ORIGIN/lib"`
   (см. 3 — проверено). Размер ~33 МБ + фронтенд.

### 14.2 Makefile

```
make web        # npm ci && vite build → web/dist
make build      # go build (встраивает web/dist)
make dist       # relocatable tar.gz с .so
make docker     # образ
make lint       # golangci-lint + eslint + tsc
make test       # go test ./... + vitest
make run        # локальный запуск с configs/nanoasr.dev.yaml
```

Тег сборки `noui` полностью исключает SPA из бинаря (для headless-инсталляций).

### 14.3 CI (GitHub Actions)

`lint` → `test` → `build (linux/amd64, linux/arm64)` → `docker` → `release`
(теги `v*` → GitHub Release с tar.gz). Кэш: `~/go/pkg/mod` (там 95 МБ модуля sherpa-onnx),
`~/.npm`.

---

## 15. Тестирование

| Уровень | Что покрывается |
|---|---|
| Unit | сборка слов из токенов (golden-таблицы на bpe/cjkchar/char, включая пунктуацию и пустые durations), WAV-декодер (s16/s24/f32/a-law/µ-law, битые заголовки), ресемплер (THD на синусе), сниффер форматов, приоритет конфигов, маппинг ошибок в обоих диалектах, LRU-вытеснение и refcount |
| Fuzz | парсер WAV-заголовка, распаковщик архивов моделей (traversal / bomb) |
| Integration | самая маленькая модель из каталога + `testdata/*.wav` → golden-транскрипт; тайминги сверяются с допуском ±50 мс; hot swap под нагрузкой (нет ошибок у in-flight); отмена запроса реально останавливает декодирование |
| Race | `go test -race` на пуле, очереди и губернаторе — обязательно |
| Frontend | vitest на `words→highlight` (бинарный поиск), сборку SRT/VTT, стор уведомлений; ESLint-правила проверяются собственными тестами на фикстурах |
| Нагрузка | скрипт на 100 параллельных 5-минутных файлов: очередь не переполняется, память не растёт, RSS стабилен после 30 минут |

Golden-файлы обновляются только явной командой `make golden-update`.

---

## 16. Структура репозитория

```
NanoASR/
├── cmd/nanoasr/            # main, подкоманды: serve, models, version
├── internal/
│   ├── config/             # загрузка, дефолты, валидация, авто-расчёт
│   ├── core/               # доменные типы + интерфейс Service (без HTTP)
│   ├── audio/              # сниффер, WAV/PCM-декодер, ffmpeg-декодер, ресемплер
│   ├── vad/                # silero-обёртка, зоны тишины
│   ├── asr/                # интерфейс Recognizer + реестр семейств
│   │   ├── families/       # transducer.go, zipformer_ctc.go, nemo_ctc.go, …
│   │   └── sherpa/         # cgo-обёртка над sherpa-onnx
│   ├── words/              # токены → слова, тайминги, confidence
│   ├── postproc/           # punctuation, itn, hotwords
│   ├── diarize/            # pyannote + embeddings + clustering
│   ├── registry/           # каталог, скачивание, манифесты, кэш на диске
│   ├── pool/               # LRU, refcount, hot swap, CPU-губернатор
│   ├── job/                # очередь, воркеры, SSE, статусы
│   ├── store/sqlite/       # схема, миграции, JobStore
│   ├── api/
│   │   ├── adapter/        # интерфейс Adapter + реестр диалектов
│   │   ├── openai/         # диалект OpenAI
│   │   └── native/         # нативный диалект
│   ├── httpx/              # middleware: requestid, recover, auth, limits, rate, sse
│   └── ui/                 # embed.FS + handler (build-тег noui)
├── web/                    # Vite SPA
├── configs/                # nanoasr.example.yaml, nanoasr.dev.yaml
├── deploy/                 # Dockerfile, docker-compose.yml, nanoasr.service
├── docs/                   # SPEC.md (этот файл), API.md, MODELS.md
├── testdata/               # короткие wav + golden-транскрипты
└── scripts/                # dist.sh, golden-update.sh
```

---

## 17. Вехи

Оценки — в человеко-днях одного разработчика, знакомого с Go; фронтенд и бэкенд
считаются отдельно, но веха закрыта только целиком.

| Веха | Содержание | Оценка |
|---|---|---|
| **M0 — каркас** | структура, конфиг, HTTP-скелет, health, Docker, CI, релокация в tar.gz | 3 |
| **M1 — вертикальный срез** ✅ | Декодеры (WAV/PCM + ffmpeg), полифазный ресемплер, VAD, nemo_ctc, слова, `/v1/audio/transcriptions` во всех пяти форматах, локальный реестр | 5 → 10 факт |
| **M2 — модельный слой** ✅ | манифесты, каталог из 4 проверенных записей, скачивание с sha256 и защитой распаковки, LRU-пул, refcount, hot swap, CPU-губернатор, `models inspect/pull/catalog`. HTTP-ручки управления моделями перенесены в M3 | 6 |
| **M3 — очередь и API** ✅ | SQLite с курсорной пагинацией, очередь с приоритетами и байтовым бюджетом, возобновление после рестарта, SSE с `Last-Event-ID`, отмена, изоляция задач по ключу, нативный диалект целиком (включая управление моделями, уехавшее из M2), вебхуки с проверкой адреса и HMAC, лимит запросов на ключ | 9 |
| **M4 — тестовый UI** ✅ | пять экранов, плеер с волной и пословной перемоткой, SSE-статусы, управление моделями, скачивание с прогрессом, история с курсором, i18n ru/en, тема, история уведомлений; Vitest на чистую логику и Playwright на сквозной путь | 11 |
| **M5 — качество** | ffmpeg-декодер, ресемплер, `channel_mode: split`, диаризация, пунктуация, ITN(ru), hotwords, нагрузочные и golden-тесты | 8 |
| **M6 — эксплуатация** *(за рамками v1)* | Prometheus, OTel, `bench`/`eval` (RTF/WER), forced alignment, изоляция инференса в процессы, Postgres+S3 | 10+ |

Итого v1 (M0–M5): **≈38 человеко-дней**. Критический путь — M1 → M2 → M3; M4 можно
вести параллельно после M1, если зафиксировать контракт API.

---

## 18. Риски

| # | Риск | Вероятность | Влияние | Что делаем |
|---|---|---|---|---|
| 1 | Точность word-таймингов на 8 кГц телефонии хуже ожиданий | средняя | высокое | измерить на реальных данных **до M4** (19.1); при провале — forced alignment из M6 поднять в v1 |
| 2 | Падение C++-кода роняет процесс | низкая | высокое | рестарт-политика; изоляция в процессы — M6 |
| 3 | Нет метрик → деградация незаметна | **высокая** | среднее | no-op `Observer` во всех точках; включение — часы работы (2.2) |
| 4 | Лицензии весов моделей (бывают non-commercial) | средняя | высокое | поле `license` в манифесте, показ в UI, отказ грузить `non-commercial` при `registry.strict_license: true` |
| 5 | Oversubscription потоков onnxruntime | высокая | среднее | CPU-губернатор (12.4), проверка на старте |
| 6 | Обновление sherpa-onnx ломает Go API | средняя | среднее | версия зафиксирована (`v1.13.6`), обновление — только через прогон golden-тестов |
| 7 | Каталог моделей на HF/GitHub недоступен из контура | средняя | среднее | `registry.mirrors`, air-gapped режим |
| 8 | 30-минутные файлы × параллелизм съедают память | средняя | высокое | 115 МБ/задача учтено в авто-дефолтах, проверка на старте (12.5) |
| 9 | «Стена» ESLint тормозит разработку фронта | средняя | низкое | правила — `error`, но каждое с `message`, объясняющим, чем заменить |

---

## 19. Открытые вопросы

Отсортированы по влиянию на архитектуру. Для каждого указано предположение,
по которому работа пойдёт, если ответа не будет.

### 19.1 Блокирующие до начала M1

1. **Есть ли 10–30 реальных телефонийных записей с эталонной разметкой?** Без них
   нельзя ни выбрать модель, ни проверить тайминги, ни настроить VAD.
   *Предположение:* берём публичные ru-датасеты, точность таймингов не гарантируем.
2. **Какая точность word-таймингов считается приемлемой?** ±50 мс, ±100 мс, ±200 мс?
   Частично закрыто на M1: узкополосность добавляет ≤48 мс (см. 12.2). Абсолютная
   погрешность против ручной разметки по-прежнему неизвестна.
   *Предположение:* ±100 мс достаточно для навигации по плееру.
3. **Целевой SLO на латентность синхронного запроса.** 10-мин файл за 60 с? за 120 с?
   *Предположение:* p95 ≤ 0.2 RTF, то есть 10 минут за 2 минуты.
4. **Кодек исходных файлов.** g711 a-law/µ-law в WAV-контейнере? Сырой PCM без заголовка?
   opus из WebRTC? От этого зависит, нужен ли ffmpeg вообще.
   *Предположение:* WAV/PCM a-law + mp3; ffmpeg ставим, но он не обязателен.

### 19.2 Влияют на объём M2–M3

5. ~~Какая модель — **дефолтная**?~~ Измерено (12.1): CTC втрое дешевле RNNT при
   расхождении 3.8 % WER, но не даёт confidence. Дефолт — CTC, если вам не нужна
   пословная уверенность.
6. Сколько моделей должны быть **резидентными одновременно** в проде?
7. Нужен ли **выбор модели по языку** (`model` не указан, указан `language`) или только явный id?
8. Нужна ли **автодетекция языка** (в sherpa-onnx есть, но она whisper-based — а whisper в v1 исключён)?
9. **Идемпотентность:** нужен ли `Idempotency-Key` для повторных POST?
10. **Webhook** на завершение задачи — нужен в v1? Ретраи, подпись HMAC?
11. Нужны ли **приоритеты** в очереди (интерактив vs батч) или FIFO достаточно?
12. Нужна ли **пакетная отправка** (несколько файлов одним запросом)?
13. Нужен ли `GET /v1/models` в формате OpenAI **строго** (клиенты часто парсят жёстко)?
14. Что делать с **пустым результатом** (тишина/шум): `200` с пустым текстом или `422`?
15. Нужны ли **квоты** на ключ (минут аудио в сутки) или достаточно RPS?

### 19.3 Эксплуатация

16. Одна инсталляция или **несколько реплик**? SQLite не переживёт общий том.
17. Куда идут **логи** (stdout/journald/файл) и есть ли требование к формату?
18. Есть ли требование **не выпускать трафик наружу** (тогда нужно зеркало каталога)?
19. Нужен ли **graceful drain** по сигналу с ожиданием окончания очереди (сколько ждать)?
20. Кто и как **обновляет модели** в проде — админ через UI, CI, или git-ops конфиг?
21. Требуется ли **аудит-лог** обращений (кто, когда, какая модель, сколько секунд)?

### 19.4 Функциональность

22. Нужен ли **`/v1/audio/translations`** по-настоящему (нужны модели перевода — другое семейство)?
23. Нужны ли **`srt`/`vtt`** с пословной разбивкой (karaoke-style) или посегментной?
24. Нужен ли **редактор транскрипта** в UI (правка с сохранением таймингов)?
25. Нужно ли **сравнение моделей** бок о бок (было в списке, но не выбрано)?
26. Нужна ли **идентификация говорящих** (не diarization, а «это Иванов») через `SpeakerEmbeddingManager`?
27. Нужен ли **денойзер** (GTCRN) как опция для шумной телефонии?
28. Нужны ли **эмоции/события** SenseVoice (поля `Emotion`/`Event`) — они требуют семейства, исключённого решением №8?
29. Нужна ли **фильтрация мата / маскирование PII** (было в вариантах, не выбрано)?
30. Какой набор **языков ITN** нужен реально — только ru, или ещё en?

### 19.5 UI

31. **Обязательна ли авторизация в UI** в проде, или он всегда за внутренним периметром?
32. Нужен ли **тёмный режим по умолчанию** или следовать системе?
33. Нужен ли **drag-n-drop нескольких файлов** сразу (батч в UI)?
34. Какой **акцентный цвет** поверх Slate (Radix: indigo / blue / violet / teal)?
35. Нужна ли **страница метрик** в UI, если метрик в v1 нет (19.2 п.16 / решение №16)?
36. Нужны ли **шорткаты** сверх плеера (командная палитра ⌘K в духе Linear)?

### 19.6 Процесс

37. Кто **пишет код** — я целиком, или это спецификация для вашей команды?
38. Нужны ли **ADR** (architecture decision records) по каждому решению из раздела 2?
39. Нужен ли **OpenAPI-файл** как source of truth (сейчас решение №9 — адаптеры в коде)?
40. Есть ли **дедлайн** и что можно выбросить первым, если не влезаем?

---

## 20. Приложение: сводка проверок, которые надо выполнить до кода

- [ ] Прогнать 3–5 моделей на 20 реальных телефонийных записях, замерить WER и RTF.
- [ ] Проверить фактическую погрешность word-таймингов против ручной разметки.
- [ ] Замерить RSS каждой модели после прогрева (для `resources.approx_rss_mb`).
- [ ] Проверить деградацию при 8 кГц → 16 кГц против исходных 16 кГц записей.
- [ ] Проверить качество VAD на записях с музыкой на удержании и с DTMF.
- [ ] Убедиться, что лицензии выбранных весов допускают ваш сценарий использования.
