# DTLS TX budget/seal correction — source/offline review, 02.10.2026

Текущий cumulative candidate относительно code022e/provenance0e:32datagrams in-memory; output<=64TXlines (TXBOUND+всеtimingrows+ранниеClientHello/прочиеheaders поостатку). omitted_datagrams и omitted_headers различаютпотериdetail/output; nofullmetadatapromise. Androidmirror принимает отдельные64TX/window256/child, прежние76dial/304 иprotectedstreams/terminalslots сохранены; combinedworstcase140nativegeneratedlines принятwholeв одномwindow. Sealed/full eligibility проверяетсядоparser подобщимseal lock; fullнепарситheaders, sealedobserverнеизменяется. Targeted8Go-race/4pureJVM и parser-poisonineligible checksPASS. Никакого productPASS/build/install/run/push; timeouts/backoffнеизменены. InstalledAPK6b/finalphoneOFF прежние. Report `/home/pavel/step036-receipts/dtls-tx-budget-seal-correction-20261002/REPORT.md`. Все101ID/ихстроки ниже неизменены.

## История предыдущего source review (лимит621/76 ниже отменён текущей коррекцией)

# DTLS TX boundary — source/offline review candidate, 02.10.2026

Base code022e6535325cd44fe4dcd8eb8b9aab50f7d2d500 / provenance0e735935; installedAPK6b не менялся. Добавлен bounded TX observer: pipe_us/begin_us/end_us/index/n/result, record headers и plaintext epoch0 handshake type/message_seq/fragment fields. Encrypted epoch1 UNKNOWN, datagram не flight. Буфер первых32datagrams, по8records/8handshakeheaders, explicit truncated/limited/invalid/late/UNKNOWN. Возвращаемые I/O значения/ошибки и таймауты не менялись. Source patch ждёт review/canonical publisher; build/install/run/push не выполнялись.

Targeted offline Go-race и pureJVM checks PASS; это не productPASS. Hostallowlist расширен точными safe fields; бюджет76/window304/child прежний. При большом batch возможна неполная hostcapture (нет FINISH); полная передача всех621maxlines не обещается. Report `/home/pavel/step036-receipts/dtls-tx-boundary-source-20261002/REPORT.md`. Все101ID/их статусы ниже сохранены; accepted publicationfix и capture38c3f4d не затронуты.

## Предыдущий подтверждённый срез

# Текущее состояние после ONE old/current A/B, 02.10.2026

Установлен и SHAreadback подтверждён currentAPK `6b265c3bce25e989f4db41ebbe95f58be3cfaa27bf3154dded1ad719402608a0`, source0e735935/code022e6535. Финал `2026-10-01T22:03:00.061132Z`: native/service/VPN OFF, activity не resumed, cachedPID31301; UI закрыта. Данные/identity/user0 сохранены, no clear/uninstall/restore. Дваprocess-coldlauncher старого6f2d7652 иcurrent, безRefresh/Connect/build/sourcefix.

A/B: oldVK1326ms/fullDIAL7447ms/AUTHreplyдоdeadlineнеполучен/CATALOG_TIMEOUT15011ms/emptyUI. CurrentVK1058ms/fullDIAL7430ms (nativeDTLS7293ms)/AUTHchallenge1357ms/sessionPOST3064ms/ME201ms/GW144ms; DISARMrights_none13347ms за136ms доGWREAD_OK, noACCEPT. Видимая1карточка/nochosen/VPNOFF — частичноеUIсвидетельство, неfreshcatalog/access/full-listPASS. Паранепоказалаcurrentмедленнеестарого,причинанеизолирована. Beforeобоихlauncher encryptedstate побайтноодинаковый; server/sessionstate послеold не объявляетсяодинаковым безengineerfinalcorrelation.

Oldштатноостановилсяcatalogdeadline. Currentownnotificationcancelсработалпослеошибкиautomationвыбораheader; задержка70.845sпослеDISARM, неproducttimeout. Initialcollectorпрервался, всеего110rows и9cleanupstreamrows найденывexistingretainedbuffer; continuouscaptureнеобъявляется. Report `/home/pavel/step036-receipts/old-current-ab-20261002/REPORT.md`, result/provenance/safe traces/UI. Никакихтретьихзапусков/широкойdiagnostic/sourcefix, принятоетестирование не повторялось.

101ID ниже сохранены, ихисторические статусы не замененынаproductPASS. Предыдущиешапки/этапы ниже описываютсвойисторическийсрез, не последнееinstalled/idleсостояние.

## Сохранённая история

# Текущий клиент — состояние после auth-forward-v2 / offline regression review

## Актуальная версия и приёмка на 02.10.2026

