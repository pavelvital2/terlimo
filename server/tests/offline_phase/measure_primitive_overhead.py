"""One small synthetic OFF/ON cold+warm comparison; no live API/DB/phone."""
import json,subprocess,sys,statistics,importlib.util,time
from pathlib import Path
ROOT=Path(__file__).resolve().parents[2]
if len(sys.argv)>1:
    v=json.load(sys.stdin)
    sys.path.insert(0,str(ROOT))
    from terlimo_backend import pop,auth_api_phase as diag
    class Sink:
        def info(self,*a):pass
    on=sys.argv[1]=='on'
    def run():
        p=diag.AuthApiPhase('session',sink=Sink()) if on else None
        if p:p.activate('a'*32);p.mark('keyload_begin')
        key=pop.load_public_key(v['spki'],observer=p.mark if p else None)
        if p:p.mark('keyload_end');p.mark('proof_verify_begin')
        pop.verify_proof(key,**v['kwargs'],known_top_level=pop.KNOWN_TOP_LEVEL,server_known_fields=pop.KNOWN_TOP_LEVEL,observer=p.mark if p else None)
        if p:p.mark('proof_verify_end');p.finish()
    def timed():
        w=time.perf_counter_ns();c=time.thread_time_ns();run();return {'wall_ms':(time.perf_counter_ns()-w)/1e6,'thread_CPU_ms':(time.thread_time_ns()-c)/1e6}
    first=timed();rows=[timed() for _ in range(20)]
    print(json.dumps({'first':first,'warm20':{k:{'median':statistics.median(x[k] for x in rows),'max':max(x[k] for x in rows)} for k in rows[0]}}));sys.exit(0)
old=Path('/home/pavel/step036-receipts/private/s5-auth-between-calls-analysis-20261002/profile-sync.py').read_text().split('trials=[]')[0]
ns={'__file__':__file__};exec(compile(old,'accepted-synthetic-generator','exec'),ns)
out={}
for mode in ['off','on']:
    p=subprocess.run([sys.executable,__file__,mode],input=json.dumps(ns['v']),capture_output=True,text=True,check=True,timeout=10);out[mode]=json.loads(p.stdout)
out['limits']='One OFF/ON pair of exec-fresh children,20warm trials, synthetic282byte proof. First operation after module imports separately observed; no repeated generic benchmark. Sink formats no log text; real IO logger cost not measured. Different subprocess scheduling/provider first-use confounds cold difference; not historical API CPU proof.'
print(json.dumps(out,indent=2))
