"""Preparation only: stage root's accepted primary4 code and exact SOURCE payload.
Does not write runtime/units/env/Caddy; does not open an operator private key.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import sys

ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(ROOT / 'source/server'))
from terlimo_backend.recovery_code import verify_code

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--accepted-primary4-code', type=Path, required=True)
parser.add_argument('--public-verifier', type=Path, required=True)
args = parser.parse_args()
code = args.accepted_primary4_code.read_text(encoding='ascii').strip()
key = args.public_verifier.read_text(encoding='ascii').strip()
assert hashlib.sha256((code+'\n').encode()).hexdigest() == 'f570ecf38df6c8bf538e1cffd1afd60630eecb626da92e3d079c24ef60b205b7'
assert hashlib.sha256(key.encode()).hexdigest() == '049e80b9827dbbb6d9805f2782d358bfe0c8bde5192c55108b9397d7ec34f343'
seed = verify_code(code,key,'test')
assert seed['revision']=='4' and seed['peer_ip']=='193.5.251.217' and seed['dtls_port']==57500
stage=ROOT/'test-payload';stage.mkdir(mode=0o700,exist_ok=True)
files={}
for name in ['recovery_code.py','recovery_api.py','recovery_page.py','telegram_bot.py','recovery_publish.py']:
    destination=stage/name
    shutil.copyfile(ROOT/'source/server/terlimo_backend'/name,destination)
    os.chmod(destination,0o600)
    files[name]=hashlib.sha256(destination.read_bytes()).hexdigest()
for name,data in [('accepted-primary4.code.txt',(code+'\n').encode()),('public-verifier.txt',(key+'\n').encode())]:
    destination=stage/name;destination.write_bytes(data);os.chmod(destination,0o600)
    files[name]=hashlib.sha256(data).hexdigest()
receipt={'files':files,'environment':'test','revision':'4','signature_valid':True,'endpoint':'193.5.251.217:57500','code_mode':'0600','code_owner_uid':os.getuid(),'source_only_staging':True,'runtime_changed':False}
(stage/'staging.safe.json').write_text(json.dumps(receipt,indent=2)+'\n');os.chmod(stage/'staging.safe.json',0o600)
print(json.dumps(receipt))
