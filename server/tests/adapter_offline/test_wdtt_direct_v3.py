import json,sqlite3,threading,time,uuid
from dataclasses import replace
from datetime import datetime,UTC
from pathlib import Path
import pytest
from tools.wdtt_direct_adapter import (DirectAdapter,AdapterError,PasswordRecord,ServerInfo,GRANT_NAMESPACE,WDTTAdminClient,AdapterServer)
H='A'*43;PASSWORD='ABCDEFGHJKLMNPQR';KEY='minishop:fixture:main';FENCE=str(uuid.uuid4())
def stamp(epoch):return datetime.fromtimestamp(epoch,UTC).isoformat().replace('+00:00','Z')
def identity():return {'version':3,'external_key':KEY,'grant_id':str(uuid.uuid5(GRANT_NAMESPACE,KEY)),'fence_id':FENCE}
def claim(state='active',expiry=2000000000):return identity()|{'operation':'claim','operation_id':str(uuid.uuid4()),'revision':0,'expected_state':state,'expected_expires_at':stamp(expiry) if expiry is not None else None}
def apply(rev=1,previous=0,state='active',expiry=2100000000):return identity()|{'operation':'apply','operation_id':str(uuid.uuid4()),'revision':rev,'expected_revision':previous,'desired_state':state,'expires_at':stamp(expiry) if expiry is not None else None}
def read():return identity()|{'operation':'read'}
class Fake:
 def __init__(self,existing=True):
  self.row=PasswordRecord(PASSWORD,'tlm:'+identity()['grant_id'],'444,555,666','active',2000000000,ServerInfo('192.0.2.1','444,555,666'),H) if existing else None
  self.writes=[];self.fail_details=False;self.uncertain=False;self.enter=None;self.release=None
 def list_records(self):return (self.row,) if self.row else ()
 def details(self,p):
  if self.fail_details:raise AdapterError('wdtt_unavailable',uncertain=True)
  return self.row
 def mutate(self,name,**kw):
  self.writes.append(name);self.row=replace(self.row,**kw)
  if self.enter:self.enter.set();assert self.release.wait(3)
  if self.uncertain:raise AdapterError('wdtt_unavailable',uncertain=True)
 def create(self,marker,expiry,hashes):self.writes.append('create');self.row=PasswordRecord(PASSWORD,marker,'444,555,666','active',expiry,ServerInfo('192.0.2.1','444,555,666'),hashes)
 def set_expiry(self,p,expiry):self.mutate('expiry',expires_at=expiry)
 def activate(self,p):self.mutate('activate',status='active')
 def deactivate(self,p):self.mutate('deactivate',status='deactivated')
 def set_vk_hash(self,p,hashes):self.mutate('hash',vk_hash=hashes)
@pytest.fixture
def fixture(tmp_path):
 f=Fake();a=DirectAdapter(f,H,state_dir=tmp_path/'state',v3_enabled=True)
 yield a,f,tmp_path/'state'
 a.close()
def no_secrets(path):
 for f in path.iterdir():
  if f.is_file():assert PASSWORD.encode() not in f.read_bytes() and b'wdtt://connect' not in f.read_bytes()
class Crash(BaseException):pass

def test_existing_adoption_mismatch_no_mutation(fixture):
 a,f,p=fixture
 with pytest.raises(AdapterError,match='base_mismatch'):a.execute(claim(expiry=2100000000))
 assert not f.writes and a._journal.target(KEY) is None
 result=a.execute(claim());assert result['receipt']['applied']=={'state':'active','expires_at':2000000000} and not f.writes
 no_secrets(p)
def test_new_requires_absence(tmp_path):
 f=Fake(False);a=DirectAdapter(f,H,state_dir=tmp_path/'state',v3_enabled=True)
 try:
  with pytest.raises(AdapterError,match='base_mismatch'):a.execute(claim())
  a.execute(claim('absent',None));assert not f.writes
  r=a.execute(apply());assert r['delivery_state']=='applied' and f.writes==['create']
  no_secrets(tmp_path/'state')
 finally:a.close()
def test_absolute_apply_and_restart_replay(fixture):
 a,f,p=fixture;a.execute(claim());request=apply();r=a.execute(request);assert r['delivery_state']=='applied' and r['current_revision']==1 and f.row.expires_at==2100000000
 assert 'password='+PASSWORD in r['artifact'];receipt=r['receipt'];writes=list(f.writes);a.close()
 b=DirectAdapter(f,H,state_dir=p,v3_enabled=True)
 try:
  replay=b.execute(request);assert replay['receipt']==receipt and f.writes==writes
  assert b.execute(read())['operation_id']==request['operation_id'];no_secrets(p)
 finally:b.close()
