Traducción de ../groups-goals-schedules.md; la versión en inglés es la autoritativa.

# Sesiones, trabajos en segundo plano, goals, schedules y report/reply

Esta página cubre cómo corre el trabajo dentro de un grupo una vez que
existe: sesiones de chat, la cola de turnos, los trabajos en segundo plano de
`cs-job`, el bucle de goals, los schedules de cron y la ventana de report de
un solo uso entre `main` y un par. Los verbos en sí (argumentos, cadenas de
error, quién puede llamarlos) están en [ctl-reference.md](ctl-reference.md).
Esta página explica qué hacen.

Las rutas como `daemon/goals.go` son relativas a la raíz del repositorio.
Cada afirmación nombra la función o constante que grepear.

## 1. Sesiones

Un grupo (una microVM, un workspace) puede albergar varias conversaciones
independientes, llamadas **sesiones**.

- **La sesión por defecto.** En el cable, `""` significa la sesión por
  defecto del grupo. `"-"` y `"default"` son alias de ella
  (`normalizeSession`, `daemon/sessions.go`).
- **Regla de nombre.** Cualquier otro nombre debe cumplir
  `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$` (`sessionNameRE`). Si no lo hace, el
  error es `invalid session name (must match [A-Za-z0-9][A-Za-z0-9_-]{0,31})`
  (`normalizeSession`).
- **Concurrencia.** Los turnos corren de uno en uno dentro de una sesión, y
  en paralelo entre sesiones. Un grupo puede ejecutar como máximo
  `groupSlots = 10` turnos a la vez (`daemon/queue.go`).
- **Estado de sesión.** Cada sesión guarda su propia conversación. El
  invitado fija un id de sesión de claude por nombre bajo
  `/workspace/.cs/sessions/<name>.id` (comentario en `daemon/sessions.go`).
  El script que hace esa fijación (`entrypoint.sh`) no está rastreado en este
  repositorio, así que ese detalle queda **sin verificar** aquí.
- **Registro.** El host guarda una lista de sesiones con nombre en
  `groups/<g>/.cs/sessions.json` (`sessionRegPath`).
  - Enviar a una sesión la añade a la lista (`registerSession`).
  - La lista guarda como máximo `sessionRegMax = 256` nombres. A partir de
    ahí, los nombres nuevos no se listan, y el daemon registra
    `session registry is full (256); "<name>" will not be listed`.
  - El registro es estado de UI consultivo. Los archivos de id del invitado
    son el registro real (comentario sobre `sessionRegPath`).
- **Prefijo reservado `goal-`.** Cualquier nombre que empiece por `goal-`
  pertenece al bucle de goals (`goalSessionPrefix`, `isReservedSession`).
  Estas sesiones no pueden recibir sends ni clears de clientes ni del plano
  ctl:
  - Un `send` de ctl hacia una falla con
    `ctl: session <s> is reserved for the goal loop` (`ctlDispatch`,
    `daemon/ctl.go`).
  - Los `job_done` y `notify` dirigidos a una sesión reservada se entregan a
    la sesión por defecto (`recordJobDone` en `daemon/notify.go`, el case
    `notify` de `ctlDispatch`).

### Límites de la cola de turnos (`daemon/queue.go`, `enqueue`)

Todo turno pasa por `enqueue`: envíos del operador, disparos de schedule,
callbacks de jobs, turnos de goal y reports.

- **Tamaño de mensaje:** como máximo `sendMsgMax = 1 << 20` bytes (1 MiB).
  Un mensaje mayor falla con `message is <n> bytes; the limit is <max>`.
- **Bytes encolados por grupo:** como máximo `sendQueuedBytesMax = 16 << 20`
  (16 MiB). Por encima:
  `group "<g>" already has <n> bytes queued (max <max>); retry later`.
- **Mensajes encolados por sesión:** como máximo `sendQueueDepth = 64`. Por
  encima: `group "<g>" send queue full (64 pending); retry later`.
- **Grupo deteniéndose:** el error es
  `group "<g>" is stopping; retry once it is down`.
- **Sesión atascada (wedged):** si el turno anterior de la sesión se quedó
  atascado y el grupo no pudo recuperarse, el error es `errSessionWedged`.
  Su texto empieza con `this conversation's previous turn is wedged and the
  group could not self-heal`.

## 2. Trabajos en segundo plano (`cs-job`)

