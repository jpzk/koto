Traducción de ../operations.md; la versión en inglés es la autoritativa.

# Operaciones

Referencia de día 2: dónde van los logs, cómo funcionan la monitorización de
recursos y las alertas, actualizar con `koto update`, eliminar koto con
`koto uninstall`, y los mensajes de error con los que es más probable que te
encuentres. Cada afirmación nombra el archivo fuente y el símbolo del que se
leyó; haz grep del símbolo para comprobarla.

Relacionado: [configuration.md](configuration.md) (variables de entorno,
incl. `KOTO_REPO`, `KOTO_REPO_URL`, `KOTO_HOST_MEM_MIB`),
[ctl-reference.md](ctl-reference.md) (verbos `logs`, `resources`),
[getting-started.md](getting-started.md) (instalación, errores de
preflight), [groups-goals-schedules.md](groups-goals-schedules.md) (errores
de cola y de goals), [acl-and-auth.md](acl-and-auth.md) (errores de
denegación).

## 1. Logs

Hay tres superficies de log distintas.

### 1.1 El log del daemon

- Toda línea de log del daemon pasa por un único embudo, `logDeliver`
  (`daemon/events.go`), llamado por `emitLog` / `emitLogG` / `emitLogf` /
  `emitLogfG` / `emitLogfQuiet`.
- Cada línea se escribe en **stderr** como `[<level>] <subsystem>: <msg>`
  (el `fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", ...)` de `logDeliver`). Bajo
  la unidad de systemd instalada ese stderr acaba en el journal; la pantalla
  final del asistente apunta a `journalctl -u koto -f` ("daemon logs") y el
  fallo de la prueba de humo a `journalctl -u koto -n 50` (`stepHandoff`,
  `daemon/setup_steps.go`).
- Los mensajes se limpian con `sanitize` y luego `flattenInline`, de modo
  que un registro de log es siempre una línea (`logDeliver`).
- El daemon mantiene las últimas **200** líneas en memoria (`logRingMax`,
  `daemon/events.go`). Un stream `SubscribeLogs` nuevo (`koto ctl logs`, el
  panel de log de la TUI) recibe primero ese ring y luego las líneas en vivo
  (`SubscribeLogs`, `daemon/grpc_server.go`). Cada suscriptor tiene un canal
  de 256 entradas; un suscriptor que se queda atrás recibe una línea `warn`
  del subsistema `log`:
  `log stream fell behind: %d line(s) were not delivered to this subscriber`
  (`logDeliver`).
- Las identidades con alcance limitado (no-admin) solo ven líneas atribuidas
  a un grupo que pueden ver; las líneas sin atribuir (grupo `""`) se
  retienen (`logVisible`, `daemon/grpc_server.go`). Véase
  [acl-and-auth.md](acl-and-auth.md).

### 1.2 Las líneas de error se convierten en notificaciones al operador

(`daemon/logalert.go`)

- Toda línea de nivel `error` emitida mediante `emitLogG` se reenvía además
  como notificación de severidad **high** al grupo `main`
  (`forwardLogAlert`, entregada por `notifyDeliver(ctlMainGroup, "high",
  ...)`). `warn` y niveles inferiores se quedan solo en el log.
- Formato del título: `<LEVEL> <subsystem>` más ` [<group>]` cuando hay
  atribución, p. ej. `ERROR fc [dev]` (`logAlertTitle`).
- Límites de tasa: un token bucket por (subsistema, grupo) con ráfaga 5,
  rellenando uno por minuto (`logAlertBurst`, `logAlertRefill`), y un bucket
  de toda la flota con ráfaga 20, uno cada 10 s (`logAlertGlobalBurst`,
  `logAlertGlobalRefill`). Las líneas suprimidas siguen en el log; solo se
  descarta el banner.
- `emitLogfQuiet` omite el reenvío; las alertas de recursos lo usan porque
  encolan su propia notificación (`resNotifyOperator`).
- Los marcadores de notificación no entregados de cada grupo se limitan a 32
  (`notifyQueueMax`, `daemon/logtail.go`). La cola está en memoria, así que
  un reinicio del daemon descarta los marcadores no entregados (comentario
  encima de `notifyQueueMax`).

### 1.3 Transcripciones por grupo

(`daemon/logtail.go`)

