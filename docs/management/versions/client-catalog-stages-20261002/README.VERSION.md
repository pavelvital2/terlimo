# Browser checkout menu parity — source candidate 2026-10-02

Base: 8230feb9e9b25e3dfa67dd1ff90f1c9b749d4e3a
Commit: 67d50966b9a9fc520f278d8498038acf5170b3a9
Tree: d01ef7f12f1c0fb8b66007a755872f20d6c23d2b
Branch: laptop/browser-menu-parity-20261002

Production export labels/order; tariff/method selection requests an internal quote.
Only explicit Pay creates the invoice. Server prices, wire durations, durable keys,
single-flight, browser correlation and deadlines remain authoritative. Back/Cancel
clear local selection; existing pending order and Continue/Check survive.

Checks: 10 targeted JVM PASS; after final queued-selection adjustment only 2 affected
checks repeated, both PASS. Diff check clean. Full details in REPORT.md and test receipts.
No APK/native build, install, phone, provider, invoice, server, production or push.
Installed APKa23/native792 and rollbackef84 remain unchanged. This is source-ready,
not a new device/merchant PASS. Full historical version passport follows unchanged.

---

# CAPTCHA build/install/short acceptance — 02.10.2026

Incoming `whitelist-20261002-captcha-build-install-accept-v1`. Build/install, two required local fixtures after one fixture-only correction, ordinary catalogue/selection and final OFF completed.

## Exact installed artifact

- Canonical repository `https://github.com/pavelvital2/terlimo.git`, branch `fix/catalog-stage-deadlines-20261002`, source **bc984965de12773f5806b289c77ceeba08b7ec46**, clean at build, app source identical to accepted806d54a. Publisher remains root; Laptop did not push or change remotes.
- Installed and final readback APK **a23e39c9216992584b38ecddff8a1343c0c10c683b3f84ea004081ef84ef25e6**; package xyz.terlimo.test, versionCode14/versionName0.14-routing.
- Native input = packaged = installed **792a133e887d3937f3b9cdd19a496190049b26e667f3f95a6f30d706e42023ef**, exact reused verified native792. Its Go buildinfo remains b47069c0962868a5aa3230787d1b2ed32db99d68 as expected; Go not rebuilt, server untouched.
- Signer **42ab6d950c15742eaadcf538947f65b3e0bd3e7e1693d67afa969eb00e5d348b**. Native ELF/provenance, APK package/signature/16KiB alignment guards PASS; other native libraries preserved; private overlay verified equal locally and never exported.
- Rollback APK **ef84c3e0ae1c595ae24809a8f3043c34dc892c3e54c647156c83b2332fb210f3** saved locally as rollback-installed.apk. Replacement install-r, user0; no clear/uninstall. appId10246, ceDataInode21342, firstInstall2026-09-24 20:15:13 preserved. Installation marker and encrypted state byte-exact before/after installation; final marker unchanged. After ordinary catalogue operation encrypted state hash changed; it remains present, no clearing/reset performed. Final APK readback still exact a23e39c9.

## Two local fixture results

First prepared T0 `2026-10-02T09:59:17.914760+00:00`. Ordinary instrumentation runner executed only the two requested methods, using data: pages. Service/native/VPN were OFF before them; synthetic token stays in the local manager and is not sent to server.

1. `CaptchaWebViewFixturesTest#manualWindowSolvesAndReportsExactlyOnce` **PASS** on first run; never repeated.
2. `#backgroundShowsNotificationAndReturnRelaunchesTheSameWindow` first run reached visible notification, reopened the window and received the expected local token, then **FAIL** at line122: immediate assertion that notification already disappeared. The target notification was absent on subsequent system observation and service/native/VPN remained OFF. The fixture asserted an asynchronously applied system state without waiting.

Authorized technical correction: local commit **2d6d66a4edf7754b13bcaf7221061ff1df6bb83a**, one androidTest file,8 additions/1 deletion. It observes notification removal with the same bounded40×100ms pattern already used for appearance. Product source, APK, native, mode/operation deadlines unchanged. Only test APK rebuilt/reinstalled; only the failed method retried at `2026-10-02T10:03:20.313788+00:00` → **PASS**, runner `OK (1 test)`, per-test status0, completed `2026-10-02T10:03:23.840792+00:00`. Initial runner `2 tests/1 failure` is retained, not overwritten. Corrected test APK SHA `610b132ef353957a75e290570acb5db71ef9d58f35221da4b97797709c832707`; test-only patch included for canonical publication by root.

