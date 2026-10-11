Traducción de ../configuration.md; la versión en inglés es la autoritativa.

# Configuración

Cada ajuste de abajo fue verificado contra el código fuente de este árbol.
Cada línea lleva una referencia (`file` + función/constante) para que pueda
buscarse con grep. Donde el código no deja claro el comportamiento, el texto
dice **unverified** (no verificado).

Hay tres superficies de configuración:

1. `/etc/koto/koto.env` — entorno de servicio del daemon instalado.
2. Variables de entorno del proceso (`KOTO_*` y algunos nombres heredados)
   leídas por el daemon, `koto ctl`, `koto tui`, `koto update` y el
   instalador.
3. Claves del `config.json` por grupo (`model`, `provider`, `network`, …)
   establecidas con `koto ctl config`, el `/config` de la TUI, o el verbo
   `config_set` del plano ctl.

---

## 1. `/etc/koto/koto.env`

- Ruta: `/etc/koto/koto.env` — `daemon/install.go` const `envFilePath`.
- Cargado por systemd vía `EnvironmentFile=` en la unidad renderizada
  `/etc/systemd/system/koto.service` — `daemon/install.go` `renderUnit`,
  const `unitPath`.
- Escrito por `koto install` (`daemon/install.go` `writeEnvFile`).
  Re-ejecutar install **preserva** los valores ya presentes en el archivo
  (el bucle de fusión sobre `existing` en `writeEnvFile`). Si el archivo
  existe pero no puede leerse, install se niega con `refusing to rewrite
  it: re-running install must preserve the values set there`.
- Cabecera que escribe el instalador: ``Edit and `sudo systemctl restart koto` to apply.`` (`writeEnvFile`).
- Parseo: `KEY=VALUE` por línea, líneas en blanco y comentarios `#`
  ignorados, espacios de clave y valor recortados — `daemon/install.go`
  `parseEnvFile`.

Claves que escribe `writeEnvFile` (los slices `defaults` y `optional`):

Siempre escritas (el valor se preserva si ya estaba definido):

- `KOTO_HOME` — por defecto = el directorio `-state` de install, que por
  defecto es `/var/lib/koto` (`daemon/install.go` const `defaultStateDir`,
  flag `state`).
- `KOTO_BIND=127.0.0.1`
- `KOTO_PORT=8443`

Opcionales (escritas comentadas como `#KEY=` cuando están vacías):

- `KOTO_HOST_CPUS`
- `KOTO_HOST_MEM_MIB`
- `ANTHROPIC_API_KEY` — deliberadamente **no** sembrada desde
  `<state>/creds` (comentario en `writeEnvFile`).
- `KOTO_CLAUDE_BIN` — fijada a la ruta del CLI claude resuelta en el
  momento de la instalación (`installClaudeBin`); a diferencia de las otras
  claves, se escribe el valor recién resuelto en lugar del valor antiguo
  del archivo (rama `kv[0] != claudeBinEnv`).

La unidad también define `Environment=HOME=<state dir>`,
`WorkingDirectory=<state dir>` y `ExecStart=/usr/local/bin/koto daemon`
(`renderUnit`).

---

## 2. Variables de entorno

### 2.1 Daemon (`koto daemon`)

- `KOTO_HOME` — raíz del estado. Cuando está definida se hace absoluta;
  cuando no, el daemon usa su **directorio de trabajo actual** —
  `daemon/daemon.go` `kotoHome`, `here`. Rutas derivadas (`initPaths`):
  `groups/`, `groups.json`, `schedules.json`, `goals.json`, `run/`,
  `metrics.jsonl`.
- `KOTO_BIND` — dirección de escucha gRPC. Por defecto `127.0.0.1` —
  `daemon/daemon.go` `daemonMain`. Un valor no-loopback produce una
  advertencia de postura `KOTO_BIND=<v> exposes the control plane beyond
  this host…` — `daemon/posture.go`.
- `KOTO_PORT` — puerto de escucha gRPC. Por defecto `8443` —
  `daemon/daemon.go` `daemonMain`. El egress del invitado hacia este puerto
  (y hacia `8443` incondicionalmente) se rechaza — `daemon/proxy.go`
  `egressTargetAllowed`.
- `KOTO_PORTS_BIND` — dirección del host en la que se publican los `ports`
  de un grupo. Por defecto `127.0.0.1` — `daemon/fc.go` (`portBind :=
  envOr(...)`).
