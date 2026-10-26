Traducción de ../ctl-reference.md; la versión en inglés es la autoritativa.

# `koto ctl` y el plano ctl dentro del invitado — referencia

koto tiene dos superficies de comandos que se llaman ambas "ctl":

1. **`koto ctl`**: la CLI del lado del host. Es un cliente gRPC de la API del
   daemon (`daemon/main.go`, `case "ctl"` → `ctlCliMain`; `daemon/ctl_cli.go`).
2. **El plano ctl dentro del invitado (guest)**: los agentes dentro de una
   microVM escriben una línea JSON por comando en `/workspace/.cs/ctl` y leen
   la respuesta anexada a `/workspace/.cs/ctl.out` (comentario de cabecera de
   `fcguest/ctl.go`). El invitado reenvía esas líneas al daemon por el puerto
   vsock 9002 (`fcguest/main.go` `portCtl`, `daemon/fc.go` `fcPortCtl`), y el
   daemon responde en `ctlDispatch` (`daemon/ctl.go`).

Cada superficie tiene su propio conjunto de verbos y sus propias reglas. Ambas
se describen a continuación.

---

## Parte 1 — `koto ctl` (CLI del host)

### 1.1 Conexión y autenticación

`koto ctl` se conecta al daemon con mTLS más un token bearer (`ctlClient`,
`newKotoClient`, `tokenCreds` en `daemon/ctl_cli.go`). Véase
[configuration.md](configuration.md) §2.3 para todas las variables de entorno
implicadas. Los roles, la ACL y la PKI se cubren en
[acl-and-auth.md](acl-and-auth.md). En resumen:

- `KOTO_ADDR`: por defecto `127.0.0.1:8443` (`ctlClient`).
- `KOTO_CLIENT`: identidad del cliente. Por defecto `agent` (`ctlClient`).
- `KOTO_CREDS_DIR`: si no está definida, `ctlCredsDir` prueba, en orden,
  `./creds` (si existe), `$KOTO_HOME/creds` (si `KOTO_HOME` está definida),
  `/var/lib/koto/creds` (si existe), y finalmente recurre a `creds`.
- Los nombres por defecto dentro de ese directorio son `client-<name>.crt`,
  `client-<name>.key`, `ca.crt` y `token-<name>`. `KOTO_CERT`, `KOTO_KEY`,
  `KOTO_CA` y `KOTO_TOKEN` los sobrescriben uno a uno (`newKotoClient`).
- `KOTO_SERVER_NAME` sobrescribe el nombre de servidor TLS (`newKotoClient`).
- TLS 1.3 es el mínimo (`MinVersion: tls.VersionTLS13`, `newKotoClient`).
- El token viaja en la cabecera de metadatos `authorization: Bearer <token>`
  (`tokenCreds.GetRequestMetadata`).
- Si no hay `KOTO_TOKEN` ni archivo de token, falla con
  ``no token: set KOTO_TOKEN or provide <dir>/token-<name> (mint with `make pki-client NAME=<name> ROLE=agent`)``
  (`ctlClient`).

Ejemplo de solo lectura contra un daemon instalado:

```sh
HOME=/var/lib/koto KOTO_CLIENT=tui KOTO_TOKEN=$(cat /var/lib/koto/creds/token-tui) koto ctl list
```

### 1.2 Salida y códigos de salida

- Los verbos unarios imprimen la respuesta como **una línea protojson**, con
  los nombres de campo proto (`ctlPrint`,
  `protojson.MarshalOptions{UseProtoNames: true}`).
- Si la RPC falla: `koto ctl: rpc: <err>`, salida 1 (`ctlPrint`).
- Si la respuesta trae `"ok": false`: `koto ctl: daemon: <error>`, salida 1
  (`ctlPrint`). Este es el contrato de error de aplicación en banda.
- Los errores de uso salen con 2 (`ctlFatal(2, …)` a lo largo de
  `ctlCliMain`). Los errores de parseo de flags también salen con 2, porque
  cada flag set usa `flag.ExitOnError`.
- Los verbos de streaming (`tail`, `logs`, `watch`) imprimen una línea
  protojson por frame. Un EOF limpio sale con 0; cualquier otro error del
  stream imprime `koto ctl: stream: <err>` y sale con 1 (`ctlStreamJSON`).
