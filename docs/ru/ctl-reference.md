Перевод файла ../ctl-reference.md; английский оригинал является авторитетным.

# `koto ctl` и внутригостевой ctl-план — справочник

В koto есть две командные поверхности, обе называются «ctl»:

1. **`koto ctl`**: CLI на стороне хоста. Это gRPC-клиент API демона
   (`daemon/main.go`, `case "ctl"` → `ctlCliMain`; `daemon/ctl_cli.go`).
2. **Внутригостевой ctl-план (ctl plane)**: агенты внутри microVM пишут одну
   JSON-строку на команду в `/workspace/.cs/ctl` и читают ответ, дописанный в
   `/workspace/.cs/ctl.out` (комментарий в заголовке `fcguest/ctl.go`). Гость
   пересылает эти строки демону через vsock-порт 9002 (`fcguest/main.go`
   `portCtl`, `daemon/fc.go` `fcPortCtl`), а демон отвечает в
   `ctlDispatch` (`daemon/ctl.go`).

У каждой поверхности свой набор глаголов и свои правила. Обе описаны ниже.

---

## Часть 1 — `koto ctl` (CLI хоста)

### 1.1 Подключение и аутентификация

`koto ctl` соединяется с демоном по mTLS плюс bearer-токен (`ctlClient`,
`newKotoClient`, `tokenCreds` в `daemon/ctl_cli.go`). Все задействованные
переменные окружения — в [configuration.md](configuration.md) §2.3. Роли, ACL
и PKI описаны в [acl-and-auth.md](acl-and-auth.md). Вкратце:

- `KOTO_ADDR`: по умолчанию `127.0.0.1:8443` (`ctlClient`).
- `KOTO_CLIENT`: идентичность клиента. По умолчанию `agent` (`ctlClient`).
- `KOTO_CREDS_DIR`: если не задана, `ctlCredsDir` пробует по порядку
  `./creds` (если существует), `$KOTO_HOME/creds` (если задана `KOTO_HOME`),
  `/var/lib/koto/creds` (если существует) и в конце откатывается к `creds`.
- Файлы по умолчанию внутри этого каталога — `client-<name>.crt`,
  `client-<name>.key`, `ca.crt` и `token-<name>`. `KOTO_CERT`, `KOTO_KEY`,
  `KOTO_CA` и `KOTO_TOKEN` переопределяют их по отдельности (`newKotoClient`).
- `KOTO_SERVER_NAME` переопределяет имя TLS-сервера (`newKotoClient`).
- Минимум — TLS 1.3 (`MinVersion: tls.VersionTLS13`, `newKotoClient`).
- Токен передаётся в metadata-заголовке `authorization: Bearer <token>`
  (`tokenCreds.GetRequestMetadata`).
- Если нет ни `KOTO_TOKEN`, ни файла токена, вызов падает с ошибкой
  ``no token: set KOTO_TOKEN or provide <dir>/token-<name> (mint with `make pki-client NAME=<name> ROLE=agent`)``
  (`ctlClient`).

Пример только для чтения против установленного демона:

```sh
HOME=/var/lib/koto KOTO_CLIENT=tui KOTO_TOKEN=$(cat /var/lib/koto/creds/token-tui) koto ctl list
```

### 1.2 Вывод и коды выхода

- Унарные глаголы печатают ответ как **одну строку protojson**, используя
  proto-имена полей (`ctlPrint`, `protojson.MarshalOptions{UseProtoNames: true}`).
- Если RPC упал: `koto ctl: rpc: <err>`, выход 1 (`ctlPrint`).
- Если в ответе `"ok": false`: `koto ctl: daemon: <error>`, выход 1
  (`ctlPrint`). Это контракт прикладных ошибок внутри канала (in band).
- Ошибки использования выходят с кодом 2 (`ctlFatal(2, …)` по всему
  `ctlCliMain`). Ошибки разбора флагов тоже выходят с 2, потому что каждый
  набор флагов использует `flag.ExitOnError`.
- Потоковые глаголы (`tail`, `logs`, `watch`) печатают одну строку protojson
  на кадр. Чистый EOF выходит с 0; любая другая ошибка потока печатает
  `koto ctl: stream: <err>` и выходит с 1 (`ctlStreamJSON`).
