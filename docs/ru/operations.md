Перевод файла ../operations.md; английский оригинал является авторитетным.

# Эксплуатация

Справочник «второго дня»: куда идут логи, как работают мониторинг ресурсов
и алерты, обновление через `koto update`, удаление koto через
`koto uninstall`, и сообщения об ошибках, с которыми вы столкнётесь
вероятнее всего. Каждое утверждение называет исходный файл и символ, из
которого оно прочитано; проверяйте grep-ом по символу.

Смежные страницы: [configuration.md](configuration.md) (переменные
окружения, включая `KOTO_REPO`, `KOTO_REPO_URL`, `KOTO_HOST_MEM_MIB`),
[ctl-reference.md](ctl-reference.md) (команды `logs`, `resources`),
[getting-started.md](getting-started.md) (установка, ошибки preflight),
[groups-goals-schedules.md](groups-goals-schedules.md) (ошибки очереди и
целей), [acl-and-auth.md](acl-and-auth.md) (ошибки отказа в доступе).

## 1. Логи

Есть три отдельные поверхности логов.

### 1.1 Лог демона

- Каждая строка лога демона проходит через одну воронку, `logDeliver`
  (`daemon/events.go`), вызываемую из `emitLog` / `emitLogG` / `emitLogf` /
  `emitLogfG` / `emitLogfQuiet`.
- Каждая строка пишется в **stderr** как `[<level>] <subsystem>: <msg>`
  (`fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", ...)` в `logDeliver`).
  Под установленным юнитом systemd этот stderr попадает в journal;
  финальный экран мастера указывает на `journalctl -u koto -f` («daemon
  logs»), а сбой smoke-теста — на `journalctl -u koto -n 50`
  (`stepHandoff`, `daemon/setup_steps.go`).
- Сообщения вычищаются `sanitize`, затем `flattenInline`, так что одна
  запись лога — всегда одна строка (`logDeliver`).
- Демон держит в памяти последние **200** строк (`logRingMax`,
  `daemon/events.go`). Новый стрим `SubscribeLogs` (`koto ctl logs`,
  лог-панель TUI) сначала получает это кольцо, потом живые строки
  (`SubscribeLogs`, `daemon/grpc_server.go`). У каждого подписчика —
  канал на 256 записей; отставший подписчик получает строку `warn` от
  подсистемы `log`: `log stream fell behind: %d line(s) were not delivered
  to this subscriber` (`logDeliver`).
- Ограниченные (не-админские) идентичности видят только строки,
  отнесённые к видимой им группе; строки без атрибуции (группа `""`)
  удерживаются (`logVisible`, `daemon/grpc_server.go`). См.
  [acl-and-auth.md](acl-and-auth.md).

### 1.2 Строки уровня error становятся уведомлениями оператору

(`daemon/logalert.go`)

- Каждая строка уровня `error`, испущенная через `emitLogG`, также
  пересылается как уведомление **высокой** серьёзности группе `main`
  (`forwardLogAlert`, доставка через `notifyDeliver(ctlMainGroup, "high",
  ...)`). `warn` и ниже остаются только в логе.
- Формат заголовка: `<LEVEL> <subsystem>` плюс ` [<group>]` при атрибуции,
  например `ERROR fc [dev]` (`logAlertTitle`).
- Ограничители частоты: token bucket на пару (subsystem, group) с burst 5 и
  пополнением раз в минуту (`logAlertBurst`, `logAlertRefill`), и
  флот-уровневый bucket с burst 20, раз в 10 с (`logAlertGlobalBurst`,
  `logAlertGlobalRefill`). Подавленные строки остаются в логе; теряется
  только баннер.
- `emitLogfQuiet` пропускает пересылку; ресурсные алерты используют его,
  потому что ставят в очередь собственное уведомление
  (`resNotifyOperator`).
- Недоставленные маркеры уведомлений каждой группы ограничены 32
  (`notifyQueueMax`, `daemon/logtail.go`). Очередь — в памяти, поэтому
  перезапуск демона теряет недоставленные маркеры (комментарий над
  `notifyQueueMax`).

### 1.3 Пер-групповые транскрипты

(`daemon/logtail.go`)