def test_conflicting_body_stale_and_deliberate_shortening(fixture):
 a,f,p=fixture;a.execute(claim());one=apply();a.execute(one);writes=list(f.writes)
 with pytest.raises(AdapterError,match='operation_conflict'):a.execute(one|{'expires_at':stamp(2200000000)})
 with pytest.raises(AdapterError,match='revision_conflict'):a.execute(apply(1,0))
 assert f.writes==writes
 r=a.execute(apply(2,1,expiry=2050000000));assert r['current_revision']==2 and f.row.expires_at==2050000000
 # Replay historic operation is immutable, but separately projects latest target.
 historical=a.execute(one);assert historical['receipt']['revision']==1 and historical['current_revision']==2

def test_old_active_after_new_disable_never_reactivates(fixture):
 a,f,p=fixture;a.execute(claim());one=apply();a.execute(one);two=apply(2,1,'inactive',None);r=a.execute(two);assert r['current']['state']=='inactive'
 writes=list(f.writes);r=a.execute(one);assert r['receipt']['desired']['state']=='active' and r['current']['state']=='inactive' and r['current_revision']==2 and r['artifact']==''
 with pytest.raises(AdapterError,match='revision_conflict'):a.execute(apply(1,0))
 assert f.writes==writes

def test_crash_after_intent_before_io(fixture,monkeypatch):
 a,f,p=fixture;a.execute(claim());request=apply()
 monkeypatch.setattr(a,'_recover_v3',lambda *args:(_ for _ in ()).throw(Crash()))
 with pytest.raises(Crash):a.execute(request)
 assert not f.writes;a.close();b=DirectAdapter(f,H,state_dir=p,v3_enabled=True)
 try:assert b.execute(request)['delivery_state']=='applied' and f.writes==['expiry']
 finally:b.close()
def test_crash_after_ack_before_receipt(fixture,monkeypatch):
 a,f,p=fixture;a.execute(claim());request=apply()
 monkeypatch.setattr(a._journal,'complete',lambda *args:(_ for _ in ()).throw(Crash()))
 with pytest.raises(Crash):a.execute(request)
 assert f.writes==['expiry'];a.close();b=DirectAdapter(f,H,state_dir=p,v3_enabled=True)
 try:assert b.execute(request)['delivery_state']=='applied' and f.writes==['expiry']
 finally:b.close()
def test_uncertain_mutation_blocks_successor_and_does_not_fabricate_ack(fixture):
 a,f,p=fixture;a.execute(claim());f.uncertain=True;request=apply();r=a.execute(request)
 assert r['delivery_state']=='pending' and r['receipt'] is None
 with pytest.raises(AdapterError,match='operation_pending'):a.execute(apply(2,1))
 f.uncertain=False;a.close();b=DirectAdapter(f,H,state_dir=p,v3_enabled=True)
 try:
  r=b.execute(request);assert r['delivery_state']=='pending' and r['code']=='admin_completion_unknown' and f.writes==['expiry']
 finally:b.close()
def test_readback_failure_after_partial_then_same_intent(fixture,monkeypatch):
 a,f,p=fixture;a.execute(claim());request=apply();orig=f.set_expiry
 def partial(*args):orig(*args);f.fail_details=True
 monkeypatch.setattr(f,'set_expiry',partial);r=a.execute(request);assert r['delivery_state']=='pending' and r['receipt'] is None
 f.fail_details=False;r=a.execute(request);assert r['delivery_state']=='applied' and f.writes==['expiry']

def test_foreign_readback_conflict_blocks_successor(fixture):
 a,f,p=fixture;a.execute(claim());f.row=replace(f.row,expires_at=2300000000)
 with pytest.raises(AdapterError,match='target_readback_conflict'):a.execute(apply())
 assert not f.writes

def test_historical_receipt_after_expiry_no_ensure(fixture,monkeypatch):
 a,f,p=fixture;a.execute(claim());request=apply();r=a.execute(request);receipt=r['receipt'];writes=list(f.writes)
 f.row=replace(f.row,status='expired');monkeypatch.setattr('tools.wdtt_direct_adapter.time.time',lambda:2200000000)
 r=a.execute(request);assert r['receipt']==receipt and r['current']['state']=='inactive' and r['artifact']=='' and f.writes==writes

def test_claimed_v1_mutations_off_restart_and_unclaimed_preserved(fixture):
 a,f,p=fixture
 a.execute({'version':1,'operation':'ensure','subscription_id':KEY,'expires_at':stamp(2100000000)});assert f.writes==['expiry']
 a.execute(claim(expiry=2100000000));writes=list(f.writes);a.close();b=DirectAdapter(f,H,state_dir=p,v3_enabled=False)
 try:
  for op in ('ensure','disable'):
   request={'version':1,'operation':op,'subscription_id':KEY}
   if op=='ensure':request['expires_at']=stamp(2200000000)
   with pytest.raises(AdapterError,match='target_claimed'):b.execute(request)
  assert b.execute({'version':1,'operation':'get','subscription_id':KEY})['state']=='active' and f.writes==writes
  with pytest.raises(AdapterError,match='v3_disabled'):b.execute(read())
 finally:b.close()

