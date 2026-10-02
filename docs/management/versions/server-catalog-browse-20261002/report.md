# Каталог: серверная совместимость, этапы и минимальный кандидат

Инженер Белых списков; reply_to whitelist-20261002-catalog-stages-server-v1.
Возобновление владельца принято. Старые queued PREAPI задачи/READY остаются отозванными, не replay. Цель ближайшего результата09:00UTC не подменяет приёмку телефона/101функций.

## Решение для root review

Подготовлен ограниченный candidate: GET /api/mobile/v1/gateways?view=browse возвращает уже существующий browse DTO независимо от наличия data subject, но строго ПОСЛЕ общей session authorization. Это разделяет получение списка и применение grants для отдельного выбранного VPN. По умолчанию GET /gateways сохраняет прежний credential/admission контракт. Runtime, данные, права, deadlines, flags и сервисы не менялись.

Проблема подтверждается источниками: текущий mobile_catalog.py:843–875 выбирает _browse_body только если _data_subject=None. При активном праве он идёт в _catalog_body (:930–960), где pending grant даёт409 ACCESS_SYNC_PENDING; при failed grant503. Поэтому список для имеющего право пользователя может ждать worker/RPC/apply, хотя просмотр не должен активировать или ждать VPN. В отсутствие права browse уже есть (:878–914), поэтому новый endpoint/DTO/схема/миграция не нужны. Предложенная явная projection нужна для одинакового Refresh всех пользователей, а не для обхода серверного допуска.

Diff catalog-browse-view.diff добавляет10строк только в CatalogService.get_gateways: единственный view=browse, unknown/duplicate view→BAD_MESSAGE; после authenticate_session прямой вызов существующей _browse_body. Default credential catalog, _require_data_subject, /access/sync requiredscope, idempotency/outbox и gateway admission не меняются. Эту проекцию нельзя использовать как connection descriptor: только gateway_id/name/region/country_code, без access/transport/revision/password. Query уже поддерживается существующим service wire (раздельные path/query), node/relay сохраняют её; canonical path остаётся прежним allowlisted GET /gateways. В wire request передать path=/api/mobile/v1/gateways, query=view=browse, не вставлять '?' в path.

Offline результат:6проверок PASS без sockets/БД/служб; active pending grant теперь возвращает список3шлюзов без credentials, default всё ещё409, unknown/duplicate view отклоняются, actual shared authorizer блокирует revoked session и installation. Контрольная baseline для explicit browse FAIL именно ACCESS_SYNC_PENDING; candidate affected повтор1PASS. Первый baseline fixture потребовал stub существующей revision metadata ветки; исправлен только fixture, затем затронутый случай повторён. Проверки не являются DB/phone/end-to-end PASS.

## Текущая рабочая основа и границы воспроизводимости

Использовать текущий accepted deployed backend с primitive AUTH4SHA и node bebec6980ed3a41242cd7ac0455839bf4dd2c2297c8875df00ae781f0b8bb1ff; candidate меняет только mobile_catalog.py поверх него. Exact source/runtime projection и hashes в source-runtime.safe.json. Disk HEAD0cebcfc1c2a41a30ffa3bc822a545526cd8ac9c5 НЕ равен доказательству всех loaded bytes: deployed AUTH4SHA соответствует canonical7344a04a/donord90a0b7; accepted live tree содержит installed changes. Главный ориентир — file hashes/receipts, не Git date.

Прежний подтверждённый server GET200 Sept29 e4970cec: повторная session, уже applied gen3/lease3 до15:31:07Z, без freshAUTH/sync наT0. Исторические source anchors5d391c6 и reported node2619c86 пригодны для узкого сравнения; exact loaded node binary/API modules на старом T0, полный bootstrap/client/native/config matrix, transport/cache/cold state и paired VPN PASS не восстановлены. Старый subject отличается от ночного. Предыдущая apply уже занимала8.406с, поэтому нельзя объявлять быстрый GET доказательством быстрого cold enrollment/apply. Не откатывать БД/права/платежи или смешивать lease/revision разных subjects ради копии baseline.

