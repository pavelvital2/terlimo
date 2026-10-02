# SOURCE/OFFLINE: execution-state phases

Инженер Белых списков. Поручение whitelist-20261002-auth-phase-execution-state-prepare-v1 выполнено в isolated checkout /tmp/terlimo-auth-phase-exec-20261002. Проверенный deployed dirty-source snapshot:80db503b1110208037bc59e704ea9e4160cce5f7, candidate282c78fc9bba9f5078b9c4b7030ad5c9dfc44539. Только3 runtime source files и2 offline fixtures; точный delta в phase-execution.patch. Локальный commit, publisher/root/canonical pavelvital2/terlimo остаётся единственным publisher; push не выполнялся.

## Что изменено

- Existing AuthApiPhase теперь копит до63 samples+1truncation в одном запросе и один раз вызывает существующий logger.info после cleanup. Тот же механизм расширен на inner session, outer service challenge/session, существующий relay ContextVar/task и TraceConfig. Новых очередей, sampler/daemon/collector нет. Sink failures подавлены, business exceptions/cancellation не заменены диагностикой.
- Каждый sample: monotonic_ns wall/elapsed, thread_time_ns, nativeTID, bounded read максимум129characters /proc/thread-self/schedstat. Runtime/runqueue/slices доступны при корректном ненулевом runtime; unreadable/missing/disabled/zero malformed→UNKNOWN/null. ИзменениеTID→UNKNOWN_CHANGED_TID, CPU/sched null; unavailable threadclock→UNKNOWN. Нулевой runqueue при ненулевом runtime означает доступный счётчик без накопленного ожидания; не делает I/O выводов.
- OFF до new clock/TID/proc/sample/log; текущий env fastpath остаётся. Relay OFF сохраняет существующий _phase guard. Diagnostic failure не меняет response/status/exception/cancel/child cleanup.
- API session: validate→challengepool/fetch/release→preview→keypool/fetch/release→keyload→ES256verify→sessionpool→transaction/apply→poolrelease→response. Existing challenge phase boundaries сохранены. Outer service: body/parse→peer→gatewaycontext pool→replay→response. Existing relay frame/forward/queue/DNS/connect/headers/response/drain phase names сохранены.
- Scope service = validated outer frame RID; challenge = validated/generated challenge response RID; session = validated proof RID. Эти namespace отдельны. Для relay локальный task_id и request_id=UNKNOWN; парсинг payload и wireheaders не добавлены. Parent context наследуется _forward/TraceConfig дочерним task. Exact relay↔outer↔inner join не утверждается; внешняя parse correlation без доказательства совпадения с inner не является exactjoin.
- Поля логов фиксированы, request_id только32lowerhex, phase/outcome ограничены. Нет body/SPKI/signature/token/header/SQLvalue/address логирования. API без validatedRID отбрасывает prevalidation samples; послеcap counterreads не выполняются. UTC служит сопоставлению со saved logs, дельты вычисляются из monotonic только внутри процесса.

## Проверки

35 targeted offline tests PASS на точном candidate: fakeclock+fakeawait2s с нулевымCPU, syncCPUgrowth, runqueuegrowth, unavailablecounter; changedTID; permission/zero/malformedcounter; actual fakeDB challenge/session+синтетическая ES256verify; scoped concurrentRIDs/context; ON/OFFresponse parity и payload exclusion; outer session/challenge exception/cancel; actual relay _handle/TraceConfig success/error/child-parentcancel cleanup; sink failure; bounded64records/idempotentfinish; OFF clocks/proc/logs forbidden. No Postgres/server/socket/backend fixture. Syntax compile и diffcheckPASS. Итог в targeted-tests.txt.

Measured local overhead:200 alternating requests ON/OFF per sink,20marks/10fakeawait, real thread/proc counters. Filtered logger ON median0.764610ms vsOFF0.032180ms (delta0.732430ms); real standard StreamHandler temp-file flush ON1.180102ms vsOFF0.046998ms (delta1.133104ms), ONp953.263927ms. Один logwrite/request, безfsync/journald. Не нулевой overhead и не оценка production/historical latency. Измеритель приложен, overhead.json сохраняет numbers/limitations.

Thread counters принадлежат общему asyncio native thread, содержат другие задачи. Wall-CPU-runqueue не вычисляется как I/Owait; steal/ptrace и недоступные counters не локализуются этим patch. Причина прошлых1.5985s/2.4008s не доказана. Patch обеспечивает наблюдение boundaries будущего разрешённого обычного запроса; никаких новых phone/capture/run этим этапом не выполнено.

## Сохранение состояния

Все9 скопированных deployed source/passport hashes совпадают с исходным baseline после offline stage. README.BASELINE.VERSION.md — полный исходный101-ID паспорт с историческими статусами, bytes неизменны; новой продуктовой приёмки нет. Runtime apply/flags/restarts/backend/DB/phone/ptrace/capture/production не выполнялись. Это source/offlinePASS, не product/catalog/livePASS.
