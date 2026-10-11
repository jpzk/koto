Перевод файла ../protocol.md; английский оригинал является авторитетным.

# Проводные протоколы

У koto два проводных контракта, оба protobuf, оба в `protocol/`:

- `protocol/koto.proto`: gRPC API **клиент ↔ демон** (`service Koto`), на
  котором говорят `koto ctl`, TUI и Android-клиент.
- `protocol/guest.proto`: контракт **демон ↔ гость microVM** поверх vsock,
  на котором говорят только демон и гостевой агент PID‑1 (`fcguest/`). Он
  никогда не покидает vsock-пару хост↔гость.

Сгенерированный Go-код закоммичен в `protocol/pb` (`go_package
"koto-protocol/pb;pb"`). Перегенерация — `make proto-gen`; `make
proto-verify` перегенерирует и падает, если `protocol/pb` изменился
(`git diff --exit-code -- protocol/pb`, `Makefile`). Тулчейн кодогенерации
запинен в `Makefile`: `PROTOC_GEN_GO_VER := v1.36.12`,
`PROTOC_GEN_GO_GRPC_VER := v1.6.2`.

См. также: [architecture.md](architecture.md) (где живёт каждый канал),
[acl-and-auth.md](acl-and-auth.md) (кто что может вызывать),
[ctl-reference.md](ctl-reference.md) (CLI и внутригостевые команды,
построенные на этих сообщениях), [../firecracker-vsock.md](../firecracker-vsock.md)
(контекст по vsock).

---

## 1. Клиент ↔ демон: `service Koto` (gRPC)

### 1.1 Транспорт

Настраивается в `daemonMain` (`daemon/daemon.go`):

- Обычный TCP-слушатель (`net.Listen("tcp", addr)`), обёрнутый в TLS из
  `serverTLSConfig` (`grpc.Creds(credentials.NewTLS(tlsCfg))`). Клиенты
  обязаны предъявить сертификат из allowlist (mTLS).
- Каждый вызов проходит перехватчики `authUnary` / `authStream`.
  Bearer-токен читается из ключа метаданных `authorization`, с опциональным
  отрезанием префикса `Bearer ` (`authFromCtx`, `daemon/auth.go`).
  Идентичность токена должна совпадать с CN клиентского сертификата. Сбои
  возвращают gRPC `Unauthenticated` с одним из сообщений `invalid or
  missing token`, `no client certificate` или `client certificate and token
  name different identities`.
- Keepalive: `keepalive.ServerParameters{Time: 30 * time.Second, Timeout:
  10 * time.Second}`, и принудительные параметры `MinTime: 10 *
  time.Second, PermitWithoutStream: true`.
- Переопределений `MaxRecvMsgSize` / `MaxSendMsgSize` в `daemonMain` нет,
  поэтому действуют значения по умолчанию grpc‑go (не верифицировано, для
  какой версии grpc‑go; см. запиненный рантайм в `daemon/go.mod`).

Полные имена методов — `/koto.Koto/<Rpc>` (`package koto;`,
`service Koto`). ACL-глагол RPC — это его имя в snake_case:
`verbFromMethod` (`daemon/acl.go`) вставляет `_` перед каждой внутренней
заглавной буквой и переводит в нижний регистр, так что `SchedToggle` →
`sched_toggle`, `RunScript` → `run_script`, `AttachShell` →
`attach_shell`. Детали ACL — в [acl-and-auth.md](acl-and-auth.md).

### 1.2 Соглашения (заголовок `protocol/koto.proto`)

- Имена полей — snake_case. Клиенты, печатающие JSON, используют
  `protojson.MarshalOptions{UseProtoNames: true}` (`daemon/ctl_cli.go`,
  `tui/daemon.go`), поэтому JSON-ключи равны именам proto-полей.
- Нулевые значения proto3 не сериализуются.
- Каждый `*Resp` начинается с `bool ok = 1; string error = 2;`. Прикладные
  ошибки передаются **внутри канала** (in band) в `error` с `ok=false`;
  сбои auth/ACL и неудачные открытия стримов — это статус-ошибки gRPC.
- Временные метки (`ts`, `before`, времена расписаний) — `double` секунды.
- Свободные JSON-объекты (конфиг, метрики) — `google.protobuf.Struct`.