- У каждого унарного вызова дедлайн 30 с (`ctlCtx`). У потоков и `ask` свои
  контексты, описанные ниже.
- Запуск `koto ctl` без глагола печатает текст usage и выходит с 2.
  Неизвестный глагол печатает текст usage плюс `koto ctl: unknown verb: <verb>`
  и выходит с 2 (`ctlCliMain`).

### 1.3 Аргументы сообщения (`-` = stdin)

Для `send`, `ask` и `runscript` сообщение или скрипт — это хвостовые
аргументы, соединённые одиночными пробелами. Одиночный `-` вместо них читает
stdin, срезает завершающие переводы строки и ограничивает ввод
`ctlStdinMax = 1 << 20` байт (1 МиБ — столько же, сколько `sendMsgMax` демона
в `daemon/queue.go`). Ввод сверх предела падает с ошибкой `stdin is larger
than 1048576 bytes, which is more than the daemon accepts for one message`,
выход 2 (`ctlMsgArg`).

### 1.4 Глаголы

Глаголы перечислены в порядке текста usage (`ctlUsage`). Каждый названный
здесь `case` находится в `switch verb` функции `ctlCliMain`, если не указан
отдельный хелпер.

#### Жизненный цикл группы

- **`list`**: все группы и их состояние. RPC `List`.
- **`spawn [-provider P] [-model M] <group>`**: создать или запустить группу.
  `-provider` (текст справки `claudesdk|venice|openai`), `-model` (переопределение
  модели). RPC `Spawn`.
- **`stop <group>`**: RPC `Stop`.
- **`interrupt <group>`**: RPC `Interrupt`. Согласно комментарию к `drain`,
  прерывание выполняющегося хода — работа `interrupt`.
- **`destroy <group>`**: RPC `Destroy`.
- **`restart <group>`**: RPC `Restart`.
- **`clear [-session S] <group>`**: RPC `Clear`. `-session` очищает только
  эту чат-сессию (`-` или `default` = сессия по умолчанию). Без него
  очищается вся группа. Текст usage показывает `clear <group>` без флага, но
  флаг существует (`case "clear"`).
- **`drain [-session S] <group>`**: сбросить поставленные в очередь промпты.
  Текст usage говорит, что VM, разговор и выполняющийся ход не трогаются.
  `-session` сбрасывает только эту сессию (`-`/`default` = по умолчанию).
  Без него сбрасываются все сессии группы. RPC `Drain`.

`stop`, `interrupt`, `destroy` и `restart` принимают ровно один аргумент:
``usage: koto ctl <verb> <group>`` (`groupVerb`).

#### Разговор

- **`send [-session S] <group> <msg...>`**: поставить сообщение в очередь и
  вернуться немедленно. `-session` задаёт чат-сессию (`""` = по умолчанию).
  Этот флаг не показан в тексте usage (`case "send"`). RPC `Send`.
- **`ask [-timeout D] [-json] [-session S] <group> <msg...>`**: отправить,
  стримить ответ и выйти, когда ход закончится (`ctlAsk`).
  - `-timeout`: по умолчанию `30m` (`30*time.Minute`). По истечении:
    `timed out after <D> waiting for turn end`, выход 1.
  - `-json`: печатать захваченные события строками protojson вместо простого
    текста.
  - `-session`: не показан в основном тексте usage. Показан в собственной
    ошибке использования `ask`.
  - Поведение (`ctlAsk`): `ask` подписывается на группу (`SubscribeGroup`)
    *до* отправки. События других сессий игнорируются, причём `""`, `-` и
    `default` означают одну и ту же сессию (`normalizeAskSession`). Захват
    начинается с первого события `prompt`, чей текст равен сообщению. В
    простом режиме текст `done` печатается в stdout, текст `err` — в stderr.
    Выход 0 на `turn_end`.
- **`history [-limit N] [-before TS] <group>`**: `-limit` (int, максимум
  событий; `0` не отправляет лимит демону, а серверное значение по умолчанию
  не верифицировано), `-before` (float-курсор ts, исключающий). RPC `History`.

#### Конфигурация

