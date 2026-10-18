Перевод файла ../getting-started.md; английский оригинал является авторитетным.

# Начало работы

Эта страница описывает путь koto от нуля до работающего демона: что нужно
хосту, три стадии (получение, интеграция, настройка), платформенные
проверки, блокирующие установку, с их точными сообщениями, и что делает
каждый шаг мастера.

Каждое утверждение снабжено ссылкой на источник (файл плюс функция,
константа или цель Makefile), так что его можно найти grep-ом. Там, где
комментарии в коде или текст usage расходятся с кодом, эта страница
следует коду и говорит об этом.

См. также: [configuration.md](configuration.md) (ключи koto.env, переменные
окружения `KOTO_*`, конфигурация групп), [acl-and-auth.md](acl-and-auth.md)
(PKI и токены, которые выпускает мастер),
[ctl-reference.md](ctl-reference.md) (`koto ctl`).

## 1. Два пути внутрь

### 1.1 Установщик через pipe

```sh
curl -fsSL https://kotovm.com/install.sh | sh
curl -fsSL https://kotovm.com/install.sh | sh -s -- --version 1.0.0
```

`install.sh` выполняет четыре стадии по порядку (комментарий в заголовке
`install.sh`):

1. fetch: клонирует релизный тег во временный каталог, затем скачивает и проверяет подписанные артефакты в него (то же самое, что `make fetch`).
2. install: `koto install` (то же самое, что `make install`).
3. setup: `koto setup` (то же самое, что `make wizard`).
4. tui: `koto tui`.

Под `curl | sh` stdin — это pipe, поэтому стадии со 2-й по 4-ю читают
`/dev/tty`. Когда терминала нет, скрипт останавливается после стадии 2 и
печатает оставшиеся команды (комментарий в заголовке `install.sh`).

Флаги `install.sh` (`install.sh` `usage()`):

- `--version X`: релиз для установки. По умолчанию `latest`, из `VERSION="${KOTO_VERSION:-latest}"`.
- `--no-attach`: остановиться после `koto setup` вместо открытия TUI. Именно это запускает `koto update`.
- `--yes`: принять значения по умолчанию `koto install` и `koto setup`, включая остановку демона при обновлении.
- `--download-only`, `--dest DIR`, `--manifest FILE`: только скачать и проверить. `--dest` по умолчанию — текущий каталог. `--manifest` добавляет проверку по списку sha256sum(1), который пришёл не из релиза.
- `--next TEXT`: команда, которую предложить по завершении запуска с `--download-only`.
- `-h`, `--help`.

От чего `install.sh` отказывается (каждый случай — это `die` в `install.sh`):

- Хост, который не Linux/x86_64: ``koto is Linux/x86_64 only (this is <os>/<arch>)``.
- Отсутствующий инструмент: ``<tool> is required but not installed``. Инструменты — `curl gpg zstd sha256sum`, плюс `git`, если не был передан `--download-only`.
- Пустой отпечаток ключа: ``no release key fingerprint — refusing to accept a signature from whatever key turns up``.

Порядок проверки (комментарий в заголовке `install.sh`, раздел "WHAT THIS VERIFIES"):

1. Получить подписывающий ключ по отпечатку.
2. Проверить отсоединённую подпись на `SHA256SUMS` и потребовать, чтобы строка `VALIDSIG` называла этот отпечаток.
3. Взять имена ассетов из подписанного `SHA256SUMS`.
4. Проверить каждый ассет до распаковки и складировать всё рядом с местом назначения, пока весь набор не пройдёт проверку.

Закреплённый отпечаток — `A6E69ED6BC4779F3281721484CC1AFDE15B64EA3`
(`install.sh` `KEY_FPR`; Makefile `RELEASE_KEY_FPR`). Переменные окружения,
которые читает `install.sh`, перечислены в
[configuration.md](configuration.md) §2.5.

### 1.2 Из клона, в три стадии

Комментарий в начале Makefile ("GETTING koto RUNNING IS THREE STAGES")
определяет стадии:

1. ПОЛУЧИТЬ (ACQUIRE) пять артефактов одним из двух путей: `make fetch` или `make build`.
2. ИНТЕГРИРОВАТЬ (INTEGRATE) их в систему: `make install`.
3. НАСТРОИТЬ (CONFIGURE) установленную систему: `make wizard`.

```sh
make fetch && make install && make wizard   # download — minutes
make build && make install && make wizard   # build it yourself — ~40 min
```