- Toda llamada unaria tiene un plazo (deadline) de 30 s (`ctlCtx`). Los
  streams y `ask` usan sus propios contextos, descritos más abajo.
- Ejecutar `koto ctl` sin verbo imprime el texto de uso y sale con 2. Un
  verbo desconocido imprime el texto de uso más
  `koto ctl: unknown verb: <verb>` y sale con 2 (`ctlCliMain`).

### 1.3 Argumentos de mensaje (`-` = stdin)

Para `send`, `ask` y `runscript`, el mensaje o script son los argumentos
finales unidos con espacios simples. Un único `-` en su lugar lee stdin,
quita los saltos de línea finales y limita la entrada a
`ctlStdinMax = 1 << 20` bytes (1 MiB, lo mismo que el `sendMsgMax` del daemon
en `daemon/queue.go`). Una entrada por encima del límite falla con `stdin is
larger than 1048576 bytes, which is more than the daemon accepts for one
message`, salida 2 (`ctlMsgArg`).

### 1.4 Verbos

Los verbos se listan en el orden del texto de uso (`ctlUsage`). Cada `case`
nombrado aquí está en el `switch verb` de `ctlCliMain`, salvo que se indique
un helper aparte.

#### Ciclo de vida de grupos

- **`list`**: todos los grupos y su estado. RPC `List`.
- **`spawn [-provider P] [-model M] <group>`**: crea o arranca un grupo.
  `-provider` (texto de ayuda `claudesdk|venice|openai`), `-model` (override del
  modelo). RPC `Spawn`.
- **`stop <group>`**: RPC `Stop`.
- **`interrupt <group>`**: RPC `Interrupt`. Según el comentario de `drain`,
  abortar el turno en ejecución es tarea de `interrupt`.
- **`destroy <group>`**: RPC `Destroy`.
- **`restart <group>`**: RPC `Restart`.
- **`clear [-session S] <group>`**: RPC `Clear`. `-session` limpia solo esa
  sesión de chat (`-` o `default` = la sesión por defecto). Sin el flag se
  limpia el grupo entero. El texto de uso muestra `clear <group>` sin el
  flag, pero el flag existe (`case "clear"`).
- **`drain [-session S] <group>`**: descarta los prompts encolados. El texto
  de uso dice que la VM, la conversación y el turno en vuelo quedan intactos.
  `-session` drena solo esa sesión (`-`/`default` = por defecto). Sin él se
  drenan todas las sesiones del grupo. RPC `Drain`.

`stop`, `interrupt`, `destroy` y `restart` toman exactamente un argumento:
``usage: koto ctl <verb> <group>`` (`groupVerb`).

#### Conversación

- **`send [-session S] <group> <msg...>`**: encola un mensaje y retorna de
  inmediato. `-session` fija la sesión de chat (`""` = por defecto). Este flag
  no aparece en el texto de uso (`case "send"`). RPC `Send`.
- **`ask [-timeout D] [-json] [-session S] <group> <msg...>`**: envía,
  transmite la respuesta y sale cuando el turno termina (`ctlAsk`).
  - `-timeout`: por defecto `30m` (`30*time.Minute`). Al expirar:
    `timed out after <D> waiting for turn end`, salida 1.
  - `-json`: imprime los eventos capturados como líneas protojson en lugar de
    texto plano.
  - `-session`: no aparece en el texto de uso principal. Sí aparece en el
    error de uso propio de `ask`.
  - Comportamiento (`ctlAsk`): `ask` se suscribe al grupo (`SubscribeGroup`)
    *antes* de enviar. Ignora eventos de otras sesiones, donde `""`, `-` y
    `default` significan la misma sesión (`normalizeAskSession`). Empieza a
    capturar en el primer evento `prompt` cuyo texto es igual al mensaje. En
    modo plano imprime el texto `done` por stdout y el texto `err` por
    stderr. Sale con 0 en `turn_end`.
- **`history [-limit N] [-before TS] <group>`**: `-limit` (int, máximo de
  eventos; `0` no envía límite al daemon, y el valor por defecto del servidor
  no está verificado), `-before` (cursor ts en float, exclusivo). RPC
  `History`.

#### Config

- **`config <group>`**: sin flags, imprime la configuración efectiva
  (`ctlConfig`).
