Traducción de ../acl-and-auth.md; la versión en inglés es la autoritativa.

# Autenticación y autorización (plano de control gRPC)

Esta página cubre quién puede llamar a la API gRPC del daemon (TUI, Android,
`koto ctl`, scripts) y qué puede hacer cada llamante. El plano ctl dentro del
invitado (`/workspace/.cs/ctl`) es un eje de autorización **separado**,
basado en la identidad del grupo y cableado en `daemon/ctl.go`. No lo
gobierna `acl.json` (comentario de cabecera de `daemon/acl.go`). Para sus
reglas, véase [ctl-reference.md, Parte 2](ctl-reference.md).

Todas las rutas de abajo son relativas a la raíz de estado `HERE`:
`KOTO_HOME` cuando está definida (modo instalado, normalmente
`/var/lib/koto`), en otro caso el directorio de trabajo del daemon
(`kotoHome`, `daemon/daemon.go`). Los archivos de credenciales viven en
`<HERE>/creds/` (`credFile`, `daemon/auth.go`).

## 1. Capas

Cada llamada pasa tres comprobaciones en orden (comentario de cabecera de
`daemon/auth.go`, `authFromCtx`, `aclCheck`):

1. **mTLS**: certificado de servidor más un certificado de cliente
   obligatorio.
2. **Token bearer**: autenticación, ligada al certificado del cliente.
3. **ACL**: autorización por rol, verbo y grupo objetivo.

El comentario también menciona un overlay WireGuard por debajo de estas
capas. Corre fuera del proceso de koto.

### 1.1 Listener

- La dirección de escucha por defecto es `127.0.0.1:8443`. `KOTO_BIND`
  sobrescribe la dirección y `KOTO_PORT` el puerto (`daemon/daemon.go`,
  después de `go autostartGroups()`). Véase
  [configuration.md](configuration.md).
- Keepalive de gRPC: servidor `Time: 30s`, `Timeout: 10s`. La política de
  aplicación es `MinTime: 10s` con `PermitWithoutStream: true`
  (`daemon/daemon.go`, `grpc.KeepaliveParams`).
- Al arrancar, el daemon registra `kotod ready grpc=<addr> (mTLS+token)` y
  luego ejecuta `authBindingPreflight`.

### 1.2 mTLS (`serverTLSConfig`, `daemon/auth.go`)

- El servidor presenta `creds/server.crt` y `creds/server.key`.
- Los certificados de cliente son obligatorios y deben encadenar a
  `creds/ca.crt` (`tls.RequireAndVerifyClientCert`). La versión mínima es
  TLS 1.3 (`MinVersion: tls.VersionTLS13`).
- **Lista de permitidos (allowlist).** La huella SHA-256 del certificado hoja
  del cliente (DER, hex en minúsculas) debe aparecer además en
  `creds/clients.allow` (`VerifyPeerCertificate`, `loadClientAllow`).
  - Formato de línea: `<sha256hex> <name>`.
  - Las líneas cuyo primer campo empieza con `#` se ignoran.
- **Releída en cada handshake.** Borrar una línea revoca ese dispositivo sin
  reiniciar el daemon.
- Errores de handshake:
  - `client cert <fp> not in allowlist`
  - `clients.allow: <err>` (por ejemplo, el archivo no existe)
  - `no client certificate`

### 1.3 Token bearer (`authFromCtx`, `tokenIdentity`)

- El token se lee de los metadatos gRPC `authorization`. El prefijo
  `Bearer ` se elimina.
- El token se hashea con SHA-256 y se busca en `creds/tokens.json`, que se
  relee en cada llamada (`loadTokenIdentities`).
- **Certificado y token están ligados (auditoría L6).** El CommonName del
  certificado del cliente debe ser igual a la clave de `tokens.json` que
  coincidió con el token (`certIdentity`). Cualquiera de las dos revocaciones
  basta por sí sola: quitar la línea de `clients.allow`, o quitar la entrada
  de `tokens.json`.
- Los fallos devuelven gRPC `Unauthenticated`:
  - `invalid or missing token` (registrado como `rejected call from <addr> (bad/missing token)`)
  - `no client certificate`
  - `client certificate and token name different identities`
- **Preflight de arranque** (`authBindingPreflight`). Registra una
  advertencia por cada nombre presente en un solo lado:
  - `identity "<n>" has an allowlisted cert but no tokens.json entry — its calls will be refused (mint one: koto pki client <n>)`
  - `identity "<n>" has a token but no cert named "<n>" in clients.allow — its calls will be refused`

### 1.4 Formas de `tokens.json` (`parseTokens`, `daemon/acl.go`)

