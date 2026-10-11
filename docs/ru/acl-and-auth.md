Перевод файла ../acl-and-auth.md; английский оригинал является авторитетным.

# Аутентификация и авторизация (управляющий план gRPC)

Эта страница описывает, кто может вызывать gRPC API демона (TUI, Android,
`koto ctl`, скрипты) и что каждому вызывающему разрешено. Внутригостевой
ctl-план (`/workspace/.cs/ctl`) — **отдельная** ось авторизации, завязанная
на идентичность группы и жёстко закодированная в `daemon/ctl.go`. Он не
управляется `acl.json` (комментарий в заголовке `daemon/acl.go`). Его правила
см. в [ctl-reference.md, Часть 2](ctl-reference.md).

Все пути ниже даны относительно корня состояния `HERE`: это `KOTO_HOME`,
когда она задана (установленный режим, обычно `/var/lib/koto`), иначе —
рабочий каталог демона (`kotoHome`, `daemon/daemon.go`). Файлы учётных данных
живут в `<HERE>/creds/` (`credFile`, `daemon/auth.go`).

## 1. Слои

Каждый вызов проходит три проверки по порядку (комментарий в заголовке
`daemon/auth.go`, `authFromCtx`, `aclCheck`):

1. **mTLS**: сертификат сервера плюс обязательный клиентский сертификат.
2. **Bearer-токен**: аутентификация, привязанная к клиентскому сертификату.
3. **ACL**: авторизация по роли, глаголу и целевой группе.

Комментарий также упоминает WireGuard-оверлей ниже этих слоёв. Он работает
вне процесса koto.

### 1.1 Слушатель

- Адрес прослушивания по умолчанию — `127.0.0.1:8443`. `KOTO_BIND`
  переопределяет адрес, а `KOTO_PORT` — порт (`daemon/daemon.go`, после
  `go autostartGroups()`). См. [configuration.md](configuration.md).
- gRPC keepalive: сервер `Time: 30s`, `Timeout: 10s`. Политика принуждения —
  `MinTime: 10s` с `PermitWithoutStream: true` (`daemon/daemon.go`,
  `grpc.KeepaliveParams`).
- При старте демон логирует `kotod ready grpc=<addr> (mTLS+token)`, затем
  выполняет `authBindingPreflight`.

### 1.2 mTLS (`serverTLSConfig`, `daemon/auth.go`)

- Сервер предъявляет `creds/server.crt` и `creds/server.key`.
- Клиентские сертификаты обязательны и должны выстраивать цепочку до
  `creds/ca.crt` (`tls.RequireAndVerifyClientCert`). Минимальная версия —
  TLS 1.3 (`MinVersion: tls.VersionTLS13`).
- **Список допуска (allowlist).** SHA-256-отпечаток листового сертификата
  клиента (DER, hex в нижнем регистре) должен также присутствовать в
  `creds/clients.allow` (`VerifyPeerCertificate`, `loadClientAllow`).
  - Формат строки: `<sha256hex> <name>`.
  - Строки, чьё первое поле начинается с `#`, игнорируются.
- **Перечитывается на каждом рукопожатии.** Удаление строки отзывает это
  устройство без перезапуска демона.
- Ошибки рукопожатия:
  - `client cert <fp> not in allowlist`
  - `clients.allow: <err>` (например, файл отсутствует)
  - `no client certificate`

### 1.3 Bearer-токен (`authFromCtx`, `tokenIdentity`)

- Токен читается из gRPC-метаданных `authorization`. Префикс `Bearer `
  срезается.
- Токен хешируется SHA-256 и ищется в `creds/tokens.json`, который
  перечитывается на каждом вызове (`loadTokenIdentities`).
- **Сертификат и токен связаны (аудит L6).** CommonName клиентского
  сертификата должен совпадать с ключом `tokens.json`, по которому нашёлся
  токен (`certIdentity`). Поэтому достаточно любого одного отзыва: удалить
  строку из `clients.allow` или запись из `tokens.json`.
- Отказы возвращают gRPC `Unauthenticated`:
  - `invalid or missing token` (логируется как `rejected call from <addr> (bad/missing token)`)
  - `no client certificate`
  - `client certificate and token name different identities`