- Cada grupo tiene un stream de grupo en `<workspace>/.cs/log`
  (`groupLogPath`) y un stream por slot de concurrencia en
  `<workspace>/.cs/log.<n>` (`slotLogPath`); `logPaths` lista el stream del
  grupo más `groupSlots` streams de slot. `vol(g)` es el montaje del
  workspace del grupo en el host.
- History lee como máximo los últimos 4 MiB de cada stream
  (`historyTailCap`), devuelve como máximo 8 MiB por respuesta
  (`historyBytesMax`), y limita el tamaño de página a 5000 eventos
  (`historyLimitMax`). El scroll hacia atrás más allá de la ventana de 4 MiB
  termina con una página vacía.
- Una línea parcial (sin terminar) se guarda en búfer hasta 8 MiB
  (`tailMaxPartial`); más allá, la cola superviviente se prefija con
  `…[line truncated] ` (`tailTruncMark`).

### 1.4 Métricas del proxy

- El proxy LLM añade un registro JSON por petición completada a
  `<state>/metrics.jsonl` (`METRICS`, fijado en `daemon/daemon.go`;
  `metricsAppend`, `daemon/proxy.go`).
- Rota a los 256 MiB (`metricsMaxBytes`), conservando una generación
  anterior como `metrics.jsonl.1`, así que el peor caso en disco es
  aproximadamente el doble del techo. Una rotación fallida se registra a
  nivel error: `metrics rotation failed (%v): %s is past its %d MiB ceiling
  and still growing`.

## 2. Monitorización de recursos

(`daemon/resources.go`, tipos de red en `daemon/wire/wire.go`)

### 2.1 Recolección

- `resourcesLoop` muestrea una vez al arrancar y luego cada **30 s**
  (`resSampleInterval`). Las cifras vienen del host (`stat`, `statfs`,
  `/proc/<pid>/stat`), así que un invitado atascado o parado sigue
  reportando.
- Excepción: la memoria del invitado y el uso de su propio disco se leen
  desde dentro de cada invitado **en ejecución** con un exec acotado de
  `resGuestProbe` (`stat -f ... /workspace; cat /proc/meminfo`). El exec
  tiene un timeout de 5 s (`resGuestExecTimeout`), corre como máximo 8 a la
  vez (`resGuestExecPar`), y el barrido completo tiene un plazo de 20 s
  (`resSweepDeadline`). Las VMs paradas nunca se arrancan para esto. Si un
  invitado en ejecución se pierde un refresco, su última lectura se conserva
  hasta 3 intervalos de muestreo (`resGuestMaxAge`). La lectura de una VM
  parada se descarta de inmediato. En ambos casos pasa entonces a leerse
  como desconocida (0) (`resGuestRetain`).
- Historial por grupo: 120 muestras (`resRingLen`), es decir, 1 h a 30 s.
- Los snapshots se cachean durante 1 s (`resSnapTTL`), así que sondear más
  rápido devuelve la misma respuesta.
- Las cifras del sistema de archivos del host son `statfs` sobre `ROOT`
  (`<state>/groups`), usando `Bavail` (bloques libres para usuarios no-root)
  como recuento de espacio libre (`hostFSStats`).

### 2.2 Campos (`koto ctl resources`, verbo `resources` dentro del invitado)

Ambos planos se construyen a partir del mismo `resourcesSnapshot`
(`resourcesCtlResp`). Nombres JSON de `wire.GroupResources` /
`wire.HostResources`:

- Por grupo: `group`, `running`, `alloc_bytes`, `declared_bytes`,
  `alloc_pct`, `growth_bytes_per_hour`, `growth_span_seconds`, `rss_bytes`,
  `cpu_pct`, `vcpus`, `mem_mib`, `guest_disk_total_bytes`,
  `guest_disk_avail_bytes`, `guest_disk_used_bytes`, `guest_disk_used_pct`,
  `guest_mem_total_bytes`, `guest_mem_avail_bytes`, `guest_mem_used_pct`,
  `mem_committed_mib`.
- Host: `fs_total_bytes`, `fs_free_bytes`, `fs_used_pct`,
  `alloc_total_bytes`, `provisioned_bytes`, `groups`, `running_groups`,
  `mem_cap_mib`, `mem_committed_mib`, `mem_host_total_mib`.

Qué significan:

- `alloc_bytes` son los bloques asignados de la imagen del workspace
  (`st_blocks × 512`, `statAllocBytes`). Las imágenes son sparse y el
  virtio-blk de Firecracker no tiene discard, así que en la práctica este
  número **solo crece**. Es el coste para el host, no lo lleno que está el
  invitado (comentario de cabecera de `resources.go`).