El archivo mapea `clientid → credencial`. Se aceptan tres formas de valor:

- `"<sha256hex>"`: hash a secas, heredado. El rol es `admin`
  (`defaultRole`).
- `{"hash": "<hex>", "role": "agent"}`: un solo rol.
- `{"hash": "<hex>", "roles": ["reader", "ops"]}`: varios roles. `roles`
  gana sobre `role` si ambos están presentes.

Una entrada se omite (no autentica a nadie) en dos casos:

- no coincide con ninguna de estas formas, o tiene un `hash` vacío;
- acaba sin roles tras el recorte. Por ejemplo,
  `koto pki client -role "" x` escribe `"roles": null`, que se omite.

El hash se pasa a minúsculas antes de la búsqueda.

## 2. PKI: emitir credenciales

Hay dos rutas equivalentes. Ambas producen los mismos artefactos (comentario
de cabecera de `daemon/pki.go`):

- **Go:** `koto pki`, usada por `koto setup` y `koto install`. No necesita
  openssl ni jq.
- **Makefile:** `make pki-init` y `make pki-client`, usando openssl y jq.

### 2.1 `koto pki` (`pkiCliMain`, `daemon/pki.go`)

```
usage: koto pki init   [-creds DIR] [-san DNS:a,IP:1.2.3.4]
       koto pki client [-creds DIR] [-role r1,r2] <name>
       koto pki server [-creds DIR] -san DNS:a,IP:1.2.3.4
```

Valores por defecto: `-creds creds`. `init -san` usa por defecto
`defaultServerSANs` = `DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`.
`client -role` usa por defecto `admin`.

#### `init` (`pkiInit`)

- Crea la CA solo si está ausente (`pkiEnsureCA`): EC P-256, `CN=koto-ca`,
  válida 3650 días.
- Si `ca.crt` existe sin `ca.key`, se niega con
  `<crt> exists but <key> does not — refusing to mint a new CA over it.`,
  seguido de un consejo de restaurar o apartar el archivo.
- Emite el certificado de servidor (`pkiServerCert`): `CN=koto-daemon`,
  válido 825 días, ExtKeyUsage ServerAuth. Los SANs deben ser `DNS:…` o
  `IP:…`. Cualquier otra cosa falla con `bad SAN "<s>" (want DNS:… or IP:…)`.
- El certificado de servidor se reemite cuando está ausente, falta
  `server.key`, es ilegible o está expirado, está a menos de
  `pkiRenewWindow` (30 días) de expirar, aún no es válido, o no encadena a
  la CA (`pkiServerCertNeedsReissue`). Los SANs no se comparan.
- Siembra `acl.json` solo si el archivo está ausente (`pkiCreateNew`, modo
  0600) con:

  ```json
  {
    "agent": {
      "list": "*", "send": "*", "history": "*", "metrics": "*",
      "sched_list": "*",
      "subscribe_group": "*", "watch_state": "*"
    }
  }
  ```

- El directorio de credenciales se crea con modo 0750.

#### `client <name>` (`pkiClient`)

- **Regla de nombre:** `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` y sin `..`
  (`pkiClientNameRE`, `pkiClientNameOK`). Si no, falla con
  `invalid client name "<name>": letters, digits and ._- only, no path separators …`.
- **Escribe:**
  - `client-<name>.key` y `client-<name>.crt`: EC P-256, `CN=<name>`,
    825 días, ClientAuth.
  - Una línea `<fp> <name>` anexada a `clients.allow`, salvo que esa huella
    ya esté presente.
  - Un token hex aleatorio de 32 bytes en `token-<name>` (0600).
  - `{"hash", "roles"}` bajo `<name>` en `tokens.json`.
- La actualización de `tokens.json` mantiene un flock sobre el directorio de
  credenciales (`pkiLockCreds`) y escribe atómicamente mediante un archivo
  temporal y un rename.
- Un `tokens.json` corrupto falla con `tokens.json is corrupt — fix it on disk first: <err>`.
- Una clave de CA ausente falla con ``ca.key (run `koto pki init` first): <err>``.

#### `server -san …`

- Reemite solo el certificado de servidor. La CA no se toca, así que los
  clientes existentes siguen funcionando.
- Imprime `server cert reissued for <san> — restart the daemon to apply`.
- Sin `-san` falla con `pki server: -san is required (e.g. -san …)`
  (salida 2).

### 2.2 Ruta Makefile (`Makefile`, targets `pki-init` / `pki-client`)

- `make pki-init [SERVER_SAN=…]`:
  - `SERVER_SAN` usa por defecto `DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`.
  - Se niega si `creds/ca.key` existe:
    `creds/ca.key exists — refusing to regenerate the CA …`.
  - Siembra el mismo rol `agent` en `creds/acl.json` si ese archivo está
    ausente.
