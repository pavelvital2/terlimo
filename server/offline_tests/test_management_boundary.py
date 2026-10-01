import asyncio,json
from types import SimpleNamespace
from datetime import datetime,UTC
from uuid import uuid4
import pytest
from terlimo_backend import gateway_adapter as adapter,operation_timing as timing


def op(attempt=2):
 return {'id':uuid4(),'correlation_id':uuid4(),'attempts':attempt,'created_at':datetime.now(UTC),'available_at':datetime.now(UTC),'payload':{'credential':'DO-NOT-LOG'},'idempotency_key':'DO-NOT-LOG','operation_type':'gateway.apply_grant'}


def fixture_io(monkeypatch,caplog,failure=None,response=None):
 clock=SimpleNamespace(now=0.0)
 monkeypatch.setattr(timing,'time',SimpleNamespace(monotonic=lambda:clock.now))
 monkeypatch.setenv('TERLIMO_MANAGEMENT_RPC_BOUNDARY','1')
 caplog.set_level('INFO',logger='terlimo_backend.operation_timing')
 class Writer:
  closed=False
  def write(self,data):self.wire=json.loads(data)
  async def drain(self):
   clock.now=3.0
   if failure in ['drain','cancel_before']:raise OSError('DO-NOT-LOG') if failure=='drain' else asyncio.CancelledError()
  def close(self):self.closed=True
 writer=Writer()
 class Reader:
  async def read(self):
   # No new mid-RPC log (only existing begin may have been emitted).
   assert not any('boundary=' in r.getMessage() for r in caplog.records)
   clock.now=7.0
   if failure=='read':raise OSError('DO-NOT-LOG')
   if failure=='read_timeout':raise TimeoutError()
   if failure=='cancel_after':raise asyncio.CancelledError()
   return response if response is not None else b'{"status":"ok","readback":{"lease_seq":"116"}}'
 async def connect(*args,**kwargs):
  clock.now=2.0
  if failure=='connect':raise OSError('DO-NOT-LOG')
  if failure=='connect_timeout':raise TimeoutError()
  return Reader(),writer
 monkeypatch.setattr(adapter.asyncio,'open_connection',connect)
 monkeypatch.setattr(adapter.ManagementTlsClient,'_context',lambda self:object())
 client=adapter.ManagementTlsClient(host='127.0.0.1',port=1,server_name='test',ca_file='fake',cert_file='fake',key_file='fake',node_id='test')
 return clock,client,writer


def ends(caplog):return [r.getMessage() for r in caplog.records if 'phase=rpc_end' in r.getMessage()]


async def test_success_boundary_exact_context_clock_wire_and_no_midlog(monkeypatch,caplog):
 clock,client,writer=fixture_io(monkeypatch,caplog);operation=op()
 result=await timing.timed_rpc(operation,'refresh',client.call('refresh',credential='DO-NOT-LOG',fields={'generation':'1'}))
 assert result=={'lease_seq':'116'} and writer.closed
 msg=ends(caplog)[0]
 assert f'operation_id={operation["id"]}' in msg and 'attempt=2' in msg
 assert 'boundary=management_post_drain' in msg
 assert 'setup_send_ms=3000.000' in msg and 'response_rest_ms=4000.000' in msg and 'duration_ms=7000.000' in msg
 assert len(caplog.records)==2 and 'DO-NOT-LOG' not in caplog.text
 assert set(writer.wire)=={'v','op','node_id','fields','credential'}


@pytest.mark.parametrize('failure,code,boundary', [('connect','GATEWAY_UNREACHABLE',False),('drain','GATEWAY_UNREACHABLE',False),('read','GATEWAY_UNREACHABLE',True)])
async def test_same_errors_before_and_after_drain(monkeypatch,caplog,failure,code,boundary):
 _,client,writer=fixture_io(monkeypatch,caplog,failure)
 with pytest.raises(adapter.GatewayError) as caught:await timing.timed_rpc(op(),'refresh',client.call('refresh',credential='DO-NOT-LOG'))
 assert caught.value.code==code
 assert ('boundary=' in ends(caplog)[0])==boundary
 assert 'result=exception' in ends(caplog)[0] and 'DO-NOT-LOG' not in caplog.text
 if failure!='connect':assert writer.closed