Original16 JVM/Go/parity/full suites not repeated. Test APK is ordinary instrumentation; no second product runtime created.

## Ordinary catalogue and OFF

After completed fixtures, idle target process absence proved before a single ordinary launcher at `2026-10-02T10:03:25.330508+00:00`. No previous launcher/catalog attempt occurred during the fixture correction. Native attempt **2065de12-503b-4725-83ba-6b241e80b5e7**, hostPID3779, generation1. Autoload ARM10:03:27.061Z → ACCEPT10:03:56.125Z, marker elapsed **29065ms**. UI showed catalogue,1 available gateway, active existing access. Initially unselected; one ordinary selection produced the visible selected row and chosen-gateway card. No manual refresh, ping, VPN Connect, HTTPS/ConnectedRefresh repetition or trial/hour/payment action.

Ordinary notification action **«Отключить»** at trace10:07:30.613Z; ChildExit10:07:30.752Z, generation1/exit0/HOST_STOP, teardown complete10:07:30.844Z. Capture retained through final OFF, without clearing logcat. Independent final check **2026-10-02T10:08:03.191052+00:00**: serviceOFF, native absent, VPNOFF, target activity not resumed, identity/installation marker preserved. Cached UI process3779 remains; this is not a claim of final cold-process absence. Existing personal Internet was not changed.

## Verdict / remaining limits

**PASS:** build provenance and replacement install; required local manual success/notification-return fixtures after documented fixture correction; ordinary autoload/visible catalogue/selection; finalOFF and identity preservation.

**NOT_TESTED:** runtime Disconnect while manual CAPTCHA is still pending; that is not proved by an ordinary catalogue Stop. All other CAPTCHA branches, real VK end-to-end solving, VPN/HTTPS, ConnectedRefresh and broad101 acceptance are not promoted by this run. Natural VK CAPTCHA was **NOT_OBSERVED** after ordinary launcher (zero challenge markers in retained tags, no challenge UI). No artificial VK triggering.

Observed catalogue29.1s in this run does not remove the known historical~35s latency limit or establish a latency improvement/stability result. Full prior101 version passport retained verbatim below the new result, with its historical statuses. This is the requested installed TEST acceptance, not production/liveVKPASS. No remaining concrete failure in this scope; root can review the small fixture patch and continue browser checkout under its own next task.

## Prior full version passport (historical evidence retained)

# CAPTCHA: два локальных исправления, source receipt

Incoming: whitelist-20261002-captcha-two-local-fixes-v1.
Base: 5ab55149da7e53a030e4a08ce65183b5b89d70e2. Commit: 806d54a762848436108d645a63b7cc6fc63ccad7.
Локальная ветка `laptop/captcha-two-local-fixes-20261002`, checkout `/home/pavel/terlimo-captcha-two-fixes-20261002`. Рабочее дерево чистое. Публикация принадлежит Руководителю проекта Белые списки; Laptop push не выполнял.

## Исправления

1. `CatalogStages` и `CatalogDeadlineTimer`: бюджеты20/25/10/10 и общий65 сохранены численно. Из них исключается только bounded CAPTCHA wait с владельцем attempt+catalog cycle+request. Pause фиксирует остаток через остановку учёта времени; resume сдвигает stage/total deadlines ровно на ожидание, без выдачи нового бюджета. Во время pause запрещены accept и advance. Уже истёкший бюджет не паузится; дубликат begin/pause, чужой resume и повтор завершённого request не сбрасывают время. Pause/disarm/clear/new cycle отзывают callbacks; операции и timer callback синхронизированы, чтобы timeout не обогнал pause между проверкой и отзывом. Стандартные timeout handlers сохранены, включая Connected refresh cancel без выключения VPN. Legacy15s ARM не получает новую паузу: интеграция ограничена непустым staged catalogCycle.