- `KOTO_HOST_MEM_MIB` — tope de memoria de la flota en MiB. `0` =
  ilimitado; sin definir = 90% del MemTotal de `/proc/meminfo`
  (`fcHostMemDefaultPct = 90`); valores inválidos registran
  `KOTO_HOST_MEM_MIB=%q invalid; using derived default` —
  `daemon/fchostmem.go` `fcHostMemInit`. Si la capacidad no puede
  descubrirse en absoluto y la variable no está definida, el daemon rechaza
  arranques de VM (`REFUSING VM spawns until KOTO_HOST_MEM_MIB is set (use
  0 for explicitly unlimited)`).
- `KOTO_CLAUDE_BIN` — CLI de claude ejecutado para el refresco del token
  OAuth. Por defecto `claude` a secas (búsqueda en PATH) —
  `daemon/claudebin.go` `claudeBin`, const `claudeBinEnv`.
- `ANTHROPIC_API_KEY` — si está definida, prevalece sobre el archivo de
  clave `<state>/creds/anthropic-api-key` — `daemon/proxy.go`
  `currentAPIKey`, `proxyInitPaths`. El archivo se relee por petición.
- `CRED_PATH` (nombre heredado, sin prefijo `KOTO_`) — archivo de
  credenciales OAuth. Por defecto `<state>/.claude/.credentials.json` si
  existe, si no `$HOME/.claude/.credentials.json` — `daemon/proxy.go`
  `proxyInitPaths`.
- `PROXY_PORT` (heredado) — base para la asignación del puerto de proxy por
  grupo. Por defecto `8787` — `daemon/daemon.go` var `PORT_BASE`,
  `initPaths`.
- `BIND` (heredado) — dirección de bind para los listeners del proxy
  en-proceso. Por defecto `127.0.0.1` — `daemon/daemon.go` `daemonMain` →
  `proxyStart`.
- `INVOCATION_ID` (definido por systemd) — solo se comprueba para redactar
  un hallazgo de postura (`the daemon is not running under koto.service`) —
  `daemon/posture.go`.

### 2.2 Instalador (`koto install`)

- `KOTO_HOST_CPUS` — techo de CPU de la flota en núcleos enteros,
  renderizado en la unidad como `CPUQuota=<n×100>%`. Por defecto `nproc-1`
  (mínimo 1); `0` = ilimitado (sin directiva) — `daemon/install.go`
  `hostCPUs`, `installCPUQuota`. Se lee del entorno del proceso del
  **instalador** (`os.Getenv` en `hostCPUs`); el daemon en ejecución no la
  lee. Si un valor presente solo en `koto.env` llega a la unidad
  renderizada es **unverified** (no se encontró ninguna ruta de código que
  cargue koto.env en el entorno del instalador).
- `KOTO_HOME` — directorio de estado por defecto para `stateDirOf`
  (`daemon/install.go`).

### 2.3 Cliente (`koto ctl`, y la prueba de humo del asistente)

De `daemon/ctl_cli.go` (línea env de `ctlUsage`, `ctlClient`,
`ctlCredsDir`, `newKotoClient`):

- `KOTO_ADDR` — destino de conexión. Por defecto `127.0.0.1:8443`.
- `KOTO_CREDS_DIR` — directorio de la PKI. Orden de resolución
  (`ctlCredsDir`): `KOTO_CREDS_DIR`, si no `./creds` si existe, si no
  `$KOTO_HOME/creds`, si no `/var/lib/koto/creds` si existe, si no `creds`.
- `KOTO_CLIENT` — nombre de identidad. Por defecto `agent`. Selecciona
  `client-<name>.crt`, `client-<name>.key`, `token-<name>` en el directorio
  de credenciales.
- `KOTO_CERT`, `KOTO_KEY`, `KOTO_CA` — sobrescriben las rutas de archivo
  individuales (por defecto `client-<name>.crt`, `client-<name>.key`,
  `ca.crt`).
- `KOTO_TOKEN` — el **valor** del token bearer (no una ruta). Si no está
  definido, se lee `token-<name>`; si ese archivo falta: ``no token: set KOTO_TOKEN or provide <path> (mint with `make pki-client NAME=<name> ROLE=agent`)``.
- `KOTO_SERVER_NAME` — override del server-name TLS para destinos de
  conexión que no estén en el SAN del certificado del servidor.
