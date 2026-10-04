import asyncio
import json
import uuid
from dataclasses import replace
from pathlib import Path

import pytest
from terlimo_backend.direct_delivery_client import Request, DirectDeliveryClient, DeliveryError, parse_response, LIMIT
from tools.wdtt_direct_adapter import DirectAdapter, AdapterError
# Reuse accepted fake admin without executing its accepted tests.
from tests.adapter_offline.test_wdtt_direct_v3 import Fake, claim, apply, read, stamp, KEY, H, Crash


def request(value, base=None):
    return Request(json.dumps(value, sort_keys=True, separators=(',', ':')).encode(), base)


class Wire:
    def __init__(self, adapter, fragment=17, transform=lambda x:x, stalled=False):
        self.adapter=adapter;self.fragment=fragment;self.transform=transform
        self.stalled=stalled;self.closed=False;self.sent=None
    async def open(self, *, path):
        return self,self
    def write(self, body):
        self.sent=bytes(body)
        try:
            response=self.adapter.execute(json.loads(body))
        except AdapterError as error:
            response={'version':3,'status':'error','code':error.code}
        self.remaining=self.transform(json.dumps(response,separators=(',', ':')).encode()+b'\n')
    async def drain(self):pass
    async def read(self,size):
        if self.stalled:await asyncio.Event().wait()
        chunk=self.remaining[:min(size,self.fragment)];self.remaining=self.remaining[len(chunk):]
        return chunk
    def close(self):self.closed=True


@pytest.fixture
def setup(tmp_path):
    backend=Fake();adapter=DirectAdapter(backend,H,state_dir=tmp_path/'state',v3_enabled=True)
    wire=Wire(adapter);client=DirectDeliveryClient(Path('/isolated-fixture.sock'),opener=wire.open)
    yield backend,adapter,wire,client
    adapter.close()


def run(coro):return asyncio.run(coro)


def test_actual_claim_apply_fragmented_replay_and_read(setup):
    f,a,w,c=setup
    cr=request(claim());r=run(c.claim(cr))
    assert r.receipt and not r.historical_fulfilled(cr) and not r.current_usable(cr,now=1900000000)
    intent=request(apply());r=run(c.apply(intent))
    assert w.sent==intent.body+b'\n' and r.body_digest==intent.digest
    assert r.historical_fulfilled(intent) and r.current_usable(intent,now=1900000000)
    artifact=r.artifact_for_projection()
    assert artifact and artifact not in repr(r) and artifact not in repr(r.receipt)
    assert intent.body.decode() not in repr(intent)
    replay=run(c.apply(intent));assert replay.receipt==r.receipt and f.writes==['expiry']
    observed=run(c.read(request(read()),expected=intent));assert observed.receipt==r.receipt


def test_actual_historical_receipt_newer_disable_and_natural_expiry(setup,monkeypatch):
    f,a,w,c=setup;run(c.claim(request(claim())))
    one=request(apply());original=run(c.apply(one))
    f.row=replace(f.row,status='expired');monkeypatch.setattr('tools.wdtt_direct_adapter.time.time',lambda:2200000000)
    expired=run(c.apply(one));assert expired.receipt==original.receipt
    assert expired.historical_fulfilled(one) and not expired.current_usable(one,now=2200000000)
    monkeypatch.setattr('tools.wdtt_direct_adapter.time.time',lambda:1900000000)
    f.row=replace(f.row,status='active')
    two=request(apply(2,1,'inactive',None),base=one.desired);run(c.apply(two))
    old=run(c.apply(one));assert old.receipt==original.receipt and old.current_revision==2
    assert old.historical_fulfilled(one) and not old.current_usable(one,now=1900000000)
    writes=list(f.writes);run(c.apply(one));assert f.writes==writes


def test_actual_unacked_pending_and_conflict(setup):
    f,a,w,c=setup;run(c.claim(request(claim())));f.uncertain=True
    intent=request(apply());r=run(c.apply(intent));assert r.delivery_state=='pending' and r.receipt is None
    r=run(c.apply(intent));assert r.code=='admin_completion_unknown'
    assert not r.historical_fulfilled(intent) and not r.current_usable(intent,now=1900000000)
    assert f.writes==['expiry']
    f.row=replace(f.row,expires_at=2300000000)
    r=run(c.apply(intent));assert r.delivery_state=='conflict' and not r.current_usable(intent,now=1900000000)


def test_actual_absent_claim_inactive_conflict(tmp_path):
    f=Fake(False);a=DirectAdapter(f,H,state_dir=tmp_path/'state',v3_enabled=True)
    try:
        w=Wire(a);c=DirectDeliveryClient(Path('/fixture.sock'),opener=w.open)
        cr=request(claim('absent',None));run(c.claim(cr))
        intent=request(apply(state='inactive',expiry=None),base=cr.desired)
        r=run(c.apply(intent));assert r.code=='absent_inactive_unsupported' and not f.writes
    finally:a.close()