- `guest_disk_used_pct` es lo lleno que está el sistema de archivos del
  invitado, calculado como `df`: usado ÷ (usado + disponible), de modo que
  los bloques reservados de ext4 no cuentan ni como lo uno ni como lo otro
  (`resGuestDiskPct`). Usa este para saber si un grupo está a punto de
  llenarse.
- `growth_bytes_per_hour` se mide sobre una ventana móvil de 10 minutos
  (`resGrowthWindow`). Lee **0 con `growth_span_seconds` 0** hasta que el
  historial de muestras cubre una ventana completa (por ejemplo, justo
  después de reiniciar el daemon), y nunca baja de cero (`resGrowth`).
- `provisioned_bytes` es la suma del preset `size` de cada grupo. Es normal
  que supere `fs_total_bytes` (comentario sobre
  `HostResources.ProvisionedBytes`).
- `mem_cap_mib` 0 significa que no hay tope de memoria de flota (comentario
  en `wire.HostResources`).

### 2.3 Alertas por umbral

(`resCheckThresholds`, `resNotifyOperator`)

- Los umbrales están cableados en el código: aviso al 80% (severidad
  `normal`), crítico al 90% (severidad `high`) (`resWarnPct`, `resCritPct`,
  `resAlertSeverity`).
- Una alerta solo se dispara cuando el nivel **sube**. Un nivel se rearma
  una vez que el valor cae 5 puntos por debajo del umbral que lo disparó
  (`resClearMargin`, `resArmedLevel`).
- Tres sujetos:
  - el sistema de archivos del host (`resSubjectHostFS`). Título
    `Host disk N% full`.
  - el sistema de archivos del **invitado** de cada grupo en ejecución
    (`resSubjectDisk`). Título `Group <g> disk N% full`. Un grupo cuyo
    tamaño de disco de invitado es desconocido (parado o inalcanzable) no
    genera nada.
  - la CPU de cada grupo promediada sobre las últimas 10 muestras
    (`resCPUAvgWindow`, ≈5 min), como porcentaje de sus propias vCPUs
    (`resSubjectCPU`). Título `Group <g> CPU N% sustained`.
- Entrega: una línea de log del subsistema `resources` (`warn` para el nivel
  1, `error` para el nivel 2, enviada mediante `emitLogfQuiet`) más una
  notificación a `main`. Si la cola de notificaciones de main está llena, el
  daemon registra `notification backlog full, alert dropped: <title>`.

## 3. Actualizar: `koto update`

(`daemon/update.go`)

```
usage: koto update [--check] [--version X] [-y]
```

Flags (`updateMain`):

- `--check`: solo informa. Sale con 0 cuando está al día y con **3** cuando
  existe una actualización (`updateExitAvailable`).
- `--version X`: instala la release X en lugar de la última. Un downgrade
  solo se permite cuando nombras la versión de esta manera.
- `-y`: acepta los prompts, incluido el de parar el daemon. Se pasa al
  instalador como `--yes`.

Pasos:

1. **Versión instalada.** Si `/usr/local/bin/koto` existe
   (`installedKotoBinPath`), se usa la salida de su `version`. Si no, se usa
   el `kotoVersion` del propio binario en ejecución
   (`updateInstalledVersion`).
2. **Última release.** Una petición `HEAD <repo>/releases/latest`; el tag se
   lee de la cabecera `Location` de la redirección sin seguirla
   (`updateLatestRelease`). El repo es `https://github.com/<KOTO_REPO>`
   (por defecto `jpzk/koto`), o `KOTO_REPO_URL` cuando está definida
   (`updateRepoURL`). El comando completo tiene un contexto de 30 s, y el
   cliente HTTP recibe un timeout de 20 s cuando no tiene ninguno.
3. **Decisión** (`updateDecide`; las versiones se comparan como
   `MAJOR.MINOR.PATCH` por `parseSemver`, que ignora una `v` inicial y
   cualquier sufijo `-`/`+`). Mensajes impresos:
   - `koto %s is available (installed: %s)`: más nueva, así que actualiza.
   - `up to date: koto %s is the latest release`
   - `up to date: development build %s is based on the latest release %s`:
     un build como `1.0.0-6-gd5150df-dirty` cuenta como su release base.
   - `release %s replaces development build %s (named explicitly)`
   - `%s is a downgrade from %s (named explicitly)`
   - `installed %s is newer than the latest release %s; nothing to do`
   - `installed %s is not a release build; %s is available`
   - `%q is not a release version`: no actualiza.
