Перевод файла ../configuration.md; английский оригинал является авторитетным.

# Конфигурация

Каждая настройка ниже сверена с исходным кодом в этом дереве. Каждая строка
снабжена ссылкой (`file` + функция/константа), так что её можно найти
grep-ом. Там, где код не делает поведение однозначным, в тексте написано
**unverified**.

Есть три поверхности конфигурации:

1. `/etc/koto/koto.env` — окружение сервиса для установленного демона.
2. Переменные окружения процесса (`KOTO_*` и несколько legacy-имён),
   читаемые демоном, `koto ctl`, `koto tui`, `koto update` и установщиком.
3. Ключи `config.json` каждой группы (`model`, `provider`, `network`, …),
   задаваемые через `koto ctl config`, `/config` в TUI или глагол
   `config_set` ctl-плоскости.

---

## 1. `/etc/koto/koto.env`

- Путь: `/etc/koto/koto.env` — `daemon/install.go`, константа `envFilePath`.
- Загружается systemd через `EnvironmentFile=` в отрендеренном юните
  `/etc/systemd/system/koto.service` — `daemon/install.go` `renderUnit`,
  константа `unitPath`.
- Записывается `koto install` (`daemon/install.go` `writeEnvFile`).
  Повторный запуск install **сохраняет** значения, уже присутствующие в
  файле (цикл слияния по `existing` в `writeEnvFile`). Если файл существует,
  но не может быть прочитан, install отказывается с `refusing to rewrite
  it: re-running install must preserve the values set there`.
- Заголовок, который пишет установщик: ``Edit and `sudo systemctl restart koto` to apply.`` (`writeEnvFile`).
- Разбор: `KEY=VALUE` на строку, пустые строки и комментарии `#`
  игнорируются, пробелы вокруг ключа и значения обрезаются —
  `daemon/install.go` `parseEnvFile`.

Ключи, записываемые `writeEnvFile` (срезы `defaults` и `optional` в `envFileKeys`):

Записываются всегда (значение сохраняется, если уже задано):

- `KOTO_HOME` — по умолчанию = каталог `-state` установки, который по
  умолчанию `/var/lib/koto` (`daemon/install.go`, константа
  `defaultStateDir`, флаг `state`).
- `KOTO_BIND=127.0.0.1`
- `KOTO_PORT=8443`

Опциональные (записываются закомментированными как `#KEY=`, когда пусты):

- `KOTO_HOST_CPUS`
- `KOTO_HOST_MEM_MIB`
- `ANTHROPIC_API_KEY` — намеренно **не** заполняется из `<state>/creds`
  (комментарий в `writeEnvFile`).
- `KOTO_CLAUDE_BIN` — устанавливается в путь CLI claude, определённый во
  время установки (`installClaudeBin`); в отличие от остальных ключей,
  записывается свежеопределённое значение, а не старое значение из файла
  (ветка `kv[0] != claudeBinEnv`).
- `KOTO_DEFAULT_PROVIDER` — провайдер по умолчанию для хоста (см. 2.1).
  Записывается шагом `provider` в `koto setup`; входит в `envFileKeys`,
  поэтому сохраняется при переустановке.

Юнит также задаёт `Environment=HOME=<state dir>`, `WorkingDirectory=<state
dir>` и `ExecStart=/usr/local/bin/koto daemon` (`renderUnit`).

---

## 2. Переменные окружения

### 2.1 Демон (`koto daemon`)

- `KOTO_HOME` — корень состояния. Когда задан, приводится к абсолютному
  пути; когда не задан, демон использует свой **текущий рабочий каталог** —
  `daemon/daemon.go` `kotoHome`, `here`. Производные пути (`initPaths`):
  `groups/`, `groups.json`, `schedules.json`, `goals.json`, `run/`,
  `metrics.jsonl`.
- `KOTO_BIND` — адрес прослушивания gRPC. По умолчанию `127.0.0.1` —
  `daemon/daemon.go` `daemonMain`. Не-loopback значение порождает
  предупреждение о posture `KOTO_BIND=<v> exposes the control plane beyond
  this host…` — `daemon/posture.go`.
- `KOTO_PORT` — порт прослушивания gRPC. По умолчанию `8443` —
  `daemon/daemon.go` `daemonMain`. Гостевой egress к этому порту (и к
  `8443` безусловно) отклоняется — `daemon/proxy.go` `egressTargetAllowed`.
- `KOTO_PORTS_BIND` — адрес хоста, на котором привязываются
  опубликованные `ports` группы. По умолчанию `127.0.0.1` —
  `daemon/fc.go` (`portBind := envOr(...)`).