`cs-job` es un script de shell POSIX que corre dentro del invitado
(`sidecar/cs-job`). Arranca un comando en su propia sesión de proceso
(`setsid`) para que el comando sobreviva al final del turno. El daemon solo
lee el estado de los jobs y recibe los callbacks de finalización
(`daemon/jobs.go`, `daemon/notify.go`).

### Comandos

La línea de uso del script lista estos comandos:

- **`run [--no-notify] -- <cmd...>`**
  - Imprime un id de job y retorna de inmediato.
  - Los callbacks están activados por defecto. `--no-notify` los desactiva.
    `--notify` se acepta pero no hace nada.
  - Sin comando, falla con `cs-job run: no command`.
- **`spawn [--no-notify] <prompt...>`**: lo mismo que
  `run -- cs-subagent <prompt...>`. `cs-subagent` hace una llamada LLM:
  imprime la respuesta final por stdout y el progreso por stderr
  (`sidecar/cs-subagent`).
- **`list`**: imprime las columnas `ID STATUS RC SESSION CMD`. El comando se
  corta a 80 bytes.
- **`wait <id> [secs]`**: espera hasta que el job termina. El presupuesto
  por defecto es 600 s. Borra el marcador `notify` del job, así que
  **esperar cancela el callback**. En timeout: `cs-job: wait timeout
  (<budget> s) — job <id> still running` (salida 1).
- **`status <id>`**: imprime el estado del job. El archivo de estado
  contiene `running` o `done`. Si no hay archivo de estado, imprime
  `unknown`.
- **`peek [-n N|-new] <id>`**: retorna de inmediato. Imprime una línea de
  cabecera `<id> <status> rc=<rc> bytes=<n> age=<s>s`, y luego salida:
  - Por defecto, las últimas 40 líneas.
  - Con `-new`, solo los bytes añadidos desde el último peek. El offset se
    guarda en `<jobdir>/peek`.
  - En ambos casos la salida se limita a `CS_PEEK_MAX` bytes (por defecto
    8000).
- **`logs <id>`**: imprime el stdout y stderr combinados del job.
- **`rm <id>`**: envía SIGTERM al grupo de procesos del job, espera hasta
  unos 2 s (20 × 0.1 s), envía SIGKILL si sigue vivo, y luego borra el
  directorio del job.
- **`clean`**: borra todos los directorios de jobs excepto los jobs en
  ejecución y los jobs cuyo callback aún se está enviando (marcador
  `notifying`).

### Reglas y disposición en disco

- **Ids de job.** Los ids son basenames de `mktemp`. Cualquier id no
  alfanumérico se rechaza con `cs-job: invalid job id: <id>` (`chkid`). La
  comprobación del daemon es `jobIDRE = ^[A-Za-z0-9]{1,32}$`
  (`daemon/jobs.go`).
- **Tope de concurrencia.** Como máximo `CS_MAX_JOBS` jobs pueden correr a
  la vez (por defecto 8, `MAX=${CS_MAX_JOBS:-8}`). Por encima:
  `cs-job: job cap reached (<n>/<max> running); wait or raise CS_MAX_JOBS`.
- **Directorio del job.** Cada job recibe `/workspace/.cs/jobs/<id>/`, que
  contiene `cmd`, `started`, `session`, `status`, `pgid`, `out` (stdout y
  stderr juntos), `rc`, y los marcadores `notify` / `notifying`.
- **Atribución de sesión.** Un job pertenece a la sesión que estaba en
  `$KOTO_SESSION` cuando arrancó, o a `default` si no estaba definida.
- **Huérfanos.** La cabecera del script dice que un job matado por un
  reinicio de la VM es re-etiquetado después como `orphaned` por
  `entrypoint.sh`. Ese archivo no está en este repositorio, así que esto
  queda **sin verificar**.

### Callback de finalización

1. Cuando el comando sale, el job ejecuta `cs-job _notify <id>`. Esto
   escribe una línea `job_done` en `/workspace/.cs/ctl` con `id`, `rc`,
   `total` (tamaño de la salida), `session`, y el base64 de los últimos
   1500 bytes de salida.
2. El daemon guarda como máximo `ctlMaxJobOut = 8 << 10` bytes de esa cola
   (`daemon/ctl.go`). Registra el resultado (`recordJobDone`) y refresca su
   lista de jobs de inmediato (`kickJobsRefresh`).
