# ListPlans optional Bearer — source result, 02.10.2026

Поручение: whitelist-20261002-payment-plans-bearer-laptop-v1.
Base **c47d8beb3baa844cf60d625aad37c62f5f91f9e9**; commit **9c0027db1e197913dd2ab354f786287694655d4b**; tree **951304239cf50cd3e5a9a62558bb7d159aa3832d**.
Checkout `/home/pavel/terlimo-payment-plans-bearer-20261002`, branch `laptop/payment-plans-bearer-20261002`, один writer, tracked clean. Canonical publisher — root, `pavelvital2/terlimo`, `fix/catalog-stage-deadlines-20261002`; push не выполнялся.

## Изменение

`clients/android/go_client/accountaccess/payments.go:405`: ListPlans вызывает прежний `request` вместо `requestWith(...false)`. Он включает штатный Bearer path:
- существующий `Client.Tokens` вызывается через прежний requestWithQuery, заголовок Authorization передаётся тем же HTTP/service-channel маршрутом;
- nil Tokens → прежний public GET без Authorization;
- ошибка TokenSource возвращается до HTTP.Do, не приводит к анонимному запросу/прайсу;
- обработка HTTP/API ошибок остаётся прежней, без анонимного retry.

Обновлены комментарии в payments.go/client.go и узкий раздел `clients/android/testapp/README.md` про optional authentication. Другой runtime-код не изменён. Endpoint GET /plans, deadlines, body/schema, UI, цены, idempotency и server/gateway сохранены. UI не передаёт token/account ID. Новый login не добавлен: используется существующий TokenSource и его неизменённая политика проверки/refresh MobileSession.Bearer; авторизация/refresh в этой работе не запускались.

## Проверка: ровно две целевые проверки, 2/2 PASS

1. `TestPaymentClientPlansUsesExistingBearerAndPreservesAuthFailure`: один GET /api/mobile/v1/plans с синтетическим Bearer существующего source, без body/idempotency header; затем этот же source возвращает token validation error — исходная ошибка сохранена, число HTTP-запросов не растёт.
2. `TestPaymentClientPublicPlansIsBearerless`: nil source, тот же GET/path, без Authorization/body/idempotency header, корректный plans decode.

Команда: `GOPROXY=off GOTOOLCHAIN=local go test ./accountaccess -run '^(TestPaymentClientPlansUsesExistingBearerAndPreservesAuthFailure|TestPaymentClientPublicPlansIsBearerless)$' -count=1 -v` в clients/android/go_client, Go1.27.0. Локальный httptest и прежние synthetic fixtures; live endpoint не вызывался. Лог targeted.log. gofmt и git diff --check PASS. Меню/core/Go whole suites не повторялись.

## Передача / границы

Cumulative patch от exact base и SHA manifest приложены. Source готов к review; это не доказательство live цены10/20. Установленные APKa23/native792 и rollbackef84 не трогались. После принятия этой Go-правки **необходимы новая native-сборка и APK**: reuse792 для исправленной версии непригоден. Сейчас native/APK/install/phone/provider/invoice/TESTserver/production/деньги не запускались. Addon/UI остаются вне текущего поручения. Полный исторический паспорт сохранён в README.VERSION.md без повышения статусов.
