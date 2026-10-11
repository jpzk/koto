Traducción de ../protocol.md; la versión en inglés es la autoritativa.

# Protocolos de red

koto tiene dos contratos de red, ambos protobuf, ambos en `protocol/`:

- `protocol/koto.proto`: la API gRPC **cliente ↔ daemon** (`service Koto`),
  hablada por `koto ctl`, la TUI y el cliente Android.
- `protocol/guest.proto`: el contrato **daemon ↔ invitado de la microVM**
  sobre vsock, hablado solo por el daemon y el agente PID‑1 del invitado
  (`fcguest/`). Nunca sale del par vsock host↔invitado.

El código Go generado está comprometido bajo `protocol/pb` (`go_package
"koto-protocol/pb;pb"`). Se regenera con `make proto-gen`; `make
proto-verify` regenera y falla si `protocol/pb` cambió (`git diff
--exit-code -- protocol/pb`, `Makefile`). La cadena de herramientas de
codegen está fijada en el `Makefile`: `PROTOC_GEN_GO_VER := v1.36.12`,
`PROTOC_GEN_GO_GRPC_VER := v1.6.2`.

Véase también: [architecture.md](architecture.md) (dónde vive cada canal),
[acl-and-auth.md](acl-and-auth.md) (quién puede llamar a qué),
[ctl-reference.md](ctl-reference.md) (la CLI y los verbos dentro del
invitado construidos sobre estos mensajes),
[firecracker-vsock.md](../firecracker-vsock.md) (contexto sobre vsock).

---

## 1. Cliente ↔ daemon: `service Koto` (gRPC)

### 1.1 Transporte

Configurado en `daemonMain` (`daemon/daemon.go`):

- Listener TCP plano (`net.Listen("tcp", addr)`) envuelto en TLS desde
  `serverTLSConfig` (`grpc.Creds(credentials.NewTLS(tlsCfg))`). Los clientes
  deben presentar un certificado incluido en la lista de permitidos (mTLS).
- Toda llamada pasa por los interceptores `authUnary` / `authStream`. El
  token bearer se lee de la clave de metadatos `authorization`, quitando un
  prefijo `Bearer ` opcional (`authFromCtx`, `daemon/auth.go`). La identidad
  del token debe ser igual al CN del certificado del cliente. Los fallos
  devuelven gRPC `Unauthenticated` con uno de `invalid or missing token`,
  `no client certificate`, o `client certificate and token name different
  identities`.
- Keepalive: `keepalive.ServerParameters{Time: 30 * time.Second, Timeout:
  10 * time.Second}`, y como enforcement `MinTime: 10 * time.Second,
  PermitWithoutStream: true`.
- No se fija ningún override de `MaxRecvMsgSize` / `MaxSendMsgSize` en
  `daemonMain`, así que aplican los valores por defecto de grpc‑go (sin
  verificar de qué versión de grpc‑go son esos defaults; véase el runtime
  fijado en `daemon/go.mod`).

Los nombres completos de método son `/koto.Koto/<Rpc>` (`package koto;`,
`service Koto`). El verbo de ACL de un RPC es su nombre en snake_case:
`verbFromMethod` (`daemon/acl.go`) inserta `_` antes de cada mayúscula
interior y pasa a minúsculas, de modo que `SchedToggle` → `sched_toggle`,
`RunScript` → `run_script`, `AttachShell` → `attach_shell`. Los detalles de
la ACL están en [acl-and-auth.md](acl-and-auth.md).

### 1.2 Convenciones (cabecera de `protocol/koto.proto`)

- Los nombres de campo son snake_case. Los clientes que imprimen JSON usan
  `protojson.MarshalOptions{UseProtoNames: true}` (`daemon/ctl_cli.go`,
  `tui/daemon.go`), así que las claves JSON son iguales a los nombres de
  campo del proto.
- Los valores cero de proto3 no se serializan.
- Todo `*Resp` empieza con `bool ok = 1; string error = 2;`. Los errores de
  aplicación viajan **en banda** en `error` con `ok=false`; los fallos de
  auth/ACL y las aperturas de stream inválidas son errores de estado gRPC.
- Las marcas de tiempo (`ts`, `before`, horarios de schedule) son segundos
  en `double`.