2. `ManualCaptchaPendingOwner` и `ManlCaptchaWebViewManager`: immutable generation принадлежит pending record, foreground Intent, уникальным immutable notification PendingIntents, Activity и JS callbacks. Результат атомарно consume только своего owner, до completion. Cleanup/finish/onDestroy/onNewIntent и background reopen учитывают владельца; старый Intent не загружает чужой redirect и не закрывает новое окно. Notification cancel повторно проверяет owner в SessionService при обработке очереди. Ordinary Disconnect сохраняет прежний путь. Donor JS/способы решения/численные deadlines/внешний bridge protocol не изменены; raw error не пишется в лог.

3. В этой же границе Disconnect завершена необходимая интеграция: teardownCaptcha синхронно отменяет scope retiring Service и отзывает queue/manager ownership. Прежний cleanup ставился в actor непосредственно перед actor.close(), который мог его выбросить. Поздний coroutine launch наследует отменённый scope. Нового процесса/обработчика/транспортного механизма нет.

## Достаточная проверка

- Сохранённый `catalog-before.log`: две новые регрессии FAIL2/2 на поведении без pause. Для компиляции нового API в baseline были временные no-op pause/resume, возвращавшие false; это не отдельный запуск pristine canonical и не device reproduction. Эта проверка не повторялась после runtime recovery.
- `targeted-final.log`: CatalogStagesTest13/13 и ManualCaptchaPendingOwnerTest3/3, всего16 PASS, failures/errors/skipped0. Новых сценариев5; остальные11 — существующие непосредственно затронутые правила каталога, включая обычные deadlines/старые callbacks/accept/disarm.
- Новые проверки покрывают fake-clock wait95s (дольше прежних20/65), отсутствие accept/advance при pause, оставшиеся15s стадии после resume, общий остаток65 активных секунд, duplicate/stale/expired/Disconnect/new cycle/disarm, чужой manual callback/cleanup, текущий callback exactly once и старую notification cancellation ownership.
- `compileDebugAndroidTestKotlin` PASS: два существующих fixture callers обновлены для обязательного owner. Android tests только скомпилированы, НЕ запускались. Изолированной beforeFAIL для manual helper нет; old global-consume defect подтверждён предыдущим source review, а новый barrier покрыт3 regressions.
- `git diff --check` PASS. После первой успешной проверки16 повторён только этот же набор и compile из-за финальной source-коррекции teardown; полный suite/Go33/parity/прошлые31/10 не повторялись.
- Команда (offline): `gradle --offline --no-daemon --max-workers=2 -p clients/android -Pandroid.builder.sdkDownload=false -Dorg.gradle.jvmargs='-Xmx1536m -XX:MaxMetaspaceSize=512m -Dfile.encoding=UTF-8' :testapp:testDebugUnitTest -x :testapp:verifyNativeInput --tests xyz.terlimo.test.CatalogStagesTest --tests xyz.terlimo.test.ManualCaptchaPendingOwnerTest :testapp:compileDebugAndroidTestKotlin`. Исключён только guard наличия native для чистой JVM/compile проверки: APK/native не собирались и не упаковывались.

## Сохранённая основа и пределы

Installed app5a31/APKef84/native792/servera42 сохранены; телефон, APK, сервер, права и платежи не затрагивались. Go и остальные части monorepo не менялись. Full101 паспорт сохранён в README.VERSION.md без повышения исторических статусов. Новый commit — source candidate, не installed/live PASS.

После capacity проверен `/root/manual_owner`: errored, незавершённых Gradle/test процессов не было. Сохранённые правки приняты, помощник не перезапускался; дальнейшая интеграция одним writer Laptop в прежней сессии.

Оставшееся ограничение: реальные Android lifecycle/notification/JS scheduling и natural VK challenge не исполнены этим offline gate. Нового конкретного source blocker после интеграции не выявлено. Решение о source acceptance и единственной необходимой device проверке остаётся за Руководителем проекта; повторять live run либо искусственно вызывать CAPTCHA сейчас не требуется.

## Полный прежний паспорт установленной версии (история сохранена)

# Selection build/install/device result — 2026-10-02