- **`config <group>`**: без флагов печатает действующую конфигурацию
  (`ctlConfig`).
- **`config <group> [flags]`**: устанавливает ключи. Группа должна идти
  первой, потому что пакет flag в Go останавливается на первом не-флаге. Всё
  после флагов падает с ``config: unexpected args after group: [...] (flags
  follow the group)``. Отправляются только реально заданные флаги, так что
  `-model ""` *очищает* модель, а отсутствующий флаг оставляет её без
  изменений (`fs.Visit` в `ctlConfig`). RPC `Config`. Флаги:
  - `-model M` (`""` очищает)
  - `-provider claudesdk|venice|openai`
  - `-network none|wan|lan|full`
  - `-size small|medium|large|xlarge`
  - `-root yes|no`
  - `-effort E`
  - `-ports P` (список через запятую)
  - `-autostart yes|no`
  - `-kvm yes|no`: вложенная виртуализация (`/dev/kvm` в госте). **Не
    перечислен в `ctlUsage`**, но определён в `ctlConfig`.

  Валидация значений, значения по умолчанию и момент вступления изменения в
  силу — всё в [configuration.md](configuration.md) §4. Комментарий над
  `ctlConfig` говорит: `network`/`size`/`root`/`ports` применяются на
  `/restart`, `model`/`provider`/`effort` — на следующем сообщении, а
  `autostart` читается только при старте демона.

#### Потоки и инспекция

- **`metrics [group]`**: RPC `Metrics`. Без группы запрос идёт с пустой
  группой.
- **`resources`**: диск, память и CPU на стороне хоста по каждой группе плюс
  сводка по флоту. Текст usage помечает его `(admin)`. Аргументов не
  принимает. RPC `Resources`.
- **`tail [-since N] <group>`**: поток событий группы. `-since N` (uint64)
  перед переходом в живой режим проигрывает кадры кольца с `seq > N`. RPC
  `SubscribeGroup`.
- **`logs`**: собственный поток логов демона. Без аргументов. RPC
  `SubscribeLogs`.
- **`watch`**: снимок состояния групп при каждом изменении. Без аргументов.
  RPC `WatchState`.
- **`jobs [group]`**: перечисляет фоновые задания (`cs-job`), читая их свежими
  из гостя. Без группы — все группы. RPC `Jobs`.
- **`job-logs [-tail N] <group> <id>`**: метаданные одного задания плюс хвост
  его вывода. Согласно справке флага, `-tail` — байты с конца, по умолчанию
  4096, максимум 65536. CLI отправляет `0`, и демон применяет значения по
  умолчанию (`jobLogsDefaultTail = 4096`, `jobLogsMaxTail = 65536` в
  `daemon/jobs.go`). RPC `JobLogs`.
- **`job-tail [-tail N] <group> <id>`**: следит за выводом задания вживую.
  Согласно справке флага, `-tail` — начальное окно в байтах, по умолчанию
  65536. Значение 0 или меньше становится `jobLogsMaxTail`, а большие
  значения прижимаются к нему (`daemon/grpc_server.go`, обработчик JobTail).
  Сырые чанки `data` пишутся в stdout, выход 0 на `end`, выход 1 на `error`.
  RPC `JobTail`.

#### Расписания (`ctlSched`)

- **`sched list [group]`**: RPC `SchedList`.
- **`sched add <group> <cron...> <msg...>`**: `cron` — либо один токен
  `@alias`, либо ровно 5 полей. Всё после него — сообщение. Если для
  5-полевой формы остаётся меньше 7 аргументов: `sched add: need 5 cron
  fields (or one @alias) + msg`. Пустое сообщение: `sched add: empty
  message`. RPC `SchedAdd`. Алиасы, известные демону (`cronAliases`,
  `daemon/cron.go`), — `@yearly`, `@annually`, `@monthly`, `@weekly`,
  `@daily`, `@midnight` и `@hourly`.
- **`sched del|on|off|run <id>`**: RPC `SchedDel`, `SchedToggle`
  (`Enabled: sub == "on"`) и `SchedRun`.

#### Цели (`ctlGoal`)