- Los objetos JSON de forma libre (config, métricas) son
  `google.protobuf.Struct`.

### 1.3 RPCs

Los 36 RPCs siguientes están implementados como métodos de `kotoServer` en
`daemon/grpc_server.go` (registrados con `pb.RegisterKotoServer`).

Unarios, ciclo de vida de grupos y chat:

- `Spawn(SpawnReq) → SpawnResp`. `SpawnReq{group, main, provider, model,
  size}`; `SpawnResp` añade `int32 port`.
- `Send(SendReq) → BaseResp`. `SendReq{group, msg, image, image_mime,
  audio, audio_mime, session}`. Según el comentario del proto, una imagen se
  escribe en `groups/<g>/.cs/uploads/img-<ts>.<ext>` y se referencia desde
  el mensaje; el audio está "CURRENTLY UNSUPPORTED" y se rechaza en banda.
  `session` ""/`-`/`default` es la sesión por defecto.
- `List(ListReq) → ListResp`.
- `Stop`, `Interrupt`, `Destroy`, `Clear` `(GroupReq) → BaseResp`;
  `Restart(GroupReq) → SpawnResp`. `GroupReq{group, session}`; según el
  comentario del proto, `session` solo lo lee `Clear` ("" = todo el grupo,
  `-`/`default` = sesión por defecto, un nombre = esa sesión). `Drain`
  también filtra por `session` (abajo).
- `Drain(GroupReq) → DrainResp{ok, error, dropped}`. Descarta solo los
  prompts en cola (en espera); la VM, la memoria y el turno en vuelo quedan
  intactos. Las sesiones de goal nunca se drenan (comentario del proto sobre
  `Drain`).
- `History(HistoryReq) → HistoryResp`. `HistoryReq{group, limit, before}`:
  `limit` 0 = valor por defecto del servidor, `before` 0 = página de cola,
  en otro caso eventos con `ts < before`.
- `Config(ConfigReq) → ConfigResp`. Cada ajuste es `optional` de proto3, así
  que hay tres estados: ausente = sin cambio, presente `""` = borrar,
  presente con valor = fijar. Campos: `model`, `effort`, `ports`,
  `provider`, `internet` (obsoleto; "daemon maps full→network=wan"), `size`,
  `root`, `network`, `autostart`, `kvm`. Los números de campo 4 y 8 están
  `reserved` (una funcionalidad de skills eliminada). Los valores permitidos
  están en [configuration.md](configuration.md).
- `Metrics(MetricsReq{group}) → MetricsResp`.

Unarios, jobs y schedules:

- `Jobs(JobsReq{group}) → JobsResp`. `group` "" = todos los grupos en
  ejecución (necesita el target `*`).
- `JobLogs(JobLogsReq{group, id, tail}) → JobLogsResp{job, output,
  truncated}`. `tail` 0 ⇒ `jobLogsDefaultTail = int64(4096)`, con tope en
  `jobLogsMaxTail = int64(65536)` (`daemon/jobs.go`).
- `SchedAdd(SchedAddReq{group, cron, msg}) → SchedAddResp{item}`,
  `SchedList(SchedListReq{group}) → SchedListResp`,
  `SchedDel(SchedIDReq{id})`, `SchedToggle(SchedToggleReq{id, enabled})`,
  `SchedRun(SchedIDReq{id})`, devolviendo los tres últimos `BaseResp`.

Unarios, goals (`daemon/goals.go`):

- `GoalSet(GoalSetReq) → GoalResp`. `GoalSetReq{group, text, criteria,
  max_iterations, plan, name}`. `max_iterations` ≤ 0 ⇒
  `goalDefaultMaxIter = 20`, con tope en `goalMaxIterCeil = 200`
  (`goalSetBy`). `plan` es `optional bool`; ausente = primero el plan. Un
  `name` vacío se genera como slug a partir de `text`.
- `GoalList(GoalListReq{group}) → GoalListResp`. `group` "" = todos los
  grupos (necesita el target `*`).
- `GoalApprove`, `GoalPause`, `GoalInterrupt`, `GoalResume`, `GoalCancel`
  `(GoalGroupReq{group, name}) → GoalResp`. `name` "" se resuelve al único
  goal candidato; un "" ambiguo es un error. El comentario del proto dice
  que `GoalApprove` tiene "deliberately NO ctl-plane counterpart". Ese
  comentario está obsoleto: `CtlRequest` tiene `goal_approve` (14), y
  `ctlDispatch` (`daemon/ctl.go`) lo acepta solo para el grupo que llama. En
  otro caso devuelve `ctl: goal_approve is self-only (a plan set on a peer
  needs a human)`.

