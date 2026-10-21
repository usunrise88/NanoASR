# NanoASR внутри приложения: `nanoasr managed`

Это руководство для того, кто встраивает NanoASR в приложение (AI-софтфон)
и запускает его в фоне. Оно описывает весь путь от начала до конца:

- откуда брать бинарники и куда их класть;
- как запускать процесс, как с ним разговаривать и как его останавливать;
- как пройти первую настройку;
- где NanoASR хранит базу, модели и логи.

Документ рассчитан на то, чтобы по нему мог работать агент без доступа к
исходникам NanoASR. Полный справочник HTTP API всегда доступен у запущенного
процесса по адресу `GET {url}/docs`.

---

## 0. Коротко

1. Положите содержимое архива `nanoasr-<версия>-<ос>-<арх>-noui` из релиза
   рядом с приложением целиком: бинарник вместе с библиотеками.
2. Запускайте `nanoasr managed -data-dir <каталог> -parent-pid <pid приложения>`
   как дочерний процесс.
   - stdin — канал, который приложение держит открытым и в который ничего не
     пишет.
   - stdout — канал, из которого приложение читает строки JSON.
3. Первая строка stdout — `{"event":"ready","url":"http://127.0.0.1:<порт>","token":"…",…}`.
   Дальше это обычный HTTP API: `Authorization: Bearer <token>`, адрес строго
   `url` из этой строки.
4. Если `"configured": false`, это первый запуск:
   - пользователь выбирает модели;
   - приложение сохраняет выбор через `PATCH /api/v1/managed/settings`;
   - скачивает всё из `missing_models` через `POST /api/v1/models/{id}/download`;
   - перезапускает процесс, если `restart_required: true`.
5. Каждый следующий запуск поднимается уже настроенным.
6. Чтобы остановить процесс, закройте его stdin. Если приложение упадёт,
   ОС закроет stdin сама, и NanoASR завершится.

Права администратора не нужны ни на одном шаге. Всё работает на Windows,
Linux и macOS.

---

## 1. Как это устроено

```
┌──────────────── софтфон ────────────────┐
│                                         │   stdin  (pipe, никогда не пишем)
│  QProcess / CreateProcess / posix_spawn ├──────────────────────────────┐
│                                         │   stdout (pipe, строки JSON)  │
│  HTTP-клиент ──── Bearer <token> ───┐   │◄─────────────────────────┐   │
└─────────────────────────────────────┼───┘                          │   ▼
                                      │                 ┌─────────────┴─────────┐
                                      └── 127.0.0.1:N ─►│  nanoasr managed       │
                                                        │  (дочерний процесс)    │
                                                        └──────────┬────────────┘
                                                                   │
                                                 <data-dir>/ settings.json, nanoasr.db,
                                                             models/, spool/, logs/
```

- **Один процесс** обслуживает и распознавание файлов, и потоковое
  распознавание во время звонка, и управление моделями.
- **Порт выбирает ОС** при каждом запуске. Слушается только `127.0.0.1`,
  поэтому брандмауэр Windows и macOS ничего не спрашивает.
- **Токен выдумывается при каждом запуске** и сообщается только в stdout.
  Он не попадает ни в аргументы командной строки, ни на диск.
- **Настройки** (какие модели, какие функции) хранит сам NanoASR в
  `<data-dir>/settings.json`. Приложение меняет их через API.
- **Время жизни** процесса привязано к приложению: через stdin и через
  `-parent-pid`.
- **Приоритет** процесса ниже обычного, чтобы звонок не терял CPU.
- **Конфиги и окружение игнорируются.** Файл `nanoasr.yaml` и переменные
  окружения `NANOASR_*` в этом режиме не читаются: вся конфигурация берётся из
  флагов и `settings.json`.

---

## 2. Откуда брать бинарники

### 2.1. Релизы GitHub

Релизы лежат на <https://github.com/usunrise88/NanoASR/releases>. Для
встраивания нужны архивы **без веб-UI** (`-noui`):

| Платформа | Архив |
|---|---|
| Windows x64 | `nanoasr-<tag>-windows-amd64-noui.zip` |
| Linux x64 | `nanoasr-<tag>-linux-amd64-noui.tar.gz` |
| macOS Apple silicon | `nanoasr-<tag>-darwin-arm64-noui.tar.gz` |
| macOS Intel | `nanoasr-<tag>-darwin-amd64-noui.tar.gz` |

- **Контрольные суммы.** Рядом лежит `sha256sums.txt`. Сверяйте с ним
  загруженный архив в сборочном конвейере софтфона.
- **Версия.** Зафиксируйте конкретный тег (например `v1.6.0`), а не
  «последний». Проверить версию: `nanoasr version`. Её же сообщает событие
  `ready` (`version`).
- **Совместимость протокола.** За неё отвечает поле `api_version` в событии
  `ready`, сейчас `1`. Если оно больше, чем умеет приложение, запуск лучше
  считать несовместимым.

### 2.2. Сборка из исходников

Если нужного архива нет, его можно собрать (нужны Go 1.25+ и C-компилятор):

```bash
git clone https://github.com/usunrise88/NanoASR && cd NanoASR
git checkout v<версия>
UI=0 GOOS=linux   ARCH=amd64 ./scripts/dist.sh   # на Linux
UI=0 GOOS=windows ARCH=amd64 ./scripts/dist.sh   # на Windows (Git Bash + MinGW gcc)
UI=0 GOOS=darwin  ARCH=arm64 ./scripts/dist.sh   # на Mac с Apple silicon
UI=0 GOOS=darwin  ARCH=amd64 ./scripts/dist.sh   # на Mac с Intel
```