- **`goal set [-max N] [-plan=no] [-name x] <group> <goal> :: <criteria>`**.
  Текст цели и критерии разделяются литеральным аргументом `::`, и обе
  стороны должны быть непустыми.
  - `-max`: максимум итераций. `0` означает значение по умолчанию —
    `goalDefaultMaxIter = 20`. Значение прижимается к
    `goalMaxIterCeil = 200` (`daemon/goals.go`, `goalSetBy`).
  - `-plan`: `yes` (по умолчанию) или `no`. Всё остальное падает с
    `goal set: -plan takes yes or no`.
  - `-name`: короткое имя запуска, также используется как имя сессии. По
    умолчанию — slug из текста цели.
  - Отправляет RPC `GoalSet`. Цель на `main` отклоняется с `cannot set a goal
    on main` (`goalSetBy`).
- **`goal list [group]`**: RPC `GoalList`.
- **`goal approve|pause|interrupt|resume|cancel <group> [name]`**: RPC
  `GoalApprove`, `GoalPause`, `GoalInterrupt`, `GoalResume` и `GoalCancel`.
  `[name]` выбирает одну из параллельных целей группы и может быть опущено,
  когда кандидат только один. `[name]` не показано в `ctlUsage`, но есть в
  ошибке использования глагола. `interrupt` означает «поставить на паузу
  сейчас и прервать выполняющийся ход цели» (текст usage).

#### Общий терминал (`ctlShell`)

- **`shell <group> [session]`**: присоединяет интерактивный терминал к
  microVM группы через RPC `AttachShell`.
  - Stdin должен быть терминалом, иначе `shell: stdin is not a terminal: …`.
  - Терминал переводится в raw-режим со сброшенным `ISIG`, так что ctrl-C
    доходит до гостя.
  - **ctrl-]** (байт `0x1d`) отсоединяет и печатает `[detached — session left
    running]`.
  - SIGWINCH отправляет изменение размера. Без размера TTY размер — 80×24
    (`ctlWinsize`).
  - Вывод проходит через `shellFilter`.
  - Согласно тексту usage, tmux-сессия по умолчанию — `koto-shell`.
    Tmux-сессия именованной чат-сессии — `koto-shell-<name>`.

#### Только роль admin

Текст usage помещает эти глаголы под «admin role only». Комментарий к
`ctlAcl` говорит, что глаголы `acl_*` жёстко закодированы как admin-only на
стороне сервера. Модель авторизации — в [acl-and-auth.md](acl-and-auth.md).

- **`runscript [-raw] <group> <script...>`**: выполняет POSIX-скрипт в
  microVM группы от имени `node`. Вывод стримится вживую в stdout. RPC
  `RunScript`.
  - `-raw` распознаётся только как **первый** аргумент (он не разбирается
    пакетом flag).
  - Без `-raw` демон санирует вывод (комментарий в `case "runscript"`).
  - `-raw` при stdout на TTY **отклоняется**: ``-raw writes the guest's bytes
    to your terminal unfiltered; redirect stdout to a file (koto ctl runscript
    -raw <group> ... > out.bin) or drop -raw to get the sanitized stream``,
    выход 2 (`stdoutIsTTY`).
- **`acl get`**: печатает документ ACL. RPC `AclGet`.
- **`acl set <role> <verb>[:<groups>] ...`**: *заменяет* гранты роли (RPC
  `AclSetRole`).
  - Каждый токен — `verb` или `verb:g1,g2`. Голый `verb` или `verb:*`
    становится `"*"`. Иначе он становится списком обрезанных имён групп.
  - Пустой глагол падает с `acl set: empty verb in "<tok>"`.
  - Пример из текста usage:
    `acl set ops stop:ghost restart:ghost list`.
- **`acl del <role>`**: RPC `AclDelRole`.
- Неизвестная субкоманда печатает `unknown acl subcommand: <sub> (get|set|del)`.

### 1.5 Расхождения между текстом usage и кодом

Эти флаги и аргументы существуют в `daemon/ctl_cli.go`, но отсутствуют в
`ctlUsage`:

- `config -kvm`
- `send -session`
- `ask -session`
- `clear -session`
- необязательный `[name]` у `goal approve|pause|interrupt|resume|cancel`

---

## Часть 2 — внутригостевой ctl-план (`/workspace/.cs/ctl`)

