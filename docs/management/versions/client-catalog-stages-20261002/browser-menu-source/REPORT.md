# Browser menu parity — source candidate, 02.10.2026

Поручение: `whitelist-20261002-browser-menu-parity-source-v1`.
Base: `8230feb9e9b25e3dfa67dd1ff90f1c9b749d4e3a`, canonical `pavelvital2/terlimo`, ветка публикации `fix/catalog-stage-deadlines-20261002`.
Изолированный checkout: `/home/pavel/terlimo-browser-menu-parity-20261002`, локальная ветка `laptop/browser-menu-parity-20261002`; один writer. Новый source commit `67d50966b9a9fc520f278d8498038acf5170b3a9`, tree `d01ef7f12f1c0fb8b66007a755872f20d6c23d2b`; SHA пакета — в manifest.json. Публикация через Руководителя проекта, push не выполнялся.

## Изменено

- Периоды показываются в порядке 1→3→6 с подписями `1 мес.`, `3 мес.`, `6 мес.`. Это только display: `days:30` остаётся `days:30`, wire не менялся. Цена каждой строки формируется из серверных amount_minor/currency, через точную десятичную арифметику. Нет встроенных 200/600/1200 либо 200/480/840; тестовый export не является конфигурацией приложения.
- Только реально предложенные сервером методы, в порядке `🏦 СБП (QR-код)` → `💳 MIR` → `🪙 Криптовалюта`. `card` остаётся прежним wire alias, backend methodID11 не тронут. Кнопка `💳 Оплатить`, возврат `⬅️ Назад`, отмена `❌ Отмена` соответствуют приложенному export.
- В существующей «Подписке» два спиннера заменены обычными Android-диалогами выбора тарифа и метода. Это позволяет отличить явный выбор от программного adapter callback. Тариф с серверной суммой → выбор метода → внутренний quote → Pay. Отдельной кнопки «Получить предложение» больше нет; новых framework/экранов нет.
- Только явный выбор метода отправляет один purchase_quote. Открытие диалога, render, onResume, поздний quote ничего не создают/не повторяют. На смене выбора старый quote_id исключается из локального допуска Pay; service очищает quote/createAck до нового запроса. Пока quote запускается, `sending` вместе с прежним single-flight не даёт второму queued выбору заменить владельца. Ошибка/цена не подтверждена: видимый текст, повтор только через новый явный выбор метода.
- Общий `PurchaseFlow.payableQuote` проверяет выбранный server plan, метод, исходный duration code, amount/currency, фазу и срок quote; paid-awaiting-binding не допускает Pay. Проверка применяется к UI и service, включая повтор перед отправкой после cold service startup. Продуктовые deadlines неизменны. Invoice по-прежнему создаётся только явным Pay; durable keys, single-flight/correlation и серверная цена сохранены.
- «Назад»/«Отмена» очищают только UI-выбор и live browser marker. Existing payment не удаляется; Continue/Check работают независимо от отмены выбора нового заказа. Continue открывает тот же payment без payment_create. Paid-awaiting-binding сохраняет запрет новой оплаты. Выход из браузера не подтверждает права локально.

## Основные места

Все пути относительно `clients/android/testapp/src/main/java/xyz/terlimo/test/`:
- `PaymentsText.kt`: methodLabel/orderedPlans/orderedMethods/purchasePlanLine и точные подписи.
- `MainActivity.kt`: showPurchasePlans/showPurchaseMethods/clearPurchaseSelection/selectedPurchaseQuote/renderPurchase; внутренний quote только из метода выбора, Pay только из кнопки.
- `PurchaseFlow.kt`: payableQuote — общий допуск по текущему серверному предложению.
- `SessionService.kt`: purchase_quote/purchase_pay, pre-send перепроверка; остановленный service отвечает ошибкой вместо зависшего ожидания выбора.
- `PurchaseVisibility.kt`: убрана отслужившая policy отдельной quote-кнопки; сохранены условия price/Pay и paid guard.

## Проверка

Подробные результаты: `tests-initial.json`, `tests-final.json`, `targeted.log`, `targeted-final.log`.
Первая адресная проверка: 10 PASS (BrowserMenuTest2, PurchaseVisibility4, три изменённых label/price assertions PaymentsContract, один изменённый source-wiring метод CheckoutOpenPolicy). После уточнения queued-selection/service-stop обработки повторены только stale-selection/Continue и затронутый UI/service wiring: **2/2 PASS**, BUILD SUCCESSFUL; итог записан в tests-final.json.

Новая проверка сравнивает подписи/порядок с fixture, извлечённой из проверенного production export SHA256 `0613dbee931072baeceb459afda06432eb992eae8df58d2484a0fdcbbabf831f`. Проверяет другую серверную сумму без подстановки прайса, сохранение wire duration и фильтрацию отсутствующих методов. Регрессия: смена метода/тарифа и поздний старый quote, иная сумма/валюта, expiry не дают Pay; matching новый quote допускается, старый pending order продолжим, paid guard сохраняется.

Команды offline Gradle/JVM, workers2/in-process, heap1536m; `:testapp:testDebugUnitTest -x :testapp:verifyNativeInput` с точными --tests. Исключён только guard наличия готового native для JVM compile/test; APK/native не собирались и не упаковывались. Все main/test Kotlin исходники скомпилированы; исполнены только указанные tests. Прежние correlation14/policy9/full suites/Go/CAPTCHA не повторялись. Diff проверен `git diff --check`.

## Границы / следующий шаг

Это готовый к review source-пакет для ordinary/nonpromo. На устройстве новый путь не проверялся; APK/install/phone/invoice/provider/TESTserver/production не выполнялись. APKa23/native792/rollbackef84 сохранены. Данные export подтверждают подписи/порядок; они не означают наличие TEST merchant. Изменений server/Go/security/rights нет.

Root review cumulative patch → штатная публикация → отдельное назначение необходимой build/device приёмки и разрешённого TEST checkout. Закрытый fake provider не запускался; реальные merchant/деньги не входят в этот результат. Несостыковки lifecycle, требующей альтернативного решения root вместо данной source-коррекции, не осталось; price retry остаётся явным выбором метода, без фонового цикла.
