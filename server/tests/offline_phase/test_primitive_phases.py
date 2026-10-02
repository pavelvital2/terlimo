"""Affected primitive boundaries only; in-memory doubles, no network/DB."""
import asyncio
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import pytest
from terlimo_backend import auth_api, auth_api_phase as diag, pop
spec=importlib.util.spec_from_file_location('phase_fakes',Path(__file__).with_name('test_execution_phases.py'))
f=importlib.util.module_from_spec(spec);spec.loader.exec_module(f)

@pytest.mark.asyncio
async def test_session_order_metadata_and_budget(monkeypatch):
    service,body,db,key=f.session_fixture(monkeypatch)
    calls=[];db.connection.get_server_pid=lambda:calls.append(1) or 1234
    off=await service.create_session(body)
    assert calls==[]
    sink=f.Sink();p=diag.AuthApiPhase('session',sink=sink)
    on=await service.create_session(body,phase=p);p.finish()
    assert on==off and db.active==0
    rows=f.records(sink)[0];names=[r['phase'] for r in rows]
    ordered=['keyload_begin','key_decode_end','key_derparse_end','key_validation_end','keyload_end','proof_verify_begin','payload_decode_end','payload_jsonparse_end','payload_canonical_end','proof_fields_message_end','ecdsa_verify_begin','ecdsa_verify_end','proof_binding_end','proof_verify_end','session_transaction_begin','session_apply_begin','session_apply_end','session_transaction_end','session_pool_release_end']
    assert [names.index(n) for n in ordered]==sorted(names.index(n) for n in ordered)
    assert len(rows)<=42 and p.omitted==0 and len(calls)==3
    metadata=[r['metadata'] for r in rows if 'metadata' in r]
    assert {'algorithm':'p256'} in metadata and {'key_der_bytes':91} in metadata
    assert all(set(m)<= {'algorithm','key_der_bytes','signed_payload_bytes','signature_bytes','pg_backend_pid'} for m in metadata)
    text=''.join(sink.lines)
    for secret in [key,body['proof']['signature_b64'],body['proof']['signed_payload_b64'],'NEVER_LOG_SYNTHETIC_TOKEN']:assert secret not in text

@pytest.mark.asyncio
@pytest.mark.parametrize('failure',['rate','cancel'])
async def test_count_boundary_reject_and_cancel_no_upsert(monkeypatch,failure):
    c=f.FakeConnection();c.get_server_pid=lambda:1234
    async def count(sql,*a):
        c.events.append('count')
        if failure=='cancel':raise asyncio.CancelledError
        return 10
    c.fetchval=count;db=f.FakeDB(c)
    svc=auth_api.InstallationSessionService(SimpleNamespace(challenge_ttl_seconds=30,challenge_rate_limit_per_minute=9,public_challenge_limit_per_minute=9),db)
    sink=f.Sink();p=diag.AuthApiPhase('challenge',sink=sink)
    error=asyncio.CancelledError if failure=='cancel' else auth_api.ApiError
    with pytest.raises(error):await svc.create_challenge({'installation_fingerprint':'a'*64,'purpose':'session','environment':'test'},phase=p)
    p.finish();assert c.events==['count'] and db.active==0
    names=[r['phase'] for r in f.records(sink)[0]]
    assert ('rate_count_end' in names)==(failure=='rate')
    assert 'rate_queries_end' not in names

@pytest.mark.asyncio
async def test_successful_count_split(monkeypatch):
    c=f.FakeConnection();c.get_server_pid=lambda:1234;db=f.FakeDB(c)
    svc=auth_api.InstallationSessionService(SimpleNamespace(challenge_ttl_seconds=30,challenge_rate_limit_per_minute=9,public_challenge_limit_per_minute=9),db)
    sink=f.Sink();p=diag.AuthApiPhase('challenge',sink=sink)
    await svc.create_challenge({'installation_fingerprint':'a'*64,'purpose':'session','environment':'test'},phase=p);p.finish()
    names=[r['phase'] for r in f.records(sink)[0]]
    assert names.index('rate_queries_begin')<names.index('rate_count_end')<names.index('rate_queries_end')
    assert c.events==['fetchval','fetchval','execute']

