Перевод файла ../architecture.md; английский оригинал является авторитетным.

# Архитектура

Как части koto соединяются друг с другом — от gRPC-клиента оператора до
процесса агента внутри microVM. Каждое утверждение называет файл (и обычно
функцию или константу), из которого оно взято, так что его можно проверить
через `grep`. Высокоуровневая диаграмма находится в `README.md` репозитория
в разделе `## Architecture`. Эта страница — сверенная с кодом версия той
диаграммы.

Смежные страницы: [getting-started.md](getting-started.md) (установка),
[configuration.md](configuration.md) (настройки),
[acl-and-auth.md](acl-and-auth.md) (граница доверия gRPC),
[../firecracker-vsock.md](../firecracker-vsock.md) (исходные проектные
заметки по vsock/jailer).

## 1. Уровни одним взглядом

1. **Уровень 1, пользователь хоста.** gRPC-клиенты (`koto tui`, `koto ctl`)
   и хранилище учётных данных в `creds/` внутри каталога состояния. См.
   [acl-and-auth.md](acl-and-auth.md).
2. **Уровень 2, демон koto.** Один Go-процесс (`koto daemon`,
   `daemonMain` в `daemon/daemon.go`). Он работает rootless от имени
   установившего пользователя под юнитом systemd и содержит gRPC-сервер,
   пер-групповой LLM-прокси, сетевой шлюз gVisor, cron, цели (goals) и
   ctl-плоскость.
3. **Уровень 3, по одному изолированному (jailed) Firecracker VMM на
   группу.** Каждый VMM запускается через `koto fcjail` (`daemon/fcjail.go`)
   и загружает microVM, чей PID 1 — `fc-agent` (`fcguest/main.go`).

Каждый слой исходит из того, что слой внутри него уже скомпрометирован. Это
утверждение «defense in depth» (эшелонированная оборона) из README. Разделы
ниже описывают, что каждый слой фактически делает в коде.

## 2. Уровень 2: демон

### 2.1 Порядок запуска

Порядок задан в `daemonMain` в `daemon/daemon.go`:

1. `usernsEnsure()` (`daemon/userns.go`) перезапускает демон (re-exec) в
   свежем пользовательском пространстве имён. `newuidmap`/`newgidmap`
   устанавливают отображение из `/etc/subuid` и `/etc/subgid`. Это
   безусловно. При сбое демон печатает
   ``koto: run `koto userns-check` to diagnose`` и выходит с кодом 1.
   Причина, из заголовка `userns.go`: fcjail должен записывать `uid_map` из
   двух записей на каждый VMM, а `fcJailFixupPerms` должен делать chown
   файлов на per-VM uid. Ни то, ни другое невозможно для непривилегированного
   процесса вне userns.
2. `initPaths()` выводит пути состояния из `HERE` (`kotoHome()`):
   - `groups/` (`ROOT`)
   - `groups.json`, `schedules.json`, `goals.json`
   - `run/` (`SOCK_DIR`)
   - `metrics.jsonl`

   `PORT_BASE` по умолчанию `8787` и может быть переопределён через
   `PROXY_PORT`.
3. Выполняются `hardenStatePaths()` и `fcClearStalePids()`, затем
   `fcHostMemInit()` и `fcCgroupInit()` (см. §4.3), затем
   `postureStartup()` (`daemon/posture.go`). Шаг posture логирует каждое
   хост-уровневое ограничение, которое *не* действует.
4. `proxyStart(bind)` запускает внутрипроцессный LLM-прокси (§3).
5. SIGTERM/SIGINT регистрируются *до* загрузки первой VM (комментарий
   "audit 2026-09-11 L146" в `daemon/daemon.go`). Это гарантирует, что при
   завершении всё равно выполнится `fcStopAll`.
6. `spawnEnsure("main", true)` поднимает группу `main`. Затем
   `go autostartGroups()` в фоне загружает группы с настройкой
   `autostart=yes`.