### 2.1 Форма проводного формата

- Каждый запрос — один JSON-объект на строку. Его поле `cmd` выбирает глагол
  (`cmdEnvelope` в `ctlDispatch`). Некорректный JSON получает `json: <err>`.
- Гость транслирует каждую строку в `CtlRequest`, а ответ — обратно в плоскую
  JSON-строку `ok`/`error` в `ctl.out` (`ctlRequestFromJSON`,
  `ctlResponseJSON` в `fcguest/ctl.go`). Кадры идут по vsock-порту 9002. См.
  [firecracker-vsock.md](../firecracker-vsock.md).
- Вызывающая группа (`owner`) определяется демоном по соединению, а не по
  полезной нагрузке (`ctlDispatch(owner, line)`, `fcCtlConn` в
  `daemon/fc.go`).
- Неизвестный глагол получает `ctl: verb not allowed: <cmd>` (ветка default
  в `ctlDispatch`).

### 2.2 Глаголы и кому они доступны

Здесь «main» — это группа с именем `main` (`ctlMainGroup`). Все остальные
группы — non-main. Non-main группа, вызвавшая main-only глагол, получает
`ctl: verb not allowed for non-main groups: <verb>`.

**Оркестрация только для main**

- **`spawn`** `{"group"}`. Имя группы должно соответствовать
  `^[a-z0-9][a-z0-9_-]{0,31}$` (`ctlGroupRE`). Иначе:
  `ctl: invalid group name (must match [a-z0-9][a-z0-9_-]{0,31})`.
  - `main` нельзя заспавнить: `ctl: cannot spawn 'main'`.
  - Не более `ctlMaxSpawn = 100` зарегистрированных групп:
    `ctl: spawn cap reached (100 groups)`. Повторный spawn уже существующей
    группы разрешён.
  - Всегда спавнит с `main:false`.
  - В ответе есть `port`.
- **`send`** `{"group","msg","session"?,"reply"?,"from_session"?}`.
  - Отправка в `main` падает с `ctl: cannot send to self`.
  - Сообщение ставится в очередь и немедленно подтверждается.
  - Сессия goal-цикла отклоняется с
    `ctl: session <s> is reserved for the goal loop`.
  - `reply:true` одновременно с постановкой в очередь взводит одноразовое
    окно отчёта у цели и дописывает `reportRequestNote` к сообщению
    (`armReportAndEnqueue`).
  - Плохая или зарезервированная `from_session` откатывается к сессии по
    умолчанию.
- **`stop`** `{"group"}`. `main` нельзя остановить: `ctl: cannot stop 'main'`
  (`stopGroup`).
- **`list`**: в ответе есть `groups` (`listGroups`).
- **`resources`**: кешированные замеры со стороны хоста, только чтение
  (`resourcesCtlResp`).
- **`config_set`** `{"group"?, model/effort/provider}`.
  - По умолчанию — собственная группа вызывающего.
  - **Ключи posture (посадки) отклоняются**. Это `network`, `internet`,
    `root`, `ports`, `size`, `autostart` и `kvm` (`configPostureKey`). Ошибка:
    ``ctl: <key> is an operator-only setting — ask the operator (TUI /config, or koto ctl config)``.
  - Здесь настраиваются только `model`, `effort` и `provider` (`configCmd`).
- **`tail`** `{"group","n"}`. Возвращает последние `n` строк файла `.cs/log`
  пира:
  - `n` по умолчанию 50, прижимается к 200.
  - С конца файла читается не более 256 КиБ (`readTail(…, 256*1024)`).
  - Ограничение частоты: пер-групповой token bucket с burst
    `ctlPollBurst = 30` и пополнением одного токена каждые
    `ctlPollRefill = 500ms`. Сверх лимита:
    `ctl: tail is being polled too fast — slow down (this is rate limited per group)`.
  - Лог написан другой группой (peer), поэтому он не возвращается как есть
    (`ctlTailFence`, `daemon/report.go`): сначала он очищается (удаляются
    управляющие символы и символы переопределения bidi), затем каждая строка
    получает префикс цитаты `> `, как тело report. В начале добавляется
    заголовок `[tail of <group>]`, в конце тот же завершающий блок о
    peer-контенте (`peerTrailer`), что и у report turn. Текстом peer являются
    только строки с префиксом цитаты `> `.