4. **Instalación** (`updateRun`). Se niega a correr como root (`run as your
   normal user, not root — koto installs as you and uses sudo where it
   must`). Escribe el `install.sh` **embebido en el binario**
   (`embeddedInstaller`, `//go:embed installer/install.sh`) en un archivo
   temporal y ejecuta `sh <file> --version <target> --no-attach [--yes]` en
   tu terminal. Ese instalador verifica la release y ejecuta el
   `koto install` y el `koto setup` de la nueva release (comentario de
   cabecera del archivo). La instalación para el daemon, y con él todas las
   microVMs en ejecución.
   En caso de fallo: `koto update: the installer failed: <err>`.

El instalador va embebido en lugar de descargarse porque uno descargado
traería su huella fijada de clave de firma por el mismo canal que la release
que comprueba (comentario de cabecera de `update.go`). Las comprobaciones de
firma en sí viven en `install.sh` y se describen en
[getting-started.md](getting-started.md).

## 4. Eliminar koto: `koto uninstall`

(`daemon/uninstall.go`)

```
usage: koto uninstall [flags]
```

Flags (`uninstallMain`):

- `-state DIR`: directorio de estado (por defecto `/var/lib/koto`,
  `defaultStateDir` en `daemon/install.go`).
- `--purge`: borra también el directorio de estado y `/etc/koto/koto.env`.
- `-n`: dry run (simulación). Cada comando privilegiado se imprime como
  `would: sudo …` y nada cambia (`uninstallOpts.step`).
- `-y`: acepta los valores por defecto. **Combinado con `--purge` borra sin
  preguntar.**
- `-no-color`: el código lo acepta pero no aparece en el texto de uso.

Los argumentos posicionales se rechazan: `unexpected argument %q — this
command takes flags only`. Ejecutar como root se rechaza: `run as your
normal user, not root (sudo is used only for the system files)`
(`runUninstall`).

Orden (`runUninstall`):

1. Si no existe ninguno de la unidad, `/usr/local/bin/koto`,
   `/usr/local/bin/koto-tui` o `/etc/koto/koto.env`, imprime
   `nothing installed — …` y para, salvo que se haya pasado `--purge`.
2. **Parar primero.** Si el archivo de unidad
   `/etc/systemd/system/koto.service` (`unitPath`) existe **o**
   `systemctl is-active koto` dice `active` (`systemctlIsActive`), ejecuta
   `systemctl stop koto`, `disable`, elimina la unidad, `daemon-reload` y
   `reset-failed`. Un stop fallido aborta:
   `stop koto: … (fix, or `sudo systemctl kill koto`, then re-run)`. La
   unidad fija `TimeoutStopSec=25` (`daemon/install.go`).
3. **Comprobación de daemon vivo.** Escanea `/proc` buscando un proceso
   `koto daemon` cuya raíz de estado (`KOTO_HOME` de su entorno, o en su
   defecto su cwd) sea el mismo directorio (`runningKotoDaemon`,
   `procKotoHome`). Si todavía hay uno corriendo, se niega con
   `a koto daemon is still running against …`. Con `-n` solo avisa.
4. Elimina `/usr/local/bin/koto` y `/usr/local/bin/koto-tui`.
5. Sin `--purge`, lista lo que conservó (el archivo env, que puede contener
   `ANTHROPIC_API_KEY`, y el directorio de estado con su recuento de grupos
   y tamaño `du -sh`) y para. Un `koto install` posterior los retoma.
6. Con `--purge` (`uninstallPurge`):
   - El directorio de estado lo comprueba `purgeRefusal`. Se niega
     (`refusing to delete <dir>: …`) cuando la ruta no es absoluta, es la
     raíz del sistema de archivos, es tu directorio home, o contiene `.git`
     o `go.work` ("that is a koto clone"). También se niega cuando la ruta
     no tiene ninguno de `.koto-version`, `groups`, `creds`, `fcassets`,
     `groups.json` (`no koto state found there …`).
   - Pregunta `Delete it?` (por defecto no), salvo con `-y`.
   - Su (device, inode) se registra antes del prompt y se comprueba de nuevo
     antes de borrar (`dirIdentity`). Si difieren, se niega con `… is not
     the directory that was checked (it was replaced while the confirmation
     was open) — refusing to delete it`.
   - El directorio de estado se elimina **sin sudo** (`os.RemoveAll`);
     pertenece al usuario.
   - Elimina `/etc/koto/koto.env` (con sudo), y luego
     `rmdir --ignore-fail-on-non-empty /etc/koto`.

