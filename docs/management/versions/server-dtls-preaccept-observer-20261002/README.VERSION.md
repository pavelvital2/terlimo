# Шлюз — ordinary Unix terminal logging: TEST READY

## Точная версия и текущий уровень проверки

- Source `d847a41168c54f413e0a961e1ce198db116650b9`, isolated ordinary source base `b3185dac8b0c44652ff9aeb961d4af492b53444b`; canonical publish root `da91c171f3c43739f3ef095b61eb47ff6d26d3d6` branch diag/auth-terminal-error-20261001. Cumulative monorepo gateway tree не использован для build.
- Artifact `wdtt-server-auth-terminal-d847a41-go1.26.5`, SHA256 `bebec6980ed3a41242cd7ac0455839bf4dd2c2297c8875df00ae781f0b8bb1ff`, 13980291bytes, ELF64/LinuxAMD64/CGO0/GOAMD64v1/Go1.26.5. Actual running `/proc/4000938/exe` совпадает с artifactSHA. Source identity и artifact identity разделены.
- Matching toolchain получен штатным Go механизмом в isolated cache, обычные TLS/sumdb проверки включены. System Go не менялся. Dependency buildinfo совпал сf7, go.mod/go.sum exactunchanged. Ровно одна normal build; matching compiler targeted14+2PASS, полныйsuite не повторялся. Recipe/buildinfo/teststdout/provenance сохранены рядом сartifact.
- TESTA apply/readiness UTC `2026-10-01T18:57:28.477424+00:00`: oldnode3935653→newnode4000938, existingunit/launcher/path сохранены; candidate оставлен готовым без второго restore/restart. Existing filename59bf41c не идентификатор source версии.
- API/relay/worker/evidence/mgmt/PG PID без изменений; env/diagflags/timeout/network/DB не менялись. Netns `net:[4026532254]`, nodeUDP57500/56002 и relayUnix endpoint `/run/terlimo-test-a/service-relay.sock` verified, startuperrors0. SyntheticAUTH/sync/phone не выполнялись.
- Уровень: source tests/build/TEST install/readiness PASS; product AUTH/catalog outcome и первичная причина прежнего10858ms SERVICE_UNAVAILABLE этим этапом не проверены. Root далее назначает один обычный APK070aafdd run.

## Полная функциональность и совместимость этой delta

Только ordinary error-only `[SVCUNIX] terminal` в пяти SERVICE_UNAVAILABLE exits PRECTX/SOCKET_PATH/DIAL/ENCODE/READSLICE: fixed error class/context snapshot/total+IOelapsed/технический request_id. Success/parsedremoteerror/SERVICE_BAD_RESPONSE не добавляют log. Snapshot ctx.Err непосредственно на error branch до defers/forced watcher unblock/cancelRelay. Нет process-start window120s/512 budget или новых probes/observer; остальные frame/API/relay diagnosticsOFF. Payload/headers/token/path/rawerror не логируются. Protocol/returns/watcher/cancellation/15sIO+3sdial+5serrorwrite unchanged. Маркер покажет состояние terminal error, не обещает backend-latency repair и не доказывает первопричину совпавших EOF/cancel.14 baseline paritycases+snapshot-before-postcancel/no-startwindow tests PASS.

## Source/build provenance и пределы

Ordinaryf7SHA f7f17e84d7851bc556820709e3390f366fc0a03ade1756a7cfb90d9af86fb0e1 связан сb3185da через accepted build/apply receipts+clean source, не embedded historicalVCS/byte-rebuild. MatchingGo1.26.5 устраняет прежнюю compiler patchdelta1.26.8; неизвестныеliteraloriginalbuildflags не восстановлены выдумкой. Future/current recipe exact вbuild-manifest, actual buildinfo отдельно. Broad90da diagnosticartifact не использован/не пересобран.

## Exact rollback

Private `/home/pavel/step036-receipts/private/s5-auth-terminal-build-test-20261001/backup/node.rollback.f7` SHA f7f17e84d7851bc556820709e3390f366fc0a03ade1756a7cfb90d9af86fb0e1; exact unit/launcher/env snapshots рядом, env содержит секреты и не передаётся. При отдельно необходимом idle node-only rollback: штатный stop того же terlimo-test-a-node.service, install backup root0755 во временный sibling и atomicmv в текущий executablepath, start node, проверитьprocSHA/netns/UDP/endpoints/neighbors/env. При startupfailure такойrollback был предусмотрен; он не понадобился. DB/neighbors/flags не откатывать. Сейчас healthy candidate оставлен для root-assigned run.

## Исторические уровни полного101-ID паспорта

