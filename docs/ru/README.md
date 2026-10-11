Перевод файла ../README.md; английский оригинал является авторитетным.

# Документация koto

Справочная документация для пользователей koto (тех, кто общается с
группами) и операторов (тех, кто устанавливает и запускает демон). Каждое
фактическое утверждение в этих документах снабжено ссылкой на источник
(файл плюс имя функции/константы), так что его можно найти grep-ом в этом
репозитории; там, где код был неоднозначен, в документах написано
"unverified" вместо догадок. Если документ и код расходятся, прав код —
пожалуйста, исправьте документ.

## С чего начать

- [getting-started.md](getting-started.md) — требования к сборке и среде
  выполнения, стадии `make` (fetch / build / install / wizard), путь через
  `install.sh`, предварительные проверки `koto install` и шаги мастера
  `koto setup`.
- [architecture.md](architecture.md) — уровни (host CLI, демон, прокси,
  сетевой шлюз gVisor, fcjail, гость Firecracker), порядок запуска демона,
  пресеты размеров VM и карта портов vsock.

## Справочник

- [configuration.md](configuration.md) — каждый ключ `/etc/koto/koto.env`
  и каждая переменная окружения `KOTO_*`, читаемая кодом, со значениями по
  умолчанию, плюс ключи конфигурации групп (model, provider, network,
  size, root, effort, ports, autostart, …) и их допустимые значения.
- [ctl-reference.md](ctl-reference.md) — каждый глагол и флаг `koto ctl`
  на хосте и глаголы ctl-плоскости внутри гостя (`/workspace/.cs/ctl`).
- [acl-and-auth.md](acl-and-auth.md) — PKI, клиентские токены, роли,
  ACL «глагол × цель» и глаголы только для администратора.
- [protocol.md](protocol.md) — gRPC-сервис в `koto.proto` (все RPC и поля
  запросов) и vsock-фрейминг гостя по каждому каналу.

## Работа с группами

- [groups-goals-schedules.md](groups-goals-schedules.md) — чат-сессии и
  лимиты очередей, фоновые задания `cs-job`, автопилот целей,
  cron-расписания и окно делегирования report/reply.

## Эксплуатация

- [operations.md](operations.md) — логи демона и уведомления, сэмплер
  ресурсов и алерты, обновление через `koto update`, удаление через
  `koto uninstall` и устранение неполадок по точному тексту ошибки.

## Заметки о дизайне (существовали ранее)

- [../firecracker-vsock.md](../firecracker-vsock.md) — заметки о дизайне
  групп на микроВМ Firecracker и vsock IPC.
- [../kernel-amzn-vs-vanilla.md](../kernel-amzn-vs-vanilla.md) — почему
  гостевое ядро собирается из исходников Amazon Linux, а не из ванильного
  kernel.org.

## История (архивные отчёты, не поддерживаются)

Анализы на момент времени; они описывают код таким, каким он был на их
дату, и могут устареть.

- [../history/SECURITY_19_MAY.md](../history/SECURITY_19_MAY.md) — анализ
  безопасности бэкенда, 2026-05-19.
- [../history/ANALYSIS_31_MAY.md](../history/ANALYSIS_31_MAY.md) — анализ
  проблем, 2026-05-31.
- [../history/SECURITY_11_SEP.md](../history/SECURITY_11_SEP.md) — триаж
  оценки безопасности, 2026-09-11.
- [../history/FUZZ_09_OCT.md](../history/FUZZ_09_OCT.md) — фаззинговая
  кампания, 2026-10-09.