3. Los resultados se agrupan por (grupo, sesión). Un temporizador de
   `notifyDebounce = 3s` se reinicia con cada nueva finalización
   (`armNotifyTimerLocked`, `daemon/notify.go`). Cuando dispara, todos los
   resultados pendientes se envían a la sesión que lanzó los jobs como **un
   solo** turno. Ese turno empieza con `Background job(s) completed:` y cita
   cada cola con `> ` (`flushNotify`).
4. Si ese turno no puede encolarse, se reintenta tras
   `notifyRetryDelay = 30s`.
5. Topes de buffer:
   - `notifyMaxPending = 64` resultados por clave. Los más antiguos se
     descartan.
   - `notifyMaxKeysPerGroup = 32` sesiones pendientes por grupo. Los
     resultados de sesiones extra van a la sesión por defecto.

### Cómo ve el host los jobs

El host no puede montar el workspace del invitado. Lee el estado de los jobs
ejecutando un pequeño script de shell a través del agente invitado
(`fcReadJobs`, `jobsListScript`).

- **Caché:** los resultados se cachean durante `jobsRefreshTTL = 3s`.
- **Fallos:** tras una lectura fallida, el host espera
  `jobsFailBackoff = 60s` antes de reintentar. Tras
  `jobsFailWarnAfter = 3` fallos seguidos registra una advertencia.
- **Tamaño de lista:** se listan como máximo `jobsMaxPerGroup = 256` jobs.
- **Logs:** `JobLogs` devuelve los últimos `jobLogsDefaultTail` bytes por
  defecto (`fcJobLogs`). Los límites están en [protocol.md](protocol.md).

## 3. Goals (el bucle de goals)

Un goal le dice al daemon que siga ejecutando turnos en un grupo hasta que
un juez independiente acepte los criterios de aceptación, o hasta alcanzar el
tope de iteraciones. Todo esto está en `daemon/goals.go`.

### Crear un goal (`goalSetBy`)

- **Campos obligatorios:** grupo, texto y criterios. Si falta uno, el error
  es `group is required`, `goal text is required` o
  `acceptance criteria are required`.
- **Límite de tamaño:** el texto y los criterios se limitan cada uno a
  `goalTextMax = 16 << 10` bytes. Una entrada mayor se rechaza, no se
  trunca: `goal text is <n> bytes; the limit is <max>`.
- **No sobre main:** un goal sobre `main` se rechaza con
  `cannot set a goal on main`.
- **Tope de iteraciones:** un `max_iterations` de `<= 0` se convierte en
  `goalDefaultMaxIter = 20`. Los valores por encima de
  `goalMaxIterCeil = 200` se bajan a 200.
- **Límite de goals activos:** un grupo puede ejecutar como máximo
  `goalMaxActive = 8` goals a la vez. Por encima:
  `group "<g>" already has <n> active goals; finish or cancel one first`.
- **Registros viejos:** fijar un goal nuevo borra los registros de goals
  terminados del grupo (met, cancelled, exhausted).
- **Ids:** un id de goal son 12 caracteres hex (`newSchedID`, `goalIDLen`).
- **Persistencia:** los goals se guardan en `<state>/goals.json`
  (`GOALS_FILE`, `daemon/daemon.go`).

### Nombres y sesiones

Cada ejecución de goal recibe dos sesiones propias, nombradas a partir del
**nombre** del goal (`goalSessionSlug`). Los registros antiguos sin nombre
usan el id en su lugar.

- El worker corre en `goal-<name>` (`goalWorkSessionFor`).
- El juez corre en `goal-<name>-judge` (`goalJudgeSessionFor`).

Cómo se elige el nombre (`resolveGoalNameLocked`):

- Si no se da ninguno, se usa un slug del texto del goal
  (`slugifyGoalName`).
- **Longitud:** como máximo `goalNameMax = 16` caracteres. Si no:
  `goal name must be at most 16 characters`.
- **Apto para sesión:** ambos nombres de sesión derivados deben pasar
  `sessionNameRE`. Si no:
  `goal name "<n>" is not usable as a session name`.
- **No debe parecer un id:** un nombre que cumpla `goalIDRE` (12 caracteres
  hex) se rechaza. El error empieza con
  `goal name "<n>" looks like a goal id`.
