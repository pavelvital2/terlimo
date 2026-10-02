# Сервер — паспорт принятой рабочей пары, 2026-10-02

## Принятая совместимая пара и уровень свидетельств

Обновление по `whitelist-20261002-working-pair-server-passports-v1`; только документация, без новых проверок, установки, runtime действий или публикации. Текущий selection run принят руководителем в доказанном объёме. PASS ниже означает только названный сценарий/подпункт; NOT_TESTED не удаляет требование; KNOWN_ISSUE не означает установленную причину.

| Компонент | Точная версия |
| --- | --- |
| App source | `5a31c7aa1120c6045f76c35b6c02c7e70c61fbec`, canonical `pavelvital2/terlimo`, branch `fix/catalog-stage-deadlines-20261002` |
| APK | SHA256 `ef84c3e0ae1c595ae24809a8f3043c34dc892c3e54c647156c83b2332fb210f3` |
| Packaged native | SHA256 `792a133e887d3937f3b9cdd19a496190049b26e667f3f95a6f30d706e42023ef`; Go subtree reused from `b47069c0962868a5aa3230787d1b2ed32db99d68`, no new Go build |
| Gateway binary | SHA256 `a42fa90e0b7459b128e445e473078f31b6c180d8da00310d332182c9dd210f1d` |
| Server relay module | SHA256 `8c4667ba2eac7c8d34526578a0890945b95e11301278bdcba97dc08c708b69de` |
| Server AUTH API module | SHA256 `d16bd8f4678200ab8068baa5e7fc3a6a219b970cce59da7811bc6aef0c76eef3` |
| Server browse module | SHA256 `7bec1ebd5b38f214871b70bcbeee36d62808e644a09774d8441c84fbff3419cc` |
| Canonical source / earlier package passport | `de6681e1cc9d92cb1d92e824ab885d026651aa84` / `13a4a4d55708e38792af042d301efa093fc243d0` |
| Current client passport publication | Root receipt `2ed1aaf`; full commit not provided here; server passport does not invent it |

Canonical snapshot is **not byte-identical to deployed gateway source**. Accepted runtime build uses ordinary `d847a41168c54f413e0a961e1ce198db116650b9` plus reviewed `client_test_service.go` SHA256 `301ebbe0ff4032b598b8c6706e8f5007ba7ee026465d456252340415162ff75d`. Canonical `client_test_service.go` and `udp_listener.go` retain optional frame/preaccept observer hooks absent from this ordinary deployed build. Relocated `go_client` is not the gateway main build. Root explicitly accepted runtime overlay + full input inventory + artifact receipt; matching canonical Python was confirmed. No rebuild to introduce extra hooks; whole backend checkout/heap byte equality is not claimed from three module hashes.

## Current пользовательский сценарий — ограниченный PASS

Evidence: original `laptop-20261002-selection-build-acceptance-result-engineer-v1`, server `terlimo-20261002-selection-acceptance-server-correlated-result-v1` (delivered/accepted); root acceptance in this task. Actual one launcher `2026-10-02T08:31:00.544973Z`, one gateway selection, original attempt `9e2f48e0`, no repeated build/source gates.

| Проверенная функция / участие компонента | Current status и граница |
| --- | --- |
| AUTH, текущий TEST доступ, каталог и штатный sync | PASS: same original account/installation/binding locally confirmed; AUTH/ME/browse200; expired technical lease advanced by ordinary client sync and existing worker |
| G10/G11/G14: видимый выбранный gateway после fresh Refresh | PASS только выбранной строке: preConnect Refresh736ms, no reselect; country/ping/large catalog/offline not retested |
| G12: Connected Refresh | PASS: one Refresh1798ms; displayed selection retained, same VPN network122/creation527391707149 before/after; blocked-network variant NOT_TESTED |
| G17: explicit Connect и полезный VPN HTTPS | PASS: UIConnected + system VPN, ordinary VPN-bound VpnReadinessProbe DNS/HTTPS200/expectedExit/ready,3007ms; no separate browser probe |
| G19/G20: ordinary Disconnect и OFF | PASS: user_cancel/teardown_complete, ChildExitgeneration1/exit0/HOST_STOP, independentOFF08:36:18.755113Z, native/service/VPNOFF/UIclosed, identity/installation marker preserved; other lifecycle variants not retested |
| Data admission и совместимость gateway | PASS в этом сценарии:36auth_ok/36relay_attached, client36/36ready; counts do not establish speed/long stability |
| Subsequent ordinary credential snapshot BEFORE Connect | NOT_TESTED: checkpoint NOT_OBSERVED, UI stayedbrowse; no synthetic update/replay |
| Runtime stablegateway ID equality | NOT_TESTED: UNKNOWN; visible name/endpoint matching is not stable-ID proof; accepted source sameID/reorder/removal/revocation/secondsnapshot proof is separate |
| Fresh VPN consent; full101; long stability | NOT_TESTED; existing consent reused, no all101PASS or long stability assertion |