def test_unclaimed_v1_disable_and_v2_forwarding(fixture):
 a,f,p=fixture;assert a.execute({'version':1,'operation':'disable','subscription_id':KEY})['state']=='deactivated'
 class V2(WDTTAdminClient):
  def __init__(self):self.calls=[]
  def client_test(self,command):self.calls.append(command);return {'fixture':True}
 admin=V2();b=DirectAdapter(admin,H)
 command={'operation':'grant_get','opaque':'unchanged'}
 assert b.execute({'version':2,'operation':'client_test','command':command})=={'version':2,'status':'ok','result':{'fixture':True}} and admin.calls==[command]

def test_concurrent_v1_writer_waits_for_v3_claim_fence(fixture):
 a,f,p=fixture;a.execute(claim());request=apply();f.enter=threading.Event();f.release=threading.Event();result=[]
 t=threading.Thread(target=lambda:result.append(a.execute(request)));t.start();assert f.enter.wait(2)
 done=threading.Event();errors=[]
 def legacy():
  try:a.execute({'version':1,'operation':'disable','subscription_id':KEY})
  except AdapterError as e:errors.append(e.code)
  finally:done.set()
 old=threading.Thread(target=legacy);old.start();assert not done.wait(.05);f.release.set();t.join(3);old.join(3)
 assert result[0]['delivery_state']=='applied' and errors==['target_claimed'] and f.writes==['expiry']

def test_corrupt_unavailable_journal_and_single_process_lock(fixture,tmp_path):
 a,f,p=fixture
 with pytest.raises(AdapterError,match='journal_unavailable'):DirectAdapter(f,H,state_dir=p,v3_enabled=True)
 a.close();(p/'delivery.sqlite3').write_bytes(b'corrupt')
 with pytest.raises(AdapterError,match='journal_unavailable'):DirectAdapter(f,H,state_dir=p,v3_enabled=True)

def test_absent_inactive_is_honest_unresolved(tmp_path):
 f=Fake(False);a=DirectAdapter(f,H,state_dir=tmp_path/'state',v3_enabled=True)
 try:
  a.execute(claim('absent',None));r=a.execute(apply(state='inactive',expiry=None))
  assert r['delivery_state']=='conflict' and r['code']=='absent_inactive_unsupported' and not f.writes
 finally:a.close()

@pytest.mark.parametrize('change',[{'revision':True},{'expected_revision':'0'},{'account_ref':'foreign'},{'grant_id':str(uuid.uuid4())},{'expires_at':'2036-01-01T00:00:00.5Z'},{'desired_state':'revoked'},{'version':True}])
def test_v3_closed_identity_types(fixture,change):
 a,f,p=fixture;a.execute(claim())
 with pytest.raises(AdapterError):a.execute(apply()|change)
 assert not f.writes

def test_v3_wire_metadata_fixture_secret_exclusion(fixture):
 a,f,p=fixture;initial=claim();a.execute(initial);request=apply();result=a.execute(request)
 assert result['receipt']['operation_id']==request['operation_id'] and result['receipt']['digest']==result['body_digest']
 # The live artifact is intentionally excluded from all saved evidence.
 result={k:v for k,v in result.items() if k!='artifact'}
 out=Path(__file__).parents[4]/'wire-fixtures.safe.json'
 out.write_text(json.dumps({'request':request,'response_metadata':result,'artifact_field':'required string, protected runtime only; omitted from fixture'},indent=2)+'\n')
 no_secrets(p)

def test_existing_peer_and_framing_offline(fixture):
 import struct
 a,f,p=fixture
 class Connection:
  def __init__(self,payload,uid=10001,gid=10001):self.payload=payload;self.uid=uid;self.gid=gid;self.sent=None
  def getsockopt(self,*args):return struct.pack('3i',123,self.uid,self.gid)
  def settimeout(self,*args):pass
  def recv(self,*args):value=self.payload;self.payload=b'';return value
  def sendall(self,body):self.sent=json.loads(body)
 server=AdapterServer(a,10001,10001)
 for payload,uid,code in [(b'{}\n',0,'peer_rejected'),(b'{"version":1,"version":1}\n',10001,'request_invalid'),(b'{}\ntrailing',10001,'request_invalid')]:
  c=Connection(payload,uid);server._handle(c);assert c.sent['code']==code
 assert not f.writes