7. gRPC-сервер слушает TCP `KOTO_BIND:KOTO_PORT`. Значения по умолчанию —
   `127.0.0.1` и `8443`. Используется mTLS (`serverTLSConfig`) плюс
   перехватчики `authUnary`/`authStream`. Подробности — в
   [acl-and-auth.md](acl-and-auth.md).

Хост-сторонних ctl FIFO не существует. ctl-плоскость группы обслуживается
по vsock, как только её VM поднята. Так говорит комментарий над
gRPC-слушателем в `daemon/daemon.go`.

### 2.2 Песочница systemd

Текст юнита формируется `daemon/install.go`. Он содержит следующие
директивы:

- `Type=exec` и `ExecStart=/usr/local/bin/koto daemon`.
- `ProtectSystem=strict`.
- `ProtectHome=yes`, либо `ProtectHome=tmpfs` плюс read-only bind-монтирования,
  когда бинарник `claude` лежит в вашем домашнем каталоге
  (`installHomeScoping`).
- `DevicePolicy=closed` и `DeviceAllow=/dev/kvm rw`.
- `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`.
- `RestrictNamespaces=user mnt pid net ipc uts cgroup`.
- `LockPersonality=yes` и `RestrictRealtime=yes`.
- `NoNewPrivileges=no`. Должно оставаться `no`: иначе setuid-бинарник
  `newuidmap` игнорировался бы, как объясняет комментарий в
  `installCapBounding`.
- `CapabilityBoundingSet=…`, строится `installCapBounding`.
- `CPUQuota` — только когда этого требует `KOTO_HOST_CPUS`
  (`installCPUQuota`).
- `WantedBy=multi-user.target`.

## 3. LLM-прокси (внедрение учётных данных)

Прокси работает внутри процесса демона. Это не отдельный бинарник
(`proxyStart`, `proxyListen` в `daemon/proxy.go`).

- **Один unix-сокет на группу, не TCP.** Слушатель каждой группы —
  `run/proxy/p<port>.sock` (`proxySockDir`, `proxySockPath`) в каталоге с
  правами только для владельца (`0700`). Комментарий над `proxySockDir`
  объясняет причину (audit M1): loopback TCP-порт позволил бы любому
  локальному uid получить LLM-доступ с учётными данными. Номер порта группы
  (выделяется от `PORT_BASE`, `allocPort` в `daemon/groups.go`, сохраняется
  в `groups.json`) по-прежнему существует. Он идентифицирует группу и даёт
  имя файлу сокета.
- **Инвариант один-к-одному.** Если вызвать `proxyListen` с портом, уже
  привязанным к другой группе, вызов завершится ошибкой
  `port %d already bound for group %q (requested for %q)`.
- **Как гость до него достаёт.** У гостя нет TCP-пути к хосту.
  `ANTHROPIC_BASE_URL` в госте — `http://127.0.0.1:18888`
  (`fcGuestProxyTCP` в `daemon/fc.go`). fc-agent слушает там и пересылает
  каждое соединение на vsock-порт хоста 9000. Демон затем сращивает это
  соединение с прокси-сокетом группы (`fcSpliceToProxy`).
- **Апстримы.** Константы: `upstream = "https://api.anthropic.com"` и
  `veniceUpstream = "https://api.venice.ai"`. Провайдер группы читается из
  `config.json` при каждом запросе (`groupProvider`).
- **Учётные данные.** Читаются на каждый запрос (`currentAPIKey`,
  `readCreds`). Ключ Venice лежит в `venice.key` рядом с файлом учётных
  данных (`veniceKeyPath`). Гость не видит ни того, ни другого.
- **Лимиты.** Буферизованное тело запроса ограничено
  `proxyMaxBody = 64 << 20` (64 MiB). `http.Server` использует
  `ReadHeaderTimeout` 30 с и `IdleTimeout` 90 с. Комментарий у `proxyListen`
  объясняет, почему это важно: они подчищают соединения, чтобы таблица
  vsock-соединений VM не переполнялась.
