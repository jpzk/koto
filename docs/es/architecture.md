Traducción de ../architecture.md; la versión en inglés es la autoritativa.

# Arquitectura

Cómo encajan las piezas de koto, desde el cliente gRPC del operador hasta el
proceso del agente dentro de una microVM. Cada afirmación nombra el archivo
(y normalmente la función o constante) del que proviene, para que puedas
comprobarla con `grep`. El diagrama de alto nivel vive en el `README.md` del
repositorio, bajo `## Architecture`. Esta página es la versión de ese
diagrama verificada contra el código.

Relacionado: [getting-started.md](getting-started.md) (cómo instalarlo),
[configuration.md](configuration.md) (los ajustes),
[acl-and-auth.md](acl-and-auth.md) (la frontera de confianza de gRPC),
[firecracker-vsock.md](../firecracker-vsock.md) (las notas de diseño
originales de vsock/jailer).

## 1. Los niveles de un vistazo

1. **Nivel 1, usuario del host.** Los clientes gRPC (`koto tui`, `koto ctl`)
   y el almacén de credenciales bajo `creds/` en el directorio de estado.
   Véase [acl-and-auth.md](acl-and-auth.md).
2. **Nivel 2, el daemon de koto.** Un solo proceso Go (`koto daemon`,
   `daemonMain` en `daemon/daemon.go`). Corre rootless (sin privilegios de
   root) como el usuario que lo instaló, bajo una unidad de systemd, y
   alberga el servidor gRPC, el proxy LLM por grupo, la pasarela de red
   gVisor, cron, los goals y el plano ctl.
3. **Nivel 3, un VMM Firecracker enjaulado por grupo.** Cada VMM se inicia
   mediante `koto fcjail` (`daemon/fcjail.go`) y arranca una microVM cuyo
   PID 1 es `fc-agent` (`fcguest/main.go`).

Cada capa asume que la capa interior ya ha sido comprometida. Es la
afirmación de "defensa en profundidad" del README. Las secciones siguientes
describen lo que cada capa hace realmente en el código.

## 2. Nivel 2: el daemon

### 2.1 Orden de arranque

El orden es el de `daemonMain` en `daemon/daemon.go`:

1. `usernsEnsure()` (`daemon/userns.go`) re-ejecuta el daemon dentro de un
   user namespace nuevo. `newuidmap`/`newgidmap` instalan el mapeo desde
   `/etc/subuid` y `/etc/subgid`. Esto es incondicional. Si falla, el daemon
   imprime ``koto: run `koto userns-check` to diagnose`` y sale con código 1.
   La razón, según la cabecera de `userns.go`: fcjail debe escribir un
   `uid_map` de dos entradas por VMM, y `fcJailFixupPerms` debe hacer chown
   de archivos al uid por-VM. Ninguna de las dos cosas es posible para un
   proceso sin privilegios fuera de un userns.
2. `initPaths()` deriva las rutas de estado desde `HERE` (`kotoHome()`):
   - `groups/` (`ROOT`)
   - `groups.json`, `schedules.json`, `goals.json`
   - `run/` (`SOCK_DIR`)
   - `metrics.jsonl`

   `PORT_BASE` tiene por defecto `8787` y puede sobreescribirse con
   `PROXY_PORT`.
3. Se ejecutan `hardenStatePaths()` y `fcClearStalePids()`, luego
   `fcHostMemInit()` y `fcCgroupInit()` (véase §4.3), y después
   `postureStartup()` (`daemon/posture.go`). El paso de postura registra en
   el log cada restricción a nivel de host que *no* está en vigor.
4. `proxyStart(bind)` arranca el proxy LLM en el propio proceso (§3).
5. SIGTERM/SIGINT se registran *antes* de que arranque la primera VM
   (comentario "audit 2026-09-11 L146" en `daemon/daemon.go`). Esto
   garantiza que un apagado todavía ejecuta `fcStopAll`.
6. `spawnEnsure("main", true)` levanta el grupo `main`. Después,
   `go autostartGroups()` arranca en segundo plano los grupos configurados
   con `autostart=yes`.