Canonical app source5a31c7aa1120c6045f76c35b6c02c7e70c61fbec, repo https://github.com/pavelvital2/terlimo branchfix/catalog-stage-deadlines-20261002; app source equals reviewed dfe99b8 cumulative. APKef84c3e0ae1c595ae24809a8f3043c34dc892c3e54c647156c83b2332fb210f3, packaged/installed native792a133e887d3937f3b9cdd19a496190049b26e667f3f95a6f30d706e42023ef reused exactly from priorb470. Go subtree unchanged, no Go build/serverchanges; native Go buildinfo deliberately remainsb470. Signer42ab6d950c15742eaadcf538947f65b3e0bd3e7e1693d67afa969eb00e5d348b unchanged. Private overlay preserved local-only; other native libs match previousAPK. apksigner/zipalign16KiB/ELF/provenance checks PASS. INSTALL-VERIFIED and artifact-manifest are before-run receipts, final result.json records actual run.

## Build/install

Existing working APKd375818957ef6ec24da100b8e7c90875e2a0675896b47af76117c6db955d9677 pulled and retained locally as rollback-installed.apk. Ordinary install-r/user0; readback APK and packagednative hashes exact. identity10246/ceDataInode21342/firstInstall2026-09-24 20:15:13 preserved; installation marker and encrypted state bytewise unchanged across replacement; no reset/uninstall. Prior31/target10 sourcegates accepted and not repeated. No donorpush/canonical source changes by Laptop.

## Ordinary acceptance

One launcher at planned08:31:00.543530Z (actual inresult), cold autoload ARM→ACCEPT35564ms. Initial native DTLS candidateTIMEOUT8097ms observed, native internally recovered; this was not a hostcatalogTIMEOUT or repeatlauncher. Active access24h stillconfirmed, onegateway. Initiallyunselected, one exact UI gatewayselect08:32:09Z. SubsequentmanualpreConnectRefresh08:32:31Z/736ms preserved visible selectedrow, no second select. UI remainedbrowse both08:32:35Z and08:33:21Z; no separate subsequent ordinary credential snapshot BEFOREConnect observed. This requested checkpoint is NOT_OBSERVED, not silently upgraded toPASS. Runtime raw stablegatewayIDs were not available in existing safe trace/UI; exact-ID equalityUNKNOWN, not inferred from gatewayname. Source sameID/reorder/removal/revocation/secondsnapshot target proof remains accepted separately.

ExplicitConnect08:34:09Z, priorVPNconsent sufficient/no newdialog. UIConnected08:35:03Z, 36/36readychannels. Existing built-in VpnReadinessProbe completed: DNStrue/HTTPStrue/HTTP200true/expectedExittrue/readytrue, readiness3007ms/deadlinefalse/stageREADY/exceptionNONE/freshhandshake. Runtimefacts obtained from normal completed-attempt diagnostics afterDisconnect. No separatebrowserrequest or new diagnostic instrumentation.

OneConnectedRefresh08:35:17Z/ARM→ACCEPT1798ms, immediate selectedrow retained, no reselect, UIConnected36/36. Current systemVPNnetwork122 and creation527391707149 exact before/after; no networkreplacement inferred from names. OrdinaryOrbitDisconnect08:35:38Z; originalattempt9e2f48e0-e083-4291-a0d7-cbf8948079ef/gen1/exit0/HOST_STOP/user_cancel/teardowncomplete. IndependentOFF08:36:18.755113Z: serviceOFF/nativeabsent/VPNOFF/UIclosed, identity+installationmarkerpreserved. Retained safe-tag capture throughChildExit+>=5s/logcatnotcleared. TESTrights/server unchanged by Laptop; no newhour/trial/payment. OwnerVPNleftOFF.

## Verdict and limits

Build/installPASS; visible selection after freshRefreshPASS; explicitVPN/HTTPS/ConnectedRefresh selection+network continuity/OFFPASS. Separate subsequentcredentialupdate beforeConnect NOT_OBSERVED; runtimeIDequalityUNKNOWN. Therefore complete all-checkpoints acceptance NOT_CLAIMED. No blind repeat/new instrumentation/longstabilityrun. Current installed candidate operational in testedflow; fallbackd375 retained. Finalservercorrelation belongs toEngineer. Passport full101 preserved as historical requirements/statuses, not all101 currentPASS. Stage/UI exact durations beyond loggedcatalogARM→ACCEPT not claimed.