- **Colisiones:** si el nombre está tomado por una sesión de chat o por las
  sesiones de otro goal, el daemon prueba `-2`, `-3`, etc., recortando la
  base para que el resultado quepa en 16 caracteres. Después de `-99` se
  rinde con `could not find a free name near "<base>"`.

### Ciclo de vida

Los estados son las constantes `goalStatus*`: `planning`,
`awaiting_approval`, `running`, `paused`, `met`, `cancelled` y `exhausted`.
`met`, `cancelled` y `exhausted` son finales (`goalTerminal`).

1. **Plan (opcional, activado por defecto).**
   - Con `plan`, el goal empieza en `planning`. El daemon ejecuta un turno
     de planificación (`goalPlanPhase`, prompt `goalPlanMsg`) y pasa el goal
     a `awaiting_approval`.
   - Envía una notificación: `goal plan ready for review`.
   - **Quién puede aprobar:** la RPC `GoalApprove` del host, o el
     `goal_approve` del plano ctl. El plano ctl solo funciona cuando el
     grupo fijó el goal sobre sí mismo (`ctlGoalSelfSet`). Un plan que main
     o el operador fijó sobre un grupo necesita que lo apruebe un humano.
   - Cuando main fija un goal con `plan:false` por el plano ctl, se cambia
     a plan-primero (`ctlDispatch`, véase
     [ctl-reference.md](ctl-reference.md)).
2. **Iterar.**
   - Cada iteración en `running` incrementa `Iteration`.
   - Limpia el contexto de la sesión del worker (`clearGoalSession` →
     `clearSessionContext`). El transcript del host se conserva.
   - Envía el prompt del worker (`goalWorkerMsg`). El prompt incluye:
     - el directorio del goal `/workspace/goal/<name>` (`goalDirFor`);
     - el informe de cierre del turno anterior (`LastHandoff`, tomado del
       transcript por `goalCaptureHandoff`, cortado a sus **últimos**
       `goalNoteMax = 4000` caracteres);
     - el último feedback del juez, si lo hay (`LastFeedback`).
3. **Reclamación.** El worker reclama la finalización enviando `goal_done`
   mientras su turno está en ejecución (`recordGoalDone`).
   - Las reclamaciones solo se aceptan durante el turno del goal. Si no:
     `no goal turn in flight for "<g>"`.
   - La primera reclamación gana.
   - Una reclamación sin id se acepta solo cuando hay exactamente un turno
     de goal abierto en el grupo (`resolveGoalWindowLocked`).
4. **Juez.** El juez corre solo tras una reclamación (`goalJudgeCheck`):
   - Su contexto se limpia, y recibe `goalJudgeMsg`.
   - Debe responder con `goal_verdict`.
   - Un turno de juez sin veredicto se reintenta una vez.
   - Tras `goalSilentJudgeMax = 3` reclamaciones seguidas sin veredicto, el
     goal se pausa con razón `judge`.
   - Si el veredicto es no-cumplido, las razones se guardan como
     `LastFeedback` y el bucle continúa.
5. **Met** (`goalMarkMet`).
   - El estado pasa a `met`.
   - Se envía una notificación `goal met`.
   - Un turno `[koto] goal finished: ...` va a la sesión **por defecto** del
     grupo, citando la nota del worker (`goalInformCoordinatorMsg`).
6. **Exhausted** (`goalMarkExhausted`).
   - Cuando `Iteration >= MaxIterations` y nada fue aceptado, el goal
     termina como `exhausted`. No se pausa.
   - Se envía una notificación de severidad alta `goal exhausted`.
   - Un turno `[koto] goal ended WITHOUT completing: ...` va a la sesión por
     defecto (`goalCoordinatorExhaustedMsg`).

### Pausar, reanudar y cancelar

- **Pause** (`goalPause`): solo desde `running`. La razón es `operator`.
- **Interrupt** (`goalInterrupt`): solo desde `running`. Pausa con razón
  `interrupted` y además cancela el turno actual del worker.
- **Resume** (`goalResume`): solo desde `paused`. El goal vuelve a su estado
  anterior (`running`, o `planning` si se pausó mientras planificaba).
- **Approve** (`goalApprove`): solo desde `awaiting_approval`. El goal pasa
  a `running`.
- **Cancel** (`goalCancel`): desde cualquier estado no final. El goal pasa a
  `cancelled`.

Pausas que el daemon aplica por sí mismo (`goalPauseWith`,
`goalPauseOnStop`):