- **L7-шлюз исходящего трафика.** `egressTargetAllowed` в `daemon/proxy.go`
  применяет сетевую политику группы к целям CONNECT/absolute-form запросов
  прокси. `proxySetBootNetwork` фиксирует политику при запуске VM. Согласно
  заголовку `fcnet.go`, этот шлюз разделяет `fcClassifyDst` с L3-фильтром
  (§5).

## 4. Уровень 3: изолированный VMM

### 4.1 fcjail

Из `daemon/fcjail.go`:

- **Как он стартует.** Демон перезапускает себя как `koto fcjail <spec>` с
  `CLONE_NEWUSER|NEWNS|NEWPID|NEWNET|NEWIPC|NEWUTS`. Дочерний процесс
  (`fcjailMain`) делает следующее, по порядку:
  1. Bind-монтирует в per-VM chroot только то, что нужно Firecracker:
     бинарник, ядро, rootfs, образ workspace этой VM, `/dev/kvm`,
     `/dev/urandom` и каталог vsock-сокетов VM.
  2. Делает chroot внутрь.
  3. Устанавливает `PR_SET_NO_NEW_PRIVS`.
  4. Понижается до per-VM uid.
  5. Выполняет exec Firecracker.

  Встроенный seccomp-фильтр Firecracker остаётся включённым, потому что koto
  никогда не передаёт `--no-seccomp`.
- **Диапазон per-VM uid.** Начинается с `fcJailBaseUID = 30000` и
  заканчивается `fcJailMaxUID = 60000`. Если `/etc/subuid` даёт меньше,
  верхняя граница урезается, и урезанное значение передаётся
  перезапущенным стадиям в `KOTO_JAIL_MAX_UID` (`jailMaxEnv`).
- **Вывод uid.** `fcJailUID` вычисляет
  `uid = fcJailBaseUID + (proxyPort − PORT_BASE)`. Поскольку у каждой группы
  свой прокси-порт, каждая группа получает свой uid. Результат вне диапазона —
  это ошибка, а не запасной вариант ("audit 2026-09-11 L6").
- **Пути.** chroot — это `run/fc/<g>.jail` (`fcJailDir`). Сокеты — в
  `run/fc/<g>.sock/` (`fcSockDir`). Каталог run `run/fc` имеет права `0700`
  (`fcEnsureRunDir`).

### 4.2 VM

Из `daemon/fc.go`:

- **Ассеты.** Бинарник, ядро и rootfs — это `fcassets/firecracker`,
  `fcassets/vmlinux` и `fcassets/rootfs.img` под `HERE` (`fcBinPath`,
  `fcKernelPath`, `fcRootfsPath`).
- **Персистентный диск.** Гостевой `/workspace` — это
  `groups/<g>/workspace.img` (`fcWorkspaceImg`). Это per-групповой
  ext4-образ, подключённый через virtio-blk. Никогда не монтируйте его на
  хосте, пока VM работает.
- **Пресеты размеров** (`fcSizePresets`). Каждая запись — vCPU / RAM / диск /
  IO MiB/s / IO ops/s:
  - `small` (по умолчанию): 2 / 1024 MiB / 8 GiB / 100 / 15000
  - `medium`: 2 / 2048 MiB / 12 GiB / 150 / 22500
  - `large`: 4 / 4096 MiB / 16 GiB / 200 / 30000
  - `xlarge`: 8 / 8192 MiB / 24 GiB / 250 / 37500

  Каждая VM также получает одноразовый IO-burst `fcIOBurstBytes = 256 << 20`.
  VMM работает с nice `fcVMNice = 10`.
- **Гостевой рабочий процесс.** Работает как uid/gid 1000
  (`fcWorkerUID`/`fcWorkerGID`, пользователь `node`).
- **Устройство vsock.** Настраивается с `"guest_cid": 3` (`fcVMConfig`).
  Гость подключается к хосту по CID 2 (`hostCID` в `fcguest/main.go`).

### 4.3 Ограничения ресурсов вокруг VMM