- `make pki-client NAME=<client> [ROLE=r1,r2]`:
  - `ROLE` usa por defecto `admin`.
  - Requiere `creds/ca.key` y usa `jq` para editar `tokens.json`.
  - Escribe los mismos archivos que `koto pki client`.
- Las claves se generan bajo `umask 077` y luego `chmod 600`.

### 2.3 Identidades creadas por setup (`daemon/setup_steps.go`)

- El paso PKI del setup ejecuta `pkiInit` con `defaultServerSANs` más los
  SANs extra que el operador teclee.
- Luego emite dos clientes, **ambos con rol `admin`**, cada uno solo si su
  certificado está ausente:
  - `tui`
  - `agent`, el `KOTO_CLIENT` por defecto de `koto ctl`.
- El *cliente* `agent` es por tanto admin por diseño. El comentario cita la
  auditoría M11, decidida el 2026-09-05.
- El *rol* llamado `agent` en el `acl.json` sembrado es otra cosa: existe
  para identidades acotadas emitidas con `koto pki client -role agent <name>`.

## 3. Autorización: roles, verbos, objetivos

### 3.1 Modelo (`daemon/acl.go`)

- Un usuario (clientid) tiene uno o más roles, y cada rol concede verbos
  sobre objetivos. El permiso efectivo es la **unión** sobre los roles del
  usuario (`rolesAllowed`).
- `creds/acl.json` mapea `role → {verb → targets}`. `daemon/acl.go` es la
  fuente; el comentario del Makefile sobre `pki-init` dice lo mismo.
  - Los objetivos son nombres de grupo, `"*"` o `["*"]` (cualquier grupo), o
    una lista.
  - Una clave de verbo `"*"` concede todos los verbos sobre sus objetivos,
    **excepto** los verbos solo-admin (`grantFor`).
  - La forma plana heredada `{"agent": ["list", "send"]}` significa esos
    verbos sobre cualquier objetivo (`parseACL`).
- `acl.json` se relee en cada llamada (`loadACL`). Editar roles o grants no
  necesita reinicio.

### 3.2 El rol `admin`

`admin` está cableado en el código (`grantFor`, `defaultRole = "admin"`):

- Puede llamar a cualquier verbo sobre cualquier objetivo.
- Cualquier clave `admin` en `acl.json` se ignora.
- Un `acl.json` ausente o corrupto no puede dejar fuera a admin.
- `acl set admin …` falla con
  `the admin role is hardcoded and cannot be defined in the ACL`.
- `acl del admin` falla con `the admin role is hardcoded and cannot be deleted`
  (`aclSetRoleCmd`, `aclDelRoleCmd`).

### 3.3 Fallar cerrado

Las peticiones se deniegan en cada uno de estos casos (`grantFor`,
`roleAllowed`, `parseACL`, `parseTargets`):

- **Rol desconocido:** el rol está ausente de `acl.json`.
- **Archivo ausente:** sin `acl.json`, no existe ningún rol no-admin.
- **Archivo corrupto:** un `acl.json` malformado (`parseACL` devuelve nil)
  deniega todos los roles no-admin.
- **Rol malformado:** un rol cuyo valor está malformado no concede nada.
- **Valor de grant malo:** un único valor de grant malo, o uno que sea
  `null` o `""`, produce un conjunto de objetivos vacío (auditoría M35).
- **Verbo no listado:** un verbo que ni está listado ni lo cubre `"*"`. Las
  RPCs nuevas quedan por tanto denegadas por defecto.

### 3.4 Verbos

- Un verbo es la forma snake_case del nombre del método gRPC, por ejemplo
  `/koto.Koto/SchedAdd` → `sched_add` (`verbFromMethod`).
- Para la lista de RPCs, véase `service Koto` en `protocol/koto.proto`.

El conjunto completo de verbos, derivado mecánicamente de las RPCs (así que
nombres como `goal_list` o `job_logs` no tienen por qué aparecer literalmente
en el código Go):

- **Ciclo de vida:** `spawn`, `send`, `list`, `stop`, `interrupt`,
  `destroy`, `restart`, `history`, `config`, `metrics`, `clear`, `drain`
- **Jobs:** `jobs`, `job_logs`, `job_tail`
- **Schedules:** `sched_add`, `sched_list`, `sched_del`, `sched_toggle`,
  `sched_run`
- **Goals:** `goal_set`, `goal_list`, `goal_approve`, `goal_pause`,
  `goal_interrupt`, `goal_resume`, `goal_cancel`