- У каждой группы есть групповой поток `<workspace>/.cs/log`
  (`groupLogPath`) и по одному потоку на слот конкурентности
  `<workspace>/.cs/log.<n>` (`slotLogPath`); `logPaths` перечисляет
  групповой поток плюс `groupSlots` слот-потоков. `vol(g)` — это
  workspace-монтирование группы на хосте.
- History читает максимум последние 4 MiB каждого потока
  (`historyTailCap`), возвращает максимум 8 MiB на ответ
  (`historyBytesMax`) и урезает размер страницы до 5000 событий
  (`historyLimitMax`). Прокрутка назад за пределы 4 MiB окна заканчивается
  пустой страницей.
- Частичная (незавершённая) строка буферизуется до 8 MiB
  (`tailMaxPartial`); сверх этого уцелевший хвост получает префикс
  `…[line truncated] ` (`tailTruncMark`).

### 1.4 Метрики прокси

- LLM-прокси дописывает одну JSON-запись на каждый завершённый запрос в
  `<state>/metrics.jsonl` (`METRICS`, задаётся в `daemon/daemon.go`;
  `metricsAppend`, `daemon/proxy.go`).
- Ротация на 256 MiB (`metricsMaxBytes`), с сохранением одного предыдущего
  поколения как `metrics.jsonl.1`, так что худший случай на диске — около
  двух потолков. Неудачная ротация логируется на уровне error: `metrics
  rotation failed (%v): %s is past its %d MiB ceiling and still growing`.

## 2. Мониторинг ресурсов

(`daemon/resources.go`, wire-типы в `daemon/wire/wire.go`)

### 2.1 Сбор

- `resourcesLoop` снимает один образец при старте и затем каждые **30 с**
  (`resSampleInterval`). Цифры приходят с хоста (`stat`, `statfs`,
  `/proc/<pid>/stat`), поэтому зависший или остановленный гость всё равно
  отчитывается.
- Исключение: память гостя и использование его собственного диска читаются
  изнутри каждого **запущенного** гостя ограниченным exec-ом
  `resGuestProbe` (`stat -f ... /workspace; cat /proc/meminfo`). У exec —
  таймаут 5 с (`resGuestExecTimeout`), параллельно не более 8
  (`resGuestExecPar`), и у всей выборки — дедлайн 20 с
  (`resSweepDeadline`). Остановленные VM для этого никогда не загружаются.
  Если запущенный гость пропускает обновление, его последнее чтение
  сохраняется до 3 интервалов сэмплирования (`resGuestMaxAge`). Чтение
  остановленной VM сбрасывается сразу. В обоих случаях дальше оно читается
  как неизвестное (0) (`resGuestRetain`).
- Пер-групповая история: 120 образцов (`resRingLen`), то есть 1 ч при 30 с.
- Снимки кэшируются на 1 с (`resSnapTTL`), так что более частый опрос
  возвращает тот же ответ.
- Цифры файловой системы хоста — `statfs` на `ROOT` (`<state>/groups`), с
  `Bavail` (блоки, свободные для не-root пользователей) в качестве счётчика
  свободного (`hostFSStats`).

### 2.2 Поля (`koto ctl resources`, внутригостевой глагол `resources`)

Обе плоскости строятся из одного `resourcesSnapshot`
(`resourcesCtlResp`). JSON-имена из `wire.GroupResources` /
`wire.HostResources`:

- По группе: `group`, `running`, `alloc_bytes`, `declared_bytes`,
  `alloc_pct`, `growth_bytes_per_hour`, `growth_span_seconds`, `rss_bytes`,
  `cpu_pct`, `vcpus`, `mem_mib`, `guest_disk_total_bytes`,
  `guest_disk_avail_bytes`, `guest_disk_used_bytes`, `guest_disk_used_pct`,
  `guest_mem_total_bytes`, `guest_mem_avail_bytes`, `guest_mem_used_pct`,
  `mem_committed_mib`.
- Хост: `fs_total_bytes`, `fs_free_bytes`, `fs_used_pct`,
  `alloc_total_bytes`, `provisioned_bytes`, `groups`, `running_groups`,
  `mem_cap_mib`, `mem_committed_mib`, `mem_host_total_mib`.

Что они означают:

- `alloc_bytes` — аллоцированные блоки образа workspace (`st_blocks × 512`,
  `statAllocBytes`). Образы разреженные (sparse), а у virtio-blk
  Firecracker нет discard, поэтому это число фактически **только растёт**.
  Это стоимость для хоста, а не заполненность гостя (комментарий в
  заголовке `resources.go`).