- `KOTO_HOST_MEM_MIB` — потолок памяти флота в MiB. `0` = без ограничений;
  не задан = 90% MemTotal из `/proc/meminfo` (`fcHostMemDefaultPct = 90`);
  некорректные значения логируют `KOTO_HOST_MEM_MIB=%q invalid; using
  derived default` — `daemon/fchostmem.go` `fcHostMemInit`. Если ёмкость
  вообще не удалось определить и переменная не задана, демон отказывает в
  запуске VM (`REFUSING VM spawns until KOTO_HOST_MEM_MIB is set (use 0
  for explicitly unlimited)`).
- `KOTO_CLAUDE_BIN` — CLI claude, исполняемый для обновления OAuth-токена.
  По умолчанию голое `claude` (поиск по PATH) — `daemon/claudebin.go`
  `claudeBin`, константа `claudeBinEnv`.
- `KOTO_DEFAULT_PROVIDER` — провайдер, записываемый в `config.json` группы,
  когда провайдер там отсутствует или недействителен: каждая новая группа
  (включая main), а также группы, созданные до появления провайдеров, при
  следующем ensure(). `claudesdk` | `venice` | `openai` (без учёта
  регистра). Не задан или неизвестное значение = `claudesdk` —
  `daemon/providers.go` `defaultProvider()`. Группа с уже действительным
  провайдером никогда не перезаписывается. Только для хоста: это не ключ
  `config.json` и не поле ctl, гость не может его задать. Задаётся через
  `koto setup --only provider` (или правкой koto.env) и перезапуском демона.
- `ANTHROPIC_API_KEY` — если задан, имеет приоритет над файлом ключа
  `<state>/creds/anthropic-api-key` — `daemon/proxy.go` `currentAPIKey`,
  `proxyInitPaths`. Файл перечитывается на каждый запрос.
- `CRED_PATH` (legacy-имя, без префикса `KOTO_`) — файл OAuth-учётных
  данных. По умолчанию `<state>/.claude/.credentials.json`, если он
  существует, иначе `$HOME/.claude/.credentials.json` — `daemon/proxy.go`
  `proxyInitPaths`.
- `PROXY_PORT` (legacy) — база для выделения порта прокси каждой группе.
  По умолчанию `8787` — `daemon/daemon.go`, переменная `PORT_BASE`,
  `initPaths`.
- `BIND` (legacy) — адрес привязки для внутрипроцессных прослушивателей
  прокси. По умолчанию `127.0.0.1` — `daemon/daemon.go` `daemonMain` →
  `proxyStart`.
- `INVOCATION_ID` (задаётся systemd) — проверяется только для
  формулировки posture-наблюдения (`the daemon is not running under
  koto.service`) — `daemon/posture.go`.

### 2.2 Установщик (`koto install`)

- `KOTO_HOST_CPUS` — потолок CPU флота в целых ядрах, рендерится в юнит
  как `CPUQuota=<n×100>%`. По умолчанию `nproc-1` (минимум 1); `0` = без
  ограничений (директива не пишется) — `daemon/install.go` `hostCPUs`,
  `installCPUQuota`. Он читается из окружения процесса **установщика**
  (`os.Getenv` в `hostCPUs`); работающий демон его не читает. Достигает ли
  отрендеренного юнита значение, присутствующее только в `koto.env`, —
  **unverified** (не найден путь кода, загружающий koto.env в окружение
  установщика).
- `KOTO_HOME` — каталог состояния по умолчанию для `stateDirOf`
  (`daemon/install.go`).

### 2.3 Клиент (`koto ctl` и smoke-тест мастера)

Из `daemon/ctl_cli.go` (строка env в `ctlUsage`, `ctlClient`,
`ctlCredsDir`, `newKotoClient`):

- `KOTO_ADDR` — цель подключения. По умолчанию `127.0.0.1:8443`.
- `KOTO_CREDS_DIR` — каталог PKI. Порядок разрешения (`ctlCredsDir`):
  `KOTO_CREDS_DIR`, иначе `./creds`, если существует, иначе
  `$KOTO_HOME/creds`, иначе `/var/lib/koto/creds`, если существует, иначе
  `creds`.
- `KOTO_CLIENT` — имя идентичности. По умолчанию `agent`. Выбирает
  `client-<name>.crt`, `client-<name>.key`, `token-<name>` в каталоге creds.
- `KOTO_CERT`, `KOTO_KEY`, `KOTO_CA` — переопределяют пути отдельных
  файлов (по умолчанию `client-<name>.crt`, `client-<name>.key`, `ca.crt`).