@pytest.mark.parametrize('failure,boundary',[('cancel_before',False),('cancel_after',True)])
async def test_cancel_preserved_and_context_reset(monkeypatch,caplog,failure,boundary):
 _,client,writer=fixture_io(monkeypatch,caplog,failure)
 with pytest.raises(asyncio.CancelledError):await timing.timed_rpc(op(),'refresh',client.call('refresh'))
 assert writer.closed and ('boundary=' in ends(caplog)[0])==boundary
 # Outside the cancelled original context: no marker or clock access.
 monkeypatch.setattr(timing,'time',SimpleNamespace(monotonic=lambda:(_ for _ in ()).throw(AssertionError('context leaked'))))
 timing.mark_management_refresh_drained()


@pytest.mark.parametrize('response,code',[(b'broken','GATEWAY_BAD_RESPONSE'),(b'{"status":"error","code":"LEASE_CONFLICT"}','LEASE_CONFLICT'),(b'{"status":"ok","readback":null}','GATEWAY_BAD_RESPONSE')])
async def test_decode_remote_errors_unchanged(monkeypatch,caplog,response,code):
 _,client,writer=fixture_io(monkeypatch,caplog,response=response)
 with pytest.raises(adapter.GatewayError) as caught:await timing.timed_rpc(op(),'refresh',client.call('refresh'))
 assert caught.value.code==code and writer.closed and 'boundary=' in ends(caplog)[0]


@pytest.mark.parametrize('enabled,rpc,method',[(False,'refresh','refresh'),(True,'get','refresh'),(True,'refresh','get')])
async def test_off_or_nonapplicable_call_keeps_old_completion(monkeypatch,caplog,enabled,rpc,method):
 _,client,_=fixture_io(monkeypatch,caplog)
 monkeypatch.setenv('TERLIMO_MANAGEMENT_RPC_BOUNDARY','1' if enabled else '0')
 await timing.timed_rpc(op(),rpc,client.call(method))
 assert 'boundary=' not in ends(caplog)[0] and len(caplog.records)==2


async def test_parallel_refresh_contexts_are_isolated(monkeypatch, caplog):
 from contextvars import ContextVar
 clock = ContextVar('offline_clock', default=0.0)
 monkeypatch.setattr(timing, 'time', SimpleNamespace(monotonic=clock.get))
 monkeypatch.setenv('TERLIMO_MANAGEMENT_RPC_BOUNDARY', '1')
 caplog.set_level('INFO', logger='terlimo_backend.operation_timing')
 both = asyncio.Event()
 ready = []
 operations = [op(1), op(3)]
 async def run(index):
  class Writer:
   def write(self, data): pass
   async def drain(self):
    ready.append(index)
    if len(ready) == 2: both.set()
    await both.wait()
    clock.set([3.0, 8.0][index])
   def close(self): pass
  class Reader:
   async def read(self):
    await asyncio.sleep(0)
    clock.set([5.0, 11.0][index])
    return b'{"status":"ok","readback":{}}'
  async def connect(*args, **kwargs): return Reader(), Writer()
  # Each task gets a separate client but uses the actual shared callback/context.
  client = adapter.ManagementTlsClient(host='fake', port=index+1, server_name='fake', ca_file='fake', cert_file='fake', key_file='fake', node_id='test')
  connections[index+1] = connect
  await timing.timed_rpc(operations[index], 'refresh', client.call('refresh'))
 connections = {}
 async def dispatch(host, port, **kwargs): return await connections[port]()
 monkeypatch.setattr(adapter.asyncio, 'open_connection', dispatch)
 monkeypatch.setattr(adapter.ManagementTlsClient, '_context', lambda self: object())
 await asyncio.gather(run(0), run(1))
 for index, (setup, rest) in enumerate([(3000, 2000), (8000, 3000)]):
  message = next(m for m in ends(caplog) if f'operation_id={operations[index]["id"]}' in m)
  assert f'attempt={operations[index]["attempts"]}' in message
  assert f'setup_send_ms={setup:.3f}' in message and f'response_rest_ms={rest:.3f}' in message
 assert timing._management_refresh_boundary.get() is None