- `guest_disk_used_pct` — насколько заполнена гостевая файловая система,
  считается как `df`: used ÷ (used + avail), так что зарезервированные
  блоки ext4 не считаются ни тем, ни другим (`resGuestDiskPct`). Именно по
  нему судите, не вот-вот ли группа заполнится.
- `growth_bytes_per_hour` измеряется по скользящему 10-минутному окну
  (`resGrowthWindow`). Он читается как **0 с `growth_span_seconds` 0**,
  пока история образцов не покроет полное окно (например, сразу после
  перезапуска демона), и никогда не уходит ниже нуля (`resGrowth`).
- `provisioned_bytes` — сумма пресетов `size` всех групп. Для него
  нормально превышать `fs_total_bytes` (комментарий у
  `HostResources.ProvisionedBytes`).
- `mem_cap_mib` 0 означает отсутствие флот-уровневого лимита памяти
  (комментарий в `wire.HostResources`).

### 2.3 Пороговые алерты

(`resCheckThresholds`, `resNotifyOperator`)

- Пороги жёстко заданы: предупреждение на 80% (серьёзность `normal`),
  критично на 90% (серьёзность `high`) (`resWarnPct`, `resCritPct`,
  `resAlertSeverity`).
- Алерт срабатывает только когда уровень **растёт**. Уровень взводится
  заново, когда значение падает на 5 пунктов ниже сработавшего порога
  (`resClearMargin`, `resArmedLevel`).
- Три субъекта:
  - файловая система хоста (`resSubjectHostFS`). Заголовок
    `Host disk N% full`.
  - **гостевая** файловая система каждой запущенной группы
    (`resSubjectDisk`). Заголовок `Group <g> disk N% full`. Группа с
    неизвестным размером гостевого диска (остановлена или недоступна)
    ничего не поднимает.
  - CPU каждой группы, усреднённый по последним 10 образцам
    (`resCPUAvgWindow`, ≈5 мин), как процент от её собственных vCPU
    (`resSubjectCPU`). Заголовок `Group <g> CPU N% sustained`.
- Доставка: строка лога от подсистемы `resources` (`warn` для уровня 1,
  `error` для уровня 2, через `emitLogfQuiet`) плюс уведомление в `main`.
  Если очередь уведомлений main заполнена, демон логирует `notification
  backlog full, alert dropped: <title>`.

## 3. Обновление: `koto update`

(`daemon/update.go`)

```
usage: koto update [--check] [--version X] [-y]
```

Флаги (`updateMain`):

- `--check`: только отчёт. Выходит с 0, когда всё актуально, и с **3**,
  когда обновление существует (`updateExitAvailable`).
- `--version X`: установить релиз X вместо последнего. Даунгрейд разрешён
  только когда вы называете версию явно таким способом.
- `-y`: принять подтверждения, включая остановку демона. Передаётся
  установщику как `--yes`.

Шаги:

1. **Установленная версия.** Если `/usr/local/bin/koto` существует
   (`installedKotoBinPath`), используется вывод его `version`. Иначе
   используется собственный `kotoVersion` работающего бинарника
   (`updateInstalledVersion`).
2. **Последний релиз.** Запрос `HEAD <repo>/releases/latest`; тег читается
   из заголовка `Location` редиректа без перехода по нему
   (`updateLatestRelease`). Репозиторий — `https://github.com/<KOTO_REPO>`
   (по умолчанию `jpzk/koto`), либо `KOTO_REPO_URL`, если задан
   (`updateRepoURL`). У всей команды — 30-секундный контекст, а HTTP-клиент
   получает таймаут 20 с, когда контекста нет.
3. **Решение** (`updateDecide`; версии сравниваются как
   `MAJOR.MINOR.PATCH` через `parseSemver`, который игнорирует ведущий `v`
   и любой суффикс `-`/`+`). Печатаемые сообщения:
   - `koto %s is available (installed: %s)`: новее, значит обновляет.
   - `up to date: koto %s is the latest release`
   - `up to date: development build %s is based on the latest release %s`:
     сборка вида `1.0.0-6-gd5150df-dirty` считается своим базовым релизом.
   - `release %s replaces development build %s (named explicitly)`
   - `%s is a downgrade from %s (named explicitly)`
   - `installed %s is newer than the latest release %s; nothing to do`
   - `installed %s is not a release build; %s is available`
   - `%q is not a release version`: не обновляет.