- **Предварительная проверка при старте** (`authBindingPreflight`). Для
  каждого имени, присутствующего только на одной стороне, логируется
  предупреждение:
  - `identity "<n>" has an allowlisted cert but no tokens.json entry — its calls will be refused (mint one: koto pki client <n>)`
  - `identity "<n>" has a token but no cert named "<n>" in clients.allow — its calls will be refused`

### 1.4 Формы `tokens.json` (`parseTokens`, `daemon/acl.go`)

Файл отображает `clientid → credential`. Принимаются три формы значения:

- `"<sha256hex>"`: легаси-форма, голый хеш. Роль — `admin` (`defaultRole`).
- `{"hash": "<hex>", "role": "agent"}`: одна роль.
- `{"hash": "<hex>", "roles": ["reader", "ops"]}`: несколько ролей. `roles`
  побеждает `role`, если присутствуют оба.

Запись пропускается (она никого не аутентифицирует) в двух случаях:

- она не подходит ни под одну из этих форм или имеет пустой `hash`;
- после обрезки у неё не остаётся ролей. Например,
  `koto pki client -role "" x` пишет `"roles": null`, что пропускается.

Хеш переводится в нижний регистр перед поиском.

## 2. PKI: выпуск учётных данных

Есть два эквивалентных маршрута. Оба производят одни и те же артефакты
(комментарий в заголовке `daemon/pki.go`):

- **Go:** `koto pki`, используется `koto setup` и `koto install`. Не требует
  openssl или jq.
- **Makefile:** `make pki-init` и `make pki-client`, с openssl и jq.

### 2.1 `koto pki` (`pkiCliMain`, `daemon/pki.go`)

```
usage: koto pki init   [-creds DIR] [-san DNS:a,IP:1.2.3.4]
       koto pki client [-creds DIR] [-role r1,r2] <name>
       koto pki server [-creds DIR] -san DNS:a,IP:1.2.3.4
```

Значения по умолчанию: `-creds creds`. `init -san` по умолчанию —
`defaultServerSANs` = `DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`.
`client -role` по умолчанию — `admin`.

#### `init` (`pkiInit`)

- Создаёт CA только если он отсутствует (`pkiEnsureCA`): EC P-256,
  `CN=koto-ca`, срок действия 3650 дней.
- Если `ca.crt` существует без `ca.key`, отказывает с
  `<crt> exists but <key> does not — refusing to mint a new CA over it.`,
  затем следует совет по восстановлению или перемещению в сторону.
- Выпускает сертификат сервера (`pkiServerCert`): `CN=koto-daemon`, срок
  825 дней, ExtKeyUsage ServerAuth. SAN должны быть `DNS:…` или `IP:…`. Всё
  остальное падает с `bad SAN "<s>" (want DNS:… or IP:…)`.
- Сертификат сервера перевыпускается, когда он отсутствует, отсутствует
  `server.key`, он нечитаем или просрочен, до истечения осталось меньше
  `pkiRenewWindow` (30 дней), он ещё не вступил в силу или не выстраивает
  цепочку до CA (`pkiServerCertNeedsReissue`). SAN не сравниваются.
- Засевает `acl.json`, только если файл отсутствует (`pkiCreateNew`, режим
  0600), содержимым:

  ```json
  {
    "agent": {
      "list": "*", "send": "*", "history": "*", "metrics": "*",
      "sched_list": "*",
      "subscribe_group": "*", "watch_state": "*"
    }
  }
  ```

- Каталог creds создаётся с режимом 0750.

#### `client <name>` (`pkiClient`)

- **Правило имени:** `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$` и никаких `..`
  (`pkiClientNameRE`, `pkiClientNameOK`). Иначе падает с
  `invalid client name "<name>": letters, digits and ._- only, no path separators …`.
- **Пишет:**
  - `client-<name>.key` и `client-<name>.crt`: EC P-256, `CN=<name>`,
    825 дней, ClientAuth.
  - Строку `<fp> <name>`, дописанную в `clients.allow`, если этот отпечаток
    ещё не присутствует.
  - 32-байтовый случайный hex-токен в `token-<name>` (0600).
  - `{"hash", "roles"}` под ключом `<name>` в `tokens.json`.
- Обновление `tokens.json` держит flock на каталоге creds (`pkiLockCreds`) и
  пишет атомарно через временный файл и rename.
- Повреждённый `tokens.json` падает с `tokens.json is corrupt — fix it on disk first: <err>`.
- Отсутствующий ключ CA падает с ``ca.key (run `koto pki init` first): <err>``.

#### `server -san …`