Принятый PREAPI_EXISTING_REGRESSION_ACCEPTED уже сравнил narrow path: API/evidence listener byte-identical old5d, core Unix/relay протокол без новой waitqueue; current relay6f6ca308 один и тот же в последних попытках, nodebinary тот же. Доказанного источникового дефекта7s нет. Combined NO_CLIENT_RUN не является новым замером. Нынешний candidate устраняет конкретную зависимость browse списка от data admission; он не объявляется устранением всех transport/CPU/SQL задержек.

## Фактические серверные ceilings и scopes

Read-only projection текущих /proc env: API141668, relay141671,node141673; оба diagnostic flags ABSENT. ONBOARDING_SERVICE_TIMEOUT_SECONDS и DB_COMMAND_TIMEOUT_SECONDS отсутствуют, применяются defaults15. Это ограниченная проверка числовых настроек, не новая health/network/phone попытка.

| Граница | Значение | Источник / смысл |
|---|---:|---|
| Node Unix connect |3с|exact gateway client_test_service.go:115,441–460; отдельный DialContext |
| Node Unix IO |15с|:116,471–495; Encode+полный newline reply, один общий IO deadline |
| Node ответ клиенту |5с|:117,416; отдельный WriteDeadline, после upstream |
| Node ожидание следующего полного frame |10с|:393; idle/fragment read, не все50с устройства |
| Node frames/service connection |8|:111; AUTHchallenge+session+ME+GW=4, не бесконечная session |
| Relay callback frame read |15с|service_relay.py:587–611 |
| Relay forward wait / HTTPS ClientTimeout |15с|:558–565,617–620; параллельный watcher cancellation |
| Outer mTLS API handler |15с|:354; contextresolve+inner replay вместе |
| Inner loopback replay |15с|:270–278, вложен в outer budget |
| PG command/connect |15с|config.py:204,db.py:95–101; percommand, не whole multiquery transaction |
| /me snapshot retry |до3attempts|mobile_account.py:619–641, в shared outer envelope |
| /gateways snapshot retry |до3attempts|mobile_catalog.py:843–875, в shared outer envelope |
| AUTH challenge validity |300с|config.py:209; nonce validity, НЕ request budget |
| Session validity |86400с|config.py:210; revoked/expired/binding generation всё равно проверяются |
| Browse cache validity |600с|config.py:222; cache TTL, НЕ grant/right/operation timeout |

Вложенные15с relay/outer/replay/PG НЕ складывать в60с: outer children работают внутри родительского15с, relay/Unix deadlines могут оборвать раньше завершения вложенного SQL/replay. Верхняя техническая оценка одного node обмена от вызова до конца response write — до3+15+5=23с плюс scheduler/log overhead. Это НЕ гарантированный срок node fwd_begin (синхронный log перед Unix тоже входит в span); не применять её как измеренную норму.

API SessionEnsure отдельной операции не имеет: это клиентский координатор technical session, обычно challenge→localPoP→session, либо reuse действительного bearer. Server auth_api.py:423+ не выдаёт data право самим созданием session. Допустимые unlinked scopes :78–86: enrollment/session:read/session:write/management-only. Не требовать access:sync для browse до часа/Telegram: сервер справедливо отклонит недоступный scope (:979–1010). При переходе к отдельному data Connect использовать разрешённые scopes/действующее право и credential path, а не постоянный management-only session для VPN. /me возвращает account/entitlement/registration/data_access=none как полученное состояние, не сетевую ошибку (mobile_account.py:371+,198–224). /gateways browse не имеет двухузлового cap: registered rows отсортированы, список не обрезан.

## Предлагаемый конечный профиль клиентских этапов

Это конкретный стартовый TEST кандидат для согласования root/Laptop по малой принятой выборке, не статистическая гарантия и не уже установленная настройка.