Installed APK `6b265c3bce25e989f4db41ebbe95f58be3cfaa27bf3154dded1ad719402608a0`; source HEAD `0e735935bbb46480fede8d7e4be1e77f2b4ab438`, code `022e6535325cd44fe4dcd8eb8b9aab50f7d2d500`. Native packaged `d0b37aaa0d44eee0fafa9cb1a093f5c1ff0fed352a3bdce6f81e1b48db0ad96c`, signer `42ab6d950c15742eaadcf538947f65b3e0bd3e7e1693d67afa969eb00e5d348b`. Native-first build, install-r/readback и один cold auth-forward-v2 запуск завершены; это уже не несобранный source-only candidate.

Последний подтверждённый idle `2026-10-01T21:27:04.283888Z`: native/service/VPN OFF, cached UI PID27660, activity не resumed. Штатная остановка — собственная notification action «Отключить»/PendingIntent cancel. Source tests приняты ранее (5 Go + дополнительный bounds, 2 JVM), здесь не повторялись.

Последний run: DTLS4251ms, AUTH read357ms, sessionPOST304ms. DISARM rights_none7900ms был за1275ms доGWREAD_OK и **не равен catalog-ready**; нет catalogACCEPT/full-list/accessPASS. Видимаяоднакарточка доcleanup — UI evidence. ChildExit407.091s включает задержкуcleanup; ранние7ME/GWexchange не доказывают работудоChildExit; logcapturegap не восстановлен. PrevioussameAPK AUTHread10759ms не воспроизведён. ServerprobesOFF подтвержденыroot/engineer; restartпулов нефикс.

Offline Sep28 comparison: рабочий8b2f4ba/APK6f2d7652 подтверждёнinstall+Connect/HTTPS/Disconnect; отдельно3eca4d5/APK920a4fcc далnativecold/processwarmARM→ACCEPT2500ms и ранееTIMEOUT15005ms. Нового доказанногоcritical-pathclientdefect не найдено; existingpublicationfix сохранён. Exact report: `/home/pavel/step036-receipts/sep28-current-regression-delta-20261002/REPORT.md`; planned A/B не выполнялся. Все101ID ниже сохранены; их прежние source-only строки — исторический срез, не новая совместная productприёмка.

## Исторические этапы и таблица требований (сохранены)

Текст ниже относится к этапам указанной даты/версии. Формулировки «не собрана/не запускалась» описывают тогдашний этап, не текущий installed APK.

# Mobile service dial chronology — offline source candidate

## Точная версия и scope

Source code commit `b65be933351f973194b97954eb663bde0e5fc909`, base reviewed `c7feddd780dea786e36cedbe108232995dc036ff` (publicationfixf68 сохранён). Branch `laptop/mobile-dial-stage-20261001`, checkout `/home/pavel/terlimo-mobile-dial-stage-20261001`. Отдельный candidate, privateoverlay не копировался, donor origin не менялся/push не выполнялся. APK этой measurementверсии **не собрана**, native/packagedSHA/signature новойверсии не установлены. Installed ordinaryAPK070aafdd остаётся прежнимартефактом, егоhistoricalsource/install/device результаты не переносятся на новыйmeasurementcommit.

## Измерение и проверка отдельно

| Поведение | Source | Проверка |
|---|---|---|
| Scope толькоmanagedServiceEstablish | contextvalue включает per-dial observer, VPNcaller его не получает | Go scope/cancellation/NilwrapperPASS; обычныеtimeouts/config/order не менялись |
| Fixedchronology candidate/socket/TLS/client/Allocate/cert/semaphore/DTLS/firstwrite | Реализовано | Go bounds/safeerror/pass-throughPASS; Kotlinmatcher/budgetPASS |
| Monotonic elapsed отначалаdial, никакихpayloadfields | Реализовано | Fixedrecordformat/code review; rawerror rejected, on-wiretrace отсутствует |
| Bounded64events +FINISH/truncated, однаwriteпослерeturn | Реализовано | cap/singleemit/uniquecall/latefrozenracePASS |
| FirstWrite span | ВключаетPermission ивесьfirstrelay.WriteTo, не равенpurepermissionlatency | Pass-through n/error/addr/bytes, singlefirstamong8concurrentcallsPASS; lateENDотбрасывается, отсутствующийEND=незавершённыйspan |
| APK/artifact/device | Не собраны/не запускались | NOT TESTED; measurementизменение не лечит7573msсамопосебе |

7targetedGo tests-racePASS; 3purehostJVM unit testsPASS, безGradle/native/APKbuild/SDKdevice иwithoutnetworkfixtures. КонструкторTURNearlyfailure используетinvalidport+in-memoryPacketConn, никакихsocket/HTTP/DNSremote операций. Ранеепринятаяsuite не повторялась.

