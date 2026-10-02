"""Isolated route policy checks: no sockets, DB, services or credentials."""
import asyncio
from contextlib import asynccontextmanager
from datetime import UTC,datetime,timedelta
from types import SimpleNamespace
import pytest
from multidict import MultiDict
from terlimo_backend import mobile_catalog as m
from terlimo_backend.auth_api import ApiError
from terlimo_backend.session_auth import AuthError

class Connection:
    session_row=None
    @asynccontextmanager
    async def transaction(self,**kwargs):yield self
    async def fetch(self,*args):
        return [{'gateway_key':f'gw-{i}','display_name':f'Gateway {i}','endpoints':{'region':'test'}} for i in range(3)]
    async def fetchrow(self,*args):return self.session_row
class Database:
    def __init__(self):self.connection=Connection()
    @asynccontextmanager
    async def acquire(self):yield self.connection

def setup(monkeypatch,query=None):
    db=Database();service=m.CatalogService(SimpleNamespace(catalog_validity_seconds=600,environment='test'),db)
    req=SimpleNamespace(query=MultiDict(query or []),headers={'Authorization':'Bearer synthetic-offline'},get=lambda key,default=None: default)
    monkeypatch.setattr(m,'request_id_for',lambda req:'0'*32)
    async def authenticated(*args):return SimpleNamespace(environment='test')
    monkeypatch.setattr(m,'authenticate_session',authenticated)
    monkeypatch.setattr(m,'_data_subject',lambda ctx:('binding','synthetic'))
    monkeypatch.setattr(service,'_require_data_subject',lambda *args:None)
    async def pending(*args):raise ApiError('ACCESS_SYNC_PENDING',http=409,retryable=True)
    monkeypatch.setattr(service,'_catalog_body',pending)
    async def revision(*args):return 1
    monkeypatch.setattr(m,'_subject_catalog_revision',revision)
    return service,req,db

def test_explicit_browse_with_active_pending_grant(monkeypatch):
    service,req,db=setup(monkeypatch,[('view','browse')])
    body=asyncio.run(service.get_gateways(req))
    assert body['catalog_mode']=='browse' and len(body['gateways'])==3
    assert 'revision' not in body
    assert all(set(x)<= {'gateway_id','name','region','country_code'} for x in body['gateways'])

def test_default_still_requires_data_admission(monkeypatch):
    service,req,db=setup(monkeypatch)
    # Admission error handling writes only existing revision metadata; stub this unrelated path.
    async def revision(*args):return 1
    monkeypatch.setattr(m,'_subject_catalog_revision',revision)
    with pytest.raises(ApiError) as exc:asyncio.run(service.get_gateways(req))
    assert exc.value.code=='ACCESS_SYNC_PENDING' and exc.value.http==409

@pytest.mark.parametrize('query',[[('view','credential')],[('view','browse'),('view','browse')]])
def test_invalid_or_duplicate_view_rejected(monkeypatch,query):
    service,req,db=setup(monkeypatch,query)
    with pytest.raises(ApiError) as exc:asyncio.run(service.get_gateways(req))
    assert exc.value.code=='BAD_MESSAGE'

@pytest.mark.parametrize('revoked',['session','installation'])
def test_real_authorizer_still_rejects_revoked(monkeypatch,revoked):
    service,req,db=setup(monkeypatch,[('view','browse')])
    # Use actual shared authorizer; one synthetic row hits the production revoke checks.
    from terlimo_backend.session_auth import authenticate_session
    monkeypatch.setattr(m,'authenticate_session',authenticate_session)
    db.connection.session_row={'revoked_at':datetime.now(UTC) if revoked=='session' else None,'expires_at':datetime.now(UTC)+timedelta(hours=1),'installation_state':'revoked' if revoked=='installation' else 'active'}
    with pytest.raises(AuthError) as exc:asyncio.run(service.get_gateways(req))
    assert exc.value.code==('SESSION_INVALID' if revoked=='session' else 'DEVICE_REVOKED')