11 available native endpointSHA markers matched server DTLS markers. Native call != server generation; exact HTTP wireRID/xid mapping and calibrated cross-clock latency remain unproven. Saved source/build/install receipts and prior targeted gates are historical PASS at their own scope, not rerun or promoted to full product acceptance.

## KNOWN_ISSUE и совместимость сроков

- Cold autoload35564ms, internal DTLS candidateTIMEOUT8097ms recovered; server AUTH11005/988ms. Latency remains unresolved, cause UNKNOWN. Both AUTH durations below old15s; nested budget patch does not prove the original delay fixed. Do not repeat completed preAPI investigations on the strength of this passport.
- GW409×5 during ordinary access sync, then GW200 at08:31:48UTC. This is recorded transient pending behavior, not hidden/rewritten as all-200; it is not an independent cause diagnosis.
- One unattributed managed `auth_failed` in the saved interval.36 successful attachments do not erase it; root cause/per-channel binding unknown. No blanket error-free/stability claim.
- Existing TEST entitlement ends `2026-10-03T07:41:20.364350Z`, revision2; one authorized24h extension, kind/starts/plan/limits/device/keys retained. No trial activation/payment/hour reset/new entitlement. Last recorded technical grant: desired=appliedgeneration122/lease119, applied08:31:47.934506UTC, `applied_not_after=2026-10-02T08:46:40Z`. This15min technical lease is distinct from24h TEST entitlement and is a historical receipt, **not fresh readiness after its timestamp**. Ordinary sync updates it; this documentation neither grants nor extends access.
- Wire authority, validation, replay/idempotency, audit, DB fences and cancellation retained. TEST evidence is not a production release, real payment/customer change or standalone clean distribution acceptance. New diagnostics/flags are not enabled.

## Сервер: source/runtime профиль и rollback

Modules reside in `/home/pavel/terlimo-test-a-live/terlimo_backend`; API/relay are Python, not separate compiled artifacts. Recorded AUTH-only finite absolute profile: node22s → relay21s → mTLS/loopback/API20s; per validated challenge/session request, caller can shorten, shared client budget unchanged. Loopback-only monotonic deadline header cannot expand20s; public frame validation rejects it. Unknown-input frame15s, nonAUTH timings and SQL15s per operation remain. Sync CPU/scheduler stalls are not preempted by asyncio. Existing accepted10 offline Python/targeted Go checks were reused, no new test/SLA claim.

Existing pre-apply private backup `/home/pavel/step036-receipts/private/s5-auth-nested-budget-test-apply-20261002/backup`: `auth_api.py` beforeSHA `383ffff577a9fce802b43e7d7593237e924bd2634aeeb22e6531470d940f3ca2`, `service_relay.py` beforeSHA `6f6ca3081b0c2dbf1de97b0434516ad6fa109fe29b595a33d4c94f96fd41f83d`; uid/gid1000/1000,0644. Node and private unit/launcher/env backups+metadata recorded in the same accepted apply receipt; secret contents are not published.

Future **authorized** coordinated rollback only: idle accepted scope, stopnode→relay→API, atomic restore exact two Python backup files and gateway binary with saved ownership/mode; startAPI/ownedTCP→relay/ownedUnix→node/ownedUDP, compare saved hashes/catalog7bec/config/flags and protected neighbors. Do not overwrite later accepted work or migrate/delete data. This passport executes none of these actions. Backend-only arbitrary rollback compatibility was not separately tested. Existing TEST record renewal backup/fixed target at private `s5-existing-test-access-vpn-20261002`; access rollback is a separate authorized operation, not a binary rollback or a grant SQL recipe.

## Сохранённый исторический паспорт и полный перечень101 требований

Ниже исходный паспорт сохранён без переписывания строк/статусов. Его SOURCE ONLY, прежние FAIL, offline PASS и «не проверено» относятся к соответствующему историческому этапу. «Не проверено в этом паспорте» означает historical NOT_TESTED, не отсутствие функции. Текущая ограниченная приёмка указана выше; остальные101-ID требования не получили новую blanket приёмку. Предыдущие source/runtime таймауты или флаги в истории не являются текущей настройкой.

---

# Management refresh: одна граница после drain

Статус: SOURCE ONLY, offline проверка. Runtime, flags, services, БД, phone и worker не менялись. Применение назначает руководитель после review и канонического snapshot push в pavelvital2/terlimo; этот checkout не публиковался.

## База и полная функциональность

Точная принятая база: 87e77ec798b3002961220ad14b70344c039c0894.
До изменения оба runtime файла checkout сверены с принятой live базой:
- operation_timing.py: 722391a545fe541f4febc811f0b5f9d3be0944ac517c3d7f58c537eef720788e;
- gateway_adapter.py: 3812a4e9edb81fc57e96948aac2ea876fb5a9b5750cba4c2435b4651bd8a175d.