Nativecap64events/percall, summaryFINISH/truncated=0|1 отдельно. Приoverflowсохраняютсяпервые64, ordinalнеперенумеровывается, callIDatomicuint64непереиспользуетсяиз-забуфера. Наseal событияпозжеcapturedFINISHtime исключены, нетwaitingforfirstWrite/extraobservergoroutines. ENDотсутствуетприin-flightwrite, не выводить0ms. РезультатыBEGIN/OK/CANCELED/TIMEOUT/EOF/CLOSED/OTHER; transportenumNONE/UDP/TCP/TLS, толькочисла/фиксированныестадии. callIDprocess-local; PID/childattempt+соседнийserviceESTABLISHtrace даютсопоставление безновыхwirefields.

Androidconsumer используетотдельныйbudget65lines/10s и260lines/child; старыеcaps/reservedterminalslots не подняты и не расходуются. Одинmaxnativebatchвмещается. Поздниеbatch при исчерпанномhostratebudget могутбытьотброшены: **missingFINISH означаетincompletecapture**; native truncationflag описывает nativebuffer, не host/logcat/delivery. Не утверждатьfulltraceприотсутствииFINISH/наличииtruncated=1.

## Buildpath после source review

Root переносит reviewedclient subtree вcanonicalpavelvital2/terlimo черезmonorepopublisher; Laptop не пушитdonor. Толькопослеreview отдельно назначить обычныйnative-first build: свежийGoNDKAndroidarm64→inputELFguard→Gradleordinarydebug→packagedstripidentity/signature/alignment. Нужны обе части новогоsource: nativeproducer И Kotlinallowlist; APKсо старымhostparserне покажетdialstage. НетновогоGradleproperty/diagflag, Piondep илитаймаута; fullframecaptureOFF. PrivateTESToverlay толькоштатнолокально поотдельнойbuildзадаче. Build/install/phone сегодня в этомэтапе не выполнялись.

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
| G11 | Кэш каталога и выбранного узла; offline-first отображение | Действующее ТЗ; AT09 | Частично: native snapshot публикация/выбор реализованы; весь Android offline-first отдельно не проверен | PASS targeted real-bridge/race tests; phone не проверено |
| G12 | Ручное обновление при обычном VPN и удерживаемой блокировке | Действующее ТЗ; AT07, AT09, AT40 | Частично: snapshot не меняет Connected/KillSwitch, manual runner сохранён; реальный VPN refresh не проверен | PASS фазовые/runner offline tests; phone не проверено |
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



Набор101ID/order сохранён. Source measurementtestsнеявляютсяполнымproductPASS. Acceptedpublicationsourceкодсохранёнbyte-for-byte вterlimo_mobile.go/terlimo_runner.go/mobile_catalog_publication_test.go; повторпрежнихpublicationtestsне выполнялся.


## DTLS RX/TX boundary — source only 2026-10-01

Base74db78c0da2f5b70dba577e572c98cbccb37b9ec. This source adds allocationendpoint hash and bounded RX/TX pump summaries + final8 received/unwrapped DTLS record headers; no payload/address/key/credentials logging. No new APK/native/install/device test: NOT_TESTED for this source. Historical artifact/test entries above belong prior source, not this addition. Publication root only clients/android in pavelvital2/terlimo.

Scope still managedServiceEstablish only. Fixed64 stage events + allocationhash + RXsummary + TXsummary + last8record samples + FINISH ≤76lines. Host dedicated dial budget76/10s,304/child, other budgets unchanged. Exact new IOgrammar max314bytes incl worst numericfield widths (stage cap256 unchanged). Caps increased by11lines/window and44/child from65/260; no hot-path output/new goroutine/read/write/wait. Single batch at existing finish. Successive bursts can still hit host rate caps; missing FINISH means incompletecapture.

RXcounts successful relay reads; peer mismatch/unwrapfailure counters; handoffOK/pipeerror; first/lastRX monotonicms (rx=0 means no receive, not a zero-duration span). TX relayWrite OK/error plus wrap/read errors; parser invalid/limited; tailtruncated explicit. Counters are seal-time snapshot including existing cleanup and independent of ring truncation; latest timestamp after captured FINISH sets late=1 so this is not claimed a strict FINISH cutoff. Samples afterFINISH omitted; sealed events ignored. Socketstillrunning/newattemptwithoutFINISH remains unobserved/incomplete.

Parser bounds8records/datagram, tail8records; accepts outerheaders ofcoalesced/fragmentedDTLS records but doesn't parse/reassemble handshake bodies. UnsupportedCID/version/type/malformed lengths incrementinvalid and never affect forwarding; excessrecords setlimited. Tailring preserves recentrecords after earlyretransmissions, counts never depend on samples. Encrypted ct22 not calledFinished. Successful pipeWrite means asynchronous handoff, not Pionreceipt/decrypt/verify. See PION-RECEIVE.md. Meaningful new offline Go-race/JVM checks; prior suites not repeated. All101featureIDs retained in order.

Source code commit: 022e6535325cd44fe4dcd8eb8b9aab50f7d2d500. New5Go-race tests PASS1.068s; final exact314bytecap check PASS1.046s; 2purehostJVM tests PASS0.076s. No Android/native/APK compilation or device actions.