- La versión mínima de TLS es 1.3 (`tls.VersionTLS13` en `newKotoClient`).

### 2.4 `koto tui`

`daemon/tui_cmd.go`: el flag `-state` toma por defecto `$KOTO_HOME`, si no
`/var/lib/koto`. Lee `<state>/creds/token-tui` y ejecuta `koto-tui` con este
entorno:

- `KOTO_CA=<state>/creds/ca.crt`
- `KOTO_CERT=<state>/creds/client-tui.crt`
- `KOTO_KEY=<state>/creds/client-tui.key`
- `KOTO_TOKEN=<contents of token-tui>`
- `KOTO_ENDPOINT=$KOTO_ADDR` (por defecto `127.0.0.1:8443`)
- `KOTO_SERVER_NAME=koto-daemon`
- `KOTO_PROMPTS_DIR=<state>/prompts`
- `SOCK_PATH=<state>/run/tui/koto.sock`
- `KOTO_TUI_TERM=$TERM` (por defecto `xterm-256color`)
- `KOTO_SCRIPTS_DIR=<state>/scripts` — solo si ese directorio existe.

Error de token ausente: ``no TUI token in <creds> — mint one with `koto pki client -creds <creds> tui` ``.

### 2.5 `koto update`

- `KOTO_REPO_URL` — URL completa del repositorio (se recorta la `/` final).
- `KOTO_REPO` — `owner/name`, por defecto `jpzk/koto`; la URL se convierte
  en `https://github.com/<repo>`.
— `daemon/update.go` `updateRepoURL` (espejo de `install.sh`).

El propio `install.sh` lee además `KOTO_KEY_FPR`, `KOTO_KEYSERVER`,
`KOTO_ARCH` (por defecto `x86_64`), `KOTO_VERSION` (por defecto `latest`),
`KOTO_DEST`, `KOTO_RELEASE_PUBKEY` — asignaciones al inicio de `install.sh`.

### 2.6 Variables internas (no son ajustes del operador)

Estas aparecen en el código de `daemon/` pero las define el propio koto; no
las definas tú.

- `KOTO_USERNS_READY` — marcador de etapa del bootstrap de userns
  (`unset`/`1`/`2`) — `daemon/userns.go` const `usernsEnv`.
- `KOTO_JAIL_MAX_UID` — extremo acotado de la banda de uids por VM
  transportado a través de los re-execs del bootstrap; aceptado solo en
  `(30000, 60000]` — `daemon/fcjail.go` const `jailMaxEnv`,
  `fcJailBaseUID`, `fcJailMaxUID`.
- `KOTO_DEFAULT_CLAUDE_MODEL`, `KOTO_DEFAULT_VENICE_MODEL` — inyectadas en
  el entorno del init del invitado con los valores de `defaultClaudeModel` /
  `defaultVeniceModel` — `daemon/fc.go` (mapa `env` del init),
  `daemon/groups.go`. También se inyecta: `ANTHROPIC_BASE_URL=http://127.0.0.1:18888`
  (`fcGuestProxyTCP`), y `NO_PROXY`/`no_proxy=127.0.0.1,localhost` cuando la
  red del grupo no es `none`.
- `KOTO_SESSION` — exportada por turno dentro del invitado por
  `entrypoint.sh`; el daemon la lee de los entornos de proceso del invitado
  para atribuir los trabajos — `daemon/jobs.go`, `daemon/send.go`.
- `KOTO_ENDPOINT`, `KOTO_PROMPTS_DIR`, `KOTO_SCRIPTS_DIR`, `KOTO_TUI_TERM` —
  definidas por `koto tui` para el hijo de la TUI (sección 2.4).

---

## 3. Configuración por grupo (`config.json`)

- Ubicación: `<state>/groups/<group>/.cs/config.json` —
  `daemon/config.go` `groupConfigPath`. Escrito atómicamente (archivo
  temporal + rename, modo 0600) bajo un lock por grupo —
  `updateGroupConfig`.
- El grupo debe existir: `no such group: <g> (spawn it first)`; nombres
  inválidos: `invalid group name` — `daemon/config.go` `configCmd`.
- Los valores desconocidos/inválidos se **ignoran silenciosamente** (se
  conserva el valor anterior) — `daemon/config.go` `applyConfig`.