ManagementTlsClient.call после успешного writer.drain для op=refresh вызывает callback, который только сохраняет первый monotonic timestamp. Callback не пишет лог, не копирует request/response, не добавляет wire fields, не меняет ожидания, TLS, timeout, close или decode.

Состояние принадлежит конкретному timed_rpc через ContextVar и сбрасывается в finally. Разные asyncio tasks получают разные state dict. Вложенный timed_rpc устанавливает собственное состояние (либо None), затем восстанавливает внешнее; обычный callback вне контекста ничего не делает. Это локальная привязка к существующему operation_id/correlation_id/attempt completion marker, без глобального текущего operation.

Существующий rpc_end получает три поля только при наличии границы:
`boundary=management_post_drain setup_send_ms=<...> response_rest_ms=<...>`.
Нет дополнительной записи в середине RPC. Начало, конец, outcome и технические ID сохраняются. Payload, credential, operation idempotency key, ответы и текст ошибок не логируются этой диагностикой.

## Точные границы и ограничения вывода

- Начало: существующий monotonic started в timed_rpc, перед существующим rpc_begin логом и await. setup_send включает этот лог/context setup, сериализацию, синхронный TLS context, connect/TLS handshake, write и успешный drain.
- Разделитель: monotonic непосредственно после возврата успешного writer.drain. Drain означает локальный flow control, не подтверждение обработки сервером.
- Конец: существующий rpc_end log_phase измеряет monotonic после возврата awaitable или его исключения. Успешный путь уже выполнил read до EOF, writer.close и decode/валидацию результата; close здесь инициируется, wait_closed не добавляется.
- response_rest включает ожидание ответа/EOF, management handler, Unix/node, IO/scheduling, close initiation, клиентское decode/валидацию и выход из awaitable. Это НЕ чистое время node. Total и обе части используют один конечный timestamp.
- При ошибке/cancel до успешного drain дополнительные поля отсутствуют; после него сохраняются вместе с прежним result=exception. Исходное исключение/cancel повторно выбрасывается.
- Publish остаётся отдельным существующим измерением; старые 2079 ms нельзя приписывать RPC.

Протокол/schema, authentication/validation, idempotency, DB fences/audit/rollback, продуктовые таймауты и публичный клиентский API не изменены. OFF: старый формат rpc_end без новых полей; обычный management вызов без timed_rpc также не собирает timestamp. Другие RPC и Unix transport не получают границу. Реальные TLS/сеть/нагрузка и старые loaded binaries этим offline тестом не проверены; отсутствие серверной регрессии не утверждается.

## Scoped включение после отдельного назначения root

Только env будущего согласованного worker: `TERLIMO_MANAGEMENT_RPC_BOUNDARY=1`. По умолчанию отсутствует/OFF; любое значение кроме точного 1 выключает сбор. Требуются два исходных файла из patch, без flags на API/relay/node. Никакое включение или restart сейчас не выполнено. Root назначает применение к idle worker и предусмотренный им эксперимент; этот документ не открывает live окно.

## Проверки и воспроизводимость

Из checkout с зависимостями принятого backend:
`/home/pavel/projects/terlimo-backend/.venv/bin/python3 -m pytest -q offline_tests/test_management_boundary.py`

16 PASS: deterministic clocks/fake IO; success/identity/attempt/wire keys/no mid-log; connect/drain/read ошибки; cancel до/после drain и reset; decode/remote ошибки; OFF/nonapplicable; два перекрывающихся refresh без смешения; прежнее отображение connect/read TimeoutError в GATEWAY_UNREACHABLE. Последняя проверка проходит через настоящие OutboxWorker._process → GatewayControlHandlers.apply_grant → timed_rpc → ManagementTlsClient.refresh_lease/call и заканчивается finalize done. Только DB/ownership/finalize, heartbeat, TLS context и IO заменены fake, PostgreSQL/socket не используются.

Before: на точной неизменённой базе success-тест падает из-за отсутствующего boundary (старый completion работает). After: все 16 проходят. Полный suite не повторялся. SHA, base provenance, before/after stdout и runtime-only patch переданы отдельно. Runtime-only patch включает два production Python файла; полный source commit дополнительно включает этот паспорт и offline тесты.

## Точный rollback

На source checkout: вернуть два файла из commit 87e77ec798b3002961220ad14b70344c039c0894 (SHA выше); новые тесты/паспорт не влияют на runtime. Reverse runtime-only patch также возвращает точную базу при совпадающих after SHA.