- **`stalled`:** falló un reset de contexto, un enqueue o un turno, o un
  turno nunca produjo `turn_end`. Antes de rendirse, el encolado se
  reintenta `goalEnqueueRetries = 3` veces, con `goalRetrySleep = 30s` de
  separación (`goalEnqueue`).
- **`judge`:** véase el paso 4 de arriba.
- **`stopped`:** el grupo fue detenido. La notificación dice
  `goal paused (group stopped)`.

Un turno de goal encolado cuyo goal ya no está en `running` ni `planning` se
descarta en lugar de ejecutarse (`goalTurnShouldRun`). El texto del error es
`goal turn skipped (goal no longer active)` (`daemon/queue.go`).

**Destroy:** destruir un grupo cancela sus goals y borra sus registros
(`goalCancelOnDestroy`, `delGoalsFor`).

**Reinicio del daemon:** los goals en `running` o `planning` recuperan sus
drivers cuando el daemon arranca (`resumeGoalDrivers`, llamado desde
`daemon/daemon.go`).

### Resolver a qué goal se refiere un verbo

Los verbos approve, pause, interrupt, resume y cancel toman un nombre
opcional (`resolveGoalLocked`). El nombre coincide con el nombre de un goal
o con su id. Si lo omites:

- con exactamente un goal posible, se usa ese goal;
- sin goal: `group "<g>" has no goal`;
- con varios: `group "<g>" has <n> goals — name one of: ...`.

### Nota de seguridad

El comentario al principio de `daemon/goals.go` dice que la ventana de
reclamación protege contra **accidentes, no contra malicia**. Todas las
sesiones de un grupo comparten un uid, un workspace y un FIFO ctl, así que
cualquier turno podría escribir una reclamación. Usa grupos separados cuando
la separación tenga que aguantar frente a un agente hostil.

## 4. Schedules (cron)

Los schedules están en `daemon/schedules.go`, y el parser en
`daemon/cron.go`. Los schedules se guardan en `<state>/schedules.json`
(`SCHED_FILE`) con modo `0600` (`saveSched`).

### Sintaxis cron (`parseCron`, `parseField`)

- **Cinco campos:** minuto `0-59`, hora `0-23`, día del mes `1-31`, mes
  `1-12`, día de la semana `0-6`.
  - El día de la semana `7` está fuera de rango.
  - Los nombres de mes y de día no se aceptan, porque los valores pasan por
    `strconv.Atoi`.
- **Alias** (`cronAliases`): `@yearly` / `@annually` → `0 0 1 1 *`,
  `@monthly` → `0 0 1 * *`, `@weekly` → `0 0 * * 0`, `@daily` /
  `@midnight` → `0 0 * * *`, y `@hourly` → `0 * * * *`. No hay `@reboot`.
- **Sintaxis de campo:** `*`, `N`, `A-B`, listas con `,`, y pasos con `/S`.
  - `S` debe estar entre 1 y el tamaño del rango del campo.
  - `N/S` significa desde N hasta el máximo del campo, cada S.
- **Día del mes y día de la semana:** si ambos están restringidos (ninguno
  es `*`), un día coincide cuando **cualquiera** de los dos coincide. Es la
  regla clásica de cron (`dateMatches`).
- **Zona horaria:** la zona horaria local del daemon. Se usa `time.Now()`
  sin zona explícita.
- **Cadenas de error:**
  - `cron: expected 5 fields, got <n> (...)`
  - `bad step "<s>" (must be 1..<n>)`
  - `range <a>-<b> out of bounds <lo>-<hi>`
  - Los errores de campo llevan prefijo: `minute: `, `hour: `,
    `day-of-month: `, `month: ` o `day-of-week: `.

### Límites (`addSched`)

- **Campos obligatorios:** `group is required`, `msg is required`.
- **Expresión cron:** como máximo `schedCronMax = 256` bytes.
- **Mensaje:** como máximo `schedMaxMsg = 16 << 10` bytes.
  Por encima: `msg too long (<n> bytes; max <max>)`.
- **Por grupo:** como máximo `schedMaxPerGroup = 100` schedules.
  Por encima: `schedule cap reached for <g> (100)`.
- **A nivel de daemon:** como máximo `schedMaxTotal = 2000` schedules.
  Por encima: `daemon-wide schedule cap reached (2000)`.
