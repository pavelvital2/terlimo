package xyz.terlimo.test
import org.junit.Assert.*
import org.junit.Test
class ServiceDialIOTest {
 private val prefix="dialio: call=1 candidate=1 transport=UDP "
 private val endpoint=prefix+"kind=ENDPOINT hash="+"f".repeat(64)
 private val rx=prefix+"kind=RX rx=1 readerr=0 peer=0 unwrap=0 handoff=1 pipeerr=0 first_ms=10 last_ms=10 late=0"
 private val tx=prefix+"kind=TX tx=1 txerr=0 wraperr=0 readerr=0 invalid=0 limited=0 truncated=0"
 private val record=prefix+"kind=RECORD ct=22 epoch=1 seq=123 len=50 elapsed_ms=10"
 @Test fun strictFixedFields() {
  listOf(endpoint,rx,tx,record,endpoint.replace("f".repeat(64),"NONE")).forEach {assertNotNull(NativeStderrCodes.match(it))}
  listOf(endpoint+" host=private",rx+" secret=private",record.replace("ct=22","ct=25"),record.replace("kind=RECORD","kind=FINISHED"),endpoint.replace("f".repeat(64),"192.0.2.1:3"),rx.replace("late=0","late=2"),prefix+"kind=TX "+"X".repeat(400)).forEach {assertNull(NativeStderrCodes.match(it))}
 }
 @Test fun complete76LineBatchFitsSharedDialBudget() {
  val m=NativeStderrMirror()
  repeat(64){assertNotNull(m.accept("dialstage: call=1 candidate=1 transport=UDP stage=SOCKET_END result=OK elapsed_ms=1",0))}
  listOf(endpoint,rx,tx).forEach {assertNotNull(m.accept(it,0))};repeat(8){assertNotNull(m.accept(record,0))}
  assertNotNull(m.accept("dialstage: call=1 candidate=0 transport=NONE stage=FINISH result=OK elapsed_ms=20 truncated=1",0))
  assertNull(m.accept(endpoint,0));assertNotNull(m.accept("svcstage: ESTABLISH_DIAL_READY",0));assertNotNull(m.accept(endpoint,10_000_000_000L))
 }
}