- **Inspección:** `resources`
- **Gestión de la ACL:** `acl_get`, `acl_set_role`, `acl_del_role`
- **Ejecución y streams:** `run_script`, `attach_shell`, `subscribe_group`,
  `subscribe_logs`, `watch_state`

### 3.5 Verbos solo-admin (`adminOnlyVerbs`)

Estos verbos no pueden concederse mediante `acl.json`, ni siquiera con el
verbo `"*"`. Solo `admin` pasa:

- `acl_get`, `acl_set_role`, `acl_del_role`. La gestión de la ACL no debe
  ser delegable a través de la propia ACL.
- `run_script`: ejecución directa de código en una VM invitada.
- `config_posture`: un verbo sintético, cubierto en la sección siguiente.

### 3.6 `config` vs `config_posture` (`postureVerb`, `daemon/auth.go`)

- Una petición `Config` que fija cualquier clave de **postura** se
  re-etiqueta como `config_posture`, que es solo-admin. Las claves de
  postura son `network`, `internet`, `root`, `ports`, `size`, `autostart` y
  `kvm`.
- Un grant de `config` cubre por tanto solo los ajustes restantes: `model`,
  `effort` y `provider`.
- La comprobación de objetivo sigue aplicándose. El plano ctl dentro del
  invitado rechaza las claves de postura por su cuenta (`configPostureKey`
  en `daemon/ctl.go`; véase [configuration.md](configuration.md)).

### 3.7 Con objetivo vs solo-verbo (`targetOf`)

Un verbo tiene ámbito de grupo ("targeted") cuando su mensaje de petición
lleva un grupo.

Tipos de petición con objetivo, con las RPCs que los usan:

- `SpawnReq`: Spawn
- `SendReq`: Send
- `GroupReq`: Stop, Interrupt, Destroy, Restart, Clear, Drain
- `HistoryReq`: History
- `ConfigReq`: Config
- `MetricsReq`: Metrics
- `JobsReq`: Jobs
- `JobLogsReq`: JobLogs
- `JobTailReq`: JobTail
- `SchedAddReq`: SchedAdd
- `SchedListReq`: SchedList
- `GoalSetReq`: GoalSet
- `GoalListReq`: GoalList
- `GoalGroupReq`: GoalApprove, GoalPause, GoalInterrupt, GoalResume,
  GoalCancel
- `SubscribeReq`: SubscribeGroup
- `RunScriptReq`: RunScript
- `ShellInput`: AttachShell. Cada mensaje del stream lleva `group` y se
  comprueba.

Reglas:

- Un verbo con objetivo pasa solo si el grant tiene `"*"` o nombra el grupo
  (`roleAllowed`).
- Una petición con objetivo cuyo grupo está **vacío** necesita el grant
  `"*"`, porque lee a través de todos los grupos. Ejemplos: `metrics`
  global, `sched_list` sin filtrar, y `jobs` / `goal_list` sin grupo. Un
  nombre vacío nunca se guarda como objetivo (`parseTargets`).
- **Solo-verbo** (sin dimensión de objetivo): `list`, `watch_state`,
  `resources`, `subscribe_logs`, `sched_del`, `sched_toggle`, `sched_run` y
  los verbos `acl_*`. Cualquier grant del verbo, con cualquier conjunto de
  objetivos, deja pasar la llamada.

### 3.8 Proyección de lecturas solo-verbo (`visibleTargets`)

Para estos verbos el conjunto de objetivos del grant estrecha la
**respuesta** en lugar de la llamada (`daemon/grpc_server.go`):

- **`list` y `watch_state`.** Solo se devuelven los grupos cubiertos por el
  grant (`projectGroups`). Los registros de jobs de cada grupo también se
  omiten salvo que el grant de `jobs` cubra ese grupo.
- **`resources`.** Los otros grupos se eliminan, y los totales de flota del
  host (número de grupos, grupos en ejecución, bytes asignados y
  aprovisionados, memoria comprometida) se recalculan solo sobre los grupos
  visibles, salvo que el grant sea `"*"` (`projectResources`). Los campos a
  nivel de máquina, como el tamaño y el espacio libre del sistema de
  archivos, el tope de memoria y MemTotal, se conservan.
- **`subscribe_logs`.** Solo se entregan los eventos atribuidos a un grupo
  cubierto. Los eventos de ámbito daemon (grupo vacío) se descartan salvo
  que el grant sea `"*"` (`logVisible`).
- **`metrics`.** El campo `GlobalMetric` (el registro más reciente de
  cualquier grupo) solo se rellena para llamantes cuyo grant de `metrics`
  es `"*"` (handler `Metrics`, auditoría M30).