- **Per-VM cgroup** (`daemon/fccgroup.go`). Каждая VM работает в
  `vms/<group>`. `cpu.weight` масштабируется с числом vCPU
  (`fcCgroupWeightPerVCPU = 50`). `memory.high` устанавливается в
  `mem_mib + fcCgroupMemMarginMiB` (512). Это сознательно никогда не
  `memory.max`. Если записываемое дерево cgroup недоступно, демон логирует
  одну строку, запускает VM без cgroups, а в логе запуска значится
  `cgroup=off`.
- **Лимит памяти флота** (`daemon/fchostmem.go`). Для `vms/` устанавливается
  `memory.max`. `fcHostMemAdmit` отказывается загружать VM, которая не
  поместится. Лимит берётся из `KOTO_HOST_MEM_MIB`, где `0` означает «без
  ограничения». Если переменная не задана, лимит по умолчанию — 90% от
  MemTotal (`fcHostMemDefaultPct = 90`).
- **Лимит соединений.** Группа может иметь не более
  `fcMaxConnsPerGroup = 64` открытых vsock-соединений гость→хост по всем
  своим слушателям. Дальнейшие соединения отклоняются с
  `refusing guest vsock connection: %d already open (per-group cap)`
  (`fcAcceptLoop`).

## 5. Сеть

- **`network=none`, значение по умолчанию.** У VM нет NIC. Её единственный
  исходящий канал — L7-прокси через vsock 9000 (заголовок `fc.go`, заголовок
  `fcnet.go`).
- **`network=wan|lan|full`.** `fcSpawn` подключает userspace-шлюз gVisor
  (`containers/gvisor-tap-vsock`, `fcNetGateway` в `daemon/fcnet.go`) и
  открывает слушатель 9003 только когда `groupNetwork(g) != fcNetNone`.
  Политика фиксируется при запуске, поэтому её изменение требует перезапуска
  группы.
- **Топология** (константы в `daemon/fcnet.go`):
  - гость `192.168.127.2` (`fcNetGuestIP`)
  - шлюз `192.168.127.1` (`fcNetGatewayIP`), он же обслуживает DNS
  - подсеть `192.168.127.0/24`, MTU 1500

  Ethernet-кадры идут в Qemu-фрейминге, с 4-байтовым big-endian префиксом
  длины на кадр, по vsock 9003.
- **Фильтр кадров.** `fcEgressConn`, `fcClassifyDst` и `fcDstAllowed`
  выполняются до того, как пакет увидит netstack. Есть четыре класса
  назначений:
  - `ctl`: loopback, link-local и собственные IP интерфейсов хоста. Всегда
    отбрасываются.
  - `gw`: `192.168.127.0/24`. Всегда разрешён.
  - `lan`: RFC1918, IPv6 ULA, multicast, limited broadcast и CGNAT
    `100.64/10`. Разрешён для `lan` и `full`.
  - `wan`: всё остальное. Разрешён для `wan` и `full`.

  netstack также блокирует `169.254.169.254` (`Ec2MetadataAccess=false`).
- **Оговорка** (указана в заголовке `fcnet.go`). DNS резолвится на хосте,
  вне фильтра кадров. Поэтому `lan`-гость всё же может резолвить публичные
  имена. Кадры данных по-прежнему фильтруются по IP назначения.

Устаревший комментарий: комментарий у `fcPortNet` в `daemon/fc.go` говорит,
что 9003 подключается «only for internet=full». Код в `fcSpawn` подключает
его при любом значении network, кроме `none`. Эта страница следует коду.

## 6. Карта портов vsock

Авторитетная карта — комментарий в заголовке `daemon/fc.go` вместе с
константами `fcPortProxy`, `fcPortLog`, `fcPortCtl`, `fcPortNet`,
`fcPortLogSlot` и `fcPortAgent`.