Ниже полный gateway requirementsection сохранён byteexact; исторические уровни не повышаются из-за readiness. Новая loggingdelta не является product/catalogPASS.

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


## Ordinary APK070aafdd observation — 2026-10-01 19:05 UTC

Единственный command-layer launch actual19:05:11.807079UTC вернул cached UI; Laptop подтвердил NO_NEW_ATTEMPT, без ARM/attempt/generation/AUTH. Старый CATALOG_TIMEOUT на экране не является текущей terminal ошибкой. Direct idle19:06:53.289493UTC: native absent, TESTservice/VPN OFF, activity not resumed. Новый SVCUNIX logger не был exercised; продуктовый PASS/FAIL и причина AUTH задержки этим запуском не установлены.

Server final19:08:21.929529UTC: node4000938 SHA bebec6980ed3a41242cd7ac0455839bf4dd2c2297c8875df00ae781f0b8bb1ff active, neighbors3935646/3935648/3976236 unchanged, NRestarts0. Same grant desired/applied119, leaseexpired, новых операций0, global pending/processing0. SVCTIME/SVCUNIX после readinessoffset12786997 отсутствуют. Управляющие splitmarkers OFF, отсутствующие durations не равны нулю. Snapshot19:06:23 был поздним, не prelaunch. Healthy candidate оставлен; никаких build/apply/restart/flag/SQLwrite/phone actions инженером. Исторические уровни всех101-ID сохранены.


## Corrected coldprocess ordinary run — 2026-10-01 19:14 UTC

После отдельно разрешённого Laptop завершения idle TESTprocess с сохранением данных один coldlaunch APK070aafdd actual19:14:06.327083UTC создал appPID21779/attempt8bfe8c22-6801-4a1e-9b45-73f5263e08e0. DeviceFAIL CATALOG_TIMEOUT15012ms, terminal_phase ImportVerified, verified_acceptedfalse, AUTH WRITE_OK/READ_BEGIN без READ_OK. Directidle19:14:45.669428UTC native/service/VPNOFF.

Новый serverlogger exercised: AUTHrecv19:14:17.807UTC, fwd_begin17.808, terminal READSLICE/CLOSED/ctxCANCELED elapsed/io6051ms requestid51d5d366093000ba6387d1e45bc667d0, fwd_end19:14:23.859 SERVICE_UNAVAILABLE/status0/relay_errempty; errorwriteattempted не доказательство clientreceipt. ExistingAPI challenge20019:14:24.531UTC, temporalonly безexactwirejoin. Device ARM→AUTHREAD9070ms, READ→TIMEOUT5941ms. Device-host[-877,-752]ms, serverclockнеcalibrated; timeouthostbracket19:14:23.831–23.956 включает serverclosure23.859, порядок не доказан. ctxCANCELED не объясняет precedingdelay; dial/encode дошли до чтения, причина upstreamзадержки остаётсянеустановленной.

Final19:15:42.970858UTC node4000938 bebec698SHA иneighbors3935646/3935648/3976236 active/NRestarts0 unchanged; samegrantdesired/applied119 expired,newops0/globalpending0. Relaylogdelta0, workerOPTIME0, managedsplitflagsOFF durationsUNMEASURED. Healthycandidateоставлен безbuild/apply/restart/flags/SQLwrites/phone/production. Все101-ID иисторическиеуровни сохранены; этотrun неPASS.


## API exact span preparation — 2026-10-01 19:32 UTC

Root whitelist-20261001-auth-api-exact-span-prepare-v1. Only existing API envflag TERLIMO_AUTH_API_PHASE_PROBE=1 enabled; API3935646→4022978, readiness20019:32:25.383592UTC. Source/artifact unchanged; node4000938/bebec698,relay3935648,worker3976236 unchanged; relaytraceOFF. Exact private originalenv snapshot/hash/modeuidgid preserved. Flag has no startup expiry, root assigns original T0 separately, no phone attempt by engineer. APIrestart can reset transientstate, future fastrequest notcausefixproof. Device status NOT_YET_RUN, historical NO_NEW_ATTEMPT/currentcoldFAIL retained; full101-IDlevels unchanged. After original terminalidle retain evidence then restore exactenv OFF with API-onlyrestart; restore not yet executed.


## Joint client dial/API measurement — original run, restored OFF