При будущей установке root/исполнитель сначала сохраняет именно устанавливаемые runtime файлы и scoped env/drop-in. Откат: восстановить эти backup байты, убрать только добавленный TERLIMO_MANAGEMENT_RPC_BOUNDARY, восстановить исходный scoped env/drop-in и выполнить только назначенный worker restart в разрешённом idle состоянии. Проверить SHA двух файлов и отсутствие flag. База выше применима только к совпадающим before SHA: более позднюю утверждённую работу этим rollback не затирать. Сейчас backup runtime или restart не требовались, поскольку live не менялся.


## Привязка к проектной приёмке

Root принял точный source639c28a после чтения runtime diff и16 targeted tests. Ниже полный101-ID срез действующих требований сервера; исторические статусы baseline не переносятся на новый candidate. Все не проверенные данной offline правкой функции остаются без новой приёмки. Изменён только management refresh timing; runtime пока не применён, device PASS отсутствует.

## Полный перечень функций и требований

Срез действующего объёма: G01–G74 и дополнения. Все строки сохранены в каждом паспорте для проверки совместного продукта. Для данного компонента указывать его участие; «не относится» допустимо только с объяснением. «Не установлено» означает отсутствие привязанного свидетельства, а не отсутствие кода. Полный критерий — в указанном ТЗ/AT; новые решения добавляются, отложенные требования не удаляются. Исторические паспорта сохраняют свой срез.