- Перевыпускает только сертификат сервера. CA не трогается, так что
  существующие клиенты продолжают работать.
- Печатает `server cert reissued for <san> — restart the daemon to apply`.
- Без `-san` падает с `pki server: -san is required (e.g. -san …)` (выход 2).

### 2.2 Маршрут Makefile (`Makefile`, цели `pki-init` / `pki-client`)

- `make pki-init [SERVER_SAN=…]`:
  - `SERVER_SAN` по умолчанию `DNS:koto-daemon,DNS:localhost,IP:127.0.0.1`.
  - Отказывает, если `creds/ca.key` существует:
    `creds/ca.key exists — refusing to regenerate the CA …`.
  - Засевает ту же роль `agent` в `creds/acl.json`, если файл отсутствует.
- `make pki-client NAME=<client> [ROLE=r1,r2]`:
  - `ROLE` по умолчанию `admin`.
  - Требует `creds/ca.key` и использует `jq` для правки `tokens.json`.
  - Пишет те же файлы, что и `koto pki client`.
- Ключи генерируются под `umask 077`, затем `chmod 600`.

### 2.3 Идентичности, создаваемые setup (`daemon/setup_steps.go`)

- PKI-шаг setup выполняет `pkiInit` с `defaultServerSANs` плюс любые
  дополнительные SAN, которые вводит оператор.
- Затем он выпускает двух клиентов, **оба с ролью `admin`**, каждого только
  если его сертификат отсутствует:
  - `tui`
  - `agent` — `KOTO_CLIENT` по умолчанию для `koto ctl`.
- *Клиент* `agent`, таким образом, является админом намеренно. Комментарий
  ссылается на аудит M11, решение от 2026-09-05.
- *Роль* с именем `agent` в засеянном `acl.json` — другая вещь: она
  существует для ограниченных по области (scoped) идентичностей, выпущенных
  через `koto pki client -role agent <name>`.

## 3. Авторизация: роли, глаголы, цели

### 3.1 Модель (`daemon/acl.go`)

- У пользователя (clientid) одна или несколько ролей, и каждая роль выдаёт
  глаголы на целях. Действующее разрешение — **объединение** по ролям
  пользователя (`rolesAllowed`).
- `creds/acl.json` отображает `role → {verb → targets}`. `daemon/acl.go` —
  первоисточник; комментарий в Makefile над `pki-init` говорит то же.
  - Цели — имена групп, `"*"` или `["*"]` (любая группа), или список.
  - Ключ глагола `"*"` выдаёт все глаголы на своих целях, **кроме**
    admin-only глаголов (`grantFor`).
  - Легаси плоская форма `{"agent": ["list", "send"]}` означает эти глаголы
    на любой цели (`parseACL`).
- `acl.json` перечитывается на каждом вызове (`loadACL`). Правка ролей или
  грантов не требует перезапуска.

### 3.2 Роль `admin`

`admin` жёстко закодирована (`grantFor`, `defaultRole = "admin"`):

- Она может вызывать любой глагол на любой цели.
- Любой ключ `admin` в `acl.json` игнорируется.
- Отсутствующий или повреждённый `acl.json` не может выкинуть админа.
- `acl set admin …` падает с
  `the admin role is hardcoded and cannot be defined in the ACL`.
- `acl del admin` падает с `the admin role is hardcoded and cannot be deleted`
  (`aclSetRoleCmd`, `aclDelRoleCmd`).

### 3.3 Закрытие при сбое (fail closed)

Запросы отклоняются в каждом из этих случаев (`grantFor`, `roleAllowed`,
`parseACL`, `parseTargets`):

- **Неизвестная роль:** роль отсутствует в `acl.json`.
- **Отсутствующий файл:** без `acl.json` не существует ни одной не-admin
  роли.
- **Повреждённый файл:** некорректный `acl.json` (`parseACL` возвращает nil)
  отклоняет каждую не-admin роль.
- **Некорректная роль:** роль с некорректным значением не выдаёт ничего.
- **Плохое значение гранта:** одно плохое значение гранта, или `null`, или
  `""`, даёт пустой набор целей (аудит M35).
- **Неперечисленный глагол:** глагол, который не перечислен и не покрыт
  `"*"`. Новые RPC, таким образом, отклоняются по умолчанию.

### 3.4 Глаголы

- Глагол — это snake_case-форма имени gRPC-метода, например
  `/koto.Koto/SchedAdd` → `sched_add` (`verbFromMethod`).