APK8827c0bad83e0a8e01c4bce67e33112fd82fcc6d74e1fd261dbaf6e6cd079331 source74db78c0/codeb65be933; scheduled20:05:08.000018UTC, actualT0 not yet supplied in directidle report. Newattempt6af42be6-d3ca-466f-8c6b-5fb373ed5f30/appPID24638/childgen1. DeviceFAIL CATALOG_TIMEOUT15012ms/terminalphaseImportVerified/noACCEPT; dial8097ms, DTLS_END8002ms TIMEOUT after Allocate92ms/firstWrite51ms. FINISH OTHER is wrappedouterclass, not contradiction ofDTLSTIMEOUT. NoAUTHWRITE/READ/ME; serviceAPIspan not exercised, noAPI/nodeSVCmarkers. No claim of purePermission/retransmissioncount, no serverAUTH fix/SQLcause established.

Directidle20:06:10.720170UTC native/service/VPNOFF/activitynotresumed. Evidence saved beforeOFF. Exactoriginalenv restored20:07:28.531523UTC SHA0c4bdf2f07c0e0a9190c120a2016133552079f271bc71da5ecb4f915546e5010/mode0600/uidgid1000. API4022978→4044777 actualflagABSENT/OFF/readiness200, sourceunchanged. Node4000938/bebec698,relay3935648,worker3976236 unchanged activeNRestarts0, final20:07:47.615825UTC samegrant119expired/newops0/globalpending0. No node/relay/worker restart, phone action, SQLwrite command, timeoutchange, production orautomaticretry. API preparationrestartconfound retained; noAPI latency conclusion fromthisrun. Full101-IDhistoricallevels preserved, source/artifact/install statuses separated fromcurrentdeviceFAIL.


### Final client packet linkage (no new run/runtime action)

Laptop final laptop-20261001-mobile-dial-final-terlimo-v1: actualhostT0 20:05:08.001651UTC (+1.633ms), originalARM1790885109739→TIMEOUT1790885124751. Call1 measurement complete, timestamps captured before bufferedemit;17diallines arrived batch1790885119.015..019, use nativeelapsed not reception cadence. Existing nativecycle retried after994ms, secondDIAL_BEGIN1790885120.018 with noFINISH beforeteardown: incompletecapture, not0ms and not operatorretry/secondlauncher. FirstWrite51ms includesPermission+otherwork, notpurePermission; retransmissioncountunknown. These details do not change DEVICEFAIL/preAUTH/unexercisedAPIspan or alreadyverified exactenvOFF. Full101-IDhistoricallevels remain preserved.


## Exact DTLS RX endpoint correlation — 2026-10-01 20:54 UTC

One coldprocess APK6b265c3bce25e989f4db41ebbe95f58be3cfaa27bf3154dded1ad719402608a0/source0e735935/code022e6535 actual20:54:27.537151UTC, attempt5c7e7c64-46e8-4d0f-beaf-7de6ec469097/PID26588. Singlecompletecall1/noauto retry: FINISH1427msOK/DTLS1339msOK/RX4handoff4/nopeer-unwrappipeerrors/TX4errors0. Call1allocation canonicalSHAa794cc31fee4d60a38a02762bcf232cdc5951e8e49104bfc2bc64b866abec47a EXACTMATCHservergen3, noIPv4mappednormalization. ct20epoch0/ct22epoch1headers alone notdecryptverifyproof; separateDTLS_ENDOK confirms. Counters sealedatdial, notAUTHcoverage.

Servergen3accept20:54:31.503429873→OK32.612891069; AUTHseq1fwd10645ms/HTTP200/writeend43.359; clientchallengeREAD10759ms/decoded13240cumulative,13231notREADduration. Subsequent sessionREADnoresponse untilcatalogTIMEOUT15011ms/noACCEPT, serverseq2READSLICE/CLOSED/ctxCANCELED1690ms codeSERVICE_UNAVAILABLE; APIchallengeaccess20042.900/sessionaccess20046.738 temporalonly, APIphaseOFF/internalwaitUNMEASURED. No concretefix/proofhistoricalgen2finalflightloss fromthissuccessfulmeasurement.

Directidle20:55:44.330790 native/serviceVPNOFF. Finalhealth unchangednode4000938bebec698/API4044777probeOFF/relay3935648/worker3976236 activeNRestarts0. Hostserver/observerboundedclock before[-0.188,+1.185]ms sharedtime namespace; clientdevice-observer[-998,-871]ms, no perpacketlatency/driftclaim. No runtime/flags/restart/DBwrite/phone/production modifications. Full101-ID/order/historicallevels retained, source/artifact/install andDTLSmeasurementPASS distinct fromPRODUCTCATALOGFAIL.


## Auth forward phases ONE v2 — final 2026-10-02