### 3.9 Streams

- **Puerta de verbo.** Al abrir el stream, el llamante debe tener el verbo
  en algún rol (`anyGrant`, en `authStream`). La comprobación de objetivo
  corre al recibirse cada mensaje de petición (`aclStream.RecvMsg`).
- **Re-comprobación.** Los streams de larga vida se re-autorizan al enviar,
  como máximo cada `aclStreamRecheck` = 5s (método `revalidate` de
  `aclStream`, auditoría M25).
  - La identidad se re-resuelve por nombre en `tokens.json`. Si ya no está,
    el stream se cierra con `Unauthenticated`
    `identity <name> has been revoked`.
  - Si los roles ya no permiten el verbo, se cierra con `PermissionDenied`
    `roles <r> may no longer call <verb> on "<group>"`.
- **Topes de admisión.** `streamMaxPerIdentity = 512` y
  `streamMaxGlobal = 2048` (`streamAdmit`). Por encima de cualquiera de los
  dos topes la llamada devuelve `ResourceExhausted`
  `too many live streams (per-identity 512, global 2048)`.

### 3.10 Errores de denegación y logs (`aclCheck`)

- Las denegaciones devuelven gRPC `PermissionDenied`:
  - `roles <r1,r2> may not call <verb> on "<group>"` (con objetivo)
  - `roles <r1,r2> may not call <verb>` (solo-verbo)
- Cada decisión se registra bajo la fuente `acl`:
  `<name> (roles …) allowed|denied <verb> [on "<group>"] from <addr>`.
- Las decisiones con objetivo se atribuyen al log del grupo cuando el nombre
  del grupo es válido (`aclLogGroup`).

## 4. Gestionar la ACL

Hay tres RPCs solo-admin: `AclGet`, `AclSetRole` y `AclDelRole`
(`daemon/grpc_server.go`). Desde la CLI son `koto ctl acl get|set|del`
(`ctlAcl`, `daemon/ctl_cli.go`; véase [ctl-reference.md](ctl-reference.md)).

```sh
koto ctl acl get
koto ctl acl set ops stop:ghost restart:ghost list 'metrics:*'
koto ctl acl del ops
```

- **`acl set` reemplaza el objeto de grants completo del rol**
  (`aclSetRoleCmd`).
- **Los nombres de rol** siguen `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`
  (`roleNameRE`). Si no: `invalid role name "<r>"`.
- **La validación de grants** (`validateGrants`) rechaza la petición entera
  en la primera entrada mala:
  - Una clave de verbo debe ser `"*"` o cumplir `roleNameRE`, si no
    `invalid verb "<v>"`.
  - Un objetivo debe ser `"*"` o una lista de nombres que cumplan
    `roleNameRE` (o `"*"`). Errores: `verb "<v>": target must be "*" or a
    list of groups` y `verb "<v>": invalid target <t>`.
- **Las escrituras** mantienen `aclFileLock` y pasan atómicamente por
  `creds/.acl.json.tmp` y un rename, con modo 0600 (`writeACLDoc`).
- **Un archivo corrupto bloquea la mutación:**
  `acl.json is corrupt — fix or remove it on disk first` (`readACLDoc`).
- **`acl del` sobre un rol inexistente** falla con `no such role: <role>`.

## 5. Recetas

**Identidad acotada para un script.** Emite un cliente y apunta `koto ctl` a
él:

```sh
koto pki client -creds /var/lib/koto/creds -role agent ci
KOTO_CLIENT=ci koto ctl list
```

`KOTO_CLIENT` selecciona `client-<name>.{crt,key}` y `token-<name>`
(`newKotoClient`, `daemon/ctl_cli.go`).

**Confinar un rol a un grupo** reemplazando los valores `"*"` por listas:

```sh
koto ctl acl set agent list:main send:main history:main subscribe_group:main watch_state:main
```

**Revocar un dispositivo.** Borra su línea en `creds/clients.allow`, **o** su
entrada en `creds/tokens.json`. Cualquiera de las dos basta por la ligadura
certificado/token. Las llamadas nuevas fallan de inmediato, y los streams
abiertos se cierran en menos de 5s (`aclStreamRecheck`).

**Alcanzar el daemon desde otra máquina.** Añade un SAN sin tocar la CA, y
luego reinicia el daemon:

```sh
koto pki server -creds /var/lib/koto/creds -san DNS:koto-daemon,DNS:localhost,IP:127.0.0.1,IP:192.168.1.20
```

El daemon solo escucha en `127.0.0.1` salvo que `KOTO_BIND` esté definida.
Véase [configuration.md](configuration.md).