- Список RPC — см. `service Koto` в `protocol/koto.proto`.

Полный набор глаголов, механически выведенный из RPC (поэтому имена вроде
`goal_list` или `job_logs` не обязаны встречаться в Go-исходниках буквально):

- **Жизненный цикл:** `spawn`, `send`, `list`, `stop`, `interrupt`,
  `destroy`, `restart`, `history`, `config`, `metrics`, `clear`, `drain`
- **Задания:** `jobs`, `job_logs`, `job_tail`
- **Расписания:** `sched_add`, `sched_list`, `sched_del`, `sched_toggle`,
  `sched_run`
- **Цели:** `goal_set`, `goal_list`, `goal_approve`, `goal_pause`,
  `goal_interrupt`, `goal_resume`, `goal_cancel`
- **Инспекция:** `resources`
- **Управление ACL:** `acl_get`, `acl_set_role`, `acl_del_role`
- **Выполнение и потоки:** `run_script`, `attach_shell`, `subscribe_group`,
  `subscribe_logs`, `watch_state`

### 3.5 Admin-only глаголы (`adminOnlyVerbs`)

Эти глаголы нельзя выдать через `acl.json`, даже глаголом `"*"`. Проходит
только `admin`:

- `acl_get`, `acl_set_role`, `acl_del_role`. Управление ACL не должно быть
  делегируемым через сам ACL.
- `run_script`: прямое выполнение кода в гостевой VM.
- `config_posture`: синтетический глагол, описан в следующем разделе.

### 3.6 `config` против `config_posture` (`postureVerb`, `daemon/auth.go`)

- Запрос `Config`, устанавливающий любой ключ **posture**, переименовывается
  в `config_posture`, который admin-only. Ключи posture — `network`,
  `internet`, `root`, `ports`, `size`, `autostart` и `kvm`.
- Грант `config`, таким образом, покрывает только оставшиеся настройки:
  `model`, `effort` и `provider`.
- Проверка цели всё равно применяется. Внутригостевой ctl-план отклоняет
  ключи posture самостоятельно (`configPostureKey` в `daemon/ctl.go`; см.
  [configuration.md](configuration.md)).

### 3.7 С целью и без цели (`targetOf`)

Глагол ограничен группой («targeted»), когда его сообщение-запрос несёт
группу.

Типы запросов с целью и использующие их RPC:

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
- `ShellInput`: AttachShell. Каждое сообщение в потоке несёт `group` и
  проверяется.

Правила:

- Глагол с целью проходит, только если грант содержит `"*"` или называет
  группу (`roleAllowed`).
- Запрос с целью и **пустой** группой требует гранта `"*"`, потому что он
  читает по всем группам. Примеры: глобальный `metrics`, нефильтрованный
  `sched_list`, а также `jobs` / `goal_list` без группы. Пустое имя никогда
  не сохраняется как цель (`parseTargets`).
- **Только глагол** (без измерения цели): `list`, `watch_state`,
  `resources`, `subscribe_logs`, `sched_del`, `sched_toggle`, `sched_run` и
  глаголы `acl_*`. Любой грант этого глагола, с любым набором целей,
  пропускает вызов.

### 3.8 Проекция чтений «только глагол» (`visibleTargets`)

Для этих глаголов набор целей гранта сужает **ответ**, а не вызов
(`daemon/grpc_server.go`):

- **`list` и `watch_state`.** Возвращаются только группы, покрытые грантом
  (`projectGroups`). Записи заданий каждой группы тоже отбрасываются, если
  грант `jobs` не покрывает эту группу.
- **`resources`.** Чужие группы удаляются, а итоги по флоту хоста (число
  групп, работающие группы, байты alloc/provisioned, закоммиченная память)
  пересчитываются только по видимым группам, если грант не `"*"`
  (`projectResources`). Поля уровня машины — размер файловой системы и
  свободное место, предел памяти и MemTotal — сохраняются.
- **`subscribe_logs`.** Доставляются только события, отнесённые к покрытой
  группе. События уровня демона (пустая группа) отбрасываются, если грант не
  `"*"` (`logVisible`).
- **`metrics`.** Поле `GlobalMetric` (новейшая запись из любой группы)
  заполняется только для вызывающих, чей грант `metrics` равен `"*"`
  (обработчик `Metrics`, аудит M30).

### 3.9 Потоки