## Prior historical full101 passport, preserved

# Active TEST access VPN acceptance — 2026-10-02

Current b47069c0962868a5aa3230787d1b2ed32db99d68 / APK d375818957ef6ec24da100b8e7c90875e2a0675896b47af76117c6db955d9677 / native792a133e887d3937f3b9cdd19a496190049b26e667f3f95a6f30d706e42023ef. Без сборки/установки/смены identity. READY exact existing TEST от Engineer принят: nodea42fa90e/relay8c4667ba/authdd16bd8f/browse7bec1ebd, существующее право24h/gateway applied. Это server READY; корреляция именно этого run — отдельно Engineer.

T0 planned07:48:32.863675Z, actual ordinary HOT launcher07:48:32.864920Z. Launcher только вернул прежний subscription screen со старой expired проекцией, нового autoload/ARM не было. Выбран cached gateway, первый tap Connect недоступной кнопки не дал native attempt. Для фактически актуального статуса выполнен один обычный initial Refresh07:50:25Z, ARM→ACCEPT13812ms. Accessactive до03.10.2026 07:41:20Z, один gateway STEP036 device node. Browse→verified list потребовал повторного выбора; после выбора active gateway explicit Connect07:52:06Z с уже сохранённым VPN consent (новый диалог не появился), UIConnected07:52:33Z. Один native attempt917151a7-ecc3-425e-9973-d5d660e7d73b/host24595; native generation2 при закрытии, не второй launcher.

## Checked

- VPN PASS: системный текущий VPN network121 + UIConnected; 36/36 ready channels в первом снимке, 35/36 перед Disconnect. Счётчики каналов не скорость и не long-term stability.
- Полезный HTTPS PASS штатным VpnReadinessProbe: VPN-bound HttpsURLConnection, DNStrue/HTTPStrue/HTTP200true/expectedExittrue/readytrue, свежий handshake, readiness843ms/deadlinefalse/stageREADY/exceptionNONE. Прямые runtime facts получены из обычной «Диагностики последней попытки» после Disconnect; отдельный browser HTTPS не запускался.
- ConnectedRefresh PASS: единственный tap07:52:56Z/ARM→ACCEPT883ms. UI остаётся Connected, системный network121/creation523096739853 до и после одинаков. Краткая browse projection показала невыбранную строку, перед Disconnect выбранный STEP036 снова отображён; выбор сохранился без дополнительного select.
- Видимые stages OBSERVED4/4: connecting/checking screenshots + subscription_status/loading_catalog original screenrecord frames. VideoPTS125.563078/125.944533s, не exactUTC/fullstage duration. Никаких synthetic stages/замедления/product budget изменений.
- Disconnect/OFF PASS: ordinary Orbit07:55:10Z; user_cancel, ChildExitgeneration2/exit141/HOST_STOP, teardowncomplete. Это observed exit141, не exit0. FinalOFF07:56:08.819740Z: native/service/VPNOFF/UIclosed. Identity10246/21342/firstInstall2026-09-24 20:15:13 + installation marker сохранены, APK неизменён. Retained safe-tag capture ChildExit+>=5sec, logcat не очищался.
- Laptop не нажимал trial/hour/payment. Существующий kindtrial отображён, его ends_at ранее продлил Engineer; новая trial не создана Laptop.

## Limits

Горячий launcher не сделал autoload: stale old status устранён ordinary Refresh без cold process/replay. Первое disabled Connect не было отдельной попыткой. Новая consent-подача NOT_TESTED (существующее разрешение). Full101 сохранён без повышения исторических статусов; долгосрочная стабильность/all101PASS не заявлены. Video179.31s сохранено private локально, включает стадии, заканчивается до activeConnect; Connect/Refresh/OFF подтверждены screenshots/trace/system checks. Видео не отправлено; отправляются только визуально проверенные безопасные кадры. Серверная cross-run correlation ещё не утверждается.