Скрипт печатает имя готового архива. Под macOS собирайте на Mac нужной
архитектуры, кросс-компиляции нет.

### 2.3. Что внутри архива

| ОС | Файлы |
|---|---|
| Windows | `nanoasr.exe`, `onnxruntime.dll`, `sherpa-onnx-c-api.dll`, `sherpa-onnx-cxx-api.dll`, `nanoasr.yaml`, `README.md` |
| Linux | `nanoasr`, `lib/libonnxruntime.so`, `lib/libsherpa-onnx-c-api.so`, `lib/libsherpa-onnx-cxx-api.so`, `nanoasr.yaml`, `README.md`, `nanoasr.service` |
| macOS | `nanoasr`, `lib/libonnxruntime.dylib`, `lib/libsherpa-onnx-c-api.dylib`, `lib/libsherpa-onnx-cxx-api.dylib`, `nanoasr.yaml`, `README.md` |

`nanoasr.yaml`, `README.md` и `nanoasr.service` для встраивания не нужны.
Библиотеки нужны обязательно: распознавание работает через них.

---

## 3. Куда класть

Правило одно: **бинарник и его библиотеки лежат вместе, в той же взаимной
раскладке, что в архиве.**

- На Windows загрузчик ищет DLL рядом с `.exe`.
- На Linux в бинарник зашит путь поиска `$ORIGIN/lib`.
- На macOS в бинарник зашит путь поиска `@executable_path/lib`.

Переименовывать библиотеки и разносить их по разным папкам нельзя.

### Windows

```
<каталог установки софтфона>\
    Softphone.exe
    nanoasr\
        nanoasr.exe
        onnxruntime.dll
        sherpa-onnx-c-api.dll
        sherpa-onnx-cxx-api.dll
```

- **Без админа.** Установка на пользователя в
  `%LOCALAPPDATA%\Programs\<Softphone>` прав администратора не требует.
- **Отдельная папка.** Держите `nanoasr\` отдельной папкой, чтобы её DLL не
  смешивались с DLL софтфона. Это особенно важно для `onnxruntime.dll`: в
  Windows 11 есть своя, более старая копия в System32, и NanoASR должен
  загрузить свою.
- **Подпись.** Подпишите `nanoasr.exe` и DLL своим сертификатом
  Authenticode, как и остальные файлы софтфона. Иначе SmartScreen и антивирусы
  будут относиться к ним настороженно.

### Linux

```
<каталог софтфона>/
    softphone
    nanoasr/
        nanoasr            (chmod +x)
        lib/
            libonnxruntime.so
            libsherpa-onnx-c-api.so
            libsherpa-onnx-cxx-api.so
```

- **Форматы пакетов.** Подходит для AppImage, deb, rpm и tar.gz.
- **Flatpak.** Каталог данных должен быть внутри песочницы, например
  `$XDG_DATA_HOME` приложения: `~/.var/app/<id>/data`.

### macOS

```
Softphone.app/
  Contents/
    MacOS/Softphone
    Helpers/
      nanoasr/
        nanoasr
        lib/
          libonnxruntime.dylib
          libsherpa-onnx-c-api.dylib
          libsherpa-onnx-cxx-api.dylib