### 1.3 RPC

Все 36 RPC ниже реализованы как методы на `kotoServer` в
`daemon/grpc_server.go` (регистрируются через `pb.RegisterKotoServer`).

Унарные, жизненный цикл групп и чат:

- `Spawn(SpawnReq) → SpawnResp`. `SpawnReq{group, main, provider, model,
  size}`; `SpawnResp` добавляет `int32 port`.
- `Send(SendReq) → BaseResp`. `SendReq{group, msg, image, image_mime,
  audio, audio_mime, session}`. Согласно комментарию в proto, изображение
  записывается в `groups/<g>/.cs/uploads/img-<ts>.<ext>` и упоминается в
  сообщении; audio — "CURRENTLY UNSUPPORTED" и отклоняется in band.
  `session` ""/`-`/`default` — сессия по умолчанию.
- `List(ListReq) → ListResp`.
- `Stop`, `Interrupt`, `Destroy`, `Clear` `(GroupReq) → BaseResp`;
  `Restart(GroupReq) → SpawnResp`. `GroupReq{group, session}`; согласно
  комментарию в proto, `session` читает только `Clear` ("" = вся группа,
  `-`/`default` = сессия по умолчанию, имя = эта сессия). `Drain` тоже
  ограничивается по `session` (ниже).
- `Drain(GroupReq) → DrainResp{ok, error, dropped}`. Сбрасывает только
  ожидающие в очереди промпты; VM, память и выполняющийся ход не
  затрагиваются. Goal-сессии никогда не дренируются (комментарий в proto у
  `Drain`).
- `History(HistoryReq) → HistoryResp`. `HistoryReq{group, limit, before}`:
  `limit` 0 = серверное значение по умолчанию, `before` 0 = хвостовая
  страница, иначе — события с `ts < before`.
- `Config(ConfigReq) → ConfigResp`. Каждая настройка — proto3 `optional`,
  поэтому состояний три: отсутствует = без изменений, присутствует `""` =
  сбросить, присутствует значение = установить. Поля: `model`, `effort`,
  `ports`, `provider`, `internet` (устарело; "daemon maps full→network=wan"),
  `size`, `root`, `network`, `autostart`, `kvm`. Номера полей 4 и 8 —
  `reserved` (удалённая функциональность skills). Допустимые значения — в
  [configuration.md](configuration.md).
- `Metrics(MetricsReq{group}) → MetricsResp`.

Унарные, задачи (jobs) и расписания:

- `Jobs(JobsReq{group}) → JobsResp`. `group` "" = все запущенные группы
  (требуется цель `*`).
- `JobLogs(JobLogsReq{group, id, tail}) → JobLogsResp{job, output,
  truncated}`. `tail` 0 ⇒ `jobLogsDefaultTail = int64(4096)`, с потолком
  `jobLogsMaxTail = int64(65536)` (`daemon/jobs.go`).
- `SchedAdd(SchedAddReq{group, cron, msg}) → SchedAddResp{item}`,
  `SchedList(SchedListReq{group}) → SchedListResp`,
  `SchedDel(SchedIDReq{id})`, `SchedToggle(SchedToggleReq{id, enabled})`,
  `SchedRun(SchedIDReq{id})` — последние три возвращают `BaseResp`.

Унарные, цели (goals) (`daemon/goals.go`):

- `GoalSet(GoalSetReq) → GoalResp`. `GoalSetReq{group, text, criteria,
  max_iterations, plan, name}`. `max_iterations` ≤ 0 ⇒
  `goalDefaultMaxIter = 20`, с потолком `goalMaxIterCeil = 200`
  (`goalSetBy`). `plan` — `optional bool`; отсутствие = сначала план.
  Пустое `name` слагифицируется из `text`.
- `GoalList(GoalListReq{group}) → GoalListResp`. `group` "" = все группы
  (требуется цель `*`).
