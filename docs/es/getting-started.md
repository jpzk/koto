Traducción de ../getting-started.md; la versión en inglés es la autoritativa.

# Primeros pasos

Esta página cubre cómo llevar koto desde cero hasta un daemon en ejecución:
qué necesita el host, las tres etapas (adquirir, integrar, configurar), las
comprobaciones de plataforma que condicionan la instalación con sus mensajes
exactos, y qué hace cada paso del asistente.

Cada afirmación tiene una referencia a la fuente (archivo más función,
constante u objetivo del Makefile) para que puedas buscarla con grep. Donde
los comentarios del código o el texto de uso discrepan del código, esta
página sigue al código y lo indica.

Véase también: [configuration.md](configuration.md) (claves de koto.env,
variables de entorno `KOTO_*`, configuración de grupo),
[acl-and-auth.md](acl-and-auth.md) (la PKI y los tokens que genera el
asistente), [ctl-reference.md](ctl-reference.md) (`koto ctl`).

## 1. Dos vías de entrada

### 1.1 El instalador por tubería

```sh
curl -fsSL https://kotovm.com/install.sh | sh
curl -fsSL https://kotovm.com/install.sh | sh -s -- --version 1.0.0
```

`install.sh` ejecuta cuatro etapas en orden (comentario de cabecera de
`install.sh`):

1. fetch: clona la etiqueta de la release en un directorio temporal, luego descarga y verifica en él los artefactos firmados (lo mismo que `make fetch`).
2. install: `koto install` (lo mismo que `make install`).
3. setup: `koto setup` (lo mismo que `make wizard`).
4. tui: `koto tui`.

Bajo `curl | sh`, stdin es la tubería, así que las etapas 2 a 4 leen de
`/dev/tty`. Cuando no hay terminal, el script se detiene tras la etapa 2 e
imprime los comandos restantes (comentario de cabecera de `install.sh`).

Flags de `install.sh` (`install.sh` `usage()`):

- `--version X`: la release a instalar. Por defecto `latest`, de `VERSION="${KOTO_VERSION:-latest}"`.
- `--no-attach`: detenerse después de `koto setup` en lugar de abrir la TUI. Es lo que ejecuta `koto update`.
- `--yes`: aceptar los valores por defecto de `koto install` y `koto setup`, incluida la parada del daemon en una actualización.
- `--download-only`, `--dest DIR`, `--manifest FILE`: solo descargar y verificar. `--dest` apunta por defecto al directorio actual. `--manifest` añade una comprobación contra una lista sha256sum(1) que no provino de la release.
- `--next TEXT`: el comando a sugerir cuando termina una ejecución con `--download-only`.
- `-h`, `--help`.

Lo que `install.sh` rechaza (cada caso es un `die` en `install.sh`):

- Un host que no sea Linux/x86_64: ``koto is Linux/x86_64 only (this is <os>/<arch>)``.
- Una herramienta ausente: ``<tool> is required but not installed``. Las herramientas son `curl gpg zstd sha256sum`, más `git` salvo que se haya pasado `--download-only`.
- Una huella de clave vacía: ``no release key fingerprint — refusing to accept a signature from whatever key turns up``.

El orden de verificación (comentario de cabecera de `install.sh`, sección
"WHAT THIS VERIFIES"):

1. Obtener la clave de firma por su huella.
2. Verificar la firma separada de `SHA256SUMS` y exigir que la línea `VALIDSIG` nombre esa huella.
3. Tomar los nombres de los assets del `SHA256SUMS` firmado.
4. Comprobar cada asset antes de descomprimirlo, y preparar todo junto al destino hasta que el conjunto completo haya pasado.

La huella fijada es `A6E69ED6BC4779F3281721484CC1AFDE15B64EA3`
(`install.sh` `KEY_FPR`; Makefile `RELEASE_KEY_FPR`). Las variables de
entorno que `install.sh` lee como overrides están listadas en
[configuration.md](configuration.md) §2.5.

### 1.2 Desde un clon, en tres etapas