4. **Установка** (`updateRun`). Отказывается работать под root (`run as
   your normal user, not root — koto installs as you and uses sudo where it
   must`). Записывает `install.sh`, **встроенный в бинарник**
   (`embeddedInstaller`, `//go:embed installer/install.sh`), во временный
   файл и запускает `sh <file> --version <target> --no-attach [--yes]` на
   вашем терминале. Этот установщик верифицирует релиз и запускает
   `koto install` и `koto setup` нового релиза (комментарий в заголовке
   файла). Установка останавливает демон, а вместе с ним — каждую
   запущенную microVM.
   При сбое: `koto update: the installer failed: <err>`.

Установщик встроен, а не скачивается, потому что скачанный принёс бы свой
запиненный отпечаток ключа подписи по тому же каналу, что и проверяемый им
релиз (комментарий в заголовке `update.go`). Сами проверки подписи живут в
`install.sh` и описаны в [getting-started.md](getting-started.md).

## 4. Удаление koto: `koto uninstall`

(`daemon/uninstall.go`)

```
usage: koto uninstall [flags]
```

Флаги (`uninstallMain`):

- `-state DIR`: каталог состояния (по умолчанию `/var/lib/koto`,
  `defaultStateDir` в `daemon/install.go`).
- `--purge`: также удалить каталог состояния и `/etc/koto/koto.env`.
- `-n`: сухой прогон. Каждая привилегированная команда печатается как
  `would: sudo …` и ничего не меняется (`uninstallOpts.step`).
- `-y`: принять значения по умолчанию. **В сочетании с `--purge` удаляет
  без подтверждения.**
- `-no-color`: принимается кодом, но не указан в тексте usage.

Позиционные аргументы отклоняются: `unexpected argument %q — this command
takes flags only`. Запуск под root отклоняется: `run as your normal user,
not root (sudo is used only for the system files)` (`runUninstall`).

Порядок (`runUninstall`):

1. Если не существует ни юнит, ни `/usr/local/bin/koto`, ни
   `/usr/local/bin/koto-tui`, ни `/etc/koto/koto.env`, печатается
   `nothing installed — …` и работа останавливается, если не задан
   `--purge`.
2. **Сначала остановка.** Если файл юнита `/etc/systemd/system/koto.service`
   (`unitPath`) существует **или** `systemctl is-active koto` говорит
   `active` (`systemctlIsActive`), выполняются `systemctl stop koto`,
   `disable`, удаление юнита, `daemon-reload` и `reset-failed`. Неудачная
   остановка прерывает: `stop koto: … (fix, or `sudo systemctl kill koto`,
   then re-run)`. Юнит задаёт `TimeoutStopSec=25` (`daemon/install.go`).
3. **Проверка живого демона.** Сканируется `/proc` в поисках процесса
   `koto daemon`, чей корень состояния (`KOTO_HOME` из его окружения, иначе
   его cwd) — тот же каталог (`runningKotoDaemon`, `procKotoHome`). Если
   такой ещё работает, следует отказ с `a koto daemon is still running
   against …`. С `-n` — только предупреждение.
4. Удаляются `/usr/local/bin/koto` и `/usr/local/bin/koto-tui`.
5. Без `--purge` перечисляется, что осталось (env-файл, который может
   содержать `ANTHROPIC_API_KEY`, и каталог состояния с числом групп и
   размером `du -sh`), и работа останавливается. Последующий
   `koto install` подхватит их.
6. С `--purge` (`uninstallPurge`):
   - Каталог состояния проверяется `purgeRefusal`. Отказ (`refusing to
     delete <dir>: …`) следует, когда путь не абсолютный, является корнем
     файловой системы, вашим домашним каталогом, либо содержит `.git` или
     `go.work` ("that is a koto clone"). Отказ также следует, когда в пути
     нет ни одного из `.koto-version`, `groups`, `creds`, `fcassets`,
     `groups.json` (`no koto state found there …`).
   - Запрашивается `Delete it?` (по умолчанию «нет»), если не задан `-y`.
   - Его (device, inode) записывается до запроса и проверяется снова перед
     удалением (`dirIdentity`). Если они различаются, следует отказ с `… is
     not the directory that was checked (it was replaced while the
     confirmation was open) — refusing to delete it`.
   - Каталог состояния удаляется **без sudo** (`os.RemoveAll`); он
     принадлежит пользователю.
   - Удаляется `/etc/koto/koto.env` (sudo), затем
     `rmdir --ignore-fail-on-non-empty /etc/koto`.