## Previous full101 passport, unchanged historical rows

# Corrected client/server pair acceptance — 02.10.2026

Обычная приёмка исправленной пары завершена: catalog PASS, условный active-VPN сценарий NOT_TESTED из-за отсутствия действующего доступа. Один launcher, без первого manualRefresh: autoload сработал. PlannedT0 2026-10-02T07:17:56.772803Z, actualT0 2026-10-02T07:17:56.772969+00:00; originalPID24595/attempt b2f9f386-d588-46bf-ad2f-35df5f1b9151/gen1. One ARM→ACCEPT16067ms, no TIMEOUT. Literal catalog_cycle UUID не логируется (UNKNOWN), не утверждаю значение по stderr; событий ARM/ACCEPT ровно по одному.
Список на реальном экране 2026-10-02T07:18:24.498863+00:00: 1 шлюз STEP036 device node. Первоначальный видимый этап 2026-10-02T07:18:02.125094+00:00: Подключение к серверу на Orbit subtitle и карточке каталога. checking_device/subscription_status/loading_catalog не попали между редкими снимками: NOT_OBSERVED, не lostbridge/не PASS всех четырёх UI переходов. Native chronology: establish2962ms, challenge decoded6247ms отAUTHbegin; session exchange3626ms, firstME5836ms, firstGW108ms. Это native request durations, не измеренные host/UIstage durations. ME/GW2xx и hostACCEPT подтверждены.
Статус UI на 2026-10-02T07:18:56.858664+00:00: Срок доступа истёк · VPN не активен; срокподписки01.10.2026 21:58 local. Literal grant.dataAccess не выводится retained safe tags (UNKNOWN); не выдаю inference за rawgrant. Connect/полезныйHTTPS/ConnectedRefresh NOT_TESTED по условию root отсутствующего active права. Новый час/trial/payment/rights не создавал.
Штатное original own notification Отключить ровноодин раз, caller=user_cancel; ChildExit0/HOST_STOP deviceepoch1790925687.254; teardown штатный. Finalidle 2026-10-02T07:21:29.284907+00:00: nativeabsent/serviceOFF/VPNOFF/UIhome, identity иinstallationmarker сохранены. Scoped safe-tag capture доChildExit+5s завершена, fullunfilteredlogcapture не заявляется; никаких logclear/новыхdiagnosticflags/windows/force-stop.
Exact app source b47069c0962868a5aa3230787d1b2ed32db99d68/APKd375818957ef6ec24da100b8e7c90875e2a0675896b47af76117c6db955d9677/native792a133e887d3937f3b9cdd19a496190049b26e667f3f95a6f30d706e42023ef. Сервер по принятому root READY: nodea42fa90e/relay8c4667ba/authd16bd8f/browse7bec, flagsOFF; crossserver join ожидает Engineer ordinary correlation. Без builds/install/sourcechanges/replay. Паспорт full101 сохранён, product101/VPNPASS не объявляю; предыдущий17d/63a4 FAIL не переписан. TERMINAL/idle уже отправлены root и TERLIMO с собственными новымиIDs.

| Acceptance item | Status |
|---|---|
| Ordinary opening / auto catalog | PASS |
| Visible initial real connecting stage | PASS |
| Other three UI stage labels this run | NOT_OBSERVED |
| Fresh host ACCEPT + visible 1-gateway list | PASS,16067ms |
| Current access status | Expired / VPN not active |
| Active-right Connect / useful HTTPS | NOT_TESTED |
| Connected manual Refresh preserving VPN | NOT_TESTED |
| Normal cleanup, native/service/VPNOFF | PASS |
| All101 current requirements | NOT_CLAIMED |

## Preserved build/install and historical full101 passport

# Catalog active-budget client — build/install receipt, 02.10.2026

## Exact installed version

Canonical repository https://github.com/pavelvital2/terlimo, branch fix/catalog-stage-deadlines-20261002, exact source `b47069c0962868a5aa3230787d1b2ed32db99d68`. Separate clean detached checkout `/home/pavel/terlimo-catalog-active-budget-build-20261002`; no push. Root native writer; Laptop build/install only. Includes visible stage, accumulated-active budgets20/25/10/10 + wall65 and ordinary credential wake correction.