- `GoalApprove`, `GoalPause`, `GoalInterrupt`, `GoalResume`, `GoalCancel`
  `(GoalGroupReq{group, name}) → GoalResp`. `name` "" разрешается в
  единственную цель-кандидата; неоднозначное "" — ошибка. Комментарий в
  proto говорит, что у `GoalApprove` "deliberately NO ctl-plane
  counterpart". Этот комментарий устарел: в `CtlRequest` есть
  `goal_approve` (14), и `ctlDispatch` (`daemon/ctl.go`) принимает его
  только для вызывающей группы. Иначе возвращается `ctl: goal_approve is
  self-only (a plan set on a peer needs a human)`.

Унарные, флот и ACL:

- `Resources(ResourcesReq{}) → ResourcesResp`. Без цели (нет поля group):
  авторизуется только по глаголу (`targetOf`, `daemon/acl.go`). Сбор — на
  стороне хоста (`daemon/resources.go`).
- `AclGet`, `AclSetRole`, `AclDelRole` → `AclResp`. Жёстко только для
  админа (`adminOnlyVerbs`, `daemon/acl.go`).

Потоковые:

- `RunScript(RunScriptReq{group, script, raw}) → stream ScriptEvent`.
  Жёстко только для админа. Выполняет `script` под `/bin/sh -c` в госте от
  имени `node` (uid 1000), cwd `/workspace` (комментарий в proto). Вывод
  санитизируется, если не `raw=true`. `ScriptEvent.event` — `data` (байты в
  `chunk`) … `end`; сбой — один кадр `error`. Код выхода не передаётся.
- `JobTail(JobTailReq{group, id, tail, parsed}) → stream ScriptEvent`.
  `tail` ≤ 0 ⇒ `jobLogsMaxTail` (65536) байт начального окна, оно же
  потолок; затем живые строки через гостевой `exec tail -c <window> -f`
  (`kotoServer.JobTail`). С `parsed=true` демон испускает кадры `event`,
  несущие `ScriptEvent.parsed` (`Event`) вместо чанков `data`.
- `AttachShell(stream ShellInput) ↔ stream ShellFrame`. Двунаправленный.
  Первое сообщение обязано быть `open` (`ShellOpen{session, cols, rows}`;
  пустой `session` = `koto-shell`), затем любое число `data` / `resize`, и
  опционально `close`. `group` повторяется в каждом `ShellInput`, так что
  каждое сообщение переавторизуется `aclStream.RecvMsg`
  (`daemon/auth.go`); более позднее сообщение с другой группой — ошибка
  протокола. `ShellFrame.event` — `data | end | error`; `end` означает, что
  гостевая обвязка этого соединения разобрана, а не что tmux-сессия
  умерла.
- `SubscribeGroup(SubscribeReq{group, since_seq}) → stream Event`. Каждый
  живой `Event` несёт per-групповой `seq` (начинается с 1, сбрасывается при
  перезапуске демона). `since_seq > 0` воспроизводит события с
  `seq > since_seq` из кольца в памяти; если кольцо не покрывает разрыв,
  стрим начинается с синтетического `Event{event:"gap"}`, и клиент должен
  перечитать через `History`. События из `History` имеют `seq` 0.
- `SubscribeLogs(LogsReq{}) → stream LogEvent`. `LogEvent{event:"log",
  level, msg, ts, subsystem, group}`.
- `WatchState(WatchReq{}) → stream StateFrame{groups, ts,
  global_tok_per_sec}`. Первый кадр отправляется сразу, последующие —
  только при изменении. Каждый кадр — полный снимок.

Авторизация стрима периодически перепроверяется, пока стрим открыт. См.
`aclStreamRecheck` (`daemon/auth.go`) и
[acl-and-auth.md](acl-and-auth.md).

---

## 2. Демон ↔ гость: `protocol/guest.proto` поверх vsock

### 2.1 Фрейминг (`daemon/fcframe.go`, `fcguest/frame.go`)

Каждый protobuf-канал использует один и тот же кадр: **одна `uint32`
big‑endian длина, затем одно сериализованное сообщение**. `fcReadFrame`
читает 4‑байтовый заголовок, отвергает `n > max` **до** выделения тела,
затем читает ровно `n` байт через `io.ReadFull`. `fcWriteFrame`
сериализует, отвергает тела больше `fcFrameMaxWrite` и пишет заголовок и
тело одним `Write`. Оба случая превышения дают ошибку `frame of %d bytes
exceeds the %d-byte channel maximum`. Гостевая копия (`writeFrame` /
`readFrame` в `fcguest/frame.go`) описана как тот же код.