- **`config <group> [flags]`**: fija claves. El grupo debe ir primero, porque
  el paquete flag de Go se detiene en el primer no-flag. Cualquier cosa tras
  los flags falla con ``config: unexpected args after group: [...] (flags
  follow the group)``. Solo se envían los flags realmente dados, así que
  `-model ""` *borra* el modelo mientras que un flag ausente lo deja igual
  (`fs.Visit` en `ctlConfig`). RPC `Config`. Los flags son:
  - `-model M` (`""` borra)
  - `-provider claudesdk|venice|openai`
  - `-network none|wan|lan|full`
  - `-size small|medium|large|xlarge`
  - `-root yes|no`
  - `-effort E`
  - `-ports P` (lista separada por comas)
  - `-autostart yes|no`
  - `-kvm yes|no`: virtualización anidada (`/dev/kvm` en el invitado). **No
    aparece en `ctlUsage`**, pero está definido en `ctlConfig`.

  La validación de valores, los valores por defecto y cuándo surte efecto un
  cambio están en [configuration.md](configuration.md) §4. El comentario
  sobre `ctlConfig` dice que `network`/`size`/`root`/`ports` se aplican en
  `/restart`, `model`/`provider`/`effort` se aplican en el siguiente mensaje,
  y `autostart` solo se lee al arrancar el daemon.

#### Streams e inspección

- **`metrics [group]`**: RPC `Metrics`. Sin grupo, la petición lleva un grupo
  vacío.
- **`resources`**: disco, memoria y CPU del lado del host por grupo, más el
  agregado de la flota. El texto de uso lo marca `(admin)`. No toma
  argumentos. RPC `Resources`.
- **`tail [-since N] <group>`**: el stream de eventos del grupo. `-since N`
  (uint64) reproduce los frames del ring con `seq > N` antes de pasar a
  vivo. RPC `SubscribeGroup`.
- **`logs`**: el stream de logs del propio daemon. Sin argumentos. RPC
  `SubscribeLogs`.
- **`watch`**: una instantánea del estado de los grupos en cada cambio. Sin
  argumentos. RPC `WatchState`.
- **`jobs [group]`**: lista los trabajos en segundo plano (`cs-job`), leídos
  en fresco del invitado. Sin grupo significa todos los grupos. RPC `Jobs`.
- **`job-logs [-tail N] <group> <id>`**: los metadatos de un job más la cola
  de su salida. Según la ayuda del flag, `-tail` son bytes desde el final,
  por defecto 4096, máximo 65536. La CLI envía `0` y el daemon aplica los
  valores por defecto (`jobLogsDefaultTail = 4096`, `jobLogsMaxTail = 65536`
  en `daemon/jobs.go`). RPC `JobLogs`.
- **`job-tail [-tail N] <group> <id>`**: sigue la salida de un job en vivo.
  Según la ayuda del flag, `-tail` es la ventana inicial en bytes, por
  defecto 65536. Un valor de 0 o menos se convierte en `jobLogsMaxTail`, y
  los valores mayores se recortan a ese máximo (`daemon/grpc_server.go`,
  handler JobTail). Escribe los chunks `data` crudos a stdout, sale con 0 en
  `end` y con 1 en `error`. RPC `JobTail`.

#### Schedules (`ctlSched`)

- **`sched list [group]`**: RPC `SchedList`.
- **`sched add <group> <cron...> <msg...>`**: `cron` es o bien un token
  `@alias` o exactamente 5 campos. Todo lo que sigue es el mensaje. Si quedan
  menos de 7 args para la forma de 5 campos: `sched add: need 5 cron fields
  (or one @alias) + msg`. Un mensaje vacío: `sched add: empty message`. RPC
  `SchedAdd`. Los alias que el daemon conoce (`cronAliases`,
  `daemon/cron.go`) son `@yearly`, `@annually`, `@monthly`, `@weekly`,
  `@daily`, `@midnight` y `@hourly`.
- **`sched del|on|off|run <id>`**: RPCs `SchedDel`, `SchedToggle`
  (`Enabled: sub == "on"`) y `SchedRun`.

#### Goals (`ctlGoal`)