| Artifact | SHA-256 |
|---|---|
| Native fresh input | f53b59f8fc4c599aa58a24c773208ff8c9d7ebaf4f791622cb49552dec603ade |
| Native packaged and installed readback | 792a133e887d3937f3b9cdd19a496190049b26e667f3f95a6f30d706e42023ef |
| APK and installed readback | d375818957ef6ec24da100b8e7c90875e2a0675896b47af76117c6db955d9677 |
| Saved immediate rollback APK | 63a4ae1a5223aea0b7e371db0afdfd3dc86a21ffdc61f4c0e03475f39a287f1c |

Native FIRST from final Go source, Go VCS matches canonical SHA. Packaged native exactly equals freshly built input after NDK llvm-strip; previous native hash differs. Other packaged libraries and private bootstrap overlay preserved locally. Go1.27/NDK28.2.13676358/JDK21/Gradle9.1 offline; signature verification and16KiB alignment PASS. Package xyz.terlimo.test/versionCode14; signer42ab6d950c15742eaadcf538947f65b3e0bd3e7e1693d67afa969eb00e5d348b unchanged. Accepted97/43source gates reused, no rerun.

install-r/readback PASS. user0/appId10246/inode21342/firstInstall2026-09-24 20:15:13 preserved; installation marker and encrypted state unchanged between immediate before/after. Rollbacks63a4,7d and6b retained. No uninstall/clear/key/rights/runtime flag changes.

## Current readiness and limitations

Final idle `2026-10-02T06:40:26.457416+00:00`: serviceOFF/nativeabsent/VPNOFF, activity not resumed. No launcher/Refresh/acceptance after this update. Current product/list/Connect/HTTPS/ConnectedRefresh NOT_TESTED. No new server/window/flags from Laptop. Server TEST A previous7bec1ebd did not pass original AUTH: SVCUNIX READSLICE deadline≈15s, SERVICE_UNAVAILABLE; underlying upstream cause UNKNOWN. This installed candidate awaits root confirmation of server correction before a new acceptance.

Previous17d12e7/APK63a4 one-run result remains FAIL before catalog: no ACCEPT/list; host connecting timeout20s after real internal retry. READ_OK was error frame, not AUTH success; lostprogress not proven. Updating the APK does not revise that earlier result.

## Historical full101 passport

All101 unique requirement IDs preserved below from canonical source. Historical statuses belong to their stated snapshots; they are not current device/product PASS.

# Active-time stage semantics accepted (source, not device)

Android fb464048 integrated after root technical review: cumulative active time per real stage, budgets20/25/10/10 unchanged, absolute whole65 never paused. Reconnect resumes unused connection budget; duplicate/foreign/expired events do not reset progress. Observed CONNECTING0→DEVICE3→CONNECTING19 now leaves17s connection, DEVICE21 leaves9s device. 43 focused JVM and compile PASS per executor. Earlier first-visit wall semantics below are superseded. Server AUTH nested-budget correction remains separate; this source is for native-first build/install, no blind repeat of the failed server path.

# Visible-stage follow-up (not installed)

Android f62e334 source diff accepted: Orbit subtitle and ServerCatalogView loading now render actual catalogStage.label; working VPN title/selection retained. Executor Kotlin compile PASS. Integrated real-service-channel regression on17d12e7 PASS/race2.037s confirms connecting→checking_device→reconnecting preserves cycle across SERVICE_UNAVAILABLE. Phone bridge stage acceptance remains unlogged; the test does not retroactively prove that phone delivery. Stage accounting correction and server AUTH nested-budget review still pending. No new phone run.

# First device acceptance and native follow-up

Installed candidate17d12e7/APK63a4ae1a failed first ordinary launch: establish2.951s, AUTH frame completed18.173s with SERVICE_UNAVAILABLE, retry then CATALOG_CONNECTING_TIMEOUT at20s. No visible catalog/Connect/HTTPS acceptance; final idle06:19:28.137111Z. READ_OK does not establish AUTH success. Missing stage in filtered stderr does not establish lost product events; revisiting CONNECTING restores its original wall deadline. Android visible-stage/reconnect corrections pending; server wait cause under correlation.