Гибридный vsock Firecracker работает так. Для соединений гость→хост
Firecracker подключается к `<uds>_<port>`, и демон слушает там. Для
соединений хост→гость демон подключается к `<uds>` и отправляет
`CONNECT <port>\n` (`fcHostDial`). `<uds>` — это `run/fc/<g>.sock/v`
(`fcUDS`).

Гость → хост:

- **9000, исходящий API-трафик** (`fcPortProxy`). Сырой TCP сращивается с
  прокси-сокетом группы (`fcSpliceToProxy`).
- **9001, выведен из эксплуатации** (`fcPortLog`). Это был сырой поток
  логов. `fcSpawn` не открывает для него слушатель.
- **9002, ctl-плоскость** (`fcPortCtl`). Фреймированные protobuf
  `CtlRequest` / `CtlResponse` (`fcCtlConn`). В госте shell-интерфейс — это
  файл `/workspace/.cs/ctl`, а ответы дописываются в
  `/workspace/.cs/ctl.out` (`fcguest/ctl.go`).
- **9003, L3 ethernet-кадры** (`fcPortNet`). Только для групп с сетью (§5).
- **9004, потоки ходов** (`fcPortLogSlot`). Одно соединение на ход (turn).
  Сначала идёт `TurnOpen{slot}`, затем события хода, затем `TurnEnd`.
  `fcTurnSink` (`daemon/fcturn.go`) отрисовывает их в хостовый лог
  `.cs/log.<slot>`. Текст из гостя, похожий на маркер, экранируется, так что
  гость не может подделывать маркеры.

Хост → гость:

- **10000, agent RPC** (`fcPortAgent`). Один фреймированный `AgentRequest`
  на соединение. Заголовок `fc.go` перечисляет операции `init`, `msg`,
  `exec`, `exec_stream` и `shutdown`, а `run_script` и `shell_attach`
  называет потоковыми операциями, отвечающими кадрами `AgentFrame`.

Фрейминг (`daemon/fcframe.go`, байт-в-байт зеркалируется в
`fcguest/frame.go`) — это `uint32` big-endian длина, за которой следует одно
protobuf-сообщение. Максимальная длина проверяется до выделения памяти:

- `fcFrameMaxCtl = 1 << 20` (1 MiB) для 9002
- `fcFrameMaxGuest = 16 << 20` (16 MiB) для agent-кадров гость→хост
- `fcFrameMaxHost = 64 << 20` (64 MiB) для запросов хост→гость

Подробнее — в [protocol.md](protocol.md) и
[../firecracker-vsock.md](../firecracker-vsock.md).

Устаревший комментарий: заголовок `fcguest/main.go` всё ещё описывает 9001
как живой лог-FIFO и называет agent RPC «JSON line». Сторона демона
(заголовок `fc.go`, `fcframe.go`) использует protobuf-фрейминг и не имеет
слушателя 9001. Там, где они расходятся, доверяйте `daemon/fc.go`.

## 7. Внутри гостя

- **PID 1 — это `fc-agent`** (`fcguest/main.go`). Он обрабатывает ранний
  этап загрузки (монтирования, поднятие loopback и монтирование workspace
  `/dev/vdb`, который форматируется только когда устройство доказуемо
  пустое, согласно `mountWorkspace`). Он также подчищает зомби-процессы.
- **Ходы.** Операция `msg` выполняет один ход. Агент записывает файлы и
  подаёт данные во внутригостевой FIFO `.cs/in`, который читает
  `entrypoint.sh` (заголовок `fc.go`, операция "msg"). Комментарии называют
  его `sidecar/entrypoint.sh`, но никакой `entrypoint.sh` в этом репозитории
  не отслеживается (`git ls-files`), так что его происхождение
  **не верифицировано**.
- **Контейнеры.** rootfs поставляется с rootless podman, который работает на
  гостевом ядре за KVM (`fcguest/Dockerfile.rootfs`).
- **Файлы на стороне хоста.** Хост хранит per-групповые файлы, которые он
  читает (лог, `config.json`, `prompt.md`), в `groups/<g>/` как обычные
  файлы (заголовок `fc.go`).