Volver a ejecutarlo termina un uninstall interrumpido, porque cada elemento
se detecta y se omite cuando ya no está (comentario de cabecera).

Equivalentes para un checkout de desarrollo en el `Makefile`: `make clean`
elimina la salida de build, `metrics.jsonl`, `proxy.log` y `run`.
`make clean-groups` **borra permanentemente** `groups groups.json
schedules.json goals.json` tras pedirte que escribas `yes` (`FORCE=1` omite
el prompt). `make clean-creds` elimina `creds`.

## 5. Resolución de problemas

Cada entrada da el mensaje exactamente como aparece en el código, de dónde
viene, y qué hacer.

### Instalación / setup

- `daemon did not answer within 45s: … check `systemctl status koto` and
  `journalctl -u koto -n 50``: el paso de prueba de humo del asistente
  (`daemon/setup_steps.go`). Lee el journal.
- `no usable /dev/kvm on this host — groups will not boot until that is
  fixed`: un aviso del mismo paso (`checkKVM`). Las comprobaciones de
  preflight y sus mensajes están listados en
  [getting-started.md](getting-started.md).

### Credenciales y el proxy LLM (`daemon/proxy.go`)

- `no credentials: run `koto claude-login` (or set ANTHROPIC_API_KEY)`:
  no se pudo encontrar ninguna credencial de Anthropic.
- `oauth token refresh failed — turns will 401 until it works (check `koto
  claude-login --status`): …` y `… but the token on disk is still expired —
  the CLI did not rotate it; run `koto claude-login``: problemas de refresco
  de OAuth. Son líneas de error, así que también aparecen como banners en
  `main`.
- `the venice credential is not available on this host; ask the operator`
  (HTTP 503): un grupo con el provider venice, pero no se encontró clave
  (`no venice key: write to …`).
- `koto proxy: <METHOD> <path> is outside the endpoint allowlist for
  provider <p> (the proxy relays inference and model metadata only)` (HTTP
  403, `llmRefuse`): el invitado llamó a una ruta de API que el proxy no
  retransmite.
- `koto proxy: too many concurrent requests for this group` (HTTP 503): más
  de 32 peticiones en vuelo para el grupo, o 128 en total, tras esperar 60 s
  (`proxyMaxInflightPerGroup`, `proxyMaxInflightGlobal`,
  `proxyInflightWait`).
- `koto proxy: request-body memory budget exhausted; retry shortly` (503),
  `koto proxy: request body stalled` (408) y `request body too large or
  unreadable` (413): problemas leyendo el cuerpo de la petición.

### Egress (`daemon/proxy.go`)

- `egress denied: this group's network profile is 'none' (a raised profile
  applies on /restart)` (403): configura `network` (véase
  [configuration.md](configuration.md)) y luego reinicia el grupo.
- `egress blocked: target not permitted under this group's network profile`
  (403): el destino está fuera de lo que permite el perfil del grupo.
- `koto proxy: too many concurrent egress connections for this group` (503).

### Arranque de grupos (`daemon/fchostmem.go`)

- `group %s needs %d MiB (mem %d + %d VMM margin) but the fleet cap is %d
  MiB with %d MiB already committed to running VMs — stop a VM, pick a
  smaller size, or raise KOTO_HOST_MEM_MIB`: el tope de memoria de la flota
  está lleno.
- `refusing to boot %s: the host's memory capacity could not be determined …`:
  el tope no pudo calcularse.

### Turnos

- `this conversation's previous turn is wedged and the group could not
  self-heal — /restart the group …` (`errSessionWedged`,
  `daemon/queue.go`). Reinicia el grupo. Los demás límites de cola se
  cubren en [groups-goals-schedules.md](groups-goals-schedules.md).

### Uninstall

- `a koto daemon is still running against …`: páralo con
  `sudo systemctl stop koto` o `kill <pid>` como dice el mensaje, y vuelve a
  ejecutar.
- `refusing to delete …: that is a koto clone, not an installed state dir`:
  `-state` apunta a un checkout del código fuente. Apúntalo al directorio de
  estado real.