```

**Подпись и нотаризация.** Каждый Mach-O подписывается отдельно, своим
Developer ID и с hardened runtime. Сначала подписываются библиотеки, затем
бинарник, затем само `.app`:

```bash
H="Softphone.app/Contents/Helpers/nanoasr"
for f in "$H"/lib/*.dylib "$H"/nanoasr; do
  codesign --force --timestamp --options runtime --sign "Developer ID Application: …" "$f"
done
codesign --force --timestamp --options runtime --sign "Developer ID Application: …" Softphone.app
```

Подробности:

- Особых entitlements nanoasr не нужно: JIT он не использует, а все
  библиотеки подписаны той же командой, так что library validation проходит.
- `--deep` не используйте: подписывайте по одному файлу, как выше.
- Бинарник в архиве подписан ad-hoc. Ваша подпись его просто заменяет.

**App Sandbox** (только если софтфон распространяется через Mac App Store).
Дочерний процесс наследует песочницу. Тогда нужно:

- приложению — `com.apple.security.network.server` и
  `com.apple.security.network.client`;
- `nanoasr` — `com.apple.security.inherit`;
- каталог данных — внутри контейнера приложения.

---

## 4. Где NanoASR хранит данные

Всё, что пишет NanoASR, лежит в одном **каталоге данных**. Он задаётся флагом
`-data-dir`. Рекомендуем всегда передавать его явно, внутри каталога данных
самого софтфона, например:

| ОС | Рекомендуемый `-data-dir` | Каталог по умолчанию, если флаг не передан |
|---|---|---|
| Windows | `%LOCALAPPDATA%\<Softphone>\nanoasr` | `%LOCALAPPDATA%\NanoASR` |
| macOS | `~/Library/Application Support/<Softphone>/nanoasr` | `~/Library/Application Support/NanoASR` |
| Linux | `$XDG_DATA_HOME/<softphone>/nanoasr` (обычно `~/.local/share/…`) | `$XDG_DATA_HOME/nanoasr`, иначе `~/.local/share/nanoasr` |

Каталог должен быть на локальном диске. На Windows это Local, а не Roaming:
модели весят сотни мегабайт и не должны ездить с профилем.

Что в нём лежит:

| Путь | Что это | Можно ли удалять |
|---|---|---|
| `settings.json` | Выбор приложения: модели и функции. Есть с момента первой настройки. | Да: следующий запуск снова будет первым (`configured: false`). |
| `nanoasr.db` (+ `-wal`, `-shm`) | SQLite: история задач распознавания и их результаты, словари hotwords, очередь. | Только при остановленном процессе. Пропадёт история и очередь. |
| `nanoasr.lock` | Блокировка «каталог занят». ОС снимает её, когда процесс завершается, как угодно. | Не нужно трогать. |
| `models/<id>@<ревизия>/` | Скачанные модели, каждая со своим `model.yaml`. | Через API: `DELETE /api/v1/models/{id}`. |
| `spool/` | Загруженное аудио задач, пока они в очереди или в работе. Удаляется, когда задача завершена. | Только при остановленном процессе. |
| `isolations/` | Записи режима выделения голосов, если он включён. | Через API. |
| `logs/nanoasr.log` (+ `nanoasr.log.1`) | Журнал в JSON, по строке на событие. При 64 МБ ротируется, хранится одно предыдущее поколение. | Да. |
| `settings.json.invalid` | Повреждённый файл настроек, отодвинутый в сторону (см. событие `warning`). | Да. |

- **Удаление софтфона.** Удалите весь каталог данных.
- **Обновление NanoASR.** Каталог данных совместим между версиями: база
  мигрирует сама, модели и настройки сохраняются.

---

## 5. Запуск

### 5.1. Командная строка

```
nanoasr managed -data-dir <каталог> -parent-pid <pid софтфона>
```

| Флаг | По умолчанию | Смысл |
|---|---|---|
| `-data-dir DIR` | см. раздел 4 | Каталог данных. Создаётся, если его нет. |
| `-parent-pid PID` | `0` (не следить) | Завершиться, когда процесс PID завершится. Передавайте PID софтфона. |
| `-watch-stdin=BOOL` | `true` | Завершиться, когда закроется stdin. Не выключайте без причины. |
| `-low-priority=BOOL` | `true` | Работать с пониженным приоритетом: nice 10 на Linux и macOS, BELOW_NORMAL на Windows. |
| `-ui` | выкл. | Отдать тестовый веб-интерфейс на `{url}/ui`. Только для сборок с UI, не для `-noui`. Для отладки. |

Рабочий каталог процесса значения не имеет.

### 5.2. Стандартные потоки

- **stdin.** Обязательно канал (pipe), который приложение держит открытым всё
  время жизни процесса и **в который ничего не пишет**. Когда приложение
  закрывает его конец или завершается, NanoASR видит конец потока и штатно
  останавливается.

  > Если запустить NanoASR с stdin, подключённым к `/dev/null` или `NUL`, он
  > сразу увидит конец потока и завершится. Это сделано намеренно: процесс
  > без хозяина не должен оставаться работать.
- **stdout.** Канал. NanoASR пишет туда **только** события, по одной
  JSON-строке на событие (раздел 6). Читайте построчно всё время работы
  процесса, иначе буфер канала переполнится.
- **stderr.** Пишется туда только при аварии рантайма (паника Go, падение в
  нативной библиотеке). Либо читайте его в лог софтфона, либо перенаправьте в
  файл или «в никуда». Не оставляйте канал непрочитанным.

### 5.3. Особенности ОС

**Windows**

- Создавайте процесс с флагом `CREATE_NO_WINDOW`, чтобы не мелькала консоль.
- Желательно, хотя и не обязательно: поместите процесс в Job Object с
  `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`. Тогда ОС убьёт его даже в том редком
  случае, когда stdin и `-parent-pid` не сработали. Прав администратора не
  нужно.
- Сигналов в духе SIGTERM у процесса без консоли нет. Штатная остановка —
  закрыть stdin (раздел 8).

**macOS и Linux.** Ничего особенного. SIGTERM и SIGINT тоже работают как
штатная остановка.

### 5.4. Пример: Qt (C++)

```cpp
// nanoasrprocess.h — минимальная обвязка
#include <QProcess>
#include <QJsonDocument>
#include <QJsonObject>
#include <QCoreApplication>
#include <QStandardPaths>
#include <QDir>
#ifdef Q_OS_WIN
#include <windows.h>   // CREATE_NO_WINDOW
#endif

class NanoAsrProcess : public QObject {
    Q_OBJECT
public:
    QString url, token;

    void start() {
        const QString appDir = QCoreApplication::applicationDirPath();
#if defined(Q_OS_WIN)
        const QString exe = appDir + "/nanoasr/nanoasr.exe";
#elif defined(Q_OS_MACOS)
        const QString exe = appDir + "/../Helpers/nanoasr/nanoasr";
#else
        const QString exe = appDir + "/nanoasr/nanoasr";
#endif
        const QString dataDir =
            QStandardPaths::writableLocation(QStandardPaths::AppLocalDataLocation) + "/nanoasr";

        p.setProgram(exe);
        p.setArguments({"managed", "-data-dir", QDir::toNativeSeparators(dataDir),
                        "-parent-pid", QString::number(QCoreApplication::applicationPid())});
        p.setProcessChannelMode(QProcess::SeparateChannels);
#if defined(Q_OS_WIN)
        p.setCreateProcessArgumentsModifier([](QProcess::CreateProcessArguments *a) {
            a->flags |= CREATE_NO_WINDOW;
        });
#endif
        connect(&p, &QProcess::readyReadStandardOutput, this, &NanoAsrProcess::onStdout);
        connect(&p, &QProcess::readyReadStandardError, this, [this] {
            qWarning() << "nanoasr stderr:" << p.readAllStandardError();
        });
        connect(&p, &QProcess::finished, this, &NanoAsrProcess::onFinished);
        p.start(QIODevice::ReadWrite);   // stdin остаётся открытым — в него ничего не пишем
    }

    void stop() {                         // штатная остановка
        p.closeWriteChannel();            // закрыть stdin
        if (!p.waitForFinished(15000)) p.kill();
    }

signals:
    void ready(QString url, QString token, bool configured, bool restartRequired);
    void restartRequired();
    void fatal(QString code, QString message);

private:
    QProcess p;

    void onStdout() {
        while (p.canReadLine()) {
            const QJsonObject ev = QJsonDocument::fromJson(p.readLine()).object();
            const QString name = ev["event"].toString();
            if (name == "ready") {
                url = ev["url"].toString();
                token = ev["token"].toString();
                emit ready(url, token, ev["configured"].toBool(), ev["restart_required"].toBool());
            } else if (name == "restart_required") {
                emit restartRequired();
            } else if (name == "fatal") {
                emit fatal(ev["code"].toString(), ev["message"].toString());
            }
        }
    }
    void onFinished(int code, QProcess::ExitStatus st) { /* см. раздел 8: перезапуск с паузой */ }
};
```

HTTP-запросы делайте через `QNetworkAccessManager`:

- адрес — ровно `url` из события `ready`;
- заголовок — `Authorization: Bearer <token>`.

Для SSE-потоков (прогресс загрузки модели, события задачи) читайте ответ
построчно в `readyRead`.

### 5.5. Пример: Python (для проверки руками)

```python
import json, subprocess, os, urllib.request
p = subprocess.Popen(["./nanoasr/nanoasr", "managed", "-data-dir", "/tmp/nanoasr-data",
                      "-parent-pid", str(os.getpid())],
                     stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
ready = json.loads(p.stdout.readline())
req = urllib.request.Request(ready["url"] + "/api/v1/managed",
                             headers={"Authorization": "Bearer " + ready["token"]})
print(json.load(urllib.request.urlopen(req)))
p.stdin.close()            # остановка
print(p.stdout.read().decode(), p.wait())
```

---

## 6. События на stdout

Каждая строка — один JSON-объект, первое поле всегда `event`. Незнакомые
события и поля игнорируйте: в будущих версиях их может стать больше.

### `ready` — процесс принимает запросы

```json
{"event":"ready","url":"http://127.0.0.1:53124","port":53124,
 "token":"sk-nanoasr-…","pid":4242,"version":"v1.6.0","api_version":1,
 "data_dir":"C:\\Users\\u\\AppData\\Local\\Softphone\\nanoasr",
 "configured":true,"restart_required":false,"missing_models":[]}
```

| Поле | Смысл |
|---|---|
| `url` | Базовый адрес API. Используйте его как есть, с `127.0.0.1`. |
| `token` | Ключ на этот запуск. После перезапуска будет другой. |
| `configured` | `false` — первый запуск, настроек ещё нет (раздел 9). |
| `restart_required` | Сразу после старта всегда `false`. |
| `missing_models` | Модели, которые нужны по текущим настройкам, но не скачаны. |

Событие приходит быстро, за доли секунды: основная модель загружается в
память уже в фоне. Запрос, пришедший раньше, просто дождётся её загрузки.
Разумный таймаут ожидания `ready` — 30 секунд.

### `restart_required` — следующий запуск будет обслуживать больше

```json
{"event":"restart_required","features":["realtime"]}
```

Приходит после того, как изменились настройки или скачалась модель, которую
загружают только при старте (VAD, потоковая модель). Перезапустите процесс,
когда удобно, например когда нет звонка (раздел 8).

### `warning`

```json
{"event":"warning","code":"settings_reset","message":"…"}
```

`settings_reset`: `settings.json` не удалось прочитать. Файл отодвинут в
`settings.json.invalid`, и запуск считается первым (`configured: false`).

### `stopping` — процесс останавливается

```json
{"event":"stopping","reason":"stdin closed"}
```

`reason` бывает `stdin closed`, `parent exited` или `signal`.

### `fatal` — процесс не может работать и сейчас завершится с кодом 1

```json
{"event":"fatal","code":"data_dir_locked","message":"…"}
```

| `code` | Что случилось | Что делать |
|---|---|---|
| `data_dir_locked` | В этом каталоге данных уже работает другой NanoASR. | Не перезапускать в цикле. Найти и остановить другой экземпляр (второе окно софтфона?), либо дать каждому свой `-data-dir`. |
| `data_dir_unavailable` | Каталог данных нельзя создать или в него нельзя писать. | Проверить путь и права. |
| `invalid_arguments` | Неизвестный флаг. | Исправить командную строку. |
| `settings_invalid` | Сохранённые настройки не превращаются в рабочую конфигурацию. | Исправить через API или удалить `settings.json`. |
| `listen_failed` | Не удалось занять порт на 127.0.0.1. | Перезапустить. Если повторяется, проверить защитное ПО. |
| `parent_gone` | Процесса из `-parent-pid` уже нет. | Передать правильный PID. |
| `server_failed` | Сервер не собрался или упал. | Подробности в `message` и в `logs/nanoasr.log`. |

Код выхода процесса: `0` — штатная остановка, `1` — ошибка (перед ней было
событие `fatal`, если процесс успел его написать).

---

## 7. Авторизация и безопасность

- **Токен.** Каждый запрос, кроме `/healthz`, `/readyz` и `/docs`, требует
  `Authorization: Bearer <token>`. Токен из `ready` административный: с ним
  можно всё, включая управление моделями. Не пишите его в логи софтфона и не
  сохраняйте на диск. После перезапуска он всё равно другой.
- **Заголовок Host.** NanoASR отвечает только на запросы с `Host` равным
  `127.0.0.1:<порт>`, `localhost:<порт>` или `[::1]:<порт>`. Любой другой
  получает `421`. Это защита от DNS rebinding: веб-страница в браузере
  пользователя не сможет добраться до API. Обычный HTTP-клиент, который ходит
  по `url` из `ready`, ставит правильный `Host` сам.
- **Ошибки** приходят в формате `application/problem+json`:

  ```json
  {"type":"…/errors/invalid_request","title":"Bad Request","status":400,
   "detail":"realtime_model: gigaam-v3-ctc-punct-ru is not a streaming recognition model",
   "code":"invalid_request","param":"realtime_model"}
  ```

  Ориентируйтесь на `code` и `param`. `detail` написан для человека.

---

## 8. Жизненный цикл

### Старт

1. Запустить процесс (раздел 5).
2. Дождаться события `ready`; таймаут 30 секунд.
   - Если пришло `fatal` или процесс завершился раньше, показать ошибку
     (раздел 6).
3. Если `configured == false`, пройти первую настройку (раздел 9).
4. Если `missing_models` не пуст, модели удалили или настройки сбросились.
   Предложить пользователю скачать их (раздел 9.3).

### Штатная остановка

1. Закрыть stdin процесса. В Qt это `closeWriteChannel()`.
2. NanoASR пишет `stopping`, перестаёт принимать запросы и даёт текущим
   задачам до 5 секунд. Задачи, которые не успели, остаются в очереди в базе
   и продолжатся при следующем запуске.
3. Ждать выхода до 15 секунд, затем убить процесс. Ничего не сломается: база
   SQLite в режиме WAL переживает и это.

### Перезапуск (после `restart_required`)

Остановить процесс, как описано выше, дождаться выхода, запустить снова.

- Не запускайте новый процесс, пока не вышел старый: он получит
  `data_dir_locked`.
- Перезапуск занимает около секунды плюс загрузку моделей в фоне.
- Активная потоковая сессия звонка при перезапуске оборвётся. Поэтому
  перезапускайте, когда звонка нет.

### Аварийный выход

Если процесс завершился без события `stopping` или с кодом ≠ 0 и без
`fatal`, перезапускайте с нарастающей паузой: 1, 2, 4, 8, … до 60 секунд.
После минуты стабильной работы паузу можно сбросить. При
`fatal: data_dir_locked` повторять не надо, а `fatal: data_dir_unavailable`
повторять бессмысленно.

### Если упал софтфон

Делать ничего не нужно. ОС закрывает канал stdin, NanoASR видит это и
завершается. Если по какой-то причине не увидит, его остановит `-parent-pid`,
а на Windows ещё и Job Object. Сирот не остаётся.

### Два окна софтфона

Один каталог данных — один процесс NanoASR. Второй экземпляр софтфона либо
пользуется процессом первого (это задача самого софтфона, через свой IPC),
либо запускает свой NanoASR со своим `-data-dir`.

---

## 9. Первый запуск и настройка

Первый запуск отличается полем `"configured": false` в `ready`. Процесс уже
работает, но ничего не умеет: моделей нет, и модель по умолчанию не выбрана.

### 9.1. Каталог моделей

```
GET {url}/api/v1/catalog
```

```json
{"data":[
 {"id":"gigaam-v3-ctc-punct-ru","display_name":"GigaAM v3 CTC punct (Russian)",
  "kind":"asr","streaming":false,"languages":["ru"],"license":"…",
  "state":"absent","size_bytes":163282594,"rss_mb":900,
  "capabilities":{"word_timestamps":true,"hotwords":true,…}},
 …]}
```

| Поле | Смысл |
|---|---|
| `kind` | `asr` — распознавание, `vad`, `diarization`, `embedding`, `tse` (выделение голоса), `segmentation`. |
| `streaming` | `true` — потоковая модель для распознавания во время звонка. |
| `state` | `absent` — не скачана, `downloading` — качается сейчас, `downloaded` — на диске. |
| `size_bytes` | Сколько скачивать. |
| `rss_mb` | Примерно сколько памяти занимает загруженная модель. |

Что выбрать для русскоязычного софтфона:

| Назначение | Модель | Скачивать | Память |
|---|---|---|---|
| Распознавание записей (с пунктуацией) | `gigaam-v3-ctc-punct-ru` | 163 МБ | ~900 МБ |
| То же, чуть точнее и медленнее | `gigaam-v3-rnnt-punct-ru` | 170 МБ | ~1.1 ГБ |
| Распознавание во время звонка, телефония 8 кГц | `t-one-ctc-ru` (streaming) | 128 МБ | ~600 МБ |
| Кто говорит (диаризация) | `nemotron-3-diarization` (или `-int8`: меньше и легче) | 176 МБ (89 МБ) | ~500 МБ (~210 МБ) |
| Детектор речи — нужен всегда, выбирать не надо | `silero-vad-v5` | 1 МБ | 16 МБ |

Потоковые модели пунктуацию не ставят. Окончательный текст звонка с
пунктуацией получают, распознав запись после звонка.

### 9.2. Сохранить выбор

```
PATCH {url}/api/v1/managed/settings
Content-Type: application/json

{"default_model":"gigaam-v3-ctc-punct-ru","realtime_model":"t-one-ctc-ru","diarization":true}
```

Передаются только поля, которые надо изменить. Остальные остаются как были.
Неизвестное поле — ошибка `400` (опечатки не проглатываются). Чтобы очистить
строку, передайте `""`.

| Поле | Тип | По умолчанию | Смысл |
|---|---|---|---|
| `default_model` | string | `""` | Модель для запросов, в которых модель не указана. Должна быть `kind: asr`. |
| `realtime_model` | string | `""` (выкл.) | Потоковая модель для `/v1/realtime`. `""` выключает потоковое распознавание. |
| `diarization` | bool | `false` | Разрешить `diarize=true` (кто говорит). |
| `diarization_model` | string | `""` (= `nemotron-3-diarization`) | Модель диаризации, `kind: diarization`. |
| `isolation` | bool | `false` | Выделение голосов. Требует `diarization: true`. |
| `threads` | int | `0` (авто: четверть ядер, 1–4) | Потоков на одно распознавание, 0–64. |
| `max_concurrent` | int | `0` (= 1) | Сколько файлов распознавать одновременно, 0–16. |
| `ffmpeg_path` | string | `""` | Путь к ffmpeg для сжатых форматов (mp3, opus, m4a…). Без него принимаются только WAV и сырой PCM. Файл должен существовать. |
| `mirrors` | string[] | `[]` | HTTPS-зеркала для скачивания моделей. |
| `history_ttl` | string | `""` (= `720h`, 30 дней) | Сколько хранить завершённые задачи, в формате Go: `168h`, `720h`. |
| `log_level` | string | `""` (= `info`) | `debug`, `info`, `warn` или `error`. |

Ответ — статус целиком (раздел 9.4). Настройки сохраняются сразу, а
применяются со следующего запуска. Что изменится, говорит `features` в
ответе.

### 9.3. Скачать модели

Список того, что нужно скачать, — в `missing_models` ответа на `PATCH` или
`GET /api/v1/managed`. Туда входит и `silero-vad-v5`.

```
POST {url}/api/v1/models/{id}/download
```

Ответ — поток Server-Sent Events:

```
id: 1
event: progress
data: {"model_id":"t-one-ctc-ru","downloaded":10485760,"total":128000000,"percent":8.2,"done":false}

…

id: 37
event: done
data: {"model_id":"t-one-ctc-ru","downloaded":0,"total":0,"percent":100,"done":true}

event: done
data: {"reason":"terminal"}
```

- **Успех и ошибка.** Успех — событие `done` с `"done":true` без поля
  `error`. Ошибка — `done` с `"error":"…"`. Последнее событие
  `{"reason":"terminal"}` означает конец потока.
- **Обрыв потока.** Если закрыть поток (например, пользователь закрыл окно),
  загрузка продолжится в фоне. Повторный `POST` на ту же модель
  присоединится к идущей загрузке, а не начнёт новую.
- **Отмена:** `DELETE {url}/api/v1/models/{id}/download` → `204`. Всем, кто
  следит за загрузкой, придёт `done` с ошибкой. Недокачанное с диска
  удаляется.
- **Несколько загрузок.** Одновременно идут две, остальные ждут очереди.
- **Проверка.** Каждый файл проверяется по sha256 из каталога. Повреждённое
  не устанавливается.
- **Сеть.** Модели скачиваются с GitHub и Hugging Face по HTTPS. Если они
  недоступны, задайте `mirrors`.

Модель **никогда не скачивается сама**. Запрос на распознавание моделью,
которой нет на диске, получит `404 model_not_found` с подсказкой, что её
нужно скачать.

### 9.4. Статус и перезапуск

```
GET {url}/api/v1/managed
```

```json
{
  "version":"v1.6.0","api_version":1,
  "configured":true,"restart_required":true,
  "data_dir":"…","models_dir":"…/models",
  "settings":{"default_model":"gigaam-v3-ctc-punct-ru","realtime_model":"t-one-ctc-ru","diarization":true,…},
  "features":{
    "vad":           {"state":"restart","models":["silero-vad-v5"]},
    "transcription": {"state":"ready",  "models":["gigaam-v3-ctc-punct-ru"]},
    "realtime":      {"state":"restart","models":["t-one-ctc-ru"]},
    "diarization":   {"state":"ready",  "models":["nemotron-3-diarization"]},
    "isolation":     {"state":"off"}
  },
  "missing_models":[]
}
```

| `state` | Смысл |
|---|---|
| `off` | В настройках не включено. |
| `missing` | Включено, но не все модели скачаны (они перечислены в `missing`). |
| `ready` | Работает в этом процессе. |
| `restart` | Будет работать после перезапуска: настройка изменилась или модель скачалась уже после старта. |

Когда `missing_models` пуст, а `restart_required: true`, перезапустите процесс
(раздел 8). После перезапуска все функции `ready`, а `configured: true`.

Распознавание записей и диаризация подхватывают скачанную модель сразу, без
перезапуска. Детектор речи и потоковая модель загружаются только при старте:
для них нужен перезапуск, и о нём сообщит событие `restart_required`.

### 9.5. Вся настройка — одна последовательность

```
start → ready{configured:false}
GET  /api/v1/catalog                         → показать пользователю выбор
PATCH /api/v1/managed/settings {…выбор…}     → missing_models: [vad, asr, rt, diar]
POST /api/v1/models/<каждая>/download (SSE)  → прогресс в UI
GET  /api/v1/managed                         → missing_models: [], restart_required: true
закрыть stdin → дождаться выхода → start → ready{configured:true, missing_models:[]}
```

---

## 10. Последующие запуски

`ready` приходит с `"configured": true`. Если `missing_models` пуст, всё
готово, можно работать.

Если `missing_models` не пуст (пользователь удалил модель, или каталог данных
перенесли без моделей), предложите скачать недостающее (раздел 9.3).
Остальное продолжает работать.

### Смена моделей из настроек софтфона

1. `PATCH /api/v1/managed/settings` с новой моделью.
2. Если её нет на диске — `POST …/download`.
3. Когда придёт `restart_required`, перезапустить процесс в удобный момент.

Модели, скачанные на диск:

```
GET {url}/api/v1/models                     # что скачано и что сейчас в памяти
DELETE {url}/api/v1/models/{id}             # удалить с диска → 204
```

Удалить нельзя (`409 model_in_use`), пока модель:

- обслуживает запрос;
- скачивается;
- загружена процессом при старте (VAD, потоковая модель, диаризация,
  выделение голоса).

Порядок такой: сначала смените настройку, перезапустите процесс, потом
удаляйте.

---

## 11. Работа с API

Полный справочник с параметрами — `GET {url}/docs`, без токена. Ниже то, что
нужно софтфону.

### 11.1. Распознать запись (синхронно)

```
POST {url}/api/v1/transcribe
Content-Type: multipart/form-data

file=@call.wav
diarize=true                # кто говорит (если включено в настройках)
channel_mode=split          # стерео-запись звонка: каждый канал отдельно
word_timestamps=true
```

Основные параметры:

| Параметр | Смысл |
|---|---|
| `file` | Аудио. WAV (PCM 8/16/24/32 бит, float32, A-law, μ-law) — с любой частотой и числом каналов: NanoASR сам ресемплирует. Сжатые форматы — только с `ffmpeg_path`. |
| `model` | Модель; по умолчанию `default_model`. |
| `channel_mode` | `downmix` (по умолчанию) — смешать каналы; `split` — каждый канал отдельно, сегменты помечены `channel`. Для звонка удобно писать стерео: свой микрофон слева, собеседник справа — и распознавать с `split`. |
| `diarize`, `num_speakers` | Разделить по говорящим; число говорящих, если известно. |
| `hotwords` | Слова и фразы, которые нужно узнавать увереннее (имена, термины): через запятую или повторяя поле. Работает с моделями, у которых `capabilities.hotwords: true`. |
| `word_timestamps` | Время каждого слова. |

Ответ:

```json
{"id":"…","model":"gigaam-v3-ctc-punct-ru","language":"ru","duration":42.1,
 "text":"Добрый день. Это компания …",
 "segments":[{"id":0,"start":0.32,"end":3.9,"text":"Добрый день.","channel":0,
              "speaker":"spk_0","words":[{"word":"Добрый","start":0.32,"end":0.61},…]}],
 "speakers":[{"id":"spk_0","total_speech":20.4,"segments":7}],
 "warnings":[]}
```

`warnings` — то, что было запрошено, но не сделано, с причиной. Например,
`diarization_unavailable`, если модель диаризации не скачана.

### 11.2. Распознать запись в фоне (очередь)

Для длинных записей используйте очередь:

1. Поставить задачу: `POST {url}/api/v1/jobs` с теми же полями. Ответ —
   `202` и `{"id":"…","status":"queued",…}`.
2. Следить за ней: `GET {url}/api/v1/jobs/{id}/events`. Это SSE: события
   несут статусы `queued`, `running` (с `stage` и `percent`), затем
   `succeeded`, `failed` или `canceled`.
3. Забрать результат: `GET {url}/api/v1/jobs/{id}`, поле `result`.
4. Отменить при необходимости: `DELETE {url}/api/v1/jobs/{id}`.

Задачи и их результаты хранятся в `nanoasr.db` `history_ttl` (по умолчанию
30 дней) и переживают перезапуски. Задача, прерванная остановкой процесса,
продолжится при следующем запуске.

Говорящим можно дать имена:

```
PATCH {url}/api/v1/jobs/{id}
{"speaker_names":{"spk_0":"Оператор","spk_1":"Клиент"}}
```

### 11.3. Распознавание во время звонка (потоково)

WebSocket `ws://127.0.0.1:<порт>/v1/realtime?intent=transcription` с
заголовком `Authorization: Bearer <token>`. Работает, когда
`features.realtime.state == "ready"`.

Протокол — протокол событий OpenAI Realtime API в режиме транскрипции.
Обычно на каждую сторону звонка открывается своя сессия.

1. Клиент → `{"type":"transcription_session.update","session":{"input_audio_format":"g711_ulaw"}}`.

   Форматы: `g711_ulaw`, `g711_alaw` (8 кГц, как в телефонии), `pcm16`
   (24 кГц), либо `{"type":"audio/pcm","rate":16000}` для 16-битного PCM с
   любой частотой от 8000 до 48000 Гц.
2. Клиент → бинарные кадры с аудио в объявленном формате, по 20–100 мс.
   Можно и JSON `{"type":"input_audio_buffer.append","audio":"<base64>"}`.
3. Сервер → события:
   - `conversation.item.input_audio_transcription.delta` — промежуточный
     текст. Полная текущая гипотеза лежит в поле `nanoasr.text`: потоковый
     декодер иногда переписывает уже отправленные слова.
   - `…transcription.completed` — окончательный текст фразы. Он приходит
     после паузы около 1.2 с.
   - `input_audio_buffer.speech_started` и `.speech_stopped`.
   - `error`.
4. Конец звонка: `{"type":"input_audio_buffer.commit"}`, затем закрыть сокет.

Сколько сессий одновременно, NanoASR решает сам по числу ядер. Лишняя сессия
получит отказ. Простаивающая сессия закрывается через 60 секунд без аудио.

### 11.4. Совместимость с OpenAI

`POST {url}/v1/audio/transcriptions` принимает те же поля, что OpenAI
(`file`, `model`, `language`, `response_format`: `json`, `text`, `srt`,
`vtt`, `verbose_json`). Подходит, если в софтфоне уже есть клиент OpenAI:
достаточно поменять базовый URL и ключ.

### 11.5. Здоровье

- `GET {url}/healthz` → `200` и версии. Без токена.
- `GET {url}/readyz` → `200` или `503`, если очередь переполнена.

---

## 12. Ресурсы

- **CPU.**
  - Приоритет понижен.
  - Одно распознавание использует четверть ядер (от 1 до 4 потоков; меняется
    через `threads`).
  - По умолчанию одновременно распознаётся один файл (`max_concurrent`).
  - Потоковая сессия звонка легче записи: несколько сессий укладываются в
    одно ядро.
- **Память.**
  - Модель распознавания записей загружается при первом запросе или при старте
    и выгружается после 15 минут простоя.
  - Одновременно в памяти держится одна такая модель.
  - Потоковая модель, VAD и (после первого запроса) диаризация живут, пока
    живёт процесс.
  - Ориентиры: `gigaam-v3-ctc-punct-ru` ~0.9 ГБ, `t-one-ctc-ru` ~0.6 ГБ,
    `nemotron-3-diarization` ~0.5 ГБ, сам процесс ~100 МБ.
  - Одна загруженная запись занимает ~4 байта на отсчёт 16 кГц, то есть
    ~230 МБ на час.
- **Диск.** Модели — сколько скачано (см. `size_bytes` в каталоге). Плюс
  очередь загруженных файлов в `spool/` (не больше 4 ГБ) и база с историей.
- **Сеть.** Только скачивание моделей по явной команде. Сам NanoASR никуда
  не ходит.

---

## 13. Обновление NanoASR вместе с софтфоном

1. Остановить процесс (раздел 8).
2. Заменить папку `nanoasr/` целиком: бинарник и библиотеки должны быть из
   одного архива.
3. Запустить. Каталог данных подхватится как есть.
4. Проверить `api_version` в `ready`: протокол меняется, только если это
   число растёт.

---

## 14. Диагностика

- **Журнал:** `<data-dir>/logs/nanoasr.log`, по JSON-строке на запись.
  - Первая строка каждого запуска — `starting nanoasr` с версией, потоками и
    лимитами.
  - Затем пишутся загрузки моделей и по строке на каждый HTTP-запрос: путь,
    статус, время. Тела запросов и тексты распознавания в журнал не пишутся.
  - Подробнее — `log_level: debug`.
- **Версия и библиотеки:** `nanoasr version`. Печатает версии
  nanoasr, sherpa-onnx и onnxruntime и путь, откуда загружен onnxruntime.
  Путь должен вести в папку `nanoasr/` софтфона.
- **Справочник:** `GET {url}/docs` — все маршруты этого процесса.

| Симптом | Причина |
|---|---|
| Процесс сразу выходит, `stopping` с `stdin closed` | stdin не канал (подключён к `/dev/null`/`NUL`) или его закрыли. |
| `fatal: data_dir_locked` | Второй процесс на том же каталоге данных; старый ещё не вышел. |
| `401` | Нет заголовка `Authorization` или токен от прошлого запуска. |
| `421` | Запрос не на `127.0.0.1:<порт>` (другое имя хоста, прокси). Обращайтесь строго по `url`. |
| `404 model_not_found` при распознавании | Модель не скачана; скачать (раздел 9.3). |
| `415 unsupported_media_type` | Сжатый формат без `ffmpeg_path`. Шлите WAV или задайте ffmpeg. |
| `warnings: diarization_unavailable` | Модель диаризации не скачана или `diarization` выключен. |
| `/v1/realtime` → 404 | Потоковая модель не выбрана, не скачана или процесс не перезапущен после её загрузки (`features.realtime.state`). |
| На Windows: процесс не стартует, код `0xC0000135` | Рядом с `nanoasr.exe` нет DLL из архива. |
| На macOS: «library not loaded: @rpath/libonnxruntime.dylib» | Нет `lib/` рядом с бинарником или подпись сломала одну из библиотек. |

---

## 15. Чек-лист интеграции

- [ ] Архив нужной версии и платформы сверен с `sha256sums.txt`; папка
      `nanoasr/` лежит рядом с софтфоном целиком, вместе с библиотеками.
- [ ] Windows: `nanoasr.exe` и DLL подписаны. macOS: каждый Mach-O подписан
      с `--options runtime`, приложение нотаризовано.
- [ ] Запуск: `managed -data-dir <свой каталог> -parent-pid <свой pid>`;
      stdin — открытый канал без записи; stdout читается построчно; stderr
      читается или отброшен; на Windows `CREATE_NO_WINDOW` (и Job Object).
- [ ] Ожидание `ready` с таймаутом; обработка `fatal` по коду.
- [ ] Все запросы на `url` из `ready` с `Bearer <token>`; токен нигде не
      сохраняется.
- [ ] Первый запуск (`configured: false`): каталог → выбор →
      `PATCH settings` → загрузка `missing_models` с прогрессом → перезапуск.
- [ ] Событие `restart_required` → перезапуск, когда нет звонка.
- [ ] Остановка: закрыть stdin, ждать до 15 с, затем убить.
- [ ] Аварийный выход → перезапуск с нарастающей паузой; при
      `data_dir_locked` — без повторов.
- [ ] Удаление софтфона удаляет каталог данных.