Unarios, flota y ACL:

- `Resources(ResourcesReq{}) → ResourcesResp`. Sin target (no tiene campo de
  grupo): autorizado solo por el verbo (`targetOf`, `daemon/acl.go`). Se
  recoge en el lado del host (`daemon/resources.go`).
- `AclGet`, `AclSetRole`, `AclDelRole` → `AclResp`. Solo-admin cableado en
  el código (`adminOnlyVerbs`, `daemon/acl.go`).

Streaming:

- `RunScript(RunScriptReq{group, script, raw}) → stream ScriptEvent`.
  Solo-admin cableado. Ejecuta `script` bajo `/bin/sh -c` en el invitado
  como `node` (uid 1000), cwd `/workspace` (comentario del proto). La salida
  se sanea salvo que `raw=true`. `ScriptEvent.event` es `data` (bytes en
  `chunk`) … `end`; un fallo es una única trama `error`. No se transporta
  código de salida.
- `JobTail(JobTailReq{group, id, tail, parsed}) → stream ScriptEvent`.
  `tail` ≤ 0 ⇒ `jobLogsMaxTail` (65536) bytes de ventana inicial, que es
  también el tope; después líneas en vivo mediante un `exec tail -c <window>
  -f` en el invitado (`kotoServer.JobTail`). Con `parsed=true` el daemon
  emite tramas `event` que llevan `ScriptEvent.parsed` (un `Event`) en lugar
  de chunks `data`.
- `AttachShell(stream ShellInput) ↔ stream ShellFrame`. Bidireccional.
  El primer mensaje debe ser `open` (`ShellOpen{session, cols, rows}`;
  `session` vacío = `koto-shell`), luego cualquier número de `data` /
  `resize`, y opcionalmente `close`. `group` se repite en cada `ShellInput`
  para que cada mensaje sea re-autorizado por `aclStream.RecvMsg`
  (`daemon/auth.go`); un mensaje posterior que nombre otro grupo es un error
  de protocolo. `ShellFrame.event` es `data | end | error`; `end` significa
  que la fontanería del lado del invitado de esta conexión se desmontó, no
  que la sesión tmux haya muerto.
- `SubscribeGroup(SubscribeReq{group, since_seq}) → stream Event`. Cada
  `Event` en vivo lleva un `seq` por grupo (empieza en 1, se reinicia al
  reiniciar el daemon). `since_seq > 0` repite los eventos con
  `seq > since_seq` desde un ring en memoria; si el ring no puede cubrir el
  hueco, el stream empieza con un `Event{event:"gap"}` sintético y el
  cliente debe volver a pedirlos vía `History`. Los eventos de `History`
  llevan `seq` 0.
- `SubscribeLogs(LogsReq{}) → stream LogEvent`. `LogEvent{event:"log",
  level, msg, ts, subsystem, group}`.
- `WatchState(WatchReq{}) → stream StateFrame{groups, ts,
  global_tok_per_sec}`. La primera trama se envía de inmediato; las
  posteriores solo cuando algo cambia. Cada trama es un snapshot completo.

La autorización de los streams se vuelve a comprobar periódicamente mientras
el stream está abierto. Véase `aclStreamRecheck` (`daemon/auth.go`) y
[acl-and-auth.md](acl-and-auth.md).

---

## 2. Daemon ↔ invitado: `protocol/guest.proto` sobre vsock

### 2.1 Framing (`daemon/fcframe.go`, `fcguest/frame.go`)

Todos los canales protobuf usan la misma trama: **una longitud `uint32`
big‑endian y después un mensaje serializado**. `fcReadFrame` lee la cabecera
de 4 bytes, rechaza `n > max` **antes** de reservar memoria para el cuerpo,
y luego lee exactamente `n` bytes con `io.ReadFull`. `fcWriteFrame`
serializa, rechaza cuerpos mayores que `fcFrameMaxWrite`, y escribe cabecera
y cuerpo en un solo `Write`. Ambos casos de exceso producen el error
`frame of %d bytes exceeds the %d-byte channel maximum`. La copia del
invitado (`writeFrame` / `readFrame` en `fcguest/frame.go`) se describe como
el mismo código.