Максимумы по каналам (`daemon/fcframe.go`):

- `fcFrameMaxCtl = 1 << 20` (1 MiB): ctl-запрос гость→хост, порт 9002
  (`fcCtlConn`).
- `fcFrameMaxGuest = 16 << 20` (16 MiB): agent-ответы и стрим-кадры
  гость→хост на 10000 (`fcAgentCall`, `fcReadAgentFrame`), и кадры ходов на
  9004 (`fcTurnSink`).
- `fcFrameMaxHost = 64 << 20` (64 MiB): запросы хост→гость (вложения едут
  внутри `MsgReq`). Гость читает хостовые кадры с `frameMaxHost = 64
  << 20` (`fcguest/frame.go`).
- `fcFrameMaxWrite = fcFrameMaxHost`: демон никогда не пишет кадр больше
  64 MiB.

Защита от медленного тела: на каналах, инициируемых гостем (9002, 9004),
демон читает через `fcReadFrameBounded` (`daemon/fc.go`). Он ждёт заголовок
без дедлайна, а затем применяет `fcFrameBodyWait = 60 * time.Second` к
остатку кадра.

Установка соединения хост→гость (`fcHostDial`, `daemon/fc.go`):
подключиться к vsock UDS Firecracker, записать `CONNECT <port>\n` и читать
ответ по одному байту до `\n`. Он обязан начинаться с `OK `, иначе ошибка —
`vsock CONNECT <port>: <line>`.

### 2.2 Порт 9002: ctl-плоскость (гость → хост)

- Сообщения: `CtlRequest` (`oneof cmd`) → `CtlResponse{ok, error, oneof
  result}`.
- Одно соединение может нести много пар запрос/ответ. `fcCtlConn` крутит
  цикл `fcReadFrameBounded` → `ctlDispatchPB` → `fcWriteFrame` до EOF;
  плохой кадр логируется как `[<g>] ctl frame: <err>`.
- Авторизация — по тому, **на vsock-слушатель какой группы** пришёл запрос,
  никогда по полю (заголовок guest.proto; `daemon/ctl.go`).
- Варианты `CtlRequest.cmd` с номерами полей: `spawn`(1) `SpawnReq`,
  `send`(2) `CtlSendReq{group, msg, session, reply, from_session}`,
  `stop`(3), `list`(4), `resources`(5), `config_set`(6) `ConfigReq`,
  `tail`(7) `CtlTailReq{group, n}`, `job_done`(8) `JobDoneReq{id, rc, out,
  total, session}`, `notify`(9) `NotifyReq{severity, title, msg, session}`,
  `report`(10) `ReportReq{msg}`, `goal_done`(11) `GoalDoneReq{id, note}`,
  `goal_verdict`(12) `GoalVerdictReq{id, met, reasons}`, `goal_set`(13),
  `goal_approve`(14), `goal_status`(15) `GoalListReq`, `goal_pause`(16),
  `goal_interrupt`(17), `goal_resume`(18), `goal_cancel`(19),
  `sched_add`(20), `sched_list`(21), `sched_del`(22), `sched_toggle`(23),
  `sched_run`(24). Сообщения без собственного типа выше переиспользуют
  одноимённый тип запроса из `koto.proto`.
- Варианты `CtlResponse.result`: `port`(spawn), `list`, `resources`,
  `config`, `tail` (`CtlTailResp{text}`), `goal`, `goals` (goal_status),
  `sched` (sched_add), `scheds` (sched_list).
- Гостевая сторона (`fcguest/ctl.go`): агенты пишут одну JSON-строку в
  `/workspace/.cs/ctl`. Агент парсит её в `CtlRequest` с
  `protojson.UnmarshalOptions{DiscardUnknown: false}`, так что неизвестный
  глагол, неизвестное поле или неверный тип отклоняются ещё в госте. Ответ
  сплющивается в один JSON-объект (`UseProtoNames: true`), дописываемый в
  `/workspace/.cs/ctl.out`. Семантика по глаголам:
  [ctl-reference.md](ctl-reference.md), часть 2.