Separate native source defect fixed here: ordinary Trigger("manual") for credential refresh no longer enters the UUID-bound manual operation/cancel fence; only TriggerManual uses that path. Runner lifecycle regression PASS; accountaccess full race3.832s and affected main race8.629s PASS. This follow-up is NOT_BUILT/NOT_INSTALLED/NOT_DEVICE_TESTED and does not resolve the server SERVICE_UNAVAILABLE.

# Combined source candidate

Android source b4cd9f9c26ab0992c3a4dbbe651c4e7a500917f7 (499b18c baseline) integrated after review: active-right browse Connect keeps consent and ordinary native admission, no new hour; actual reconnect displays without resetting visited-stage budgets; monotonic deadline checked before catalog publication. 97 targeted JVM tests and Android compilation PASS per attached executor receipts; expanded suite has 3 inherited source-assertion failures, not full-suite PASS. Native integration complete: separate explicit display read and ordinary credential Connect; root affected native race tests PASS (8.064s), complete accountaccess race suite PASS (3.639s). Build/device acceptance pending. Existing 101 feature rows below are retained; no new feature PASS inferred.

# Дополнение: owner catalog_cycle и отдельная отмена

start/refresh_manual несут catalog_cycle UUID; immutable контекст одного прохода сохраняет его в stage и finalcatalog. Новый ручной refresh не переименовывает старый результат. Background не наследует cycle, progress после первой публикации прекращается. cancel_catalog завершает только соответствующий operation, parentVPN остаётся жив; whole native65с относится проходу, не всему процессу. Последовательный Runner отдаёт приоритет queued manual перед одновременно готовым фоновым таймером; replacement wake не теряется за coalescing fence. Повтор одинакового browse отправляется для нового явного cycle, background dedup сохранён.

После изменения: accountaccess -race PASS4.353s; root affected main -race PASS8.245s; focused Catalog-race PASS1.071s. Существующий wiring fixture использовал bytes.Buffer одновременно из reader/writer; переведён на существующий syncBuffer, production writer не изменён ради теста. Android/API интеграция и phone приёмка ещё ожидаются.

# Native catalog stages — source candidate

Base7344a04a41ee02df365a3facd8649f1004beecd7 / clientdd9e253f5e7da1d58e78a593b996fcab8c28dfc1. Canonical pavelvital2/terlimo, clients/android subtree. Go часть; Android companion выполняет Laptop. Без него не устанавливать как готовую версию.

AUTH/ME/GATEWAYS: establish20s отдельно от request25/10/10s, реальные catalog_stage события через bridge v/attempt_id. Android владеет aggregate stage20/25/10/10s и total65s. Это первоначальный кандидат по ночным наблюдениям, не SLA. Caller deadlines/cancel сохраняются. DTLS8s, reuse lifetime, admission/права, прямой HTTPS и другие service classes не изменены. Готовность каталога только после hostpublishedlist.

Проверки: go test ./servicechannel PASS; go test -race ./servicechannel ./accountaccess PASS2.817/3.703s; go test . -run 'Test(CatalogBridge|Mobile|ManagedMobile|ServiceChannel)' -count=1 PASS2.526s; после дополнительного regressiontest go test -race ./servicechannel -run TestCatalog -count=1 PASS1.309s. Проверено отдельное время slowestablish/request, повторное использование соединения, callerdeadline, cancel/timeout, неизменный paymentlimit, отказ bridge без wire request и корреляцияproductevent.

APK/Android native build/установка/телефон НЕ выполнены; product acceptance NOT_TESTED. Устройство по readback Laptop TECNO KL5n Android14; сеть/права фиксируются при реальном прогоне. Server runtime не менялся, совместимость проверяет TERLIMO. Source rollback parent7344a04; device rollback сохранённый exact APK до установки. Source hashes прилагаются.

Полный101реестр ниже сохранён из предыдущего паспорта без повышения статусов. Исторические проверки не объявляются проверкой нового APK. Новая source-проверка относится только к описанному Go изменению.

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