| ID | Функция / требование | Объём и приёмка | Участие компонента / реализация | Проверка точной версии / evidence |
|---|---|---|---|---|
| G01 | Пять вкладок: Главная, Подписка, Маршрутизация, Настройки, Помощь | Действующее ТЗ; AT21 | Не установлено | Не проверено в этом паспорте |
| G02 | Старт на Главной; автоматический каталог до доступа; Connect только после проверки права и выбора | Действующее ТЗ; AT01, AT12, AT21 | Не установлено | Не проверено в этом паспорте |
| G03 | Повторный запуск, сохранённая подписка и отсутствие сети | Действующее ТЗ; AT09, AT31 | Не установлено | Не проверено в этом паспорте |
| G04 | Идентификация установки и дополнительный сигнал устройства | Действующее ТЗ; AT12, AT31, AT38 | Не установлено | Не проверено в этом паспорте |
| G05 | Первичный путь без персональной ссылки; отдельный разовый час с первого подключения для Telegram/оплаты | Действующее ТЗ; AT12, AT13, AT38 | Не установлено | Не проверено в этом паспорте |
| G06 | Trial 7 дней после обязательного Telegram; отдельный разовый час не является trial и не включает его автоматически | Действующее ТЗ; AT13, AT20 | Не установлено | Не проверено в этом паспорте |
| G07 | Telegram Start → подписка на канал → проверка → привязка | Действующее ТЗ; AT14, AT39 | Не установлено | Не проверено в этом паспорте |
| G08 | Один trial; сохранение первоначальных дат и истории повторного использования | Действующее ТЗ; AT13, AT14 | Не установлено | Не проверено в этом паспорте |
| G09 | Повторная установка и возврат в существующий аккаунт | Действующее ТЗ; AT12, AT31, AT39 | Не установлено | Не проверено в этом паспорте |
| G10 | Дата/тип подписки и устройства; Connect только после загрузки/выбора | Действующее ТЗ; AT21 | Не установлено | Не проверено в этом паспорте |
| G11 | Кэш каталога и выбранного узла; offline-first отображение | Действующее ТЗ; AT09 | Не установлено | Не проверено в этом паспорте |
| G12 | Ручное обновление при обычном VPN и удерживаемой блокировке | Действующее ТЗ; AT07, AT09, AT40 | Не установлено | Не проверено в этом паспорте |
| G13 | Произвольное число шлюзов без нового APK; >=3 и большой каталог, без продуктового лимита A/B | Действующее ТЗ; AT09, AT29 | Не установлено | Не проверено в этом паспорте |
| G14 | Карточка: страна, название, доступность, ping, выбор тапом | Действующее ТЗ; AT07, AT21 | Не установлено | Не проверено в этом паспорте |
| G15 | Общий/точечный настоящий ping, без автоматического запуска | Действующее ТЗ; AT10 | Не установлено | Не проверено в этом паспорте |
| G16 | Ping во время блокировки/активной сессии; сортировка всех узлов | Действующее ТЗ; AT07, AT10 | Не установлено | Не проверено в этом паспорте |
| G17 | Connected только после рабочей readiness | Действующее ТЗ; AT01, AT30 | Не установлено | Не проверено в этом паспорте |
| G18 | Не оставлять ложное зелёное состояние при последующей потере доступа | Действующее ТЗ; AT01, AT30 | Не установлено | Не проверено в этом паспорте |
| G19 | Постоянное уведомление, открытие приложения и Disconnect | Действующее ТЗ; AT04, AT11, AT22 | Не установлено | Не проверено в этом паспорте |
| G20 | Явный Disconnect снимает защиту, сохраняет каталог/выбор | Действующее ТЗ; AT04, AT09 | Не установлено | Не проверено в этом паспорте |
| G21 | Сворачивание, экран off, свайп Recents без прекращения загрузок | Действующее ТЗ; AT08 | Не установлено | Не проверено в этом паспорте |
| G22 | Wi-Fi ↔ LTE/5G без ручного переподключения и смены сервера | Действующее ТЗ; AT06 | Не установлено | Не проверено в этом паспорте |
| G23 | Автовосстановление после длительной полной потери сети | Действующее ТЗ; AT05 | Не установлено | Не проверено в этом паспорте |
| G24 | Kill Switch при всех отказах/revoke/expiry, без прямой утечки | Действующее ТЗ; AT02, AT20, AT41 | Не установлено | Не проверено в этом паспорте |
| G25 | Смена любого доступного шлюза на любой другой без ручного Disconnect; stable ID и сохранение после reorder | Действующее ТЗ; AT07 | Не установлено | Не проверено в этом паспорте |
| G26 | Новый сервер не работает: остаёмся заблокированы, доступны выбор/ping/refresh | Действующее ТЗ; AT07, AT10 | Не установлено | Не проверено в этом паспорте |
| G27 | Все/выбранные через VPN/выбранные мимо VPN | Действующее ТЗ; AT03 | Не установлено | Не проверено в этом паспорте |
| G28 | Поиск, скрытие системных приложений, массовый белый список | Действующее ТЗ; AT03, AT21 | Не установлено | Не проверено в этом паспорте |
| G29 | Исключённые приложения работают напрямую при падении VPN | Действующее ТЗ; AT03, AT22 | Не установлено | Не проверено в этом паспорте |
| G30 | Пустой include-only и момент применения новых правил | Действующее ТЗ; AT03, AT21 | Не установлено | Не проверено в этом паспорте |
| G31 | Качество по рабочим потокам; корректные workers и потоки | Действующее ТЗ; AT11, AT34 | Не установлено | Не проверено в этом паспорте |
| G32 | Текущий входящий/исходящий трафик и скорость без speedtest | Действующее ТЗ; AT11 | Не установлено | Не проверено в этом паспорте |
| G33 | Подписка, остаток и X из Y; базовые 2 устройства, расширение после R1 | Действующее ТЗ; AT15, AT21 | Не установлено | Не проверено в этом паспорте |
| G34 | Список устройств, платформа, имя, текущее устройство | Действующее ТЗ; AT15 | Не установлено | Не проверено в этом паспорте |
| G35 | Тот же Telegram добавляет устройство; последний слот не удваивается | Действующее ТЗ; AT14, AT15, AT35, AT39 | Не установлено | Не проверено в этом паспорте |
| G36 | Удаление устройства освобождает слот и прекращает его доступ | Действующее ТЗ; AT15, AT20, AT35, AT41 | Не установлено | Не проверено в этом паспорте |
| G37 | Суммарный трафик всех устройств: сегодня/7/30 дней | Действующее ТЗ; AT25, AT36 | Не установлено | Не проверено в этом паспорте |
| G38 | Общий Backend для будущих платформ; сохранение legacy прав при переходе | Действующее ТЗ; AT15, AT25, AT28, AT39 | Не установлено | Не проверено в этом паспорте |
| G39 | Четыре напоминания: за 3, 2, 1 день и в день окончания; без дублей, сброс серии после продления | Действующее ТЗ; AT23 | Не установлено | Не проверено в этом паспорте |
| G40 | Expiry прекращает data-доступ и меняет состояние VPN; публичный каталог до доступа не даёт прав | Действующее ТЗ; AT02, AT20, AT21, AT40 | Не установлено | Не проверено в этом паспорте |
| G41 | Expiry/revoke без прямой утечки; отдельный час и ограниченное платёжное окно по действующему дополнению | Действующее ТЗ; AT13, AT18, AT20 | Не установлено | Не проверено в этом паспорте |
| G42 | Покупка 30 дней / 3 месяца / 6 месяцев с серверной суммой; R1 базовые 2 устройства | Действующее ТЗ; AT16, AT37 | Не установлено | Не проверено в этом паспорте |
| G43 | Только Platega в текущем выпуске; методы СБП/карта/криптовалюта проверяются в допустимой конфигурации | Действующее ТЗ; AT16 | Не установлено | Не проверено в этом паспорте |
| G44 | Существующая платёжная state-machine и статусы | Действующее ТЗ; AT16, AT17 | Не установлено | Не проверено в этом паспорте |
| G45 | Продление добавляет срок к остатку и обновляет managed доступ | Действующее ТЗ; AT17, AT19, AT41 | Не установлено | Не проверено в этом паспорте |
| G46 | Покупка кнопками рабочего бота в приложении, ссылка во внешнем браузере; ограниченный checkout и серверная проверка | Действующее ТЗ; AT18, AT40 | Не установлено | Не проверено в этом паспорте |
| G47 | После продления: новый срок, refresh, восстановление прежнего узла | Действующее ТЗ; AT19 | Не установлено | Не проверено в этом паспорте |
| G48 | Системная/светлая/тёмная тема | Действующее ТЗ; AT26 | Не установлено | Не проверено в этом паспорте |
| G49 | Автоподключение при запуске к последнему серверу | Действующее ТЗ; AT22 | Не установлено | Не проверено в этом паспорте |
| G50 | Always-on для VPN после перезагрузки | Действующее ТЗ; AT22 | Не установлено | Не проверено в этом паспорте |
| G51 | Предупреждение энергосбережения и переход к системной настройке | Действующее ТЗ; AT08, AT26 | Не установлено | Не проверено в этом паспорте |
| G52 | Автообновление каталога: off / дважды в день / ежедневно / еженедельно | Действующее ТЗ; AT27 | Не установлено | Не проверено в этом паспорте |
| G53 | Помощь, ссылки, политика, условия, версия, инструкции | Действующее ТЗ; AT21, AT26 | Не установлено | Не проверено в этом паспорте |
| G54 | Односторонние служебные сообщения, непрочитанные, красная точка | Действующее ТЗ; AT24 | Не установлено | Не проверено в этом паспорте |
| G55 | Плановая замена VPS через каталог и предварительное уведомление | Действующее ТЗ; AT07, AT24, AT28 | Не установлено | Не проверено в этом паспорте |
| G56 | Connect без исходного интернета не запускает VPN | Действующее ТЗ; AT01, AT05 | Не установлено | Не проверено в этом паспорте |
| G57 | Понятные причины, повтор/другой сервер/поддержка | Действующее ТЗ; AT21, AT29, AT30 | Не установлено | Не проверено в этом паспорте |
| G58 | Не добавлять лишнюю автоматику и лишние функции первой версии | Действующее ТЗ; AT32 | Не установлено | Не проверено в этом паспорте |
| G59 | Создание единого TERLIMO Backend из подходящего существующего кода | Действующее ТЗ; AT00, AT33, AT40, AT41 | Не установлено | Не проверено в этом паспорте |
| G60 | Доказуемая идентичность сборок и защищённая одноразовая миграция production | Действующее ТЗ; AT28, AT31, AT33, AT34 | Не установлено | Не проверено в этом паспорте |
| G61 | Единый центральный Backend, собственная БД и внутренняя бизнес-логика | Действующее ТЗ; AT42, AT43 | Не установлено | Не проверено в этом паспорте |
| G62 | Шлюз хранит только технические разрешения и техническую телеметрию | Действующее ТЗ; AT42, AT41 | Не установлено | Не проверено в этом паспорте |
| G63 | Android первым, расширяемый клиентский API без разработки desktop в R1 | Действующее ТЗ; AT42, AT49 | Не установлено | Не проверено в этом паспорте |
| G64 | Обязательный Telegram; вспомогательный бот с одной общей бизнес-логикой | Действующее ТЗ; AT14, AT39, AT49 | Не установлено | Не проверено в этом паспорте |
| G65 | 2 устройства в базовом плане; отложенное расширение без потери старых прав | Действующее ТЗ; AT15, AT50 | Не установлено | Не проверено в этом паспорте |
| G66 | Перенос аккаунтов, сроков, прав и истории trial по Telegram | Действующее ТЗ; AT44, AT45, AT48 | Не установлено | Не проверено в этом паспорте |
| G67 | Единственный писатель коммерческого состояния при cutover | Действующее ТЗ; AT46, AT47 | Не установлено | Не проверено в этом паспорте |
| G68 | Распределение исполнения и независимая приёмка по действующим указаниям владельца; не функция продукта | Действующее ТЗ; AT00, AT51 | Не установлено | Не проверено в этом паспорте |
| G69 | Backup/restore и технические точки отката до и после cutover | Действующее ТЗ; AT47, AT52 | Не установлено | Не проверено в этом паспорте |
| G70 | Учет pending/late платежей, возвратов и балансов при переносе | Действующее ТЗ; AT46, AT53 | Не установлено | Не проверено в этом паспорте |
| G71 | Окончательное отключение старого сервиса только по доказанным условиям | Действующее ТЗ; AT48, AT52 | Не установлено | Не проверено в этом паспорте |
| G72 | План переноса кода MiniShop/адаптера без лишней смены стека | Действующее ТЗ; AT43, AT54 | Не установлено | Не проверено в этом паспорте |
| G73 | Разделение TEST/production, build identity, секреты и release gates | Действующее ТЗ; AT28, AT33, AT52 | Не установлено | Не проверено в этом паспорте |
| G74 | Функциональные доказательства и запрет самоприёмки coding-агента | Действующее ТЗ; AT51, AT55 | Не установлено | Не проверено в этом паспорте |
| VK-BOOT | Встроенный служебный VK-путь для control/каталога без зависимости от direct HTTPS | До выпуска; 14_OWNER_VK_BOOTSTRAP_AND_RECOVERY.md | Не установлено | Не проверено в этом паспорте |
| VK-UPDATE | Обновление штатных VK-данных при успешном соединении | До выпуска; 14_OWNER_VK_BOOTSTRAP_AND_RECOVERY.md | Не установлено | Не проверено в этом паспорте |
| VK-RECOVERY | Аварийная собственная VK-ссылка без смены адресата и авторизации | Live-проверка зависит от ссылки владельца; 14_OWNER_VK_BOOTSTRAP_AND_RECOVERY.md | Не установлено | Не проверено в этом паспорте |
| CAT-BROWSE | Каталог при первом открытии до часа/trial/оплаты, без запуска часов и выдачи VPN | До выпуска; 19_OWNER_CATALOG_BEFORE_ACCESS.md | Не установлено | Не проверено в этом паспорте |
| PAY-H2H | Собственный экран СБП QR/ссылка через Platega H2H и служебный канал | Целевое улучшение; текущий порядок — дополнение22; 15_OWNER_SBP_PRIMARY_AND_CHECKOUT_RESERVE.md | Не установлено | Не проверено в этом паспорте |
| REF-CODE | Пригласить друга: код/ссылка, получение и ручной ввод в приложении, работа без сайта | Утверждённый сценарий; 16_OWNER_REFERRAL_INVITATION.md | Не установлено | Не проверено в этом паспорте |
| REF-LEDGER | Связь пригласившего/приглашённого, журнал и отсутствие повторных начислений | Реальные бонусы ждут выбора бизнес-параметров; 17_OWNER_ADMIN_REFERRALS_DESIGN.md | Не установлено | Не проверено в этом паспорте |
| ADMIN-USERS | Веб-панель: поиск/карточка пользователей, подписки, устройства и история | После дизайна, до внешних пользователей; 17_OWNER_ADMIN_REFERRALS_DESIGN.md | Не установлено | Не проверено в этом паспорте |
| ADMIN-RIGHTS | Выдача срока/бессрочного доступа, продление/приостановка/отзыв; подтверждение применения шлюзами | После дизайна, до выпуска; 17_OWNER_ADMIN_REFERRALS_DESIGN.md | Не установлено | Не проверено в этом паспорте |
| ADMIN-DEVICES | Удаление привязки и ключей с освобождением слота, сохранением срока и других устройств | После дизайна, до выпуска; 17_OWNER_ADMIN_REFERRALS_DESIGN.md | Не установлено | Не проверено в этом паспорте |
| ADMIN-PLANS | Тарифы: цена, срок, лимит устройств и доступность предложения | После дизайна, до выпуска; 17_OWNER_ADMIN_REFERRALS_DESIGN.md | Не установлено | Не проверено в этом паспорте |
| ADMIN-NODES | Панель состояния шлюзов, включение в каталог и управление штатными VK-данными | После дизайна, до выпуска; 17_OWNER_ADMIN_REFERRALS_DESIGN.md | Не установлено | Не проверено в этом паспорте |
| ADMIN-REF | Управление реферальными кампаниями/кодами/ссылками и журналом; параметры бонусов по решению владельца | Бизнес-параметры не утверждены; 17_OWNER_ADMIN_REFERRALS_DESIGN.md | Не установлено | Не проверено в этом паспорте |
| ADMIN-AUDIT | Разделение административных прав, журнал кто/когда/что/результат/причина | До выпуска; 17_OWNER_ADMIN_REFERRALS_DESIGN.md | Не установлено | Не проверено в этом паспорте |
| DESIGN | Макет каждого окна/состояния после функционала, согласование и реализация по макетам | Функционал → дизайн → панель; 21_OWNER_PANEL_AFTER_DESIGN.md | Не установлено | Не проверено в этом паспорте |
| CAPTCHA | Завершение CAPTCHA по официальному поведению, отмена/ошибки и корректный бюджет операции | До выпуска, локальные изменения; 23_OWNER_WDTT_V20_ADOPTION.md | Не установлено | Не проверено в этом паспорте |
| DIAGNOSTICS | Диагностика без секретов, различимые причины отказа, сохранение работающих частей | До выпуска; 23_OWNER_WDTT_V20_ADOPTION.md | Не установлено | Не проверено в этом паспорте |
| UPDATE-ROLLBACK | ID обновления, согласованная проверенная резервная копия, post-check и допустимый откат | Этап эксплуатации/выпуска; 23_OWNER_WDTT_V20_ADOPTION.md | Не установлено | Не проверено в этом паспорте |
| DIST01 | Самостоятельная установка и настройка центрального сервера без агента | После коммерческого запуска; 18_OWNER_STANDALONE_DISTRIBUTIONS.md | Не установлено | Не проверено в этом паспорте |
| DIST02 | Установка чистого шлюза, регистрация Backend, фактическое подключение клиента | После коммерческого запуска; 18_OWNER_STANDALONE_DISTRIBUTIONS.md | Не установлено | Не проверено в этом паспорте |
| DIST03 | Штатная установка и сценарии каждого выпускаемого клиента; отдельная готовность ОС | Android для первых пользователей; остальные дистрибутивы позже; 18_OWNER_STANDALONE_DISTRIBUTIONS.md | Не установлено | Не проверено в этом паспорте |
| DIST04 | Перезагрузка, совместимые обновления, backup/restore и допустимый откат | Упаковка позже; рабочие backup/restore до запуска; 18_OWNER_STANDALONE_DISTRIBUTIONS.md | Не установлено | Не проверено в этом паспорте |
| DIST05 | Чистая среда без агентов/ключей/сессий и скрытых ручных операций | После коммерческого запуска; 18_OWNER_STANDALONE_DISTRIBUTIONS.md | Не установлено | Не проверено в этом паспорте |
| POST-WDTT-01 | Адаптивный uplink2/pacing/framing/ACK | Отложено после первого production; 23_OWNER_WDTT_V20_ADOPTION.md | Не установлено | Не проверено в этом паспорте |
| POST-WDTT-02 | Выбор UDP/TCP/TLS/MASQUE и восстановление workers | Отложено после первого production; 23_OWNER_WDTT_V20_ADOPTION.md | Не установлено | Не проверено в этом паспорте |
| POST-WDTT-03 | Заимствования TURN allocation/native shutdown | Отложено после первого production; 23_OWNER_WDTT_V20_ADOPTION.md | Не установлено | Не проверено в этом паспорте |
| POST-WDTT-04 | Полный встроенный вход VK/генерация хешей | Отложено после первого production; 23_OWNER_WDTT_V20_ADOPTION.md | Не установлено | Не проверено в этом паспорте |


