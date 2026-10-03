# Recovery v1 — клиентская source-версия, 03.10.2026

> Историческая запись первого source checkpoint `aca327d9`. Описанные ниже
> envelope4 и отложенное ordinary scheduling исправлены последующим
> [Recovery join + cleanup](../recovery-client-join-cleanup-20261003/README.VERSION.md).
> Для текущего cumulative source действует последующий документ.

## База и назначение

Изолированная ветка `laptop/recovery-code-client-20261003`, база
`32cac8dd571f8142c629d137503d00da53cc04a6` канонического `pavelvital2/terlimo`,
Android tree базы `3e21a437b1315ee40cd81050758a7fa886875909`.
Публикует только Руководитель проекта. Точные итоговые commit/tree и SHA cumulative patch
сопровождают передачу; эта версия не является опубликованной или установленной.

Задание `whitelist-20261003-recovery-code-client-source-v1` и frozen Recovery v1
заменяют персональный VK/hash редактор одним подписанным кодом из бота.
Область: исходники и локальные проверки. APK/native Android artifact не собирались,
устройство, установленное приложение, серверы, provider и ключи deployment не изменялись.

## Реализовано

- Один редактор «Восстановить подключение» из настроек и действия при ошибке;
  явные «Применить»/«Отмена». Код не сохраняется в UI-state/autofill и не попадает в сообщения.
- Только доверенный packaged `recovery_verify_key_b64`, Ed25519 public key32 в
  canonical base64url без padding. Отсутствующий/неверный ключ отключает функцию,
  не ломая обычный bootstrap. Deployment key в assets не добавлен.
- `TR1.payload.signature`, ASCII3500 после внешнего trim, точные3 сегмента.
  Подпись проверяется над `TERLIMO-RECOVERY-V1` + NUL + исходные UTF8 payload bytes.
  Закрытая схема9 Seed полей, дубликаты, case aliases, null, чужая среда,
  подпись, устаревшая или конфликтующая revision отклоняются до соединения/записи.
- Проверенный candidate помещается в отдельный memory-only Store и существующий Doer.
  Logical mobile base_url остаётся прежним. Меняются только подписанные physical
  endpoint/pin/VK поля. Защита `SourceUser` не ослаблена.
- Тот же Runner/MobileSession получает нормальную PoP session текущей installation и
  строго декодированный `/me`. Нет enrollment, каталога, sync/grant, оплаты или dataVPN.
  Проверяются installation_ref, subject и неизменная session generation.
- Только после такой готовности один commit в `service_seed_v1` сохраняет cached seed
  и очищает предыдущий user override. Старые slots/revision не изменяются на отказе.
  Equal-revision identical seed — no-op, включая отсутствие очистки override.
- Фактический Android writer держит `RecoveryCommitGate` по attempt/installation
  вокруг существующей encrypted AtomicFile namespace-записи. Stop использует тот же
  guard. Это дополняет Go context, а не подменяется одним mutex Store.
  Подтверждённая запись имеет приоритет над запоздалой ошибкой/отменой в UI.
- Recovery не запускает таймер каталога. Подготовка ограничена прежним15s operation
  budget с существующим CAPTCHA pause; таймауты не расширялись. После результата
  child ждёт обычный host Stop, чтобы EOF не отбросил сообщение actor.
- Работающий VPN/service сначала требует обычного Disconnect. Во время recovery
  основная кнопка отменяет операцию, retained account не предлагает первый Connect.
- Channel сохраняет существующее соединение при смене только hash/revision.
  После Close новое establishment читает новый seed. Смена endpoint/pin не
  переименовывает старое соединение: перед следующим exchange создаётся новое.
- Точный authorized `GET /api/mobile/v1/service-seed` разрешён в клиентском Doer;
  декодируется закрытый envelope4 полей с тем же TR1. Нет поля в `/me`.

## Точная незавершённая совместная часть

**Ordinary once-after-readiness scheduling ещё не подключён.** `Client.GetServiceSeed`
и parser/Store готовы, но вызова из обычного Runner нет. `Channel.Exchange` держит
один mutex на dial+exchange, `Store.CommitRecovery` — mutex через host persist.
Горутина после `/me` или `OnVerified` может занять транспорт/Store раньше каталога,
ручного refresh или first-connect и задержать их. Синхронный GET добавит обязательный
handshake. Это нарушило бы frozen требование «не задерживать каталог».
Нужно свести одноразовый best-effort вызов с существующим владельцем service cycle
и его отменой, сохранив приоритет foreground requests; автоматические retries,
новый HTTP client/daemon и расширение deadline в этой версии не добавлены.
Это предусмотренный контрактом integration seam, а не реализованное автообновление.

Recovery предназначен для текущего mobile bootstrap без сохранённой legacy link.
При legacy link функция явно недоступна и ссылка сохраняется: иначе следующий
обычный запуск выбрал бы legacy mode и проигнорировал сохранённый service seed.
Миграция legacy subscription не выполняется и отдельного mode/identity не создаётся.

Серверный signed producer, bot delivery, два остальных allowlist и trusted TEST key
сводит руководитель с инженером. TEST/phone четыре сценария (dead hash, moved IP,
corrupted code, restart) остаются следующим отдельным этапом после общего source review.
Нет заявления о полном product PASS или проверке на устройстве.

## Локальные доказательства

Все Go команды используют `GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off`.

| Проверка | Результат |
| --- | --- |
| `go test ./servicechannel -run '^TestRecovery' -count=1` до добавления Channel tests | PASS,5 групп; повтор только после exact-case-key исправления |
| `go test ./servicechannel -run '^TestRecoveryChannel' -count=1` | PASS,3 группы, один запуск |
| `go test ./accountaccess -run '^TestRecovery' -count=1` | PASS,7 групп; первый запуск выявил installation_ref и duplicate-key дефекты, исправлены |
| `go test . -run '^TestRecoveryMobileCandidateWiring$' -count=1` | PASS, native constructor/Doer candidate failure, no persist/origin change |
| Gradle `RecoveryCodeUiTest`, `RecoveryCommitGateTest` | PASS,8 тестов; включает writer/cancel latch и чужую installation |
| Gradle `PreAdmissionConnectTest` после UI lifecycle исправлений | PASS, затронутая политика включая запрет Connect при recovery |
| Kotlin production compilation | PASS в обоих targeted JVM запусках |
| `git diff --check` | PASS |

Gradle выполнен offline, max-workers2, JVM heap1536m, с исключением
`:testapp:verifyNativeInput` исключительно для source/JVM проверки; assemble/package
и native artifact build не запускались. Старые CAPTCHA/ping/payment suites не повторялись.
Go harness использует контролируемые localhost HTTP и in-memory transport fixtures.

Shared Python→Go fixture `go_client/servicechannel/testdata/recovery-v1/fixture.json`,
SHA256 `252c43104f5debad67a63489111dff6dc55801e5e2fc7d253d7c09e4009f1d1f`.
`generate_fixture.py` использует существующий Python cryptography и явно TEST-only
детерминированный seed. Повторная генерация byte-identical. Нет настоящего signing key.
Проверены exact original bytes, altered/foreign/stale codes, stage/commit/restart,
failed save/cancel/owner rejection, no enrollment/receipt writes, authorized GET,
hash reuse и endpoint identity. Native wire parser и Android view lifecycle проверены
исходниками/компиляцией и локальными seams; реальный APK путь не заявляется проверенным.

Локальные логи и handoff: `/home/pavel/step036-receipts/recovery-code-client-source-20261003`.