Máximos por canal (`daemon/fcframe.go`):

- `fcFrameMaxCtl = 1 << 20` (1 MiB): petición ctl invitado→host, puerto
  9002 (`fcCtlConn`).
- `fcFrameMaxGuest = 16 << 20` (16 MiB): respuestas de agente y tramas de
  stream invitado→host en el 10000 (`fcAgentCall`, `fcReadAgentFrame`), y
  tramas de turno en el 9004 (`fcTurnSink`).
- `fcFrameMaxHost = 64 << 20` (64 MiB): peticiones host→invitado (los
  adjuntos viajan dentro de `MsgReq`). El invitado lee las tramas del host
  con `frameMaxHost = 64 << 20` (`fcguest/frame.go`).
- `fcFrameMaxWrite = fcFrameMaxHost`: el daemon nunca escribe una trama
  mayor de 64 MiB.

Defensa contra cuerpos lentos: en los canales iniciados por el invitado
(9002, 9004) el daemon lee mediante `fcReadFrameBounded` (`daemon/fc.go`).
Espera la cabecera sin deadline, y luego aplica
`fcFrameBodyWait = 60 * time.Second` al resto de la trama.

Establecimiento de conexión host→invitado (`fcHostDial`, `daemon/fc.go`):
conectar al UDS vsock de Firecracker, escribir `CONNECT <port>\n`, y leer la
respuesta byte a byte hasta `\n`. Debe empezar con `OK `; en otro caso el
error es `vsock CONNECT <port>: <line>`.

### 2.2 Puerto 9002: plano ctl (invitado → host)

- Mensajes: `CtlRequest` (un `oneof cmd`) → `CtlResponse{ok, error, oneof
  result}`.
- Una conexión puede transportar muchos pares petición/respuesta.
  `fcCtlConn` itera `fcReadFrameBounded` → `ctlDispatchPB` → `fcWriteFrame`
  hasta EOF; una trama inválida se registra como `[<g>] ctl frame: <err>`.
- La autorización es por **el listener vsock de qué grupo** recibió la
  petición, nunca por un campo (cabecera de guest.proto; `daemon/ctl.go`).
- Variantes de `CtlRequest.cmd`, con sus números de campo: `spawn`(1)
  `SpawnReq`, `send`(2) `CtlSendReq{group, msg, session, reply,
  from_session}`, `stop`(3), `list`(4), `resources`(5), `config_set`(6)
  `ConfigReq`, `tail`(7) `CtlTailReq{group, n}`, `job_done`(8)
  `JobDoneReq{id, rc, out, total, session}`, `notify`(9) `NotifyReq{severity,
  title, msg, session}`, `report`(10) `ReportReq{msg}`, `goal_done`(11)
  `GoalDoneReq{id, note}`, `goal_verdict`(12) `GoalVerdictReq{id, met,
  reasons}`, `goal_set`(13), `goal_approve`(14), `goal_status`(15)
  `GoalListReq`, `goal_pause`(16), `goal_interrupt`(17), `goal_resume`(18),
  `goal_cancel`(19), `sched_add`(20), `sched_list`(21), `sched_del`(22),
  `sched_toggle`(23), `sched_run`(24). Los mensajes sin tipo propio arriba
  reutilizan el tipo de petición de `koto.proto` del mismo nombre.
- Variantes de `CtlResponse.result`: `port`(spawn), `list`, `resources`,
  `config`, `tail` (`CtlTailResp{text}`), `goal`, `goals` (goal_status),
  `sched` (sched_add), `scheds` (sched_list).
- Lado del invitado (`fcguest/ctl.go`): los agentes escriben una línea JSON
  en `/workspace/.cs/ctl`. El agente la parsea a un `CtlRequest` con
  `protojson.UnmarshalOptions{DiscardUnknown: false}`, de modo que un verbo
  desconocido, un campo desconocido o un tipo incorrecto se rechaza en el
  invitado. La respuesta se aplana a un único objeto JSON
  (`UseProtoNames: true`) añadido a `/workspace/.cs/ctl.out`. Semántica por
  verbo: [ctl-reference.md](ctl-reference.md) Parte 2.