- **Debe disparar en algún momento:** una expresión sin coincidencia en los
  próximos 4 años se rechaza: `cron "<expr>" has no fire within 4 years`
  (`next`).
- **Ids:** un id de schedule son 12 caracteres hex (`newSchedID`).

### Disparo

- **Momento:** `cronLoop` despierta en cada frontera de minuto del reloj de
  pared y dispara cada schedule habilitado cuyo `NextDueAt` haya pasado.
- **Adónde va:** un disparo encola el mensaje en la **sesión por defecto**
  del grupo (`fireSchedule` → `enqueueSend(it.Group, "", it.Msg)`). Emite un
  evento `sched_fired`, o `sched_run` cuando se arranca a mano con
  `sched run` (`runSchedNow`).
- **Sin recuperación:** los disparos perdidos mientras el daemon estaba
  caído se saltan. Al cargar, `NextDueAt` se recalcula desde la hora actual
  (`loadSched`).
- **Disparo rechazado:** si la cola del grupo rechaza un disparo programado,
  el schedule se vuelve a marcar como pendiente y se reintenta en el
  siguiente tick (`rearmSchedule`). Un `sched run` manual no se reintenta.
- **Sin coincidencia futura:** si un schedule no tiene más coincidencias, se
  deshabilita (`Enabled = false`, `cronLoop`).
- **Entradas guardadas malas:** una entrada de `schedules.json` cuyo cron ya
  no parsea se descarta al cargar, con una advertencia
  `load: drop <id> (bad cron ...)`.
- **Destroy:** destruir un grupo borra sus schedules (`delSchedsFor`).

Quién puede añadir, listar o cambiar qué schedules se cubre en
[ctl-reference.md](ctl-reference.md) §2.2.

## 5. Report/reply entre main y un par

Esto es `daemon/report.go`. Permite a `main` delegar trabajo en un par y
recibir **una** respuesta de vuelta.

1. **Armar.** `main` envía con `"reply":true` (`send` de ctl). El daemon
   encola el mensaje con `reportRequestNote` anexada. La nota le dice al par
   cómo enviar `report`. En el mismo paso el daemon arma la ventana de
   report de ese grupo (`armReportAndEnqueue`).
   - Si el encolado falla, no se arma nada.
   - Enviar de nuevo mientras una ventana está abierta la reemplaza ("gana
     el último").
   - `from_session` elige cuál de las sesiones de main recibe el report. Un
     valor inválido o reservado significa la sesión por defecto de main.
2. **Report.** El par escribe
   `{"cmd":"report","msg":"<base64>"}` en `/workspace/.cs/ctl` (el case
   `report` de `ctlDispatch`).
   - El texto se sanea y se recorta. Un resultado vacío falla con
     `ctl: report: empty message`.
   - Se corta a `reportMsgMax = 4000` bytes, en una frontera UTF-8
     (`truncateRunes`). Cuando se corta, el turno entregado termina con
     `(report clipped to 4000 bytes of <n>)`.
3. **Entregar.** `deliverReport` encola en main un turno que empieza con
   `[report from <g>]`. El texto del par se cita línea a línea con `> `
   (`reportQuoteBody`), seguido de una advertencia de que el contenido lo
   escribió un par.
4. **Reglas:**
   - **Un report por armado.**
     - Sin ventana abierta, el error es
       `no reply pending: main must delegate with reply:true first (one report per delegation)`.
     - Mientras un report ya se está entregando, el error es
       `a report for this delegation is already being delivered (one report per delegation)`.
   - **Main saturado.** Si la cola de main rechaza el report, la ventana se
     reabre y el par recibe
     `main is backlogged, report not delivered — retry later: ...`.
   - **Expiración.** Una ventana expira `reportArmedMax = 24h` después de
     armarse.
   - **Main no puede reportar:** main recibe
     `ctl: main has no delegator to report to`.
   - **Stop y destroy:** detener o destruir el par cierra su ventana
     (`disarmReport` en `daemon/groups.go`).

## Véase también

- [ctl-reference.md](ctl-reference.md): la sintaxis y los errores exactos de
  los verbos `sched*`, `goal*`, `send`, `report`, `job_done` y `notify`.
- [protocol.md](protocol.md): las RPCs `Goal*`, `Sched*`, `Jobs`, `JobLogs`
  y `JobTail`.
- [architecture.md](architecture.md): dónde se sitúan la cola, el invitado y
  el FIFO ctl.