**Любая группа, только на себя**

- **`job_done`** `{"id","rc","out"(base64),"total","session"}`. Отправляется
  `cs-job`, когда задание завершается.
  - Хвост вывода ограничен `ctlMaxJobOut = 8<<10` (8 КиБ).
  - `id` должен умещаться в 64 байта, `rc` — в 8 (`ctlJobField`).
  - Некорректная сессия откатывается к сессии по умолчанию.
  - Записывает результат (`recordJobDone`) и обновляет список заданий.
- **`notify`** `{"severity","title"(b64),"msg"(b64),"session"?}`.
  - Любая severity, кроме `high`, становится `normal`.
  - Заголовок усекается до 200 байт, сообщение — до 2000
    (`notifyTitleMax`, `notifyMsgMax`).
  - Ограничение частоты: burst 5, затем одно в 15 с (`notifyRateBurst`,
    `notifyRateRefill`).
  - Ошибки: `ctl: notify: empty title and message`,
    `ctl: notify: rate limited`, `ctl: notify: backlog full`.
  - Зарезервированная сессия перемещается в сессию по умолчанию.
- **`report`** `{"msg"(b64)}`. Только non-main группы. Main получает
  `ctl: main has no delegator to report to`.
  - Нужно взведённое окно, иначе ``no reply pending: main must delegate
    with reply:true first (one report per delegation)``.
  - Сообщение санируется и усекается до `reportMsgMax = 4000` байт
    (`daemon/report.go`).
  - Пустое сообщение: `ctl: report: empty message`.
- **`goal_done`** `{"id","note"(b64)}` и **`goal_verdict`**
  `{"id","met","reasons"(b64)}`. Заметка и причины усекаются до
  `goalNoteMax = 4000` (`recordGoalDone`, `recordGoalVerdict`).
- **`goal_set`** `{"group"?,"name"?,"text","criteria","plan"?,"max_iterations"?}`.
  - Non-main вызывающий всегда принудительно нацеливается на себя. Пустая
    группа означает собственную группу вызывающего.
  - Цель на `main` отклоняется: `ctl: cannot set a goal on main`.
  - **Когда main ставит цель с `plan:false`, её принудительно переводят в
    режим plan-first**, потому что одобрение плана — дело человека (строка
    лога в `ctlDispatch`).
- **`goal_approve`** `{"name"?}`. Только на себя. Указание другой группы
  падает с ``ctl: goal_approve is self-only (a plan set on a peer needs a
  human)``. Также требуется цель в состоянии ожидания одобрения, которую
  группа поставила сама себе. Иначе: ``ctl: goal "<slug>" was set by <who> —
  its plan needs a human to approve`` (`ctlGoalSelfSet`, `daemon/goals.go`).
- **`goal_status`** `{"group"?}`. Non-main вызывающий видит только свои цели.
- **`goal_pause` / `goal_interrupt` / `goal_resume` / `goal_cancel`**
  `{"group"?,"name"?}`. Non-main вызывающие принудительно нацеливаются на
  себя. Main может управлять любой группой.
- **`sched_add`** `{"group"?,"cron","msg"}`.
  - Non-main вызывающие принудительно нацеливаются на себя.
  - Когда main оставляет `group` пустым, значение по умолчанию — `main`
    (`addSched`).
- **`sched_list`**: non-main вызывающий видит только свои расписания.
- **`sched_del` / `sched_toggle` (`"enabled"`) / `sched_run`** `{"id"}`.
  Non-main вызывающий должен владеть расписанием, иначе `ctl: not your
  schedule` (`ownsSched`). Неизвестный id проваливается к нижележащей ошибке
  «not found».

### 2.3 Отличия от CLI хоста

- Хостовый `koto ctl config` может настраивать ключи posture, если это
  разрешает ACL. `config_set` ctl-плана — не может (`configPostureKey`).
- В ctl-плане нет глаголов `destroy`, `restart`, `runscript`, `acl` и
  `shell`. Ни один из них не появляется в switch `ctlDispatch`.