- `KOTO_TOKEN` — **значение** bearer-токена (не путь). Если не задан,
  читается `token-<name>`; если этот файл отсутствует: ``no token: set KOTO_TOKEN or provide <path> (mint with `make pki-client NAME=<name> ROLE=agent`)``.
- `KOTO_SERVER_NAME` — переопределение TLS server-name для целей
  подключения, отсутствующих в SAN серверного сертификата.
- Минимальная версия TLS — 1.3 (`tls.VersionTLS13` в `newKotoClient`).

### 2.4 `koto tui`

`daemon/tui_cmd.go`: флаг `-state` по умолчанию `$KOTO_HOME`, иначе
`/var/lib/koto`. Он читает `<state>/creds/token-tui` и исполняет `koto-tui`
с таким окружением:

- `KOTO_CA=<state>/creds/ca.crt`
- `KOTO_CERT=<state>/creds/client-tui.crt`
- `KOTO_KEY=<state>/creds/client-tui.key`
- `KOTO_TOKEN=<contents of token-tui>`
- `KOTO_ENDPOINT=$KOTO_ADDR` (по умолчанию `127.0.0.1:8443`)
- `KOTO_SERVER_NAME=koto-daemon`
- `KOTO_PROMPTS_DIR=<state>/prompts`
- `SOCK_PATH=<state>/run/tui/koto.sock`
- `KOTO_TUI_TERM=$TERM` (по умолчанию `xterm-256color`)
- `KOTO_SCRIPTS_DIR=<state>/scripts` — только если этот каталог существует.

Ошибка отсутствующего токена: ``no TUI token in <creds> — mint one with `koto pki client -creds <creds> tui` ``.

### 2.5 `koto update`

- `KOTO_REPO_URL` — полный URL репозитория (замыкающий `/` обрезается).
- `KOTO_REPO` — `owner/name`, по умолчанию `jpzk/koto`; URL становится
  `https://github.com/<repo>`.
— `daemon/update.go` `updateRepoURL` (зеркалит `install.sh`).

Сам `install.sh` дополнительно читает `KOTO_KEY_FPR`, `KOTO_KEYSERVER`,
`KOTO_ARCH` (по умолчанию `x86_64`), `KOTO_VERSION` (по умолчанию
`latest`), `KOTO_DEST`, `KOTO_RELEASE_PUBKEY` — присваивания в начале
`install.sh`.

### 2.6 Внутренние переменные (не настройки оператора)

Они встречаются в коде `daemon/`, но задаются самим koto; не задавайте их.

- `KOTO_USERNS_READY` — маркер стадии бутстрапа userns (`unset`/`1`/`2`) —
  `daemon/userns.go`, константа `usernsEnv`.
- `KOTO_JAIL_MAX_UID` — усечённый конец диапазона uid каждой VM,
  переносимый через re-exec-и бутстрапа; принимается только в
  `(30000, 60000]` — `daemon/fcjail.go`, константа `jailMaxEnv`,
  `fcJailBaseUID`, `fcJailMaxUID`.
- `KOTO_DEFAULT_CLAUDE_MODEL`, `KOTO_DEFAULT_VENICE_MODEL` — внедряются в
  окружение init гостя со значениями `defaultClaudeModel` /
  `defaultVeniceModel` — `daemon/fc.go` (карта `env` init-а),
  `daemon/groups.go`. Также внедряются: `ANTHROPIC_BASE_URL=http://127.0.0.1:18888`
  (`fcGuestProxyTCP`) и `NO_PROXY`/`no_proxy=127.0.0.1,localhost`, когда
  сеть группы не `none`.
- `KOTO_SESSION` — экспортируется на каждый ход внутри гостя скриптом
  `entrypoint.sh`; демон читает его из окружений гостевых процессов для
  атрибуции заданий — `daemon/jobs.go`, `daemon/send.go`.
- `KOTO_ENDPOINT`, `KOTO_PROMPTS_DIR`, `KOTO_SCRIPTS_DIR`, `KOTO_TUI_TERM` —
  задаются `koto tui` для дочернего процесса TUI (раздел 2.4).

---

## 3. Конфигурация каждой группы (`config.json`)

- Расположение: `<state>/groups/<group>/.cs/config.json` —
  `daemon/config.go` `groupConfigPath`. Записывается атомарно (временный
  файл + rename, режим 0600) под блокировкой группы — `updateGroupConfig`.
- Группа должна существовать: `no such group: <g> (spawn it first)`;
  плохие имена: `invalid group name` — `daemon/config.go` `configCmd`.
- Неизвестные/некорректные значения **молча игнорируются** (прежнее
  значение сохраняется) — `daemon/config.go` `applyConfig`.