ActualT0 2026-10-01T21:18:53.014041UTC (+1.287ms); installedAPK6b265c3b/source0e735935/code022e6535 reused withoutbuild/install/testrepeat. Onecoldlauncher/app27660/attempt0d9d0616-c6be-4ee2-9834-f87cb2f44334/clientgen1/call1. Initialendpointhash03924f11... exactmatches servergen1; latergen2sameendpoint notsecondoperatorrunproof. DTLS4251/FINISH4405msOK, RX3/handoff3/TX6/noerrors; counterseal doesnotcoverAUTH/background. ChallengeREAD357ms/session304ms, initialME1171/GW1280ms. ServerAUTHseq1 264ms,seq2 136ms HTTP200; firstrelay261ms callback-drain, connect23.907 inclDNS7.783, headercallback-response221.622, APIouter216.651/innerchallenge197.467 inclrate176.767/insert18.498. Distinctouter/innerRIDs, orderedtemporaljoin; callbacknotflush. Existingprobe sessioninternalspan absent, notzero. Historical10645ms slowpath not reproduced and restartpool/generationconfound prevents causalfixclaim.

CatalogDISARMrights_none7900ms/noCATALOG_TIMEOUT/noACCEPT; visibledevice-node card notfullaccess/full-list/productPASS. Cleanupnative407.091s, initial60scollector cutoff, sameattemptretained103live+7tail lines; no continuousgapclaim. Servergen1 serviceidle10s closes21:19:16.161, sameendpointgen2accept16.286 andhandshakefail46.287; no additionalclientoperatorrun inferred. Originalidle21:27:04.283888 native/serviceVPNOFF. ExactbothenvOFF restored21:28:14.351551, API4096670/relay4096672/node4096675 samebebec698 SHA/config/netns/ports; worker3976236/PG1031/evidence2537357/mgmt2537359 unchanged, readyHTTP200. Full101-ID/order/historicallevels above retained; no new levels promoted.


## Read-only old/current APK pair — 2026-10-02 final

ONE old6f2d7652/source8b2f4ba actual21:57:57.696389UTC and ONE current6b265c3b/source0e735935/code022e6535 actual22:01:06.364295, sameuser0/identity/marker/encryptedstate byteidentical/no restore, coldprocess/noRefreshConnect. OldDIAL7447/current7430hostms; old splitdial unmeasured, currentDTLS7293/FINISH7423native. OldAUTHread noreply6025ms untilCATALOG_TIMEOUT15011/noME-GW-ACCEPT; nodegen1AUTH4905msREADSLICE/CLOSED/ctxCANCELED/io4892, notcauseproof. Currentcall1endpoint exacthash7c58b920... matches initialservergen2; AUTH1226/2797ms200 vsclient1357native/3064, ME112/GW31server vs201/144client. Connectionreused subsequentrequests doesnotmean authenticatedsessionreuse: one newDBsession incurrent. CurrentDISARMrights_none13347/noACCEPT precedesGWREADOK136ms; visiblecard notfreshcatalog/accessPASS.

Readonly snapshots21:54:34/21:59:09/22:01:44/final22:04:11 preserve node4096675bebec698/config/unit/netns/API4096670relay4096672worker3976236PG1031evidence2537357mgmt2537359, flagsOFF/ready200. Sameoriginalfixture hashes/bindinggen1/grantdesiredapplied119/lease116expired/subjectrev13catalog310/entitlementsinactive/outboxcounts; sessions186→186→187→187, latest22:01:21.286284. No source/runtime/DB/grant/reset changes. Sequentialbackend/transportpoolcache warmth unmeasured, state not aligned; differentnaturalsessionoutcome notcodecauseproof. Oldcontinuouscapture31rows; current110initial+9cleanup=119retained, main interruptedautomationerror/cleanup70.845s/continuousprimaryfalse. Originalidleold21:59:15/current22:03:00, currentAPKretained/OFF. Latercurrentservergen3sameendpointhandshakefail notadditionaloperatorrun. Pair doesnotconfirm current-slowerregression, no historicalbaseline28/29 rerun, no sourcefix warrantedbyisolatedcause here. Full101rows/order/levels above unchanged, no newproductPASS.


## UDP preaccept observer source/offline only — 2026-10-02

Base d847a411, isolatedsource 440579f235a5fd605c396c3bae41c22e2104a25f. Only udp_listener.go + boundedobserver +5targetedoffline/race tests. DefaultOFF;8peer/64events/localconnseq/monoRX→decision/write/Acceptdequeue; start/interim/END summaries andoverflow/unknown explicit. No duplicatepayload/unwrap, noadmission/retry/deadline change. Artifact NOT_BUILT/NOT_DEPLOYED, existingbebec698 untouched; device NOT_RUN/productNOT_VERIFIED. Full101rows/order/historicallevels unchanged; source/offlinePASS doesnotpromote productPASS. Capturelifecycle described separately, noOBS_READY gate/kernelabsenceclaim.
