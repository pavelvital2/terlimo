import asyncio,json,time,unittest,importlib.util
from types import SimpleNamespace
from unittest.mock import patch,AsyncMock
from contextlib import asynccontextmanager
from terlimo_backend import service_relay as relay,auth_api as auth

# One real second scaled to 10ms; production selectors and cancellation execute unchanged.
def frame(path='/api/mobile/v1/auth/challenge',method='POST'):
 return json.dumps(dict(v=1,op='service.http',request_id='a'*32,method=method,path=path,query='',headers={},body_b64='')).encode()
class Writer:
 def __init__(self):self.payload=b'';self.closed=False
 def write(self,b):self.payload+=b
 async def drain(self):await asyncio.sleep(0)
 def close(self):self.closed=True
 async def wait_closed(self):await asyncio.sleep(0)
class Reader:
 def __init__(self,data,delay=0):self.data=data;self.delay=delay
 async def readuntil(self,_):await asyncio.sleep(self.delay);return self.data+b'\n'
 async def read(self,_):await asyncio.Event().wait()
class DB:
 @asynccontextmanager
 async def acquire(self):yield object()
class Tests(unittest.IsolatedAsyncioTestCase):
 def settings(self):return SimpleNamespace(service_timeout_seconds=.15,service_max_concurrency=2,service_relay_allowed_uid=1,service_relay_allowed_gid=-1,environment='TEST')
 def instance(self):
  r=object.__new__(relay.ServiceRelay);r._settings=self.settings();r._tasks=set();r._phase_probe_enabled=False;return r
 async def execute_relay(self,data,delay,frame_delay=0,cancel=False):
  r=self.instance();w=Writer();cancelled=[]
  async def upstream(_):
   try:await asyncio.sleep(delay);return b'{"ok":true}'
   finally:cancelled.append(True)
  r._forward=upstream
  with patch.object(relay,'AUTH_RELAY_BUDGET_SECONDS',.21),patch.object(relay,'_peer_credentials',return_value=(1,1)):
   task=asyncio.create_task(r._handle(Reader(data,frame_delay),w));start=time.monotonic()
   if cancel:await asyncio.sleep(.02);task.cancel()
   try:await task
   except asyncio.CancelledError:
    if not cancel:raise
  self.assertTrue(w.closed);self.assertFalse(r._tasks);self.assertTrue(cancelled)
  return w.payload,time.monotonic()-start
 async def test_relay_old_cutoff_new_late_completion(self):
  spec=importlib.util.spec_from_file_location('terlimo_backend.old_relay','/home/pavel/terlimo-test-a-live/terlimo_backend/service_relay.py')
  old=importlib.util.module_from_spec(spec);spec.loader.exec_module(old)
  r=object.__new__(old.ServiceRelay);r._settings=self.settings();r._tasks=set();r._phase_probe_enabled=False;w=Writer()
  async def late(_):await asyncio.sleep(.16);return b'{"ok":true}'
  r._forward=late
  with patch.object(old,'_peer_credentials',return_value=(1,1)):
   await r._handle(Reader(frame()),w)
  self.assertEqual(w.payload,b'');self.assertTrue(w.closed)
  payload,elapsed=await self.execute_relay(frame(),.16)
  self.assertTrue(payload);self.assertLess(elapsed,.25)
 async def test_relay_over_budget_and_body_share_deadline(self):
  self.assertEqual((await self.execute_relay(frame(),.24))[0],b'')
  # .06 body + .16 upstream exceed .21; frame completion must not renew budget.
  self.assertEqual((await self.execute_relay(frame(),.16,.06))[0],b'')
 async def test_relay_cancel_cleanup(self):
  payload,elapsed=await self.execute_relay(frame(),10,cancel=True)
  self.assertFalse(payload);self.assertLess(elapsed,.1)
 async def test_non_auth_unchanged(self):
  for path in ['/api/mobile/v1/me','/api/mobile/v1/gateways','/api/mobile/v1/payments/'+ 'b'*32]:
   self.assertEqual(relay._frame_budget(self.settings(),frame(path,'GET')),.15)
  self.assertEqual(relay._frame_budget(self.settings(),b'invalid'),.15)
  self.assertEqual((await self.execute_relay(frame('/api/mobile/v1/me','GET'),.16))[0],b'')
 async def test_auth_api_finite_and_cancellation(self):
  for handler,operation in [(auth._handle_challenge,'create_challenge'),(auth._handle_session,'create_session')]:
   for delay,success in [(.16,True),(.24,False)]:
    service=SimpleNamespace();completed=[]
    async def call(*a,**kw):
     try:await asyncio.sleep(delay);return {'ok':True}
     finally:completed.append(True)
    setattr(service,operation,call);request=SimpleNamespace(app={auth.AUTH_SERVICE_KEY:service},headers={})
    with patch.object(auth,'AUTH_REQUEST_BUDGET_SECONDS',.20),patch.object(auth,'_json_body',new=AsyncMock(return_value={})),patch.object(auth,'new_auth_api_phase',return_value=None):
     result=await handler(request)
     self.assertEqual(result.status==200,success);self.assertTrue(completed)
     if not success:self.assertIn('SERVICE_UNAVAILABLE',result.text)
    with patch.object(auth,'AUTH_REQUEST_BUDGET_SECONDS',.20),patch.object(auth,'_json_body',new=AsyncMock(return_value={})),patch.object(auth,'new_auth_api_phase',return_value=None):
     task=asyncio.create_task(handler(request));await asyncio.sleep(.01);task.cancel()
     with self.assertRaises(asyncio.CancelledError):await task
 async def test_outer_api_budget_includes_context_and_replay(self):
  for context_delay,replay_delay,success in [(.02,.16,True),(.07,.16,False),(.02,.24,False)]:
   request=SimpleNamespace(app={relay.SERVICE_ACTIVE_KEY:[0]},transport=SimpleNamespace(get_extra_info=lambda _:None))
   async def resolve(*a):await asyncio.sleep(context_delay)
   async def replay(*a,**kw):await asyncio.sleep(replay_delay);return {'ok':True}
   with patch.object(relay,'AUTH_REQUEST_BUDGET_SECONDS',.20),patch.object(relay,'_read_request_capped',new=AsyncMock(return_value=frame())),patch.object(relay,'new_auth_api_phase',return_value=None),patch.object(relay,'_peer_identities',return_value=[]),patch.object(relay,'resolve_gateway_context',new=resolve),patch.object(relay,'_replay_service_request',new=replay):
    result=await relay._service_handler(self.settings(),DB())(request)
   self.assertEqual(result.status==200,success);self.assertEqual(request.app[relay.SERVICE_ACTIVE_KEY],[0])
 async def test_replay_timeout_exact_auth_only_no_roundup(self):
  seen=[]
  class Session:
   def __init__(self,**kw):seen.append(kw['timeout']);raise RuntimeError('fixture')
  for path,method,expected in [('/api/mobile/v1/auth/challenge','POST',20),('/api/mobile/v1/auth/session','POST',20),('/api/mobile/v1/me','GET',.15)]:
   with patch.object(relay,'ClientSession',Session),patch.object(relay,'TCPConnector',return_value=None):
    with self.assertRaises(RuntimeError):await relay._replay_service_request(self.settings(),{'path':path,'method':method,'headers':{}})
   self.assertAlmostEqual(seen[-1].total,expected,delta=.01)
   self.assertEqual(seen[-1].ceil_threshold,float('inf') if method=='POST' else 5)
 async def test_inherited_api_deadline_cannot_expand_and_shortens(self):
  loop=asyncio.get_running_loop();now=loop.time()
  for value in ['', 'nan', 'inf', str(now+1000)]:
   request=SimpleNamespace(headers={auth.AUTH_DEADLINE_HEADER:value})
   self.assertLessEqual(auth._auth_deadline(request),loop.time()+20)
  inherited=now+.03
  request=SimpleNamespace(headers={auth.AUTH_DEADLINE_HEADER:str(inherited)})
  self.assertEqual(auth._auth_deadline(request),inherited)
  service=SimpleNamespace(create_challenge=AsyncMock(side_effect=lambda *a,**kw:None))
  cleaned=[]
  async def late(*a,**kw):
   try:await asyncio.sleep(.16);return {'ok':True}
   finally:cleaned.append(True)
  service.create_challenge=late;request.app={auth.AUTH_SERVICE_KEY:service}
  with patch.object(auth,'_json_body',new=AsyncMock(return_value={})),patch.object(auth,'new_auth_api_phase',return_value=None):
   result=await auth._handle_challenge(request)
  self.assertEqual(result.status,503);self.assertTrue(cleaned)
 async def test_replay_inherits_absolute_budget_and_header_is_internal(self):
  seen={}
  class Session:
   def __init__(self,**kw):seen['timeout']=kw['timeout']
   @asynccontextmanager
   async def request(self,*a,**kw):
    seen['headers']=kw['headers'];yield SimpleNamespace(headers={},status=200)
   async def close(self):pass
  settings=self.settings();settings.service_upstream_host='127.0.0.1';settings.api_port=1
  parsed=relay._parse_service_request(frame());inherited=asyncio.get_running_loop().time()+.05
  with patch.object(relay,'ClientSession',Session),patch.object(relay,'TCPConnector',return_value=None),patch.object(relay,'_read_capped',new=AsyncMock(return_value=b'{}')):
   result=await relay._replay_service_request(settings,parsed,deadline=inherited)
  self.assertEqual(result['status'],200)
  self.assertEqual(float(seen['headers'][auth.AUTH_DEADLINE_HEADER]),inherited)
  self.assertLessEqual(seen['timeout'].total,.05)
  self.assertNotIn(auth.AUTH_DEADLINE_HEADER,parsed['headers'])
  forged=json.loads(frame());forged['headers']={auth.AUTH_DEADLINE_HEADER:str(inherited)}
  with self.assertRaises(relay.EvidenceTransportError):relay._parse_service_request(json.dumps(forged).encode())
 async def test_forward_uses_auth21_without_roundup(self):
  seen=[]
  class Session:
   @asynccontextmanager
   async def post(self,*a,**kw):
    seen.append(kw['timeout']);yield object()
  r=self.instance();r._settings.evidence_backend_host='fixture';r._settings.evidence_backend_port=1;r._settings.evidence_backend_server_name='';r._session=Session()
  with patch.object(relay,'_read_capped',new=AsyncMock(return_value=b'{}')):
   await r._forward(frame());await r._forward(frame('/api/mobile/v1/me','GET'))
  self.assertEqual(seen[0].total,21);self.assertEqual(seen[0].ceil_threshold,float('inf'))
  self.assertEqual(seen[1].total,.15);self.assertEqual(seen[1].ceil_threshold,5)
if __name__=='__main__':unittest.main(verbosity=2)
