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