- **`goal set [-max N] [-plan=no] [-name x] <group> <goal> :: <criteria>`**.
  El texto del goal y los criterios se separan con un argumento literal `::`,
  y ambos lados deben ser no vacíos.
  - `-max`: iteraciones máximas. `0` significa el valor por defecto, que es
    `goalDefaultMaxIter = 20`. El valor se recorta a
    `goalMaxIterCeil = 200` (`daemon/goals.go`, `goalSetBy`).
  - `-plan`: `yes` (por defecto) o `no`. Cualquier otra cosa falla con
    `goal set: -plan takes yes or no`.
  - `-name`: nombre corto de la ejecución, usado también como nombre de
    sesión. Por defecto, un slug del goal.
  - Envía la RPC `GoalSet`. Un goal sobre `main` se rechaza con `cannot set a
    goal on main` (`goalSetBy`).
- **`goal list [group]`**: RPC `GoalList`.
- **`goal approve|pause|interrupt|resume|cancel <group> [name]`**: RPCs
  `GoalApprove`, `GoalPause`, `GoalInterrupt`, `GoalResume` y `GoalCancel`.
  `[name]` elige uno de los goals concurrentes del grupo, y puede omitirse
  cuando solo hay un candidato. `[name]` no aparece en `ctlUsage`, pero sí en
  el error de uso del verbo. `interrupt` significa pausar ya y abortar el
  turno de goal en vuelo (texto de uso).

#### Shell compartida (`ctlShell`)

- **`shell <group> [session]`**: conecta una terminal interactiva a la
  microVM del grupo mediante la RPC `AttachShell`.
  - Stdin debe ser una terminal; si no, `shell: stdin is not a terminal: …`.
  - La terminal se pone en modo raw con `ISIG` desactivado, para que ctrl-C
    llegue al invitado.
  - **ctrl-]** (byte `0x1d`) desconecta e imprime `[detached — session left
    running]`.
  - SIGWINCH envía un redimensionado. Sin tamaño de TTY, el tamaño es 80×24
    (`ctlWinsize`).
  - La salida pasa por `shellFilter`.
  - Según el texto de uso, la sesión tmux por defecto es `koto-shell`. La
    sesión tmux de una sesión de chat con nombre es `koto-shell-<name>`.

#### Solo rol admin

El texto de uso coloca estos bajo "admin role only". El comentario de
`ctlAcl` dice que los verbos `acl_*` son solo-admin, cableados en el lado del
servidor. Para el modelo de autorización, véase
[acl-and-auth.md](acl-and-auth.md).

- **`runscript [-raw] <group> <script...>`**: ejecuta un script POSIX en la
  microVM del grupo como `node`. La salida fluye en vivo a stdout. RPC
  `RunScript`.
  - `-raw` se reconoce solo como el **primer** argumento (no lo parsea el
    paquete flag).
  - Sin `-raw`, el daemon sanea la salida (comentario en `case "runscript"`).
  - `-raw` con stdout en una TTY se **rechaza**: ``-raw writes the guest's
    bytes to your terminal unfiltered; redirect stdout to a file (koto ctl
    runscript -raw <group> ... > out.bin) or drop -raw to get the sanitized
    stream``, salida 2 (`stdoutIsTTY`).
- **`acl get`**: imprime el documento ACL. RPC `AclGet`.
- **`acl set <role> <verb>[:<groups>] ...`**: *reemplaza* los grants de un
  rol (RPC `AclSetRole`).
  - Cada token es `verb` o `verb:g1,g2`. Un `verb` a secas o `verb:*` se
    convierte en `"*"`. En otro caso se convierte en una lista de nombres de
    grupo recortados.
  - Un verbo vacío falla con `acl set: empty verb in "<tok>"`.
  - Ejemplo del texto de uso:
    `acl set ops stop:ghost restart:ghost list`.
- **`acl del <role>`**: RPC `AclDelRole`.
- Un subcomando desconocido imprime `unknown acl subcommand: <sub> (get|set|del)`.

### 1.5 Diferencias entre el texto de uso y el código

Estos flags y argumentos existen en `daemon/ctl_cli.go` pero faltan en
`ctlUsage`:

- `config -kvm`
- `send -session`
- `ask -session`
- `clear -session`
- el `[name]` opcional en `goal approve|pause|interrupt|resume|cancel`

---