- Значение `null`, `""` или `[]` **очищает** ключ — `isClear`.
- Чтение возвращает *эффективную* конфигурацию с заполненными значениями
  по умолчанию — `effectiveConfig`. Она также сообщает поле `io` только
  для чтения (`<MiB>/s / <ops>ops`).

### Ключи

- `provider` — `claudesdk` | `venice` | `openai`. По умолчанию:
  `KOTO_DEFAULT_PROVIDER` хоста, иначе `claudesdk` (`daemon/providers.go`
  `defaultProvider()`, `daemon/groups.go` `ensureProviderConfig`).
- `model` — свободная строка, набор символов
  `^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`, максимум 128 символов
  (`configIdentRE`, `configMaxIdent`). По умолчанию, когда не задан:
  `claude-sonnet-5` под claudesdk, `kimi-k2.5` под venice
  (`daemon/groups.go` `defaultClaudeModel`, `defaultVeniceModel`).
- `effort` — то же правило набора символов/длины, что и у `model`.
  Показывается как `(default)`, когда не задан (`effectiveConfig`).
- `network` — `none` | `wan` | `lan` | `full` (константы `daemon/fcnet.go`
  `fcNetNone`…`fcNetFull`). По умолчанию `none` (без NIC). Любая другая
  сохранённая строка читается как `none` (`daemon/groups.go`
  `groupConfig.network`). Применяется при следующем запуске VM
  (`/restart`); понижение до `none` также вступает в силу на самом
  следующем запросе прокси (комментарий в `applyConfig`). Legacy-ключ
  `internet`: `full` → `wan`, `none` → `none`, мигрирует в `network`.
- `size` — `small` | `medium` | `large` | `xlarge`. По умолчанию `small`.
  Применяется при следующем запуске. Диск растёт, но никогда не
  уменьшается. Пресеты (`daemon/fc.go` `fcSizePresets`; vCPU, RAM MiB,
  диск workspace, IO MiB/s, IO ops):
  - `small`: 2, 1024, 8 GiB, 100, 15000 (`fcDefaultVcpus`,
    `fcDefaultMemMiB`, `fcWorkspaceBytes`)
  - `medium`: 2, 2048, 12 GiB, 150, 22500
  - `large`: 4, 4096, 16 GiB, 200, 30000
  - `xlarge`: 8, 8192, 24 GiB, 250, 37500
- `root` — `yes` | `no`. По умолчанию `no`. Беспарольный sudo +
  постоянный root-оверлей в госте; применяется при следующем запуске
  (`daemon/fc.go` `groupRoot`).
- `kvm` — `yes` | `no`. По умолчанию `no`. Открывает гостю VMX/SVM
  (вложенную виртуализацию); применяется при следующем запуске
  (`daemon/fc.go` `groupKVM`). Примечание: не указан в тексте usage
  `koto ctl`, но флаг `-kvm` существует (`daemon/ctl_cli.go`,
  `fs.String("kvm", …)`).
- `autostart` — `yes` | `no`. По умолчанию `no`. Читается только при
  старте демона (`autostartGroups`); `/restart` группы его не применяет.
- `ports` — JSON-массив int или строка через запятую (`8080,3000`).
  Каждый порт должен быть в `[1024, 65535]`; дубликаты и значения вне
  диапазона отбрасываются; хранится не более 32 (`configMaxPorts`).
  Публикуются на `KOTO_PORTS_BIND` (по умолчанию `127.0.0.1`) на хосте.

Ключи yes/no закрываются при сбое (fail closed): отсутствующее,
неразбираемое или любое иное значение — это `no`; для файлов, правленных
вручную, принимается и JSON-булево — `daemon/groups.go` `groupConfigBool`.

### Кто что может задавать

- **Ключи posture** = `network`, `internet`, `root`, `ports`, `size`,
  `autostart`, `kvm`. На gRPC запрос Config, несущий любой из них,
  авторизуется как синтетический admin-only глагол `config_posture` —
  `daemon/auth.go` `postureVerb`, `daemon/acl.go` `adminOnlyVerbs`.
- На ctl-плоскости внутри гостя `config_set` доступен только main
  (`ctl: verb not allowed for non-main groups: config_set`) и отказывает
  по каждому posture-ключу с `ctl: <key> is an operator-only setting —
  ask the operator (TUI /config, or koto ctl config)` — `daemon/ctl.go`
  `configPostureKey`. Main, следовательно, может задавать там только
  `model`, `effort`, `provider`.
- О ролях см. [acl-and-auth.md](acl-and-auth.md).