## TEST apply / один run / OFF — 2026-10-01

Эта секция уточняет текущий runtime статус исходного SOURCE ONLY паспорта выше; весь101-ID перечень и исторические уровни проверок сохранены без изменения. Source639c28a, canonical snapshot1db2a2dd PUSH_VERIFIED руководителем. Два reviewed runtime файла применены на TESTA после exact before SHA/idle; accepted16 offline tests повторно не запускались. Worker3543033→3965959, существующий launcher/env, scoped TERLIMO_MANAGEMENT_RPC_BOUNDARY=1, остальные diagnostics OFF. Import resolution/новый startup подтверждены; live Python heap не исследовался.

Единственный ordinary APK070aafdd cold auto-catalog: actual host T0 18:10:27.125670UTC, attempt915dd5a8-069c-4aec-b77b-274e0f033757 generation1. Client FAIL CATALOG_TIMEOUT, elapsed15011ms, ImportVerified без ACCEPT; последний AUTH xid1 WRITE_OK/READ_BEGIN без READ_OK. Прямой Laptop terminal idle18:11:47.624912UTC: native absent, TESTservice/VPN OFF. Серверный AUTH gen1/seq1: fwd_end18:10:44.797UTC SERVICE_UNAVAILABLE, dur10858ms, error write attempted18:10:44.819. Wire xid binding/server clock calibration недоступны; это временное сопоставление одной попытки, не доказательство точного request mapping.

Prestate того же grant: desired/applied119, lease expired, pending/processing0. После попытки новых операций этого grant нет; OPTIME после worker.log byteoffset204480 отсутствует. Management refresh/setup_send/response_rest/publish/finalize не измерены в этой попытке; это отсутствие выполненного apply, не zero latency. Прямое runtime подтверждение callback не получено. Предыдущие9422/2079ms относятся к другому прогону, который достиг apply; сравнение нынешнего AUTH failure с ними не доказывает regression или её отсутствие. Product end-to-end/catalog публикация — FAIL, production правка не выполнялась.

После прямого terminal idle и readonly pending/processing0 выполнен только scoped OFF штатным idle worker restart. Worker3965959→3976236, OFF UTC18:17:41.091561, exact исходные env bytes восстановлены, scoped flag absent, reviewed source сохранены default-OFF. Все соседние PID/units/sourceSHA сохранены, startup/idle/errors0/sink проверены. Повторного phone run, synthetic sync/refresh/DBwrite, смены таймаутов нет. Runtime rollback bytes/env доступны в private s5-management-boundary-apply-20261001/backup; секретный env не публикуется. Evidence: private s5-management-boundary-one-run-20261001/{prestate,poststate,linked-timeline,OFF-readiness}.json.