7. El servidor gRPC escucha en TCP `KOTO_BIND:KOTO_PORT`. Los valores por
   defecto son `127.0.0.1` y `8443`. Usa mTLS (`serverTLSConfig`) más los
   interceptores `authUnary`/`authStream`. Los detalles están en
   [acl-and-auth.md](acl-and-auth.md).

No existen FIFOs de ctl en el lado del host. El plano ctl de un grupo se
sirve por vsock una vez que su VM está arriba. Así lo dice el comentario
encima del listener gRPC en `daemon/daemon.go`.

### 2.2 El sandbox de systemd

El texto de la unidad lo genera `daemon/install.go`. Contiene las siguientes
directivas:

- `Type=exec` y `ExecStart=/usr/local/bin/koto daemon`.
- `ProtectSystem=strict`.
- `ProtectHome=yes`, o `ProtectHome=tmpfs` más binds de solo lectura cuando
  el binario `claude` vive bajo tu directorio home (`installHomeScoping`).
- `DevicePolicy=closed` y `DeviceAllow=/dev/kvm rw`.
- `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`.
- `RestrictNamespaces=user mnt pid net ipc uts cgroup`.
- `LockPersonality=yes` y `RestrictRealtime=yes`.
- `NoNewPrivileges=no`. Debe quedarse en `no`: de lo contrario el
  `newuidmap` setuid sería ignorado, como explica el comentario en
  `installCapBounding`.
- `CapabilityBoundingSet=…`, construido por `installCapBounding`.
- `CPUQuota`, solo cuando `KOTO_HOST_CPUS` lo pide (`installCPUQuota`).
- `WantedBy=multi-user.target`.

## 3. El proxy LLM (inyección de credenciales)

El proxy corre dentro del proceso del daemon. No es un binario separado
(`proxyStart`, `proxyListen` en `daemon/proxy.go`).

- **Un socket unix por grupo, no TCP.** El listener de cada grupo es
  `run/proxy/p<port>.sock` (`proxySockDir`, `proxySockPath`) dentro de un
  directorio solo del propietario (`0700`). El comentario encima de
  `proxySockDir` da la razón (auditoría M1): un puerto TCP en loopback
  permitiría a cualquier uid local obtener acceso LLM con credenciales. El
  número de puerto del grupo (asignado desde `PORT_BASE`, `allocPort` en
  `daemon/groups.go`, persistido en `groups.json`) sigue existiendo:
  identifica al grupo y da nombre al archivo del socket.
- **Invariante uno-a-uno.** Si llamas a `proxyListen` con un puerto ya
  ligado a otro grupo, falla con
  `port %d already bound for group %q (requested for %q)`.
- **Cómo lo alcanza el invitado.** El invitado no tiene camino TCP hacia el
  host. `ANTHROPIC_BASE_URL` en el invitado es `http://127.0.0.1:18888`
  (`fcGuestProxyTCP` en `daemon/fc.go`). fc-agent escucha ahí y reenvía
  cada conexión al puerto vsock 9000 del host. El daemon empalma entonces
  esa conexión con el socket del proxy del grupo (`fcSpliceToProxy`).
- **Upstreams.** Las constantes son `upstream = "https://api.anthropic.com"`
  y `veniceUpstream = "https://api.venice.ai"`. El provider del grupo se lee
  de `config.json` en cada petición (`groupProvider`).
- **Credenciales.** Se leen por petición (`currentAPIKey`, `readCreds`). La
  clave de Venice vive en `venice.key` junto al archivo de credenciales
  (`veniceKeyPath`). El invitado nunca ve ninguna de las dos.
- **Límites.** Un cuerpo de petición con búfer se limita a
  `proxyMaxBody = 64 << 20` (64 MiB). El `http.Server` usa
  `ReadHeaderTimeout` de 30 s e `IdleTimeout` de 90 s. El comentario de
  `proxyListen` explica por qué importan: recolectan conexiones para que la
  tabla de conexiones vsock de la VM no se llene.