@pytest.mark.parametrize('change',[
 {'external_key':'other'}, {'grant_id':str(uuid.uuid4())}, {'fence_id':str(uuid.uuid4())},
 {'operation_id':str(uuid.uuid4())}, {'body_digest':'0'*64}, {'accepted_revision':0},
 {'current_revision':True}, {'accepted_revision':2**63}, {'desired':{'state':'active','expires_at':2200000000}},
 {'extra':1}, {'version':True}, {'current':{'state':'inactive','expires_at':None}},
 {'artifact':False}, {'receipt':None},
])
def test_foreign_and_malformed_actual_response(setup,change):
    f,a,w,c=setup;a.execute(claim());intent=request(apply());v=a.execute(json.loads(intent.body))
    v.update(change)
    with pytest.raises(DeliveryError) as error:parse_response(json.dumps(v).encode(),intent)
    assert error.value.code=='direct_wire_invalid'


@pytest.mark.parametrize('field,value',[('revision',True),('expected_revision','0'),('revision',2**63),('desired_state','absent'),('expires_at','2036-01-01T00:00:00.5Z'),('extra',1)])
def test_request_closed_numeric_types(field,value):
    v=apply();v[field]=value
    with pytest.raises(DeliveryError):request(v)


@pytest.mark.parametrize('body',[b'{',b'\xff',b'{"version":3,"version":3}',b'NaN',b'{} trailing'])
def test_malformed_raw_bytes(body):
    with pytest.raises(DeliveryError) as error:Request(body)
    assert str(error.value)=='direct_wire_invalid' and error.value.__context__ is None


@pytest.mark.parametrize('boundary',['eof','partial-eof','oversize','trailing','timeout','cancel'])
def test_transport_boundaries_after_actual_send(setup,boundary,monkeypatch):
    f,a,w,c=setup;a.execute(claim());intent=request(apply())
    if boundary=='eof':w.transform=lambda _:b''
    if boundary=='partial-eof':w.transform=lambda v:v[:-1]
    if boundary=='oversize':w.transform=lambda _:b'x'*(LIMIT+1)
    if boundary=='trailing':w.transform=lambda v:v+b'{}\n'
    if boundary in ('timeout','cancel'):w.stalled=True
    monkeypatch.setattr('terlimo_backend.direct_delivery_client.DEADLINE',.01)
    async def attempt():
        task=asyncio.create_task(c.apply(intent))
        if boundary=='cancel':
            await asyncio.sleep(0);task.cancel()
        await task
    with pytest.raises(asyncio.CancelledError if boundary=='cancel' else DeliveryError) as error:run(attempt())
    if boundary!='cancel':assert error.value.unresolved
    assert w.closed and w.sent==intent.body+b'\n' and f.writes==['expiry']


def test_generic_legacy_error_and_missing_artifact(setup):
    f,a,w,c=setup;a.execute(claim());intent=request(apply());v=a.execute(json.loads(intent.body));v['artifact']=''
    r=parse_response(json.dumps(v).encode(),intent);assert r.historical_fulfilled(intent) and not r.current_usable(intent,now=1900000000)
    with pytest.raises(DeliveryError,match='direct_peer_error'):
        parse_response(b'{"version":1,"status":"error","code":"peer_rejected"}',intent)


@pytest.mark.parametrize('field,value',[
 ('operation_id',str(uuid.uuid4())),('digest','0'*64),('external_key','foreign'),
 ('fence_id',str(uuid.uuid4())),('revision',True),('revision',2),
 ('desired',{'state':'active','expires_at':2200000000}),
 ('applied',{'state':'inactive','expires_at':2100000000}),('extra',1),
])
def test_receipt_exact_correlation(setup,field,value):
    f,a,w,c=setup;a.execute(claim());intent=request(apply());v=a.execute(json.loads(intent.body))
    v['receipt'][field]=value
    with pytest.raises(DeliveryError):parse_response(json.dumps(v).encode(),intent)


def test_pending_historical_receipt_does_not_prove_current_delivery(setup):
    f,a,w,c=setup;a.execute(claim());one=request(apply());original=run(c.apply(one))
    f.uncertain=True;two=request(apply(2,1,expiry=2200000000));run(c.apply(two))
    old=run(c.apply(one))
    assert old.receipt==original.receipt and old.delivery_state=='pending'
    assert old.historical_fulfilled(one) and not old.current_usable(one,now=1900000000)


@pytest.mark.parametrize('transform',[
 lambda v:v.replace(b'"version":3',b'"version":3,"version":3',1),
 lambda v:v.replace(b'"accepted_revision":1',b'"accepted_revision":NaN',1),
 lambda v:v+b' trailing',
])
def test_raw_response_invalid_secret_not_in_exception(setup,transform):
    f,a,w,c=setup;a.execute(claim());intent=request(apply());v=a.execute(json.loads(intent.body))
    artifact=v['artifact'];body=json.dumps(v,separators=(',', ':')).encode()
    with pytest.raises(DeliveryError) as error:parse_response(transform(body),intent)
    assert artifact not in str(error.value) and artifact not in repr(error.value)
    assert error.value.__context__ is None


@pytest.mark.parametrize('field,value', [('expires_at','private-fixture-value'),('fence_id','private-fixture-value')])
def test_invalid_request_exception_contains_no_original_context(field,value):
    v=apply();v[field]=value
    with pytest.raises(DeliveryError) as error:request(v)
    assert value not in repr(error.value) and error.value.__context__ is None


def test_current_mismatch_is_not_historical_delivery(setup):
    f,a,w,c=setup;a.execute(claim());intent=request(apply());v=a.execute(json.loads(intent.body))
    v['current']['expires_at']=2200000000
    r=parse_response(json.dumps(v).encode(),intent)
    assert r.historical_fulfilled(intent) and not r.current_usable(intent,now=1900000000)
