"""Offline public issuance: no application DB/session/grant/registration/network dependency."""
import json
import os
import subprocess
from dataclasses import replace

from test_recovery_code import TEST_KEY, VECTOR, FakeDB, FakeTransport, app_client, update

from terlimo_backend import telegram_bot
from terlimo_backend.recovery_code import sign_seed, verify_code
from terlimo_backend.recovery_page import COPY_SCRIPT


def setup_code(tmp_path, settings_factory):
    path = tmp_path / 'latest.txt'
    path.write_text(VECTOR['recovery_code'])
    return settings_factory('', recovery_code_file=str(path), recovery_verify_key_b64=VECTOR['public_key_b64']), path


async def test_public_page_no_db_latest_moved_endpoint_and_missing(tmp_path, settings_factory):
    settings, path = setup_code(tmp_path, settings_factory)
    db = FakeDB(unavailable=True)
    client = await app_client(settings, db)
    try:
        r = await client.get('/api/public/recovery')
        text = await r.text()
        assert r.status == 200 and VECTOR['recovery_code'] in text
        assert r.headers['Cache-Control'] == 'no-store'
        assert 'Location' not in r.headers and 'Set-Cookie' not in r.headers
        assert 'frame-ancestors' in r.headers['Content-Security-Policy']
        assert 'aria-live="polite"' in text and 'readonly' in text and 'Скопировать' in text
        assert 'http' not in COPY_SCRIPT and 'fetch(' not in COPY_SCRIPT
        # Rotate the operator file atomically: both issuance channels must use the NEW tuple.
        seed = dict(VECTOR['seed'], revision='43', peer_ip='192.0.2.124', vk_hashes=['NEW-TEST-HASH'])
        new = sign_seed(json.dumps(seed).encode(), TEST_KEY, 'test')
        replacement = tmp_path / 'replacement'; replacement.write_text(new); os.replace(replacement,path)
        text = await (await client.get('/api/public/recovery')).text()
        assert new in text and VECTOR['recovery_code'] not in text
        outcome = await telegram_bot.handle_update(None, settings, update(1, '/start recovery'))
        assert outcome['reply'] == new and verify_code(new,VECTOR['public_key_b64'],'test') == seed
        assert db.acquisitions == 0
        path.unlink()
        r = await client.get('/api/public/recovery')
        text = await r.text()
        assert r.status == 503 and new not in text and 'id="copy-code"' not in text
        assert 'недоступен' in text and db.acquisitions == 0
        assert (await client.post('/api/public/recovery')).status == 405
        # Authenticated API contract remains protected.
        assert (await client.get('/api/mobile/v1/service-seed')).status == 401
    finally:
        await client.close()


async def test_bot_deeplink_no_registration_and_intact_code(tmp_path, settings_factory):
    settings, path = setup_code(tmp_path, settings_factory)
    settings = replace(settings, telegram_bot_username='TESTbot')
    db = FakeDB(unavailable=True)
    transport = FakeTransport([update(1, '/start recovery'),update(2,'/start@TESTbot recovery')])
    outcomes = await telegram_bot.RegistrationBotRunner(settings,db,transport).handle_once()
    assert [x['state'] for x in outcomes] == ['recovery','recovery']
    assert db.acquisitions == 0
    assert all(x['reply'] == path.read_text() for x in outcomes)
    assert transport.sent[0][1] == VECTOR['recovery_code']
    assert transport.sent[1][1] == telegram_bot.RECOVERY_INSTRUCTION


def test_copy_success_failure_and_missing_clipboard(tmp_path):
    # Execute the actual shipped script against a small DOM, checking observable copy behavior.
    source = """const assert = require('node:assert/strict');
const {runInNewContext} = require('node:vm');
const script = SCRIPT;
(async()=>{
 for(const mode of ['success','reject','missing']) {
  let click, written, focused=false,selected=false;
  const code={value:'TR1.EXACT.TEST',focus(){focused=true},select(){selected=true}};
  const button={disabled:true,addEventListener(name,fn){assert.equal(name,'click');click=fn}};
  const status={textContent:'',dataset:{copied:'copied',failed:'failed'}};
  const navigator=mode==='missing'?{}:{clipboard:{async writeText(value){if(mode==='reject')throw Error('denied');written=value}}};
  runInNewContext(script,{document:{getElementById(id){return {'recovery-code':code,'copy-code':button,'copy-status':status}[id]}},navigator});
  assert.equal(button.disabled,false);assert.equal(written,undefined);await click();
  if(mode==='success'){assert.equal(written,code.value);assert.equal(status.textContent,'copied')}
  else {assert.equal(status.textContent,'failed');assert(focused&&selected)}
 }
 runInNewContext(script,{document:{getElementById(){return null}},navigator:{}});
})().catch(e=>{console.error(e);process.exit(1)});
""".replace('SCRIPT',json.dumps(COPY_SCRIPT))
    file = tmp_path/'copy-test.cjs';file.write_text(source)
    subprocess.run(['node',str(file)],check=True,capture_output=True,text=True)


def test_atomic_publisher_approved_revision_permissions_and_guards(tmp_path):
    import hashlib
    import stat

    import pytest

    from terlimo_backend.recovery_code import RecoveryInvalid
    from terlimo_backend.recovery_publish import publish_signed_code
    candidate, public, destination = tmp_path/'candidate',tmp_path/'public',tmp_path/'served'
    candidate.write_text(VECTOR['recovery_code']);public.write_text(VECTOR['public_key_b64'])
    def publish(expected='absent',floor=42,env='test'):
        return publish_signed_code(candidate,public,destination,environment=env,minimum_revision=floor,
                                   expected_current_sha256=expected,uid=os.getuid(),gid=os.getgid())
    receipt=publish()
    assert destination.read_text().strip()==VECTOR['recovery_code']
    assert stat.S_IMODE(destination.stat().st_mode)==0o600
    assert receipt['uid']==os.getuid() and receipt['atomic']
    pre=destination.read_bytes();sha=hashlib.sha256(pre).hexdigest()
    for expected,floor,env in [('absent',42,'test'),(sha,43,'test'),(sha,42,'production')]:
        with pytest.raises(RecoveryInvalid):publish(expected,floor,env)
        assert destination.read_bytes()==pre
    seed=dict(VECTOR['seed'],revision='43',peer_ip='192.0.2.124')
    candidate.write_text(sign_seed(json.dumps(seed).encode(),TEST_KEY,'test'))
    publish(sha)
    post=destination.read_bytes();sha=hashlib.sha256(post).hexdigest()
    candidate.write_text(VECTOR['recovery_code'])
    with pytest.raises(RecoveryInvalid):publish(sha)
    assert destination.read_bytes()==post
    seed['vk_hashes']=['DIFFERENT-SAME-REVISION']
    candidate.write_text(sign_seed(json.dumps(seed).encode(),TEST_KEY,'test'))
    with pytest.raises(RecoveryInvalid):publish(sha)
    assert destination.read_bytes()==post
    assert not list(tmp_path.glob('.recovery-*'))