El comentario al inicio del Makefile ("GETTING koto RUNNING IS THREE
STAGES") define las etapas:

1. ADQUIRIR los cinco artefactos, por una de dos rutas: `make fetch` o `make build`.
2. INTEGRARLOS en el sistema: `make install`.
3. CONFIGURAR el sistema instalado: `make wizard`.

```sh
make fetch && make install && make wizard   # download — minutes
make build && make install && make wizard   # build it yourself — ~40 min
```

`make setup` es un atajo de `make build`, `make install` y `make wizard`, en
ese orden (objetivo `setup` del Makefile).

Cada etapa se niega a hacer el trabajo de la etapa anterior y nombra el
comando que lo hace. Toda etapa puede re-ejecutarse sin riesgo (comentario de
cabecera del Makefile).

## 2. Requisitos

### 2.1 Plataforma

- Solo linux/amd64. `checkPlatform` (`daemon/setup_checks.go`) falla con cualquier otro `GOOS/GOARCH` con este remedio: ``koto needs linux/amd64: Firecracker microVMs need KVM, and the guest kernel and rootfs are built for x86_64.``
- `/dev/kvm` debe estar presente y con lectura/escritura para todos (modo `&0o006 == 0o006`). Véanse `checkKVMAt` y §4.2.
- Namespaces de usuario sin privilegios, y un rango subuid/subgid para tu usuario. Véanse §4.3 y §4.4.

El README lista las distribuciones donde compilar y ejecutar se ha
verificado: Fedora 44, Ubuntu 24.04 LTS, Ubuntu 26.04 LTS y Arch
(`README.md`, "From sources"). Esta página no ha vuelto a verificar esa
lista.

### 2.2 Solo en tiempo de compilación (ruta `make build`)

Docker o podman. Cuando ambos están presentes el Makefile prefiere docker
(Makefile `CONTAINER ?= $(shell command -v docker … || command -v podman …)`;
el mismo orden está en `containerEngine` en `daemon/setup_checks.go`). Los
binarios Go se compilan dentro de la imagen fijada por digest `GO_IMAGE` con
`CGO_ENABLED=0`, así que el host no necesita toolchain de Go (Makefile
`GO_BUILD_RUN`). Si no hay ninguno de los dos motores, la compilación falla
antes de compilar (Makefile `need-container`):

```
podman or docker is required to build koto (sudo dnf install podman / sudo apt install podman)
```

Las demás herramientas de compilación son `git`, `make` y `curl`
(`checkBuildTools`). Las líneas de instalación del README añaden además
`passt` (y `uidmap` en Ubuntu) para podman rootless:

```sh
sudo dnf install -y git make podman                  # Fedora 44+
sudo apt install -y git make podman passt uidmap     # Ubuntu 24.04+
sudo pacman -S --needed git make podman passt        # Arch
```

### 2.3 Tiempo de ejecución (lo que el daemon instalado invoca)

Estos vienen de `checkRuntimeTools` en `daemon/setup_checks.go`, con los
nombres de paquete de Fedora y Debian:

- `mkfs.ext4`, `e2fsck`, `resize2fs`: `e2fsprogs`. Crean, comprueban y hacen crecer la imagen del workspace de cada grupo.
- `tar`: `tar`. Entrega skills dentro de un invitado.
- `newuidmap`, `newgidmap`: `shadow-utils` en Fedora, `uidmap` en Debian/Ubuntu. Mapean el uid/gid por VM con el que corre el monitor de la microVM enjaulada.
- `claude`: solo advertencia. Solo se necesita para la autenticación OAuth (suscripción), donde el proxy lo invoca para refrescar el token. Su sugerencia pide `node >= 22` y `npm i -g @anthropic-ai/claude-code`. En hosts de la familia Debian también señala que el paquete `nodejs` de Ubuntu es v18, y sugiere `sudo snap install node --classic --channel=22` o nvm.

Podman no es una dependencia de ejecución. Nada de lo que koto ejecuta es un
contenedor (comentario de `checkRuntimeTools`; cabecera del Makefile).

## 3. Etapa 1: adquirir

### 3.1 Los cinco artefactos

Tanto `ARTIFACTS` del Makefile como `artifacts` en `daemon/install.go` los
listan:

- `koto`
- `koto-tui`
- `fcassets/firecracker`
- `fcassets/vmlinux`
- `fcassets/rootfs.img`

Ambas rutas producen exactamente estos.

### 3.2 `make fetch`

`make fetch` ejecuta `install.sh --version $(DIST_VERSION) --dest . --manifest artifacts.sha256 --download-only --next "make install"` con `KOTO_BASE_URL="$(KOTO_DIST_URL)/$(DIST_VERSION)"` (objetivo `fetch` del Makefile). Los valores por defecto son:

- `KOTO_DIST_URL ?= https://github.com/jpzk/koto/releases/download`
- `DIST_VERSION ?= 1.0.5`
- `DIST_ARCH ?= x86_64`

Los assets de la release están comprimidos con zstd y se llaman
`<basename>_<version>_<arch>.zst`, por ejemplo
`rootfs.img_fedora44_x86_64.zst` (comentario del Makefile encima de
`DIST_ARCH`). El `artifacts.sha256` del repositorio suma los artefactos tal
como quedan instalados, tras la descompresión (comentario del Makefile
encima de `MANIFEST`).

Si `DIST_VERSION` es `unreleased`, fetch sale con código 1 y
``no koto release has been published yet, so there is nothing to fetch.``

### 3.3 `make build`

`make build` ejecuta estos sub-makes en orden: `koto`, `koto-tui`,
`firecracker`, `kernel`, `rootfs` (objetivo `build` del Makefile). Cada uno
es un no-op una vez que su salida es más reciente que sus entradas.

- `koto`: `go build -trimpath -ldflags "-s -w -X main.kotoVersion=$(KOTO_VERSION)" -o koto ./daemon`. Primero copia `install.sh` a `daemon/installer/install.sh`, porque el instalador va embebido para `koto update`. `KOTO_VERSION` es `git describe --tags --always --dirty`, con la fecha como respaldo. `koto version` lo imprime (`daemon/main.go`, `kotoVersion`, por defecto `"dev"`).
- `firecracker`: `fcguest/build-firecracker.sh`.
- `kernel`: `fcguest/build-kernel.sh`.
- `rootfs`: `fcguest/build-rootfs.sh`. Sus entradas incluyen `fcguest/*.go`, `sidecar/*` y `protocol/` (Makefile `FCGUEST_SRC`), así que un cambio en el protocolo reconstruye la imagen del invitado.

Las compilaciones no son reproducibles. Una compilación desde fuente se
considera de confianza porque la hiciste tú, y nunca se comprueba contra el
manifiesto de la release (comentario del Makefile encima de `dist`;
comentario de `requireArtifacts` en `daemon/install.go`).

## 4. Etapa 2: `make install` / `koto install`

`make install` ejecuta primero `require-artifacts`. Si falta alguno de los
cinco archivos imprime ``missing artifact(s): …`` y la sugerencia de
fetch/build, y luego ejecuta `./koto install` (objetivos `install` y
`require-artifacts` del Makefile).

### 4.1 Flags de `koto install`

Provienen de `installMain` e `installUsage` en `daemon/install.go`:

- `-state DIR`: el directorio de estado. Por defecto `/var/lib/koto` (`defaultStateDir`). El valor debe pasar `unitSafeValue`; si no, obtienes ``-state: …`` y salida 2.
- `-y`: aceptar los valores por defecto y no preguntar nunca.
- `-no-color`: desactivar el color. Este flag se parsea pero no aparece en el texto de uso.

El texto de uso dice que install "starts the service". El código solo lo
arranca en una actualización. Una instalación nueva ejecuta `systemctl
enable koto` e imprime ``service enabled (not started — `koto setup` starts
it)`` (`runInstall`). El daemon no puede levantarse hasta que el asistente
haya generado `server.crt`.

### 4.2 Qué hace install, en orden

Estos pasos provienen de `runInstall` en `daemon/install.go`:

1. Detecta una actualización (`installed()`: existen tanto `/etc/systemd/system/koto.service` como `/etc/koto/koto.env`) e imprime ``existing installation detected — upgrading in place``.
2. Se niega a ejecutarse fuera de un clon: ``run this from a koto clone (no Makefile/daemon here)``. También rechaza artefactos ausentes (`requireArtifacts`).
3. Rechaza root: ``run as your normal user, not root — the daemon runs as you (sudo is used only for the system files)``.
4. Ejecuta las comprobaciones de requisitos del host (`preflightGate`, §5).
5. Detiene un daemon en ejecución (`installStopDaemon`).
6. Siembra el directorio de estado (`seedStateDir`). Crea el directorio con `sudo install -d -o <uid> -g <gid> -m 0750` cuando no existe. Un directorio existente debe pasar `stateDirTrusted`: no debe ser un symlink, debe ser un directorio, debe pertenecerte y no debe tener bit de escritura para grupo/otros. El fallo se lee ``<dir>: <reason> — remove it or choose another -state``. Install crea después `groups/`, `creds/`, `fcassets/`, `prompts/` y `run/` (modo 0750), enlaza `.claude -> creds`, y migra `groups/`, `groups.json`, `schedules.json`, `goals.json` y `metrics.jsonl` desde el clon. Nunca sobrescribe (`migrateState`).
7. Instala los binarios: `sudo install -m 0755 <self> /usr/local/bin/koto`, y `koto-tui` a `/usr/local/bin/koto-tui`. Cuando falta `koto-tui` solo advierte.
8. Si AppArmor restringe userns y el operador aceptó la oferta (o ya existe un perfil de koto), instala o refresca el perfil de AppArmor (`installAppArmorProfile`).
9. Resuelve el CLI `claude` (`installClaudeBin`) y escribe `/etc/koto/koto.env` (`writeEnvFile`; claves en [configuration.md](configuration.md)).
10. Renderiza `/etc/systemd/system/koto.service` (`renderUnit`, `ExecStart=/usr/local/bin/koto daemon`). Si la unidad cambió, ejecuta `systemctl daemon-reload`.
11. En una actualización ejecuta `systemctl start koto`. En una instalación nueva ejecuta `systemctl enable koto`.
12. Escribe `<state>/.koto-version` desde `git describe`, con `kotoVersion` como respaldo.

Cada acción privilegiada es un exec de `sudo` separado que se muestra antes
de ejecutarse (`sudoRun`; comentario de cabecera de `daemon/install.go`).

## 5. Comprobaciones de plataforma (preflight)

`runPreflight` en `daemon/setup_checks.go` ejecuta estas comprobaciones en
este orden: plataforma, `/dev/kvm`, userns anidado, subuid/subgid,
herramientas de compilación, herramientas de ejecución, espacio en disco,
red. Tanto `koto install` como `koto setup --check` las ejecutan.

El grupo de compilación (motor de contenedores más `git`, `make`, `curl`)
pasa por `advisory()`, que convierte sus fallos en advertencias, porque la
ruta fetch no necesita esas herramientas.

### 5.1 Cómo decide la puerta (`preflightGate`)

- Cualquier fallo duro detiene la instalación. Con uno o más fallos informa ``<n> unmet requirement(s) — see the remediation above``. Si fallan a la vez un fallo duro y `/dev/kvm`, informa ``<n> unmet requirement(s), all shown above — fix all of them (<m> blocking, plus /dev/kvm)``.
- Que falle `/dev/kvm` por sí solo es blando. La pregunta ``Continue without KVM?`` tiene el no por defecto. Responder sí instala un daemon cuya API responde pero donde ningún grupo puede arrancar (`preflightResult.noKVM`). Responder no aborta (`errSetupAborted`, texto `aborted`).
- Cuando userns está ``restricted by AppArmor`` y `apparmor_parser` está disponible, install ofrece ``Install an AppArmor profile for koto?`` (por defecto sí). Rechazarlo falla con ``nested user namespaces are unavailable — see the remediation above, or re-run and accept the AppArmor profile``.

### 5.2 `/dev/kvm` (`checkKVMAt`)

- Ausente: detalle ``not present``. El remedio es habilitar la virtualización en BIOS/UEFI, o la virtualización anidada cuando koto mismo corre en una VM.
- El modo carece de rw para todos: detalle ``mode NNNN — not world-accessible``. Pertenecer al grupo kvm no ayuda. El VMM Firecracker enjaulado corre con un uid por VM y un conjunto de grupos suplementarios vacío (`kvmRemediation`; véase `daemon/fcjail.go`). El primer remedio impreso es una regla udev:

  ```sh
  echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666"' | sudo tee /etc/udev/rules.d/99-kvm.rules
  sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=kvm
  ```

  La alternativa es una ACL POSIX por uid sobre la banda de uids del jail, calculada desde el inicio de tu `/etc/subuid` vía `fcJailHostUID(start, fcJailBaseUID)` hasta `fcJailBaseUID+ctlMaxSpawn`. El texto del remedio advierte de dos problemas con la ACL: systemd-logind la reescribe, y esta comprobación lee los bits de modo, así que sigue fallando.
- El dispositivo se abre pero falla: detalle ``present but not usable: <err>``. El remedio es ``sudo usermod -aG kvm $USER``.
- OK: ``usable (mode NNNN)``.

### 5.3 Namespaces de usuario anidados (`checkUserns`)

- `/proc/sys/user/max_user_namespaces` es `0`: fallo ``user.max_user_namespaces = 0``. El remedio es ``sudo sysctl -w user.max_user_namespaces=15000``, persistido en `/etc/sysctl.d/`.
- AppArmor restringe el userns sin privilegios (`apparmorUsernsRestricted`). La comprobación ejecuta entonces el binario instalado (`apparmorProfileWorks(installedKotoBin)`). Si eso tiene éxito el resultado es ok, ``granted to koto by AppArmor profile (host-wide restriction still on)``. Si no, falla con ``restricted by AppArmor``. El remedio de respaldo (`usernsRemediation`) es el `kernel.apparmor_restrict_unprivileged_userns=0` a nivel de host, persistido en `/etc/sysctl.d/60-koto.conf`.
- Archivo ilegible: advertencia ``unknown``.
- En caso contrario ok: ``allowed (<n>)``.

### 5.4 subuid/subgid (`checkSubuid`)

Tanto `/etc/subuid` como `/etc/subgid` necesitan una línea para tu nombre de
usuario o uid. Cuando falta una, la comprobación falla con ``no range for
<user>`` y este remedio:

```sh
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 <user>
podman system migrate
```

### 5.5 Motor de contenedores (`checkContainerEngine`, consultivo)

- Ningún motor: ``neither found``.
- `<engine> info --format json` falla: `` `<engine> info` failed: … ``.
- Solo podman: advierte si podman no es rootless (``running as root``) y si falta pasta (``not found``, instala `passt`). Docker no recibe más comprobaciones.

### 5.6 Disco y red

- `checkDisk` comprueba el directorio de trabajo actual y advierte por debajo de 20 GiB libres.
- `checkNetwork` envía un `HEAD https://github.com` con un timeout de 5 s y solo advierte. Install funciona sin conexión si los artefactos ya están presentes.

## 6. Etapa 3: `make wizard` / `koto setup`

`make wizard` comprueba que `./koto` sea ejecutable. Si no, imprime
``./koto is missing — acquire the artifacts first:`` y sale con 1. Si lo es,
ejecuta `./koto setup` (objetivo `wizard` del Makefile).

### 6.1 Flags

De `setupMain` y `setupUsage` en `daemon/setup.go`:

- `--check`: modo doctor. Ejecuta cada sonda de preflight y el `detect` de cada paso, y no cambia nada. Sale con 0 cuando está listo, 1 en caso contrario (`setupCheck`). Imprime ``koto is fully set up``, ``<n> host requirement(s) unmet`` o ``<n> step(s) pending — run `koto setup` to finish``.
- `-y`: aceptar los valores por defecto y no preguntar nunca. Los pasos que necesitan entrada real fallan.
- `--only ID`: ejecutar un solo paso. Se omite su `detect`, así que el paso siempre se ejecuta. Un ID desconocido da ``no such step "<id>" (see `koto setup --list`)``, salida 2. El ejemplo del texto de uso `--only rootfs` nombra un paso que ya no existe.
- `--from ID`: empezar en este paso.
- `--no-color`: salida plana. El estilado también se desactiva cuando `NO_COLOR` está definido o `TERM` es `dumb` o está vacío (`colorOK`, `daemon/setup_ui.go`).
- `--list`: imprimir los ids de los pasos en orden.

Sin TTY y sin `-y` ni `--check`, setup sale con 2 y ``setup needs a terminal
(or pass -y / --check)``.

El directorio de estado es `KOTO_HOME`, o `/var/lib/koto` cuando no está
definido (`setupCtx.stateDir`). Las credenciales van a `<state>/creds`.

### 6.2 Pasos

Los pasos, en orden (`setupSteps` en `daemon/setup_steps.go`):

1. `installed` (Sistema instalado). Detecta `installed()`. Nunca instala nada. Cuando koto no está instalado falla con la sugerencia "adquirir, luego `make install`".
2. `pki` (Identidades TLS). La detección necesita todo esto bajo `creds/`: `ca.crt`, `ca.key`, `server.crt`, `client-tui.crt`, `token-tui`, `client-agent.crt`, `token-agent`. Pregunta si alcanzarás el daemon desde otra máquina y, en tal caso, toma SANs extra como `IP:192.168.1.20,DNS:koto.lan`. Los SANs por defecto son `DNS:koto-daemon`, `DNS:localhost` e `IP:127.0.0.1` (`defaultServerSANs`, `daemon/pki.go`). Ejecuta `pkiInit`, luego genera los clientes `tui` y `agent`, ambos con rol `admin`. Una CA existente nunca se regenera. Véase [acl-and-auth.md](acl-and-auth.md).
3. `auth` (Credenciales de Anthropic). Hecho cuando existe `creds/.credentials.json` (OAuth) o `creds/anthropic-api-key`, o cuando `ANTHROPIC_API_KEY` está definido. Ejecuta `authConnect`, el mismo flujo que `koto claude-login`. Bajo `-y` falla, porque ambos métodos necesitan una persona. El error apunta a ``koto claude-login --api-key-stdin < keyfile``.
4. `service` (Arrancar el daemon). Ejecuta `sudo systemctl restart koto`. Informa not-done siempre que esta ejecución haya cambiado credenciales (`credsChanged`). El comentario del asistente da como razón que el proxy resuelve las credenciales una vez, al arrancar. Ese comentario está obsoleto: el proxy ahora lee las credenciales por petición (`currentAPIKey` y `readCreds` en `daemon/proxy.go`), y `claudeLoginUsage` dice lo mismo. Para credenciales basadas en archivo el reinicio es por tanto inocuo pero no estrictamente necesario. `ANTHROPIC_API_KEY` del entorno sigue fijo durante la vida del proceso (`envAPIKey`).
5. `smoke` (Prueba de humo). Construye un cliente para la identidad `tui` contra `127.0.0.1:8443` y llama al RPC `List`. Reintenta cada 2 s durante hasta 45 s, y luego falla con ``daemon did not answer within 45s: … check `systemctl status koto` and `journalctl -u koto -n 50` ``. Si `/dev/kvm` no es usable advierte ``no usable /dev/kvm on this host — groups will not boot until that is fixed``.
6. `done` (Listo). Imprime ``koto is installed and running.`` y los siguientes comandos: `koto tui`, `koto ctl list`, `systemctl status koto`, `journalctl -u koto -f`. También imprime el directorio de estado y `/etc/koto/koto.env`, y cómo añadir un cliente con `koto pki client -creds <state>/creds <name>`.

La reanudación funciona por detección, no por un archivo de estado.
Re-ejecutar `koto setup` omite cada paso cuyo `detect` esté satisfecho
(cabecera de `daemon/setup.go`). Una ejecución interrumpida imprime
``interrupted — re-run `koto setup` to resume from here``. Un paso fallido
imprime ``step "<id>" failed: …``.

## 7. Primer uso

```sh
koto tui            # /new <name> spawns your first agent, /exit detaches (README.md)
koto ctl list       # list groups; see ctl-reference.md
koto setup --check  # health of the install, changes nothing
```

Cuando el turno de un grupo falla con 401, ejecuta `koto claude-login`. Sus
flags son `--status`, `--method oauth|api-key`, `--api-key-stdin` y
`--no-verify` (`claudeLoginUsage`, `daemon/claude_login.go`).

Para los providers con clave, `koto venice-login` escribe `creds/venice.key`
y `koto minimax-login` escribe `creds/minimax.key` (ambos 0600, en el state
dir; la clave nunca entra en un guest). La clave viene de `--key-file F`,
`--api-key-stdin` o de un prompt oculto en la terminal. Antes de escribir
nada se comprueba con una petición que requiere auth: Venice
`GET /api/v1/api_keys/rate_limits` (Bearer), MiniMax
`GET /anthropic/v1/models` (`x-api-key`). Un 2xx la guarda. Un 401/403 la
rechaza, sale con 1 y deja intacto el fichero de clave existente. Si el
provider no responde (error de red, timeout de 15s, 5xx) se guarda con un
aviso. `--no-verify` omite la petición; `--status` solo dice si hay clave.
Los errores llevan el código de estado o el error de transporte, nunca la
clave (`providerKeyCheck`, `daemon/provider_keycheck.go`). El paso `auth`
del asistente usa la misma comprobación.

Los demás subcomandos de nivel superior despachados en `daemon/main.go` son
`daemon`, `fcjail`, `ctl`, `setup`, `claude-login`, `openai-login`, `venice-login`, `minimax-login`, `uninstall`, `update`,
`install`, `tui`, `userns-check`, `pki` y `version`. Un subcomando
desconocido imprime ``unknown subcommand: <name>`` y sale con 2.