### 2.3 Puerto 10000: RPC del agente (host → invitado)

- Una trama `AgentRequest` por conexión (`fcAgentOpen` /
  `fcAgentCall`, `daemon/fc.go`). Variantes de `AgentRequest.op`: `init`
  `InitReq{ports, env, net, root, group, kvm}`, `msg` `MsgReq{msg,
  system_prompt, config_json, uploads_tar, session, slot}`, `exec`
  `ExecReq{script}`, `exec_stream` `ExecStreamReq{script}`, `run_script`
  `GuestScriptReq{script}`, `shell_attach` `ShellAttachReq{session, cols,
  rows}`, `shutdown` `ShutdownReq{}`.
- Lo que sigue a la petición, por operación (cabecera de guest.proto y
  comentario de `fcAgentOpen`):
  - `init`, `msg`, `exec`, `shutdown`: una trama `AgentResponse{ok, error,
    rc, out}`, leída con el límite `fcFrameMaxGuest`. Un `error` no-ok se
    sanea y se aplana a una línea antes de usarse
    (`flattenInline(sanitize(...))` en `fcAgentCall`).
  - `exec_stream`: **bytes crudos**, sin trama de respuesta; el host cierra
    la tubería.
  - `run_script`: un stream de `AgentFrame` invitado→host (`data`, `end`,
    `error`).
  - `shell_attach`: `AgentFrame` en **ambas** direcciones: `data`/`end`/
    `error` invitado→host, `input`/`resize` host→invitado (`fcShellDial`).
    Cerrar la conexión desconecta al cliente; no mata la sesión tmux.
- `AgentFrame.end` es un terminador explícito porque un cierre del lado del
  invitado no aflora como EOF en el host sobre vsock híbrido (comentario de
  guest.proto).

Comentario obsoleto: la sección `ShellInput` de `protocol/koto.proto`
todavía describe una trama `[type byte][uint32 BE len][payload]` con tipos
`'D'/'E'/'X'/'I'/'R'` y nombra `fcReadScriptFrame` / `writeScriptFrame`.
Ninguna de las dos funciones existe en el árbol. El código usa `AgentFrame`s
de protobuf (`fcReadAgentFrame`, `fcShellDial` en `daemon/fc.go`).

### 2.4 Puerto 9004: flujo de turno (invitado → host)

- Una conexión por turno. La primera trama debe ser
  `TurnFrame{open: TurnOpen{slot}}`. Después vienen las tramas de eventos, y
  `turn_end` al final.
- Variantes de `TurnFrame.kind`: `open`, `text` (bytes), `think_begin`,
  `think_end` (recuento de palabras), `tool` (`ToolUse{name, input}`),
  `tool_out_begin`, `tool_out_end` (recuento de bytes), `err`, `turn_end`.
- `fcTurnSink` (`daemon/fcturn.go`) rechaza el stream en dos casos. El
  primero es un slot ausente o fuera de rango (`slot < 0 || slot >=
  groupSlots`), registrado como `turn stream opened without a valid slot`.
  El segundo es un slot sin turno pendiente (`fcConsumeExpectedTurn`),
  registrado como `turn stream opened on slot %d with no outstanding turn —
  refused`.
- Las tramas se vuelcan al log del host del slot con la gramática
  `[[marker]]` (`daemon/logparse.go`). El texto que parsearía como marcador
  se escapa, así que el invitado no puede falsificar marcadores. Los cuerpos
  de tool se cortan a `fcMarkerMaxBody = 64 << 10` y los nombres de tool a
  `fcMarkerMaxName = 256` (`truncateBytes`).
- Si la transcripción no puede escribirse, el turno se da por fallido en
  lugar de dejarlo esperando: el daemon registra `turn stream slot %d
  abandoned: the transcript is unwritable, so this turn cannot complete` y
  llama a `failInflightTurn`.

### 2.5 Puertos vsock no-protobuf

Según la cabecera de guest.proto, el 9000 (egress de API → proxy, HTTP) y el
9003 (tramas ethernet L3 → gVisor) son tuberías de bytes con sus propios
protocolos. El 9001 (el antiguo flujo de log) está retirado. Véase
[architecture.md](architecture.md) §6.