@pytest.mark.asyncio
@pytest.mark.parametrize('bad',['signature','hash','key'])
async def test_invalid_parity_and_callback_failure(monkeypatch,bad):
    svc,body,db,key=f.session_fixture(monkeypatch)
    if bad=='signature':body['proof']['signature_b64']=pop.b64url_encode(b'BAD_SYNTHETIC_SIGNATURE')
    elif bad=='hash':body['proof']['payload_hash']='0'*64
    else:db.connection.spki='broken'
    errors=[]
    for p in [None,diag.AuthApiPhase('session',sink=f.Sink())]:
        with pytest.raises(auth_api.ApiError) as e:await svc.create_session(body,phase=p)
        errors.append(e.value.code)
    assert errors[0]==errors[1] and db.active==0
    def fail(*a,**k):raise OSError('observer only')
    assert pop.load_public_key(key,observer=fail).key_size==256

@pytest.mark.asyncio
async def test_off_no_observer_clock_and_cap_allowlist(monkeypatch):
    svc,body,db,key=f.session_fixture(monkeypatch)
    def forbidden(*a,**k):raise AssertionError('OFF diagnostic work')
    monkeypatch.setattr(pop,'_observe',forbidden)
    monkeypatch.setattr(diag,'_schedstat',forbidden)
    monkeypatch.setattr(diag.time,'monotonic_ns',forbidden)
    monkeypatch.setattr(diag.time,'thread_time_ns',forbidden)
    await svc.create_session(body)
    monkeypatch.undo()
    sink=f.Sink();p=diag.AuthApiPhase('session',sink=sink);p.activate('a'*32)
    for _ in range(100):p.mark('test',metadata={'query':'forbidden','algorithm':'rsa','signature_bytes':True,'pg_backend_pid':1234})
    p.finish();rows=f.records(sink)[0]
    assert len(rows)==64 and rows[-1]['phase']=='truncated'
    assert all(r.get('metadata')=={'pg_backend_pid':1234} for r in rows[:-1])

@pytest.mark.asyncio
async def test_full_proof_callback_errors_do_not_change_result(monkeypatch):
    svc,body,db,spki=f.session_fixture(monkeypatch)
    key=pop.load_public_key(spki)
    proof=body['proof']
    args={n:proof[n] for n in ['signed_payload_b64','payload_hash','signature_b64','request_id','challenge_id','nonce_b64']}
    args.update(expected={},known_top_level=pop.KNOWN_TOP_LEVEL,server_known_fields=pop.KNOWN_TOP_LEVEL)
    def fail(*a,**k):raise ValueError('synthetic observer failure')
    assert pop.verify_proof(key,**args)==pop.verify_proof(key,**args,observer=fail)
    for bad in ['%%%','e30A']:
        outcomes=[]
        for observer in [None,fail]:
            with pytest.raises(pop.PopError) as e:pop.decode_signed_payload(bad,observer=observer)
            outcomes.append(str(e.value))
        assert outcomes[0]==outcomes[1]

def test_wait_observer_safe_projection_only():
    spec=importlib.util.spec_from_file_location('wait_tool',Path(__file__).with_name('observe_pg_wait.py'))
    tool=importlib.util.module_from_spec(spec);spec.loader.exec_module(tool)
    assert tool.project({'state':'active','wait_event_type':'Lock','wait_event':'transactionid','query':'SECRET'})=={'state':'active','wait_type':'Lock','wait_event':'transactionid'}
    assert tool.project({'state':'strange','wait_event_type':None,'wait_event':'bad arbitrary text'})=={'state':'UNKNOWN','wait_type':'UNKNOWN','wait_event':'UNKNOWN'}