| Этап | Подтверждённые длительности, разные принятые попытки | Предлагаемый предел |
|---|---|---:|
| Соединение |transport params~1.1с; DTLS3.379с или7.343с, отдельный firstRX7.168с|12с total stage; существующий DTLS8с child не автоматически расширяется |
| Устройство/SessionEnsure |challenge node7.396с в primitive (clientREAD_OK нет), ранее2.160с; session2.201с; inner SQL/crypto различались|50с: не более2AUTH HTTP exchange×24с +2с local allowance |
| Статус /me |наблюдённые service forward~0.242с и0.061с|10с, один request, stage context cancellation |
| Каталог browse |прежний GET~0.009с, но исторический GET был credential/reused; новый explicitbrowse networkduration ещё UNKNOWN|10с, один request; никаких apply/sync/poll в Refresh |
| Render/host finish |не измерен отдельно|резерв3с, actual end сразу после validated display |
| Whole Refresh |последний общий15.011с оборвал нормальную последовательность доREAD_OK|85с=12+50+10+10+3; monotonic hard parent |

24с AUTH child согласуется с finite node3+15+5 envelope и оставляет1с host/network margin; это запас конфигурации, не обещание23с нормальной задержки. Server HTTP caps15с остаются — мы не растягиваем внутренние SQL/relay и не делаем новый build node. Статус/каталог10с выбран с большим запасом к имеющимся коротким GET, сознательно строже server15с; если ответ не уложится, это конечная ошибка соответствующего этапа, не 'прав нет'. Новая phone проверка должна подтвердить этот профиль, особенно cold/transport, и может потребовать конкретной поправки бюджета.

Laptop должен убрать старый whole15с parent из catalog coordinator/SessionEnsure/native-host-RPC, назначать deadline каждого этапа от его собственного начала и передавать min(stage_remaining,whole_remaining,child_budget) во ВСЕ обёртки. Старые скрытые native/RPC10–15с ceilings нельзя оставить для AUTH24/Session50; exact current Android/native bindings доступны клиентскому writer, локальный server checkout не содержит SessionEnsure entrypoint. Go go_client/session.go донор показывает WRAPDTLS8с/default20с, но это не доказательство fingerprint текущего Android native. Проверить actual8с child в принятом APK, не подменять донор exactinstalledbuild. Connection12с не исправляет handshake, который сам конечен8с.

Reuse живой technicalsession без искусственного AUTH; /me и browse новые реальные этапы. После rights_none всё равно GETbrowse и display; последующий Connect отдельно. Выбор browse gateway — preference, не ACL/token/activation. /access/sync и worker managementRPC10с НЕ входят в обновление списка. Более8HTTP frames открыть новую boundedservice connection; не считать86400с bearer TTL обещанием бессрочного DTLS socket. Не вставлять UI задержки между RPC: node frame idle10с. Stale-result fence/cancel закрывают текущий operation, поздний ответ не меняет UI/не запускаетVPN. Один Refresh не означает пожизненный лимит: повтор после явной ошибки — новый operation, без hidden parallel attempts/автоциклов.

## Ближайшие действия / rollback

1. Root согласует explicit query/существующий DTO и profile85с с Laptop; клиент реализует actual stages и browse parsing для всех account states. Patch допускает3+registered gateways, не требует grant/transport в browse.
2. После review принять exact mobile_catalog.py candidate SHA из manifest через штатного publisher/TEST apply отдельным поручением. Backup текущего module/hash, сохранить env/flagsOFF, необходимые API reload по будущему scope. Откат — восстановить только предыдущий mobile_catalog.py и API reload; данные/схему/node/relay не менять. Здесь не выполнялось.
3. Один подготовленный пользовательский сценарий: manualtap→stage completion→реальный список до права, cancel/error/late response конечны; действующее право позволяет отдельный Connect/Disconnect. Сохранять версии/фактические длительности. Не повторять oldAPK-currentBackend A/B/закрытый PREAPI lifecycle и не создавать новое instrumentation окно.

Ограничения: нет новой телефонной проверки/видимого списка или VPN PASS; нет exact старой working matrix; причина старого7s/startup170.5s неизвестна. Profile85с не исправление всех задержек и не готовность101. Diff/offline verification и минимальный применимый план готовы. Runtime остался OFF, production/payments/access/data не затронуты.