async def test_actual_worker_handler_management_refresh_link(monkeypatch, caplog):
 from contextlib import asynccontextmanager
 from datetime import timedelta
 from unittest.mock import AsyncMock
 from terlimo_backend.gateway_control import GatewayControlHandlers
 from terlimo_backend.worker import OutboxWorker
 grant = dict(id=uuid4(), opaque_id=uuid4(), gateway_id=uuid4(), binding_id=uuid4(), account_id=None,
              desired_generation=2, state='applied', hour_entitlement_id=None,
              endpoints={'node_id':'test', 'target_workers':2}, gateway_key='test',
              gateway_credential='DO-NOT-LOG', not_after=datetime.now(UTC)+timedelta(minutes=10),
              lease_seq=1, gateway_generation=1, public_key_fingerprint='offline-registration',
              public_key_spki_b64='offline-key')
 readback = dict(grant_id=str(grant['opaque_id']), registration_id=grant['public_key_fingerprint'],
                 node_id='test', revoked=False, expires_at=int(grant['not_after'].timestamp()),
                 runtime_applied=True, generation='1', lease_seq='2', max_workers=2)
 _, client, writer = fixture_io(monkeypatch, caplog, response=json.dumps({'status':'ok','readback':readback}).encode())
 class Connection:
  async def fetchrow(self, *args): return grant
  async def fetchval(self, query, *args): return grant['id'] if 'UPDATE grants' in query else 2
  async def execute(self, *args): return 'UPDATE 1'
  @asynccontextmanager
  async def transaction(self): yield
 handlers = GatewayControlHandlers(SimpleNamespace(), client_factory=lambda *_:client)
 worker = OutboxWorker(None, SimpleNamespace(), handlers=handlers.as_handlers(), worker_id='offline')
 worker._owns = AsyncMock(return_value=True)
 worker._finalize = AsyncMock(return_value=True)
 async def heartbeat(operation): await asyncio.Event().wait()
 worker._renew_lease = heartbeat
 operation = op(4)
 operation.update(payload={'grant_id':str(grant['opaque_id']), 'generation':'2'}, max_attempts=12)
 connection = Connection()
 await worker._process(connection, operation)
 worker._finalize.assert_awaited_once_with(connection, operation, status='done')
 message = ends(caplog)[0]
 assert f'operation_id={operation["id"]}' in message and 'attempt=4' in message
 assert 'setup_send_ms=3000.000' in message and 'response_rest_ms=4000.000' in message
 assert writer.wire['op']=='refresh' and writer.closed and 'DO-NOT-LOG' not in caplog.text


@pytest.mark.parametrize('failure,error,boundary',[('connect_timeout',adapter.GatewayError,False),('read_timeout',adapter.GatewayError,True)])
async def test_existing_timeout_mapping(monkeypatch,caplog,failure,error,boundary):
 _,client,writer=fixture_io(monkeypatch,caplog,failure)
 with pytest.raises(error) as caught:
  await timing.timed_rpc(op(),'refresh',client.call('refresh'))
 if error is adapter.GatewayError:
  assert caught.value.code=='GATEWAY_UNREACHABLE'
  if failure=='read_timeout': assert writer.closed
 assert ('boundary=' in ends(caplog)[0])==boundary
