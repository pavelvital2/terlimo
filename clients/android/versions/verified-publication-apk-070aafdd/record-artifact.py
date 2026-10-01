from pathlib import Path
import hashlib,json,re,subprocess,zipfile
out=Path(__file__).parent
repo=Path('/home/pavel/terlimo-verified-catalog-publication-20261001')
baseline=Path('/home/pavel/step036-receipts/v20-vkcalls-build-20261001')
def sha(p):return hashlib.sha256(p.read_bytes()).hexdigest()
head=subprocess.check_output(['git','-C',str(repo),'rev-parse','HEAD'],text=True).strip()
assert head=='c7feddd780dea786e36cedbe108232995dc036ff'
sig=re.search(r'certificate SHA-256 digest: ([0-9a-f]+)',(out/'candidate-signature.txt').read_text()).group(1)
assert sig=='42ab6d950c15742eaadcf538947f65b3e0bd3e7e1693d67afa969eb00e5d348b'
overlay=repo/'testapp/src/main/assets/test-mobile.json'
assert sha(overlay)=='4ba978eb7cfb9e3b5c6a8bc132d8cbee618fc9fc2d24d3956b597d2d8ea4bdec'
assert (out/'strip-check.so').read_bytes()==(out/'candidate-packaged-native.so').read_bytes()
assert head in (out/'native-buildinfo.txt').read_text() and head in (out/'packaged-buildinfo.txt').read_text()
markers=[b'SERVICE_FRAME_DIAG',b'TERLIMO_SERVICE_FRAME_DIAG',b'serviceFrameDiag',b'TERLIMO_SERVER_FRAME_DIAG']
with zipfile.ZipFile(out/'candidate.apk') as z,zipfile.ZipFile(baseline/'candidate.apk') as b:
    assert z.read('assets/test-mobile.json')==overlay.read_bytes()
    entries=[n for n in z.namelist() if n.startswith('lib/')]
    otherlibs=[n for n in entries if n!='lib/arm64-v8a/libterlimo.so']
    assert all(z.read(n)==b.read(n) for n in otherlibs)
    dex=[n for n in z.namelist() if re.fullmatch(r'classes\d*\.dex',n)]
    diag={m.decode():any(m in z.read(n) for n in dex+['lib/arm64-v8a/libterlimo.so']) for m in markers}
    assert not any(diag.values())
    dex_same={n:n in b.namelist() and z.read(n)==b.read(n) for n in dex}
    assert all(dex_same.values())
meta={'source_head':head,'source_code_commit':'f68d784473d0caa49cd127cc16f9e7c22f4232b0','root_published_snapshot':'29b238b022aa6aeff5bcda6a09fb75bbde9e151b','published_repository':'https://github.com/pavelvital2/terlimo','published_branch':'fix/verified-catalog-publication-20261001','layout':'clients/android','signer_sha256':sig,'package':'xyz.terlimo.test','version_code':14,'version_name':'0.14-routing','min_sdk':28,'target_sdk':35,'overlay_sha256':sha(overlay),'overlay_bytes':overlay.stat().st_size,'packaged_overlay_matches':True,'packaged_native_matches_stripped_fresh_input':True,'native_revision_matches':True,'diagnostic_flags':'OFF (diagnostic implementation/activation markers absent)','diagnostic_marker_presence':diag,'dex_identical_to_baseline':dex_same,'other_native_libraries_identical_to_baseline':otherlibs,'baseline_apk_sha256':sha(baseline/'candidate.apk'),'device_tested':False,'device_actions':[],'tests':'accepted source evidence reused; no rerun','hashes':{n:sha(out/n) for n in ['candidate.apk','candidate-native.so','candidate-packaged-native.so','build.log','build-candidate.sh']}}
(out/'artifact-manifest.json').write_text(json.dumps(meta,ensure_ascii=False,indent=2)+'\n')
print(json.dumps(meta,ensure_ascii=False,indent=2))