## Parte 2 — plano ctl dentro del invitado (`/workspace/.cs/ctl`)

### 2.1 Forma en el cable

- Cada petición es un objeto JSON por línea. Su campo `cmd` selecciona el
  verbo (`cmdEnvelope` en `ctlDispatch`). Un JSON malformado recibe
  `json: <err>`.
- El invitado traduce cada línea a un `CtlRequest` y la respuesta de vuelta a
  una línea JSON plana `ok`/`error` en `ctl.out` (`ctlRequestFromJSON`,
  `ctlResponseJSON` en `fcguest/ctl.go`). Los frames usan el puerto vsock
  9002. Véase [firecracker-vsock.md](../firecracker-vsock.md).
- El grupo llamante (`owner`) lo decide el daemon a partir de la conexión, no
  del payload (`ctlDispatch(owner, line)`, `fcCtlConn` en `daemon/fc.go`).
- Un verbo desconocido recibe `ctl: verb not allowed: <cmd>` (case por
  defecto de `ctlDispatch`).

### 2.2 Verbos y quién puede usarlos

Aquí "main" es el grupo llamado `main` (`ctlMainGroup`). Todos los demás
grupos son no-main. Un grupo no-main que llama a un verbo exclusivo de main
recibe `ctl: verb not allowed for non-main groups: <verb>`.

**Orquestación solo-main**

- **`spawn`** `{"group"}`. El nombre del grupo debe cumplir
  `^[a-z0-9][a-z0-9_-]{0,31}$` (`ctlGroupRE`). Si no:
  `ctl: invalid group name (must match [a-z0-9][a-z0-9_-]{0,31})`.
  - `main` no puede crearse a sí mismo: `ctl: cannot spawn 'main'`.
  - Como máximo `ctlMaxSpawn = 100` grupos registrados:
    `ctl: spawn cap reached (100 groups)`. Re-crear un grupo que ya existe
    está permitido.
  - Siempre crea con `main:false`.
  - La respuesta lleva `port`.
- **`send`** `{"group","msg","session"?,"reply"?,"from_session"?}`.
  - Enviar a `main` falla con `ctl: cannot send to self`.
  - El mensaje se encola y se confirma de inmediato.
  - Una sesión del bucle de goals se rechaza con
    `ctl: session <s> is reserved for the goal loop`.
  - `reply:true` arma la ventana de report de un solo uso del destinatario a
    la vez que encola, y añade `reportRequestNote` al mensaje
    (`armReportAndEnqueue`).
  - Un `from_session` inválido o reservado cae a la sesión por defecto.
- **`stop`** `{"group"}`. `main` no puede detenerse: `ctl: cannot stop
  'main'` (`stopGroup`).
- **`list`**: la respuesta lleva `groups` (`listGroups`).
- **`resources`**: muestras del lado del host cacheadas, solo lectura
  (`resourcesCtlResp`).
- **`config_set`** `{"group"?, model/effort/provider}`.
  - Por defecto apunta al propio grupo del llamante.
  - **Las claves de postura se rechazan.** Son `network`, `internet`,
    `root`, `ports`, `size`, `autostart` y `kvm` (`configPostureKey`). El
    error es ``ctl: <key> is an operator-only setting — ask the operator (TUI
    /config, or koto ctl config)``.
  - Aquí solo pueden fijarse `model`, `effort` y `provider` (`configCmd`).
- **`tail`** `{"group","n"}`. Devuelve las últimas `n` líneas del `.cs/log`
  del par:
  - `n` es 50 por defecto y se recorta a 200.
  - Lee como máximo 256 KiB del final del archivo
    (`readTail(…, 256*1024)`).
  - Límite de tasa: un token bucket por grupo con ráfaga
    `ctlPollBurst = 30`, rellenando un token cada `ctlPollRefill = 500ms`.
    Por encima del límite:
    `ctl: tail is being polled too fast — slow down (this is rate limited per group)`.
  - El log lo ha escrito otro grupo (un par), así que no se devuelve tal cual
    (`ctlTailFence`, `daemon/report.go`): primero se sanea (se eliminan los
    caracteres de control y de anulación bidi), y luego cada línea recibe el
    prefijo de cita `> `, igual que el cuerpo de un report. Se antepone una
    cabecera `[tail of <group>]` y se añade al final el mismo bloque de cierre
    sobre contenido de pares (`peerTrailer`) que lleva un report turn. Solo
    las líneas con el prefijo de cita `> ` son texto del par.

