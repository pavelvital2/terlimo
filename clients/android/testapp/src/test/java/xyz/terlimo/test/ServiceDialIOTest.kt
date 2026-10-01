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
 @Test fun outgoingHeaderBoundariesOnly() {
  val dat=prefix+"kind=TXDAT dir=TX id=1 pipe_us=1 begin_us=2 end_us=3 n=49 result=OK invalid=false limited=false late=false"
  val rec=prefix+"kind=TXREC dir=TX id=1 record=0 ct=22 epoch=1 seq=123 len=50 hs=UNKNOWN"
  val hs=prefix+"kind=TXHS dir=TX id=1 record=0 type=1 message_seq=7 offset=5 length=12 total=50"
  listOf(dat,rec,hs,prefix+"kind=TXBOUND seen=6 kept=6 omitted_datagrams=0 omitted_headers=0 truncated=false",dat.replace("end_us=3 n=49 result=OK","end_us=UNKNOWN n=UNKNOWN result=UNKNOWN")).forEach {assertNotNull(NativeStderrCodes.match(it))}
  listOf(dat+" cookie=private",rec.replace("UNKNOWN","Finished"),hs+" payload=private",dat.replace("dir=TX","dir=RX"),dat.replace("result=OK","result=private error")).forEach {assertNull(NativeStderrCodes.match(it))}
  val m=NativeStderrMirror()
  repeat(64){assertNotNull(m.accept(dat,0))}
  assertNull(m.accept(rec,0))
  repeat(76){assertNotNull(m.accept(rx,0))}
  assertNull(m.accept(rx,0))
 }
 @Test fun generatedCombinedWorstCaseAndIndependentOverflow() {
  val fixtureOverride=System.getenv("DIAL_TX_FIXTURE")
  val lines=if(fixtureOverride!=null) java.io.File(fixtureOverride).readLines()
      else requireNotNull(javaClass.getResourceAsStream("/dial-tx-generated-worstcase.log"))
          .bufferedReader().use { it.readLines() }
  assertEquals(140,lines.size)
  val m=NativeStderrMirror()
  lines.forEach { assertNotNull(it,m.accept(it,0)) }
  assertEquals(32,lines.count {it.contains("kind=TXDAT ")})
  assertTrue(lines.any {it.contains("kind=TXBOUND seen=40 kept=32 omitted_datagrams=8 omitted_headers=481 truncated=true")})
  listOf("kind=RX ","kind=ENDPOINT ","stage=FINISH ").forEach {field->assertTrue(lines.any {it.contains(field)})}
  val txLine=lines.first {it.contains("kind=TXDAT ")}
  assertNull(m.accept(txLine,0));assertNull(m.accept(rx,0))
  assertNotNull(m.accept("svcstage: ESTABLISH_DIAL_READY",0))
  repeat(7){assertNotNull(m.accept("accountaccess: TRANSPORT",0))}
  assertNotNull(m.accept("onboarding: ONBOARDING_TIMEOUT",0))
  val oldLine=lines.first {it.contains("stage=SOCKET_END ")}
  for(i in 1..3){lines.forEach {assertNotNull(it,m.accept(it,i*10_000_000_000L))}}
  assertNull(m.accept(txLine,40_000_000_000L));assertNull(m.accept(oldLine,40_000_000_000L))
  val txOnly=NativeStderrMirror()
  repeat(64){assertNotNull(txOnly.accept(txLine,0))}
  assertNull(txOnly.accept(txLine,0))
  lines.filter {!(it.contains("kind=TXBOUND ")||it.contains("kind=TXDAT ")||it.contains("kind=TXHS ")||it.contains("kind=TXREC "))}.forEach {assertNotNull(it,txOnly.accept(it,0))}
 }
}