- **Puerta de egress L7.** `egressTargetAllowed` en `daemon/proxy.go` aplica
  la política de red del grupo a los destinos CONNECT/absolute-form del
  proxy. `proxySetBootNetwork` registra la política al crear la VM. Según la
  cabecera de `fcnet.go`, esta puerta comparte `fcClassifyDst` con el filtro
  L3 (§5).

## 4. Nivel 3: el VMM enjaulado

### 4.1 fcjail

De `daemon/fcjail.go`:

- **Cómo arranca.** El daemon se re-ejecuta como `koto fcjail <spec>` con
  `CLONE_NEWUSER|NEWNS|NEWPID|NEWNET|NEWIPC|NEWUTS`. El hijo
  (`fcjailMain`) hace lo siguiente, en orden:
  1. Monta con bind-mount solo lo que Firecracker necesita dentro de un
     chroot por-VM: el binario, el kernel, el rootfs, la imagen de workspace
     de esta VM, `/dev/kvm`, `/dev/urandom` y el directorio de sockets vsock
     de la VM.
  2. Hace chroot dentro.
  3. Activa `PR_SET_NO_NEW_PRIVS`.
  4. Baja al uid por-VM.
  5. Ejecuta Firecracker.

  El filtro seccomp integrado de Firecracker queda activado, porque koto
  nunca pasa `--no-seccomp`.
- **Banda de uids por-VM.** Empieza en `fcJailBaseUID = 30000` y termina en
  `fcJailMaxUID = 60000`. Si `/etc/subuid` concede menos, el final se
  recorta, y el valor recortado se pasa a las etapas re-ejecutadas en
  `KOTO_JAIL_MAX_UID` (`jailMaxEnv`).
- **Derivación del uid.** `fcJailUID` calcula
  `uid = fcJailBaseUID + (proxyPort − PORT_BASE)`. Como cada grupo tiene un
  puerto de proxy distinto, cada grupo obtiene un uid distinto. Un resultado
  fuera de banda es un error, no un fallback ("audit 2026-09-11 L6").
- **Rutas.** El chroot es `run/fc/<g>.jail` (`fcJailDir`). Los sockets están
  en `run/fc/<g>.sock/` (`fcSockDir`). El directorio run `run/fc` es `0700`
  (`fcEnsureRunDir`).

### 4.2 La VM

De `daemon/fc.go`:

- **Activos.** El binario, el kernel y el rootfs son `fcassets/firecracker`,
  `fcassets/vmlinux` y `fcassets/rootfs.img` bajo `HERE` (`fcBinPath`,
  `fcKernelPath`, `fcRootfsPath`).
- **Disco persistente.** El `/workspace` del invitado es
  `groups/<g>/workspace.img` (`fcWorkspaceImg`). Es una imagen ext4 por
  grupo conectada por virtio-blk. Nunca la montes en el host mientras la VM
  corre.
- **Presets de tamaño** (`fcSizePresets`). Cada entrada es vCPU / RAM /
  disco / IO MiB/s / IO ops/s:
  - `small` (el valor por defecto): 2 / 1024 MiB / 8 GiB / 100 / 15000
  - `medium`: 2 / 2048 MiB / 12 GiB / 150 / 22500
  - `large`: 4 / 4096 MiB / 16 GiB / 200 / 30000
  - `xlarge`: 8 / 8192 MiB / 24 GiB / 250 / 37500

  Toda VM recibe además una ráfaga única de IO de
  `fcIOBurstBytes = 256 << 20`. El VMM corre con nice `fcVMNice = 10`.
- **Worker del invitado.** Corre como uid/gid 1000
  (`fcWorkerUID`/`fcWorkerGID`, el usuario `node`).
- **Dispositivo vsock.** Se configura con `"guest_cid": 3`
  (`fcVMConfig`). El invitado marca al host en el CID 2 (`hostCID` en
  `fcguest/main.go`).

### 4.3 Límites de recursos alrededor del VMM

- **cgroup por-VM** (`daemon/fccgroup.go`). Cada VM corre en `vms/<group>`.
  `cpu.weight` escala con el número de vCPUs
  (`fcCgroupWeightPerVCPU = 50`). `memory.high` se fija en
  `mem_mib + fcCgroupMemMarginMiB` (512). Deliberadamente nunca es
  `memory.max`. Si no hay un árbol de cgroups escribible, el daemon registra
  una línea, ejecuta las VMs sin cgroups, y el spawn registra `cgroup=off`.
