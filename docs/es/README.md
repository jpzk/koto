Traducción de ../README.md; la versión en inglés es la autoritativa.

# Documentación de koto

Documentación de referencia para usuarios de koto (personas que hablan con
los grupos) y operadores (personas que instalan y ejecutan el daemon). Cada
afirmación factual en estos documentos lleva una referencia a la fuente
(archivo más nombre de función/constante) para que pueda buscarse con grep
en este repositorio; donde el código era ambiguo, los documentos dicen
"unverified" (no verificado) en lugar de adivinar. Si un documento y el
código discrepan, gana el código — por favor corrige el documento.

## Empieza aquí

- [getting-started.md](getting-started.md) — requisitos de compilación y de
  ejecución, las etapas de `make` (fetch / build / install / wizard), la ruta
  de `install.sh`, las comprobaciones previas de `koto install` y los pasos
  del asistente `koto setup`.
- [architecture.md](architecture.md) — los niveles (CLI del host, daemon,
  proxy, pasarela de red gVisor, fcjail, invitado Firecracker), el orden de
  arranque del daemon, los presets de tamaño de VM y el mapa de puertos
  vsock.

## Referencia

- [configuration.md](configuration.md) — cada clave de `/etc/koto/koto.env`
  y cada variable de entorno `KOTO_*` leída por el código, con sus valores
  por defecto, además de las claves de configuración por grupo (model,
  provider, network, size, root, effort, ports, autostart, …) y sus valores
  aceptados.
- [ctl-reference.md](ctl-reference.md) — cada verbo y flag de `koto ctl` en
  el host, y los verbos del plano ctl dentro del invitado
  (`/workspace/.cs/ctl`).
- [acl-and-auth.md](acl-and-auth.md) — PKI, tokens de cliente, roles, la ACL
  de verbo × destino y los verbos exclusivos de administrador.
- [protocol.md](protocol.md) — el servicio gRPC en `koto.proto` (todos los
  RPC y campos de petición) y el framing vsock del invitado por canal.

## Uso de grupos

- [groups-goals-schedules.md](groups-goals-schedules.md) — sesiones de chat
  y límites de cola, trabajos en segundo plano con `cs-job`, el piloto
  automático de goals, programaciones cron, y la ventana de delegación
  report/reply.

## Operación

- [operations.md](operations.md) — logs y notificaciones del daemon, el
  muestreador de recursos y las alertas, actualización con `koto update`,
  eliminación con `koto uninstall`, y resolución de problemas por mensaje de
  error exacto.

## Notas de diseño (preexistentes)

- [../firecracker-vsock.md](../firecracker-vsock.md) — notas de diseño de
  los grupos en microVM Firecracker y el IPC por vsock.
- [../kernel-amzn-vs-vanilla.md](../kernel-amzn-vs-vanilla.md) — por qué el
  kernel del invitado se compila desde el código fuente de Amazon Linux en
  lugar del vanilla de kernel.org.

## Historial (informes archivados, sin mantenimiento)

Análisis puntuales en el tiempo; describen el código tal como estaba en su
fecha y pueden estar desactualizados.

- [../history/SECURITY_19_MAY.md](../history/SECURITY_19_MAY.md) — análisis
  de seguridad del backend, 2026-05-19.
- [../history/ANALYSIS_31_MAY.md](../history/ANALYSIS_31_MAY.md) — análisis
  de problemas, 2026-05-31.
- [../history/SECURITY_11_SEP.md](../history/SECURITY_11_SEP.md) — triaje de
  evaluación de seguridad, 2026-09-11.
- [../history/FUZZ_09_OCT.md](../history/FUZZ_09_OCT.md) — campaña de
  fuzzing, 2026-10-09.
