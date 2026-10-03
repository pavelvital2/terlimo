# Platega: освобождение резерва реферальной скидки

2026-10-03. Bounded SOURCE/documentation review; без merchant/API вызовов, runtime, кода, тестов или production изменений.

**Вердикт: публичная документация не доказывает finality неоплаченного CANCELED/истёкшего счёта. Автоматический release резерва на этом основании пока не обоснован. Endpoint cancel существует, но описан как возврат средств, а не безусловное закрытие неоплаченной ссылки. Это точный внешний остаток, не новое бизнес-решение и не основание объявить вечное удержание скидки готовой функцией.**

## Что действительно документировано

| Источник | Факт и граница |
|---|---|
| [PaymentStatus](https://docs.platega.io/paymentstatus-13226215d0.md) | OpenAPI enum: PENDING, CANCELED, CONFIRMED, CHARGEBACKED. EXPIRED отсутствует. Нет таблицы допустимых переходов/терминальности. |
| [CreateTransactionResponse](https://docs.platega.io/createtransactionresponse-13226218d0.md) | transactionId, redirect, status; expiresIn — оставшееся время HH:MM:SS. Не определено, исключает ли истечение уже начатое банковское исполнение, позднюю проводку или повторную оплату ссылки. |
| [GET /transaction/{id}](https://docs.platega.io/проверка-статуса-оплаты-платежа-29203844e0) и [схема ответа](https://docs.platega.io/transactionstatusresponse-13226219d0.md) | Возвращаются id, status, paymentDetails.amount/currency, expiresIn; GET также имеет 404. Нет final/nonpayable/finalizedAt/event sequence или обещания монотонного согласованного финального результата. 404 не доказательство, что создания/оплаты не было. |
| [Callback](https://docs.platega.io/callback-об-изменении-статуса-транзакции-29209725e0) | Успех CONFIRMED, неуспех CANCELED, возврат CHARGEBACKED. Поля id, amount, currency, status, paymentMethod; merchant headers. Таймаут доставки 60 секунд и до трёх повторов через пять минут. Окончательность CANCELED и порядок разных событий не оговорены. |
| [GET /transaction/{id}/cancel-supported](https://docs.platega.io/проверка-возможности-отмены-транзакции-38219023e0) | Проверяет возможность возврата; supported:true требует достаточного merchant баланса. Поля supported, totalDeductUsdt, penaltyNativeAmount/Currency, penaltyUsdt, penaltyConversionRate, blockReason. Это не receipt о закрытии счёта. |
| [POST /transaction/{id}/cancel](https://docs.platega.io/отмена-транзакции-38225949e0) | Инициирует отмену и возврат плательщику. Ответ содержит transactionId, accepted, manualControlRequired, message; пример HTTP200 имеет accepted:false/manualControlRequired:true и возврат в процессе. Ни HTTP200, ни accepted сами по себе не описаны как финальная невозможность оплаты. Не использовать автоматический refund для освобождения скидки. |
| [POST /subscription/{subscriptionId}/cancel](https://docs.platega.io/отменить-подписку-40029730e0) | Останавливает будущие recurring списания, идемпотентен. Не контракт закрытия одноразового discounted invoice. |

Проверены официальный индекс llms.txt, страницы и исходные OpenAPI Markdown схемы. Markdown схемы получены прямым HTTPS чтением после ошибки web-renderer; не обращались к merchant API. Приведённые свойства не означают существование недокументированной гарантии. Не утверждаем, что провайдер *разрешает* оплату после окончательной отмены: это **UNKNOWN**.

## Позднее уведомление и поздняя оплата — разные случаи

1. Успешная оплата уже произошла, но CONFIRMED доставлен позже/повторно: механизм повторной доставки документирован. Такое уведомление следует обработать идемпотентно по прежнему order/amount/currency, даже после локального canceled.
2. Оплата впервые происходит после окончательной отмены или истечения: возможность/невозможность не установлена документацией. Retry callback не доказывает эту возможность. Истечение UI-таймера также не доказывает её невозможность.
3. Для release нужны обе гарантии: счёт больше не примет деньги **и** нет ранее принятой/находящейся в обработке оплаты, которая позже станет CONFIRMED. Запрет новых оплат сам по себе недостаточен.

## Точное сопоставление с текущим source

База 2d823640c3823a678bee5e3d4497925d4ec57f64; файл server/terlimo_backend/payments.py из isolated /tmp/codex-referral-contract-20261003 (последующий docs-only commit не менял файл).

- PlategaHttpProvider:189+, create_payment:217+ использует POST /transaction/process либо /v2/transaction/process; get_status:278–300 использует GET /transaction/{id}, оставляет id/status/amount/currency. expiresIn не сохраняет, cancel операции не реализует. Все неоднозначные create ошибки остаются unknown.
- record_webhook:832–911: CANCELED обновляет только pending (877–882); matching CONFIRMED переводит любой ещё не succeeded order в succeeded и применяет entitlement (897–910). Это совместимость с поздним подтверждением, **не доказательство provider finality**.
- reconcile_payments:914–995 выбирает pending и succeeded с незавершённой выдачей (933–937). Локально canceled не перепроверяется. При введении отдельного unresolved-reservation состояния его нужно включить в существующий bounded reconcile независимо от canceled, иначе reserve нельзя разрешить после потерянного callback. Новый worker/core не требуется.

## Минимальный контракт сейчас и точный недостающий факт

**До получения гарантии:** состояние `RESERVED_PENDING_FINALITY` (предложенное внутреннее имя) привязано к исходному account/order/provider ID; consumed=false. Не создавать второй discounted invoice. Клиент сообщает «Проверяем завершение предыдущей оплаты; скидка не списана», не «скидка использована». Повторно сверять тот же ID существующим bounded reconciliation; не выдавать старую expired ссылку как новую, не создавать скрытый replacement. Recovery исходного order/URL допустим только при подтверждённой пригодности исходного checkout, без обещания, что PENDING/локальный TTL сами доказывают пригодность.

- Доказанный локальный отказ ДО provider write (и отсутствие in-flight/unknown/order по существующим fences): резерв можно снять атомарно вместе с постоянной инвалидизацией source quote. Это отдельный доказанный no-invoice путь.
- Matching CONFIRMED: скидка consumed по факту оплаты; immutable order/сумма сохраняются; успешная выдача entitlement отдельно определяет награду inviter. Не отбрасывать поздний валидный paid.
- CANCELED callback, expiry, failedUrl, закрытие браузера, 404, timeout, CHARGEBACKED или HTTP200 cancel: сами по себе не release proof. CHARGEBACKED — возврат уже принятой оплаты, не основание выдумать новую eligibility.

**Один запрос провайдеру, требующий ответа для используемых методов /transaction/process и /v2/transaction/process:**

> Какой authoritative endpoint/ответ для конкретного transactionId гарантирует одновременно (а) окончательное закрытие redirect/QR для новых оплат и (б) отсутствие уже принятой/банковской in-flight оплаты, способной позже стать CONFIRMED? Даёт ли GET status=CANCELED эту гарантию, включая истечение expiresIn? Если нет, каким действием закрыть неоплаченный счёт без возврата уже оплаченного, и какой финальный receipt/статус нужно дождаться? Может ли после такого receipt законно появиться CONFIRMED, относящийся к оплате до закрытия?

Нужен ответ об ordering/settlement, а не только «ссылка истекла» или «CANCELED означает неуспех». Если Platega подтвердит CANCELED как достаточный authoritative final-unpaid результат, минимальная реализация: GET того же ID → проверить exact ID/amount/currency и подтверждённое finality условие → в короткой DB транзакции с согласованным для **всех** writers lock order повторно проверить reservation/order/не-consumed → сохранить durable evidence, инвалидировать старую quote, снять reservation. Callback и release сериализуются; внешний HTTP вне DB transaction. До подтверждения это условный план, не уже доказанный контракт. Если требуется отдельное close действие, сначала уточняется его семантика и безопасный receipt; refund endpoint автоматически не подставляется.

Без этого факта можно реализовать account/candidate/trial и reserve/success ветви, но обещание повторной скидки после отменённого provider checkout остаётся **не завершено**. Безопасное временное состояние не принимается за release-ready feature и не получает выдуманного TTL.

Минимальная последующая проверка реализации: callback CONFIRMED до/после release transaction; повтор canceled/confirmed; GET timeout/stale/foreign ID; concurrent second quote/create; доказанный no-write release; canceled reserve остаётся в reconcile; отмена/refund не вызывается неявно. Здесь проверки не запускались.

Root corrections27 сохраняются: paid reward после actual entitlement apply, trial reward после actual activation, no_order durable invalidation переживает rollback ошибки ответа, единый lock order всех writers. Нового полного referral review не проводилось.