**Cualquier grupo, hacia sí mismo**

- **`job_done`** `{"id","rc","out"(base64),"total","session"}`. Lo envía
  `cs-job` cuando un job termina.
  - La cola de salida se limita a `ctlMaxJobOut = 8<<10` (8 KiB).
  - `id` debe caber en 64 bytes y `rc` en 8 (`ctlJobField`).
  - Una sesión malformada cae a la sesión por defecto.
  - Registra el resultado (`recordJobDone`) y refresca la lista de jobs.
- **`notify`** `{"severity","title"(b64),"msg"(b64),"session"?}`.
  - Cualquier severidad distinta de `high` se convierte en `normal`.
  - El título se trunca a 200 bytes y el mensaje a 2000
    (`notifyTitleMax`, `notifyMsgMax`).
  - Límite de tasa: ráfaga 5, luego uno cada 15 s (`notifyRateBurst`,
    `notifyRateRefill`).
  - Errores: `ctl: notify: empty title and message`,
    `ctl: notify: rate limited`, `ctl: notify: backlog full`.
  - Una sesión reservada se mueve a la sesión por defecto.
- **`report`** `{"msg"(b64)}`. Solo grupos no-main. Main recibe
  `ctl: main has no delegator to report to`.
  - Necesita una ventana armada; si no, ``no reply pending: main must
    delegate with reply:true first (one report per delegation)``.
  - El mensaje se sanea y se trunca a `reportMsgMax = 4000` bytes
    (`daemon/report.go`).
  - Un mensaje vacío: `ctl: report: empty message`.
- **`goal_done`** `{"id","note"(b64)}` y **`goal_verdict`**
  `{"id","met","reasons"(b64)}`. La nota y las razones se truncan a
  `goalNoteMax = 4000` (`recordGoalDone`, `recordGoalVerdict`).
- **`goal_set`** `{"group"?,"name"?,"text","criteria","plan"?,"max_iterations"?}`.
  - Un llamante no-main siempre se fuerza a apuntarse a sí mismo. Un grupo
    vacío significa el propio del llamante.
  - Un goal sobre `main` se rechaza: `ctl: cannot set a goal on main`.
  - **Cuando main fija un goal con `plan:false`, se le fuerza a
    plan-primero**, porque aprobar un plan es decisión de un humano (línea de
    log en `ctlDispatch`).
- **`goal_approve`** `{"name"?}`. Solo hacia sí mismo. Nombrar otro grupo
  falla con ``ctl: goal_approve is self-only (a plan set on a peer needs a
  human)``. También requiere un goal en espera de aprobación que el propio
  grupo haya fijado. Si no: ``ctl: goal "<slug>" was set by <who> — its plan
  needs a human to approve`` (`ctlGoalSelfSet`, `daemon/goals.go`).
- **`goal_status`** `{"group"?}`. Un llamante no-main solo ve sus propios
  goals.
- **`goal_pause` / `goal_interrupt` / `goal_resume` / `goal_cancel`**
  `{"group"?,"name"?}`. Los llamantes no-main se fuerzan a sí mismos. Main
  puede dirigir a cualquier grupo.
- **`sched_add`** `{"group"?,"cron","msg"}`.
  - Los llamantes no-main se fuerzan a sí mismos.
  - Cuando main deja `group` vacío, se usa `main` por defecto (`addSched`).
- **`sched_list`**: un llamante no-main solo ve sus propios schedules.
- **`sched_del` / `sched_toggle` (`"enabled"`) / `sched_run`** `{"id"}`. Un
  llamante no-main debe ser dueño del schedule; si no, `ctl: not your
  schedule` (`ownsSched`). Un id desconocido cae en el error subyacente
  "not found".

### 2.3 Diferencias con la CLI del host

- El `koto ctl config` del host puede fijar claves de postura, si la ACL lo
  permite. El `config_set` del plano ctl no puede (`configPostureKey`).
- El plano ctl no tiene verbos `destroy`, `restart`, `runscript`, `acl` ni
  `shell`. Ninguno aparece en el switch de `ctlDispatch`.