- Un valor de `null`, `""` o `[]` **borra** la clave — `isClear`.
- La lectura devuelve la configuración *efectiva* con los valores por
  defecto rellenados — `effectiveConfig`. También informa un campo `io` de
  solo lectura (`<MiB>/s / <ops>ops`).

### Claves

- `provider` — `claudesdk` | `venice`. Por defecto `claudesdk`
  (`daemon/groups.go` const `defaultProvider`, `ensureProviderConfig`).
- `model` — cadena libre, juego de caracteres
  `^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`, máximo 128 caracteres
  (`configIdentRE`, `configMaxIdent`). Por defecto cuando no está definida:
  `claude-sonnet-5` bajo claudesdk, `kimi-k2.5` bajo venice
  (`daemon/groups.go` `defaultClaudeModel`, `defaultVeniceModel`).
- `effort` — misma regla de caracteres/longitud que `model`. Se muestra
  como `(default)` cuando no está definida (`effectiveConfig`).
- `network` — `none` | `wan` | `lan` | `full` (`daemon/fcnet.go` consts
  `fcNetNone`…`fcNetFull`). Por defecto `none` (sin NIC). Cualquier otra
  cadena almacenada se lee como `none` (`daemon/groups.go`
  `groupConfig.network`). Se aplica en el siguiente arranque (`/restart`);
  bajar a `none` también surte efecto en la siguiente petición del proxy
  (comentario en `applyConfig`). Clave heredada `internet`: `full` → `wan`,
  `none` → `none`, migrada a `network`.
- `size` — `small` | `medium` | `large` | `xlarge`. Por defecto `small`.
  Se aplica en el siguiente arranque. El disco crece pero nunca se encoge.
  Presets (`daemon/fc.go` `fcSizePresets`; vCPU, RAM MiB, disco de
  workspace, IO MiB/s, IO ops):
  - `small`: 2, 1024, 8 GiB, 100, 15000 (`fcDefaultVcpus`,
    `fcDefaultMemMiB`, `fcWorkspaceBytes`)
  - `medium`: 2, 2048, 12 GiB, 150, 22500
  - `large`: 4, 4096, 16 GiB, 200, 30000
  - `xlarge`: 8, 8192, 24 GiB, 250, 37500
- `root` — `yes` | `no`. Por defecto `no`. Sudo sin contraseña + overlay
  raíz persistente en el invitado; se aplica en el siguiente arranque
  (`daemon/fc.go` `groupRoot`).
- `kvm` — `yes` | `no`. Por defecto `no`. Expone VMX/SVM (virtualización
  anidada) al invitado; se aplica en el siguiente arranque (`daemon/fc.go`
  `groupKVM`). Nota: no aparece en el texto de uso de `koto ctl`, pero el
  flag `-kvm` existe (`daemon/ctl_cli.go`, `fs.String("kvm", …)`).
- `autostart` — `yes` | `no`. Por defecto `no`. Leída solo al arrancar el
  daemon (`autostartGroups`); un `/restart` del grupo no la aplica.
- `ports` — array JSON de enteros o cadena con comas (`8080,3000`). Cada
  puerto debe estar en `[1024, 65535]`; los duplicados y las entradas fuera
  de rango se descartan; se conservan como máximo 32 (`configMaxPorts`).
  Publicados en `KOTO_PORTS_BIND` (por defecto `127.0.0.1`) en el host.

Las claves yes/no fallan cerradas: ausente, no parseable o cualquier otro
valor es `no`; se acepta un booleano JSON para archivos editados a mano —
`daemon/groups.go` `groupConfigBool`.

### Quién puede establecer qué

- **Claves de postura** = `network`, `internet`, `root`, `ports`, `size`,
  `autostart`, `kvm`. En gRPC, una petición Config que lleve cualquiera de
  ellas se autoriza como el verbo sintético exclusivo de administrador
  `config_posture` — `daemon/auth.go` `postureVerb`, `daemon/acl.go`
  `adminOnlyVerbs`.
- En el plano ctl dentro del invitado, `config_set` es solo-main (`ctl:
  verb not allowed for non-main groups: config_set`) y rechaza toda clave
  de postura con `ctl: <key> is an operator-only setting — ask the operator
  (TUI /config, or koto ctl config)` — `daemon/ctl.go` `configPostureKey`.
  Main solo puede por tanto establecer allí `model`, `effort` y `provider`.
- Véase [acl-and-auth.md](acl-and-auth.md) para los roles.