- **Шлюз глагола.** При открытии потока вызывающий должен иметь глагол в
  какой-то роли (`anyGrant`, в `authStream`). Проверка цели выполняется при
  получении каждого сообщения-запроса (`aclStream.RecvMsg`).
- **Перепроверка.** Долгоживущие потоки переавторизуются при отправке не
  чаще, чем раз в `aclStreamRecheck` = 5s (метод `revalidate` у `aclStream`,
  аудит M25).
  - Идентичность заново разрешается по имени из `tokens.json`. Если она
    исчезла, поток закрывается с `Unauthenticated`
    `identity <name> has been revoked`.
  - Если роли больше не разрешают глагол, он закрывается с
    `PermissionDenied` `roles <r> may no longer call <verb> on "<group>"`.
- **Пределы допуска.** `streamMaxPerIdentity = 512` и
  `streamMaxGlobal = 2048` (`streamAdmit`). Сверх любого предела вызов
  возвращает `ResourceExhausted`
  `too many live streams (per-identity 512, global 2048)`.

### 3.10 Ошибки отказа и логи (`aclCheck`)

- Отказы возвращают gRPC `PermissionDenied`:
  - `roles <r1,r2> may not call <verb> on "<group>"` (с целью)
  - `roles <r1,r2> may not call <verb>` (только глагол)
- Каждое решение логируется под источником `acl`:
  `<name> (roles …) allowed|denied <verb> [on "<group>"] from <addr>`.
- Решения с целью относятся к логу группы, когда имя группы валидно
  (`aclLogGroup`).

## 4. Управление ACL

Есть три admin-only RPC: `AclGet`, `AclSetRole` и `AclDelRole`
(`daemon/grpc_server.go`). Из CLI это `koto ctl acl get|set|del`
(`ctlAcl`, `daemon/ctl_cli.go`; см. [ctl-reference.md](ctl-reference.md)).

```sh
koto ctl acl get
koto ctl acl set ops stop:ghost restart:ghost list 'metrics:*'
koto ctl acl del ops
```

- **`acl set` заменяет весь объект грантов роли** (`aclSetRoleCmd`).
- **Имена ролей** следуют `^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$` (`roleNameRE`).
  Иначе: `invalid role name "<r>"`.
- **Валидация грантов** (`validateGrants`) отклоняет весь запрос на первой
  плохой записи:
  - Ключ глагола должен быть `"*"` или соответствовать `roleNameRE`, иначе
    `invalid verb "<v>"`.
  - Цель должна быть `"*"` или списком имён, соответствующих `roleNameRE`
    (или `"*"`). Ошибки: `verb "<v>": target must be "*" or a list of groups`
    и `verb "<v>": invalid target <t>`.
- **Записи** держат `aclFileLock` и идут атомарно через
  `creds/.acl.json.tmp` и rename, с режимом 0600 (`writeACLDoc`).
- **Повреждённый файл блокирует изменение:**
  `acl.json is corrupt — fix or remove it on disk first` (`readACLDoc`).
- **`acl del` несуществующей роли** падает с `no such role: <role>`.

## 5. Рецепты

**Ограниченная идентичность для скрипта.** Выпустите клиента и направьте на
него `koto ctl`:

```sh
koto pki client -creds /var/lib/koto/creds -role agent ci
KOTO_CLIENT=ci koto ctl list
```

`KOTO_CLIENT` выбирает `client-<name>.{crt,key}` и `token-<name>`
(`newKotoClient`, `daemon/ctl_cli.go`).

**Ограничить роль одной группой**, заменив значения `"*"` списками:

```sh
koto ctl acl set agent list:main send:main history:main subscribe_group:main watch_state:main
```

**Отозвать устройство.** Удалите его строку в `creds/clients.allow` **или**
его запись в `creds/tokens.json`. Достаточно любого из двух из-за связки
сертификат/токен. Новые вызовы падают сразу, а открытые потоки закрываются
в пределах 5 с (`aclStreamRecheck`).

**Достучаться до демона с другой машины.** Добавьте SAN, не трогая CA, затем
перезапустите демона:

```sh
koto pki server -creds /var/lib/koto/creds -san DNS:koto-daemon,DNS:localhost,IP:127.0.0.1,IP:192.168.1.20
```

Демон слушает только `127.0.0.1`, если не задан `KOTO_BIND`. См.
[configuration.md](configuration.md).
