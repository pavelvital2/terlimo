"""W1 affected real PG workers, accepted adapter/client and fake Unix only."""
import asyncio
from dataclasses import replace
from uuid import UUID

import pytest
from test_external_delivery import environment,close,claims,next_paid
from terlimo_backend.delivery_plan import obj
from terlimo_backend.external_delivery import (freeze_and_enqueue,DEPENDENCY_WAIT,APPLY,fulfillment_proof)
from terlimo_backend.worker import OutboxWorker
from tools.wdtt_direct_adapter import AdapterError

pytestmark=pytest.mark.asyncio


@pytest.mark.parametrize('completion_before_finalization',[False,True])
async def test_w1_proof_releases_dependency_without_manual_status_changes(migrated_url,settings_factory,tmp_path,monkeypatch,completion_before_finalization):
 e=await environment(migrated_url,settings_factory,tmp_path,base='active')
 c,s,a,oid,eid,first,body,allow,record,adapters,backends,wire,db,h,worker=e
 response_ready=asyncio.Event();release_response=asyncio.Event();wait_parked=asyncio.Event();finalize_wait=asyncio.Event()
 tasks=[]
 try:
  await claims(e);await freeze_and_enqueue(c,first['id'],UUID(body['id']))
  client=h.clients['offline-test'];original=client.apply
  async def hold_success(req):
   result=await original(req)
   if req.revision==2147483649:
    response_ready.set();await release_response.wait()
   return result
  monkeypatch.setattr(client,'apply',hold_success)
  first_task=asyncio.create_task(worker.run_once());tasks.append(first_task)
  await asyncio.wait_for(response_ready.wait(),3)
  second=await next_paid(c,s);await freeze_and_enqueue(c,second['id'],UUID(body['id']))
  handlers=h.as_handlers()
  async def hold_wait_finalization(connection,operation):
   result=await h.apply(connection,operation)
   if result==('failed',DEPENDENCY_WAIT):
    wait_parked.set()
    if completion_before_finalization:await finalize_wait.wait()
   return result
  handlers[APPLY]=hold_wait_finalization
  second_worker=OutboxWorker(db,s,handlers=handlers,worker_id='w1-second')
  wait_task=asyncio.create_task(second_worker.run_once());tasks.append(wait_task)
  await asyncio.wait_for(wait_parked.wait(),3)
  item=await c.fetchrow('SELECT * FROM delivery_items WHERE fulfillment_id=$1',second['id'])
  parked=await c.fetchrow('SELECT * FROM outbox_operations WHERE id=$1',item['outbox_id'])
  assert parked['status']=='failed' and parked['last_error']==DEPENDENCY_WAIT and parked['claim_token'] is None
  assert not item['dispatch_started'] and item['wire'] is None
  if not completion_before_finalization:await wait_task
  release_response.set();await first_task
  released=await c.fetchrow('SELECT * FROM outbox_operations WHERE id=$1',item['outbox_id'])
  assert released['status']=='pending' and released['last_error'] is None
  finalize_wait.set();await wait_task
  # Late original finalization must not erase the proof-driven pending wakeup.
  assert await c.fetchval('SELECT status FROM outbox_operations WHERE id=$1',item['outbox_id'])=='pending'
  await second_worker.run_once()
  applied=await c.fetchrow('SELECT * FROM delivery_items WHERE id=$1',item['id'])
  assert applied['operation_id']==item['operation_id'] and applied['expected_revision']==2147483649
  assert applied['delivery_revision']==2147483650 and applied['application_receipt'] is not None
  assert next(iter(backends.values())).writes==['expiry','expiry']
  assert (await fulfillment_proof(c,second['id']))['transport_complete']
 finally:
  release_response.set();finalize_wait.set()
  if tasks:await asyncio.gather(*tasks,return_exceptions=True)
  await close(e)


async def test_w1_saved_ack_readback_failure_uses_bounded_same_wire_retry(migrated_url,settings_factory,tmp_path,monkeypatch):
 e=await environment(migrated_url,settings_factory,tmp_path,base='active')
 try:
  c=e[0];await claims(e);await freeze_and_enqueue(c,e[5]['id'],UUID(e[6]['id']))
  f=next(iter(e[10].values()));set_expiry=f.set_expiry;details=f.details;fail=False
  def mutation(*args):
   nonlocal fail
   set_expiry(*args);fail=True
  def readback(password):
   nonlocal fail
   if fail:
    fail=False;raise AdapterError('wdtt_readback_failed')
   return details(password)
  monkeypatch.setattr(f,'set_expiry',mutation);monkeypatch.setattr(f,'details',readback)
  await e[14].run_once()
  item=await c.fetchrow('SELECT * FROM delivery_items');outbox=await c.fetchrow('SELECT * FROM outbox_operations WHERE id=$1',item['outbox_id'])
  assert item['application_receipt'] is None and item['outcome']=='pending'
  assert obj(item['observation'])['code']=='wdtt_readback_failed'
  assert outbox['status']=='pending' and outbox['last_error']=='AcknowledgedReadbackRetry'
  assert next(iter(e[9].values()))._journal.operation(str(item['operation_id']))['step']=='acked'
  # Existing worker's own bounded backoff, no reset_claim/status/available_at UPDATE.
  delay=await c.fetchval('SELECT greatest(0,extract(epoch from available_at-clock_timestamp())) FROM outbox_operations WHERE id=$1',outbox['id'])
  await asyncio.sleep(float(delay)+.05)
  await e[14].run_once();applied=await c.fetchrow('SELECT * FROM delivery_items')
  assert applied['wire']==item['wire'] and applied['operation_id']==item['operation_id'] and applied['delivery_revision']==item['delivery_revision']
  assert applied['application_receipt'] is not None and f.writes==['expiry']
  assert await c.fetchval('SELECT status FROM outbox_operations WHERE id=$1',outbox['id'])=='done'
 finally:await close(e)


@pytest.mark.parametrize('boundary',['unacked','conflict'])
async def test_w1_terminal_or_unacked_predecessor_never_releases_successor(migrated_url,settings_factory,tmp_path,monkeypatch,boundary):
 e=await environment(migrated_url,settings_factory,tmp_path,base='active')
 try:
  c=e[0];await claims(e);await freeze_and_enqueue(c,e[5]['id'],UUID(e[6]['id']))
  f=next(iter(e[10].values()))
  if boundary=='unacked':f.uncertain=True
  else:
   original=f.set_expiry
   def foreign(*args):original(*args);f.row=replace(f.row,expires_at=2300000000)
   monkeypatch.setattr(f,'set_expiry',foreign)
  await e[14].run_once();first=await c.fetchrow('SELECT * FROM delivery_items')
  assert first['application_receipt'] is None
  assert await c.fetchval('SELECT status FROM outbox_operations WHERE id=$1',first['outbox_id'])=='failed'
  second=await next_paid(c,e[1]);await freeze_and_enqueue(c,second['id'],UUID(e[6]['id']));await e[14].run_once()
  item=await c.fetchrow('SELECT * FROM delivery_items WHERE fulfillment_id=$1',second['id'])
  assert not item['dispatch_started'] and item['wire'] is None
  assert await c.fetchval('SELECT last_error FROM outbox_operations WHERE id=$1',item['outbox_id'])==DEPENDENCY_WAIT
  assert not await e[14].run_once()
  assert f.writes==['expiry'] and not (await fulfillment_proof(c,second['id']))['transport_complete']
 finally:await close(e)