@pytest.mark.parametrize('response', [
 b'', b'{', b'{"ok":', b'\xff', b'{"ok":true,"ok":true}',
 b'{}', b'{"ok":"true"}', b'[]', b'x'*(2*1024*1024+1),
], ids=['empty','malformed','truncated','bad-utf8','duplicate','missing-ok',
        'invalid-ok','invalid-schema','oversized'])
def test_a1_invalid_actual_admin_response_remains_issued(tmp_path,monkeypatch,response):
 backend=Fake()
 class Client(WDTTAdminClient):
  def list_records(self):return backend.list_records()
  def details(self,password):return backend.details(password)
 class Transport:
  def __init__(self,*args):self.remaining=response
  def __enter__(self):return self
  def __exit__(self,*args):pass
  def settimeout(self,*args):pass
  def connect(self,*args):pass
  def sendall(self,body):
   args=json.loads(body)['args']
   assert args[:1]==['set-expiry']
   backend.mutate('expiry',expires_at=int(args[args.index('--expires-at')+1]))
  def recv(self,size):
   chunk=self.remaining[:size];self.remaining=self.remaining[size:];return chunk
 monkeypatch.setattr('tools.wdtt_direct_adapter.socket.socket',Transport)
 admin=Client(bytearray(b'synthetic-admin'));path=tmp_path/'state'
 a=DirectAdapter(admin,H,state_dir=path,v3_enabled=True);request=apply()
 try:
  a.execute(claim());result=a.execute(request)
  assert result['delivery_state']=='pending' and result['code']=='wdtt_response_invalid'
  assert result['receipt'] is None
  assert a._journal.operation(request['operation_id'])['step']=='issued'
  assert backend.row.expires_at==2100000000 and backend.writes==['expiry']
 finally:a.close()
 b=DirectAdapter(admin,H,state_dir=path,v3_enabled=True)
 try:
  result=b.execute(request)
  assert result['delivery_state']=='pending' and result['code']=='admin_completion_unknown'
  assert result['receipt'] is None
  assert b._journal.operation(request['operation_id'])['step']=='issued'
  with pytest.raises(AdapterError,match='operation_pending'):b.execute(apply(2,1))
  assert backend.writes==['expiry'];no_secrets(path)
 finally:b.close()


@pytest.mark.parametrize('boundary',['natural','foreign-expiry','issued'])
def test_j1_saved_intent_restart_after_base_expiry(tmp_path,monkeypatch,boundary):
 backend=Fake();path=tmp_path/'state';request=apply()
 monkeypatch.setattr('tools.wdtt_direct_adapter.time.time',lambda:1900000000)
 a=DirectAdapter(backend,H,state_dir=path,v3_enabled=True)
 try:
  a.execute(claim())
  monkeypatch.setattr(a,'_recover_v3',lambda *args:(_ for _ in ()).throw(Crash()))
  with pytest.raises(Crash):a.execute(request)
  saved=a._journal.operation(request['operation_id'])
  assert saved['step']=='none' and saved['phase']=='pending' and not backend.writes
  digest=saved['digest']
  if boundary=='issued':a._journal.step(saved,'issued')
 finally:a.close()
 monkeypatch.setattr('tools.wdtt_direct_adapter.time.time',lambda:2050000000)
 backend.row=replace(backend.row,status='expired',expires_at=2000000001 if boundary=='foreign-expiry' else 2000000000)
 b=DirectAdapter(backend,H,state_dir=path,v3_enabled=True)
 try:
  with pytest.raises(AdapterError,match='operation_pending'):b.execute(apply(2,1))
  result=b.execute(request)
  saved=b._journal.operation(request['operation_id'])
  assert saved['digest']==digest and saved['operation_id']==request['operation_id']
  assert b._journal.target(KEY)['accepted_revision']==1
  if boundary=='natural':
   assert result['delivery_state']=='applied' and result['current_revision']==1
   assert result['receipt']['operation_id']==request['operation_id']
   assert result['receipt']['revision']==1 and result['receipt']['digest']==digest
   assert backend.writes==['expiry','activate']
   assert result['current']=={'state':'active','expires_at':2100000000}
   assert b.execute(apply(2,1,expiry=2200000000))['current_revision']==2
   assert backend.writes==['expiry','activate','expiry']
  else:
   assert result['delivery_state']==('pending' if boundary=='issued' else 'conflict')
   assert result['code']==('admin_completion_unknown' if boundary=='issued' else 'target_readback_conflict')
   assert result['receipt'] is None and not backend.writes
   assert saved['step']==('issued' if boundary=='issued' else 'none')
   with pytest.raises(AdapterError,match='operation_pending'):b.execute(apply(2,1))
  no_secrets(path)
 finally:b.close()