Повторный запуск завершает прерванное удаление, потому что каждый элемент
определяется и пропускается, когда уже отсутствует (комментарий в
заголовке).

Эквиваленты для dev-чекаута в `Makefile`: `make clean` удаляет артефакты
сборки, `metrics.jsonl`, `proxy.log` и `run`. `make clean-groups`
**безвозвратно удаляет** `groups groups.json schedules.json goals.json`,
предварительно попросив вас набрать `yes` (`FORCE=1` пропускает запрос).
`make clean-creds` удаляет `creds`.

## 5. Диагностика

Каждая запись даёт сообщение в точности как оно выглядит в коде, откуда оно
берётся и что делать.

### Установка / настройка

- `daemon did not answer within 45s: … check `systemctl status koto` and
  `journalctl -u koto -n 50``: smoke-шаг мастера (`daemon/setup_steps.go`).
  Читайте journal.
- `no usable /dev/kvm on this host — groups will not boot until that is
  fixed`: предупреждение от того же шага (`checkKVM`). Проверки preflight и
  их сообщения перечислены в [getting-started.md](getting-started.md).

### Учётные данные и LLM-прокси (`daemon/proxy.go`)

- `no credentials: run `koto claude-login` (or set ANTHROPIC_API_KEY)`:
  не найдено ни одного учётного данного Anthropic.
- `oauth token refresh failed — turns will 401 until it works (check `koto
  claude-login --status`): …` и `… but the token on disk is still expired —
  the CLI did not rotate it; run `koto claude-login``: проблемы обновления
  OAuth. Это строки уровня error, поэтому они также появляются как баннеры
  в `main`.
- `the venice credential is not available on this host; ask the operator`
  (HTTP 503): группа с провайдером venice, но ключ не найден
  (`no venice key: write to …`).
- `koto proxy: <METHOD> <path> is outside the endpoint allowlist for
  provider <p> (the proxy relays inference and model metadata only)` (HTTP
  403, `llmRefuse`): гость вызвал API-путь, который прокси не ретранслирует.
- `koto proxy: too many concurrent requests for this group` (HTTP 503):
  более 32 запросов в полёте для группы, или 128 всего, после ожидания 60 с
  (`proxyMaxInflightPerGroup`, `proxyMaxInflightGlobal`,
  `proxyInflightWait`).
- `koto proxy: request-body memory budget exhausted; retry shortly` (503),
  `koto proxy: request body stalled` (408) и `request body too large or
  unreadable` (413): проблемы чтения тела запроса.

### Исходящий трафик (`daemon/proxy.go`)

- `egress denied: this group's network profile is 'none' (a raised profile
  applies on /restart)` (403): задайте `network` (см.
  [configuration.md](configuration.md)), затем перезапустите группу.
- `egress blocked: target not permitted under this group's network profile`
  (403): цель вне того, что разрешает профиль группы.
- `koto proxy: too many concurrent egress connections for this group` (503).

### Загрузка групп (`daemon/fchostmem.go`)

- `group %s needs %d MiB (mem %d + %d VMM margin) but the fleet cap is %d
  MiB with %d MiB already committed to running VMs — stop a VM, pick a
  smaller size, or raise KOTO_HOST_MEM_MIB`: флот-уровневый лимит памяти
  заполнен.
- `refusing to boot %s: the host's memory capacity could not be determined …`:
  лимит не удалось вычислить.

### Ходы

- `this conversation's previous turn is wedged and the group could not
  self-heal — /restart the group …` (`errSessionWedged`,
  `daemon/queue.go`). Перезапустите группу. Другие лимиты очереди описаны в
  [groups-goals-schedules.md](groups-goals-schedules.md).

### Удаление

- `a koto daemon is still running against …`: остановите его через
  `sudo systemctl stop koto` или `kill <pid>`, как говорит сообщение, затем
  перезапустите команду.
- `refusing to delete …: that is a koto clone, not an installed state dir`:
  `-state` указывает на исходный чекаут. Укажите настоящий каталог
  состояния.