- **Tope de memoria de la flota** (`daemon/fchostmem.go`). `vms/` recibe
  `memory.max`. `fcHostMemAdmit` se niega a arrancar una VM que no quepa. El
  tope viene de `KOTO_HOST_MEM_MIB`, donde `0` significa ilimitado. Si no
  está definido, el tope por defecto es el 90% de MemTotal
  (`fcHostMemDefaultPct = 90`).
- **Tope de conexiones.** Un grupo puede tener como máximo
  `fcMaxConnsPerGroup = 64` conexiones vsock invitado→host abiertas entre
  todos sus listeners. Las conexiones adicionales se rechazan con
  `refusing guest vsock connection: %d already open (per-group cap)`
  (`fcAcceptLoop`).

## 5. Redes

- **`network=none`, el valor por defecto.** La VM no tiene NIC. Su único
  egress es el proxy L7 sobre vsock 9000 (cabecera de `fc.go`, cabecera de
  `fcnet.go`).
  En el tramo de Anthropic, el proxy rechaza (403) los cuerpos de petición
  que declaran herramientas del lado del servidor (cualquier elemento de
  `tools[]` cuyo `type` no sea `custom`) o `mcp_servers`, porque estas
  contactan hosts de terceros desde el lado del proveedor; los valores de
  `anthropic-beta` solo se reenvían desde una lista permitida
  (`daemon/proxy_servertools.go`). En todos los perfiles, las cabeceras de
  petición del invitado solo se reenvían desde una lista permitida.
- **`network=wan|lan|full`.** `fcSpawn` conecta una pasarela gVisor en
  espacio de usuario (`containers/gvisor-tap-vsock`, `fcNetGateway` en
  `daemon/fcnet.go`) y abre el listener 9003 solo cuando
  `groupNetwork(g) != fcNetNone`. La política se captura en el spawn, así
  que cambiarla requiere reiniciar el grupo.
- **Topología** (constantes en `daemon/fcnet.go`):
  - invitado `192.168.127.2` (`fcNetGuestIP`)
  - pasarela `192.168.127.1` (`fcNetGatewayIP`), que también sirve DNS
  - subred `192.168.127.0/24`, MTU 1500

  Las tramas Ethernet van con framing estilo Qemu, con un prefijo de
  longitud de 4 bytes big-endian por trama, sobre vsock 9003.
- **Filtro de tramas.** `fcEgressConn`, `fcClassifyDst` y `fcDstAllowed` se
  ejecutan antes de que el netstack vea un paquete. Hay cuatro clases de
  destino:
  - `ctl`: loopback, link-local y las IPs de las propias interfaces del
    host. Siempre se descartan.
  - `gw`: `192.168.127.0/24`. Siempre se permite.
  - `lan`: RFC1918, ULA de IPv6, multicast, broadcast limitado y CGNAT
    `100.64/10`. Permitido para `lan` y `full`.
  - `wan`: todo lo demás. Permitido para `wan` y `full`.

  El netstack bloquea además `169.254.169.254` (`Ec2MetadataAccess=false`).
- **Salvedad** (declarada en la cabecera de `fcnet.go`). El DNS se resuelve
  en el host, fuera del filtro de tramas. Un invitado `lan` puede por tanto
  seguir resolviendo nombres públicos. Las tramas de datos sí se filtran por
  IP de destino.

Comentario obsoleto: el comentario sobre `fcPortNet` en `daemon/fc.go` dice
que el 9003 se conecta "only for internet=full". El código en `fcSpawn` lo
conecta para cualquier valor de red distinto de `none`. Esta página sigue al
código.

## 6. Mapa de puertos vsock

El mapa autoritativo es el comentario de cabecera de `daemon/fc.go`, junto
con las constantes `fcPortProxy`, `fcPortLog`, `fcPortCtl`, `fcPortNet`,
`fcPortLogSlot` y `fcPortAgent`.