### 2.3 Порт 10000: agent RPC (хост → гость)

- Один кадр `AgentRequest` на соединение (`fcAgentOpen` / `fcAgentCall`,
  `daemon/fc.go`). Варианты `AgentRequest.op`: `init` `InitReq{ports, env,
  net, root, group, kvm}`, `msg` `MsgReq{msg, system_prompt, config_json,
  uploads_tar, session, slot}`, `exec` `ExecReq{script}`, `exec_stream`
  `ExecStreamReq{script}`, `run_script` `GuestScriptReq{script}`,
  `shell_attach` `ShellAttachReq{session, cols, rows}`, `shutdown`
  `ShutdownReq{}`.
- Что следует за запросом, по операциям (заголовок guest.proto и
  комментарий `fcAgentOpen`):
  - `init`, `msg`, `exec`, `shutdown`: один кадр `AgentResponse{ok, error,
    rc, out}`, читаемый с ограничением `fcFrameMaxGuest`. Не-ok `error`
    санитизируется и сплющивается в одну строку перед использованием
    (`flattenInline(sanitize(...))` в `fcAgentCall`).
  - `exec_stream`: **сырые байты**, без ответного кадра; хост закрывает
    трубу.
  - `run_script`: поток `AgentFrame` гость→хост (`data`, `end`, `error`).
  - `shell_attach`: `AgentFrame` в **обоих** направлениях: `data`/`end`/
    `error` гость→хост, `input`/`resize` хост→гость (`fcShellDial`).
    Закрытие соединения отсоединяет клиента; tmux-сессию оно не убивает.
- `AgentFrame.end` — явный терминатор, потому что закрытие на стороне гостя
  не проявляется как EOF на хосте в гибридном vsock (комментарий
  guest.proto).

Устаревший комментарий: раздел `ShellInput` в `protocol/koto.proto` всё ещё
описывает кадр `[type byte][uint32 BE len][payload]` с типами
`'D'/'E'/'X'/'I'/'R'` и называет `fcReadScriptFrame` / `writeScriptFrame`.
Ни одной из этих функций в дереве нет. Код использует protobuf-кадры
`AgentFrame` (`fcReadAgentFrame`, `fcShellDial` в `daemon/fc.go`).

### 2.4 Порт 9004: поток хода (гость → хост)

- Одно соединение на ход. Первый кадр обязан быть
  `TurnFrame{open: TurnOpen{slot}}`. Затем идут кадры событий, и последним —
  `turn_end`.
- Варианты `TurnFrame.kind`: `open`, `text` (байты), `think_begin`,
  `think_end` (число слов), `tool` (`ToolUse{name, input}`),
  `tool_out_begin`, `tool_out_end` (число байт), `err`, `turn_end`.
- `fcTurnSink` (`daemon/fcturn.go`) отказывает стриму в двух случаях.
  Первый — отсутствующий или вышедший за диапазон слот (`slot < 0 || slot >=
  groupSlots`), логируется как `turn stream opened without a valid slot`.
  Второй — слот без ожидающего хода (`fcConsumeExpectedTurn`), логируется
  как `turn stream opened on slot %d with no outstanding turn — refused`.
- Кадры отрисовываются в хостовый лог слота в грамматике `[[marker]]`
  (`daemon/logparse.go`). Текст, который распарсился бы как маркер,
  экранируется, так что гость не может подделывать маркеры. Тела
  инструментов режутся до `fcMarkerMaxBody = 64 << 10`, а имена — до
  `fcMarkerMaxName = 256` (`truncateBytes`).
- Если транскрипт записать нельзя, ход завершается с ошибкой, а не
  оставляется ждать: демон логирует `turn stream slot %d abandoned: the
  transcript is unwritable, so this turn cannot complete` и вызывает
  `failInflightTurn`.

### 2.5 Не-protobuf порты vsock

Согласно заголовку guest.proto, 9000 (исходящий API → прокси, HTTP) и 9003
(L3 ethernet-кадры → gVisor) — байтовые трубы со своими протоколами. 9001
(старый поток логов) выведен из эксплуатации. См.
[architecture.md](architecture.md) §6.