`make setup` — это сокращение для `make build`, `make install` и
`make wizard` в этом порядке (цель Makefile `setup`).

Каждая стадия отказывается делать работу предыдущей и называет команду,
которая её делает. Любую стадию безопасно перезапускать (комментарий в
заголовке Makefile).

## 2. Требования

### 2.1 Платформа

- Только linux/amd64. `checkPlatform` (`daemon/setup_checks.go`) отклоняет любую другую пару `GOOS/GOARCH` с таким способом исправления: ``koto needs linux/amd64: Firecracker microVMs need KVM, and the guest kernel and rootfs are built for x86_64.``
- `/dev/kvm` должен присутствовать и быть доступен на чтение/запись всем (режим `&0o006 == 0o006`). См. `checkKVMAt` и §4.2.
- Непривилегированные пользовательские пространства имён (user namespaces) и диапазон subuid/subgid для вашего пользователя. См. §4.3 и §4.4.

README перечисляет дистрибутивы, где сборка и запуск были проверены:
Fedora 44, Ubuntu 24.04 LTS, Ubuntu 26.04 LTS и Arch (`README.md`, "From
sources"). Эта страница этот список не перепроверяла.

### 2.2 Только для сборки (путь `make build`)

Docker или podman. Когда присутствуют оба, Makefile предпочитает docker
(Makefile `CONTAINER ?= $(shell command -v docker … || command -v podman …)`;
тот же порядок — в `containerEngine` в `daemon/setup_checks.go`). Go-бинарники
собираются внутри образа `GO_IMAGE`, закреплённого по digest, с
`CGO_ENABLED=0`, так что хосту не нужен тулчейн Go (Makefile
`GO_BUILD_RUN`). Если нет ни одного движка, сборка падает до компиляции
(Makefile `need-container`):

```
podman or docker is required to build koto (sudo dnf install podman / sudo apt install podman)
```

Остальные инструменты сборки — `git`, `make` и `curl` (`checkBuildTools`).
Строки установки из README также добавляют `passt` (и `uidmap` на Ubuntu)
для rootless podman:

```sh
sudo dnf install -y git make podman                  # Fedora 44+
sudo apt install -y git make podman passt uidmap     # Ubuntu 24.04+
sudo pacman -S --needed git make podman passt        # Arch
```

### 2.3 Среда выполнения (что вызывает установленный демон)

Эти требования — из `checkRuntimeTools` в `daemon/setup_checks.go`, с
именами пакетов Fedora и Debian:

- `mkfs.ext4`, `e2fsck`, `resize2fs`: `e2fsprogs`. Они создают, проверяют и расширяют образ workspace каждой группы.
- `tar`: `tar`. Он доставляет skills в гостя.
- `newuidmap`, `newgidmap`: `shadow-utils` на Fedora, `uidmap` на Debian/Ubuntu. Они отображают uid/gid каждой VM, под которыми работает изолированный монитор микроВМ.
- `claude`: только предупреждение. Он нужен лишь для OAuth-аутентификации (по подписке), где прокси вызывает его для обновления токена. Подсказка просит `node >= 22` и `npm i -g @anthropic-ai/claude-code`. На хостах семейства Debian она также отмечает, что пакет `nodejs` в Ubuntu — это v18, и предлагает `sudo snap install node --classic --channel=22` или nvm.

Podman не является зависимостью времени выполнения. Ничто из того, что
запускает koto, не является контейнером (комментарий `checkRuntimeTools`;
заголовок Makefile).

## 3. Стадия 1: получение

### 3.1 Пять артефактов

Makefile `ARTIFACTS` и `artifacts` в `daemon/install.go` перечисляют их
одинаково:

- `koto`
- `koto-tui`
- `fcassets/firecracker`
- `fcassets/vmlinux`
- `fcassets/rootfs.img`

Оба пути производят ровно эти файлы.

### 3.2 `make fetch`

`make fetch` запускает `install.sh --version $(DIST_VERSION) --dest . --manifest artifacts.sha256 --download-only --next "make install"` с `KOTO_BASE_URL="$(KOTO_DIST_URL)/$(DIST_VERSION)"` (цель Makefile `fetch`). Значения по умолчанию:

- `KOTO_DIST_URL ?= https://github.com/jpzk/koto/releases/download`
- `DIST_VERSION ?= 1.0.5`
- `DIST_ARCH ?= x86_64`

Релизные ассеты сжаты zstd и называются `<basename>_<version>_<arch>.zst`,
например `rootfs.img_fedora44_x86_64.zst` (комментарий Makefile над
`DIST_ARCH`). Закоммиченный `artifacts.sha256` содержит контрольные суммы
артефактов в том виде, в каком они устанавливаются, после распаковки
(комментарий Makefile над `MANIFEST`).

Если `DIST_VERSION` равен `unreleased`, fetch завершается с кодом 1 и
``no koto release has been published yet, so there is nothing to fetch.``

### 3.3 `make build`

`make build` запускает такие под-цели по порядку: `koto`, `koto-tui`,
`firecracker`, `kernel`, `rootfs` (цель Makefile `build`). Каждая из них —
no-op, как только её результат новее её входов.

- `koto`: `go build -trimpath -ldflags "-s -w -X main.kotoVersion=$(KOTO_VERSION)" -o koto ./daemon`. Сначала он копирует `install.sh` в `daemon/installer/install.sh`, потому что установщик встраивается для `koto update`. `KOTO_VERSION` — это `git describe --tags --always --dirty` с откатом к дате. `koto version` печатает его (`daemon/main.go`, `kotoVersion`, по умолчанию `"dev"`).
- `firecracker`: `fcguest/build-firecracker.sh`.
- `kernel`: `fcguest/build-kernel.sh`.
- `rootfs`: `fcguest/build-rootfs.sh`. Его входы включают `fcguest/*.go`, `sidecar/*` и `protocol/` (Makefile `FCGUEST_SRC`), так что изменение протокола пересобирает гостевой образ.

Сборки не воспроизводимы. Сборке из исходников доверяют, потому что её
собрали вы, и она никогда не сверяется с релизным манифестом (комментарий
Makefile над `dist`; комментарий `requireArtifacts` в `daemon/install.go`).

## 4. Стадия 2: `make install` / `koto install`

`make install` сначала запускает `require-artifacts`. Если какой-то из пяти
файлов отсутствует, он печатает ``missing artifact(s): …`` и подсказку про
fetch/build, затем запускает `./koto install` (цели Makefile `install`,
`require-artifacts`).

### 4.1 Флаги `koto install`

Из `installMain` и `installUsage` в `daemon/install.go`:

- `-state DIR`: каталог состояния. По умолчанию `/var/lib/koto` (`defaultStateDir`). Значение должно пройти `unitSafeValue`, иначе вы получите ``-state: …`` и код выхода 2.
- `-y`: принять значения по умолчанию и никогда не спрашивать.
- `-no-color`: отключить цвет. Этот флаг разбирается, но не указан в тексте usage.

Текст usage говорит, что install «запускает сервис». Код запускает его
только при обновлении. Свежая установка выполняет `systemctl enable koto`
и печатает ``service enabled (not started — `koto setup` starts it)``
(`runInstall`). Демон не сможет подняться, пока мастер не выпустил
`server.crt`.

### 4.2 Что делает install, по порядку

Эти шаги — из `runInstall` в `daemon/install.go`:

1. Он обнаруживает обновление (`installed()`: существуют и `/etc/systemd/system/koto.service`, и `/etc/koto/koto.env`) и печатает ``existing installation detected — upgrading in place``.
2. Он отказывается работать вне клона: ``run this from a koto clone (no Makefile/daemon here)``. Он также отказывается при отсутствующих артефактах (`requireArtifacts`).
3. Он отказывается от root: ``run as your normal user, not root — the daemon runs as you (sudo is used only for the system files)``.
4. Он запускает проверки требований хоста (`preflightGate`, §5).
5. Он останавливает работающий демон (`installStopDaemon`).
6. Он инициализирует каталог состояния (`seedStateDir`). Если каталог отсутствует, он создаёт его через `sudo install -d -o <uid> -g <gid> -m 0750`. Существующий каталог должен пройти `stateDirTrusted`: он не должен быть симлинком, должен быть каталогом, должен принадлежать вам и не иметь бита записи для группы/остальных. Отказ читается как ``<dir>: <reason> — remove it or choose another -state``. Затем install создаёт `groups/`, `creds/`, `fcassets/`, `prompts/` и `run/` (режим 0750), создаёт ссылку `.claude -> creds` и переносит `groups/`, `groups.json`, `schedules.json`, `goals.json` и `metrics.jsonl` из клона. Он никогда не перезаписывает (`migrateState`).
7. Он устанавливает бинарники: `sudo install -m 0755 <self> /usr/local/bin/koto`, а `koto-tui` — в `/usr/local/bin/koto-tui`. Когда `koto-tui` отсутствует, он только предупреждает.
8. Если AppArmor ограничивает userns и оператор принял предложение (или профиль koto уже существует), он устанавливает или обновляет профиль AppArmor (`installAppArmorProfile`).
9. Он находит CLI `claude` (`installClaudeBin`) и записывает `/etc/koto/koto.env` (`writeEnvFile`; ключи — в [configuration.md](configuration.md)).
10. Он рендерит `/etc/systemd/system/koto.service` (`renderUnit`, `ExecStart=/usr/local/bin/koto daemon`). Если юнит изменился, он выполняет `systemctl daemon-reload`.
11. При обновлении он выполняет `systemctl start koto`. При свежей установке — `systemctl enable koto`.
12. Он записывает `<state>/.koto-version` из `git describe` с откатом к `kotoVersion`.

Каждое привилегированное действие — это отдельный exec `sudo`, который
печатается перед запуском (`sudoRun`; комментарий в заголовке
`daemon/install.go`).

## 5. Платформенные проверки (preflight)

`runPreflight` в `daemon/setup_checks.go` запускает проверки в таком
порядке: платформа, `/dev/kvm`, вложенные userns, subuid/subgid,
инструменты сборки, инструменты среды выполнения, место на диске, сеть.
Их запускают и `koto install`, и `koto setup --check`.

Группа проверок времени сборки (контейнерный движок плюс `git`, `make`,
`curl`) проходит через `advisory()`, который превращает её отказы в
предупреждения, потому что путь через fetch в этих инструментах не
нуждается.

### 5.1 Как решает гейт (`preflightGate`)

- Любой жёсткий отказ останавливает установку. При одном или более отказах он сообщает ``<n> unmet requirement(s) — see the remediation above``. Если жёсткий отказ и `/dev/kvm` падают одновременно, он сообщает ``<n> unmet requirement(s), all shown above — fix all of them (<m> blocking, plus /dev/kvm)``.
- Отказ одного только `/dev/kvm` — мягкий. Вопрос ``Continue without KVM?`` по умолчанию — нет. Ответ «да» устанавливает демон, чей API отвечает, но ни одна группа не может загрузиться (`preflightResult.noKVM`). Ответ «нет» прерывает установку (`errSetupAborted`, текст `aborted`).
- Когда userns ``restricted by AppArmor`` и доступен `apparmor_parser`, install предлагает ``Install an AppArmor profile for koto?`` (по умолчанию — да). Отказ завершается ошибкой ``nested user namespaces are unavailable — see the remediation above, or re-run and accept the AppArmor profile``.

### 5.2 `/dev/kvm` (`checkKVMAt`)

- Отсутствует: деталь ``not present``. Способ исправления — включить виртуализацию в BIOS/UEFI или вложенную виртуализацию, когда сам koto работает в VM.
- Режим без rw для всех: деталь ``mode NNNN — not world-accessible``. Членство в группе kvm не помогает. Изолированный VMM Firecracker работает под uid конкретной VM с пустым набором дополнительных групп (`kvmRemediation`; см. `daemon/fcjail.go`). Первый предложенный способ исправления — правило udev:

  ```sh
  echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666"' | sudo tee /etc/udev/rules.d/99-kvm.rules
  sudo udevadm control --reload-rules && sudo udevadm trigger --name-match=kvm
  ```

  Альтернатива — POSIX ACL по uid на диапазон jail-uid, вычисляемый от начала вашего `/etc/subuid` через `fcJailHostUID(start, fcJailBaseUID)` до `fcJailBaseUID+ctlMaxSpawn`. Текст исправления предупреждает о двух проблемах ACL: systemd-logind переписывает его, а эта проверка читает биты режима, поэтому она продолжает падать.
- Устройство открывается, но не работает: деталь ``present but not usable: <err>``. Способ исправления — ``sudo usermod -aG kvm $USER``.
- OK: ``usable (mode NNNN)``.

### 5.3 Вложенные пользовательские пространства имён (`checkUserns`)

- `/proc/sys/user/max_user_namespaces` равен `0`: отказ ``user.max_user_namespaces = 0``. Способ исправления — ``sudo sysctl -w user.max_user_namespaces=15000``, с сохранением в `/etc/sysctl.d/`.
- AppArmor ограничивает непривилегированные userns (`apparmorUsernsRestricted`). Тогда проверка исполняет установленный бинарник (`apparmorProfileWorks(installedKotoBin)`). Если это удаётся, результат — ok, ``granted to koto by AppArmor profile (host-wide restriction still on)``. Если нет — отказ с ``restricted by AppArmor``. Резервный способ исправления (`usernsRemediation`) — общесистемный `kernel.apparmor_restrict_unprivileged_userns=0`, сохраняемый в `/etc/sysctl.d/60-koto.conf`.
- Нечитаемый файл: предупреждение ``unknown``.
- Иначе ok: ``allowed (<n>)``.

### 5.4 subuid/subgid (`checkSubuid`)

И `/etc/subuid`, и `/etc/subgid` должны содержать строку для вашего имени
пользователя или uid. Когда одна из них отсутствует, проверка падает с
``no range for <user>`` и таким способом исправления:

```sh
sudo usermod --add-subuids 100000-165535 --add-subgids 100000-165535 <user>
podman system migrate
```

### 5.5 Контейнерный движок (`checkContainerEngine`, advisory)

- Ни одного движка: ``neither found``.
- `<engine> info --format json` падает: `` `<engine> info` failed: … ``.
- Только podman: он предупреждает, если podman не rootless (``running as root``) и если отсутствует pasta (``not found``, установите `passt`). Docker дальнейших проверок не получает.

### 5.6 Диск и сеть

- `checkDisk` проверяет текущий рабочий каталог и предупреждает при менее чем 20 GiB свободного места.
- `checkNetwork` посылает `HEAD https://github.com` с тайм-аутом 5 с и только предупреждает. Установка работает офлайн, если артефакты уже на месте.

## 6. Стадия 3: `make wizard` / `koto setup`

`make wizard` проверяет, что `./koto` исполняем. Если нет, он печатает
``./koto is missing — acquire the artifacts first:`` и выходит с кодом 1.
Иначе он запускает `./koto setup` (цель Makefile `wizard`).

### 6.1 Флаги

Из `setupMain` и `setupUsage` в `daemon/setup.go`:

- `--check`: режим доктора. Запускает каждую preflight-пробу и `detect` каждого шага, ничего не меняя. Код выхода 0, когда всё готово, иначе 1 (`setupCheck`). Он печатает ``koto is fully set up``, ``<n> host requirement(s) unmet`` или ``<n> step(s) pending — run `koto setup` to finish``.
- `-y`: принять значения по умолчанию и никогда не спрашивать. Шаги, которым нужен реальный ввод, падают.
- `--only ID`: выполнить один шаг. Его `detect` пропускается, так что шаг выполняется всегда. Неизвестный ID даёт ``no such step "<id>" (see `koto setup --list`)``, код выхода 2. Пример `--only rootfs` из текста usage называет шаг, которого больше не существует.
- `--from ID`: начать с этого шага.
- `--no-color`: простой вывод. Оформление также выключено, когда установлен `NO_COLOR` или `TERM` равен `dumb` или пуст (`colorOK`, `daemon/setup_ui.go`).
- `--list`: напечатать идентификаторы шагов по порядку.

Без TTY и без `-y` или `--check` setup выходит с кодом 2 и ``setup needs
a terminal (or pass -y / --check)``.

Каталог состояния — `KOTO_HOME`, либо `/var/lib/koto`, когда он не задан
(`setupCtx.stateDir`). Учётные данные идут в `<state>/creds`.

### 6.2 Шаги

Шаги по порядку (`setupSteps` в `daemon/setup_steps.go`):

1. `installed` (Установленная система). Детектирует `installed()`. Сам он ничего не устанавливает. Когда koto не установлен, он падает с подсказкой «получите артефакты, затем `make install`».
2. `pki` (TLS-идентичности). Детекции нужны все эти файлы в `creds/`: `ca.crt`, `ca.key`, `server.crt`, `client-tui.crt`, `token-tui`, `client-agent.crt`, `token-agent`. Он спрашивает, будете ли вы обращаться к демону с другой машины, и если да — принимает дополнительные SAN, например `IP:192.168.1.20,DNS:koto.lan`. SAN по умолчанию — `DNS:koto-daemon`, `DNS:localhost` и `IP:127.0.0.1` (`defaultServerSANs`, `daemon/pki.go`). Он запускает `pkiInit`, затем выпускает клиентов `tui` и `agent`, оба с ролью `admin`. Существующий CA никогда не перегенерируется. См. [acl-and-auth.md](acl-and-auth.md).
3. `auth` (Учётные данные Anthropic). Шаг выполнен, когда существует `creds/.credentials.json` (OAuth) или `creds/anthropic-api-key`, либо когда установлен `ANTHROPIC_API_KEY`. Он запускает `authConnect`, тот же поток, что и `koto claude-login`. Под `-y` он падает, потому что обоим методам нужен человек. Ошибка указывает на ``koto claude-login --api-key-stdin < keyfile``.
4. `service` (Запуск демона). Выполняет `sudo systemctl restart koto`. Он сообщает «не выполнено» всякий раз, когда этот запуск менял учётные данные (`credsChanged`). Комментарий мастера объясняет это тем, что прокси разрешает учётные данные один раз, при старте. Этот комментарий устарел: прокси теперь читает учётные данные на каждый запрос (`currentAPIKey` и `readCreds` в `daemon/proxy.go`), и `claudeLoginUsage` говорит то же самое. Для файловых учётных данных перезапуск поэтому безвреден, но строго не обязателен. `ANTHROPIC_API_KEY` из окружения по-прежнему фиксирован на всё время жизни процесса (`envAPIKey`).
5. `smoke` (Smoke-тест). Собирает клиента для идентичности `tui` на `127.0.0.1:8443` и вызывает RPC `List`. Он повторяет попытку каждые 2 с до 45 с, затем падает с ``daemon did not answer within 45s: … check `systemctl status koto` and `journalctl -u koto -n 50` ``. Если `/dev/kvm` непригоден, он предупреждает ``no usable /dev/kvm on this host — groups will not boot until that is fixed``.
6. `done` (Готово). Печатает ``koto is installed and running.`` и следующие команды: `koto tui`, `koto ctl list`, `systemctl status koto`, `journalctl -u koto -f`. Он также печатает каталог состояния и `/etc/koto/koto.env`, и как добавить клиента через `koto pki client -creds <state>/creds <name>`.

Возобновление работает через детекцию, а не через файл состояния.
Повторный запуск `koto setup` пропускает каждый шаг, чей `detect`
удовлетворён (заголовок `daemon/setup.go`). Прерванный запуск печатает
``interrupted — re-run `koto setup` to resume from here``. Упавший шаг
печатает ``step "<id>" failed: …``.

## 7. Первое использование

```sh
koto tui            # /new <name> spawns your first agent, /exit detaches (README.md)
koto ctl list       # list groups; see ctl-reference.md
koto setup --check  # health of the install, changes nothing
```

Когда ход группы падает с 401, выполните `koto claude-login`. Его флаги —
`--status`, `--method oauth|api-key`, `--api-key-stdin` и `--no-verify`
(`claudeLoginUsage`, `daemon/claude_login.go`).

Для провайдеров с ключом `koto venice-login` пишет `creds/venice.key`,
а `koto minimax-login` пишет `creds/minimax.key` (оба 0600, в state dir;
ключ никогда не попадает в guest). Ключ берётся из `--key-file F`,
`--api-key-stdin` или скрытого запроса в терминале. До записи он
проверяется одним запросом, требующим auth: Venice
`GET /api/v1/api_keys/rate_limits` (Bearer), MiniMax
`GET /anthropic/v1/models` (`x-api-key`). 2xx — ключ сохраняется. 401/403 —
ключ отклоняется, выход с кодом 1, существующий файл ключа не меняется.
Если провайдер недоступен (сетевая ошибка, таймаут 15s, 5xx), ключ
сохраняется с предупреждением. `--no-verify` пропускает запрос; `--status`
лишь сообщает, сохранён ли ключ. Ошибки содержат код статуса или ошибку
транспорта, но никогда не ключ (`providerKeyCheck`,
`daemon/provider_keycheck.go`). Шаг `auth` мастера использует ту же проверку.

Остальные подкоманды верхнего уровня, диспетчеризуемые в `daemon/main.go`, —
`daemon`, `fcjail`, `ctl`, `setup`, `claude-login`, `openai-login`, `venice-login`, `minimax-login`, `uninstall`, `update`,
`install`, `tui`, `userns-check`, `pki` и `version`. Неизвестная подкоманда
печатает ``unknown subcommand: <name>`` и выходит с кодом 2.