El vsock híbrido de Firecracker funciona así. Para conexiones
invitado→host, Firecracker conecta a `<uds>_<port>` y el daemon escucha
ahí. Para conexiones host→invitado, el daemon conecta a `<uds>` y envía
`CONNECT <port>\n` (`fcHostDial`). `<uds>` es `run/fc/<g>.sock/v` (`fcUDS`).

Invitado → host:

- **9000, egress de API** (`fcPortProxy`). TCP crudo empalmado al socket del
  proxy del grupo (`fcSpliceToProxy`).
- **9001, retirado** (`fcPortLog`). Era el flujo de log crudo. `fcSpawn` no
  abre listener para él.
- **9002, plano ctl** (`fcPortCtl`). Protobuf con framing `CtlRequest` /
  `CtlResponse` (`fcCtlConn`). En el invitado, la interfaz de shell es el
  archivo `/workspace/.cs/ctl`, y las respuestas se añaden a
  `/workspace/.cs/ctl.out` (`fcguest/ctl.go`).
- **9003, tramas ethernet L3** (`fcPortNet`). Solo para grupos con red (§5).
- **9004, flujos de turno** (`fcPortLogSlot`). Una conexión por turno.
  Primero llega `TurnOpen{slot}`, luego los eventos del turno, y al final
  `TurnEnd`. `fcTurnSink` (`daemon/fcturn.go`) los vuelca al log del host
  `.cs/log.<slot>`. El texto del invitado con forma de marcador se escapa,
  de modo que el invitado no puede falsificar marcadores.

Host → invitado:

- **10000, RPC del agente** (`fcPortAgent`). Un `AgentRequest` con framing
  por conexión. La cabecera de `fc.go` enumera las operaciones `init`,
  `msg`, `exec`, `exec_stream` y `shutdown`, y nombra `run_script` y
  `shell_attach` como operaciones de streaming que responden con
  `AgentFrame`s.

El framing (`daemon/fcframe.go`, replicado byte a byte en
`fcguest/frame.go`) es una longitud `uint32` big-endian seguida de un
mensaje protobuf. La longitud máxima se comprueba antes de reservar
memoria:

- `fcFrameMaxCtl = 1 << 20` (1 MiB) para el 9002
- `fcFrameMaxGuest = 16 << 20` (16 MiB) para tramas de agente invitado→host
- `fcFrameMaxHost = 64 << 20` (64 MiB) para peticiones host→invitado

Hay más detalle en [protocol.md](protocol.md) y
[firecracker-vsock.md](../firecracker-vsock.md).

Comentario obsoleto: la cabecera de `fcguest/main.go` todavía describe el
9001 como un FIFO de log vivo y llama al RPC del agente "JSON line". El lado
del daemon (cabecera de `fc.go`, `fcframe.go`) usa protobuf con framing y no
tiene listener en el 9001. Donde ambos discrepan, confía en `daemon/fc.go`.

## 7. Dentro del invitado

- **El PID 1 es `fc-agent`** (`fcguest/main.go`). Se encarga del arranque
  temprano (montajes, levantar loopback, y el montaje del workspace en
  `/dev/vdb`, que formatea el dispositivo solo cuando está demostrablemente
  en blanco, según `mountWorkspace`). También recolecta zombis.
- **Turnos.** La operación `msg` ejecuta un turno. El agente escribe los
  archivos y alimenta el FIFO `.cs/in` dentro del invitado, que lee
  `entrypoint.sh` (cabecera de `fc.go`, operación "msg"). Los comentarios lo
  llaman `sidecar/entrypoint.sh`, pero ningún `entrypoint.sh` está
  rastreado en este repositorio (`git ls-files`), así que su procedencia
  queda **sin verificar**.
- **Contenedores.** El rootfs incluye podman rootless, que corre sobre el
  kernel del invitado detrás de KVM (`fcguest/Dockerfile.rootfs`).
- **Archivos del lado del host.** El host mantiene los archivos por grupo
  que él mismo lee (log, `config.json`, `prompt.md`) bajo `groups/<g>/` como
  archivos planos (cabecera de `fc.go`).
