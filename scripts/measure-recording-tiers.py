#!/usr/bin/env python3
"""Isolated Linux fixture comparison. No cloud resources or media credentials.

Build Dockerfile.recording once and compile both Linux test binaries at the same
commit under ARTIFACT_DIR. Example (GOARCH must match the local Docker server):
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c ./internal/recordingvalidation -o ARTIFACT_DIR/validation.test
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c ./internal/recordingprocessing -o ARTIFACT_DIR/processing.test
  python3 scripts/measure-recording-tiers.py IMAGE ARTIFACT_DIR

2CPU/4Gi and 1CPU/2Gi are candidate Consumption tiers. Production adoption
still requires representative long-source, stability and billing acceptance.
"""
import argparse,json,pathlib,subprocess,time,uuid
p=argparse.ArgumentParser();p.add_argument('image');p.add_argument('artifacts',type=pathlib.Path);p.add_argument('--repeats',type=int,default=3);a=p.parse_args()
if a.repeats<1 or a.repeats>5: raise SystemExit('repeats must be 1..5')
a.artifacts=a.artifacts.resolve()
for binary in ['validation.test','processing.test']:
 if not (a.artifacts/binary).is_file():raise SystemExit('compile Linux fixture binaries first')
image_id=subprocess.check_output(['docker','image','inspect',a.image,'--format','{{.Id}}'],text=True).strip()
cases=[('validation','validation.test','-test.run=^$ -test.bench=^BenchmarkPackageValidation$ -test.benchtime=1x',[(4,'8g'),(2,'4g')]),('encoding-fixture','processing.test','-test.run=^TestFFmpegFMP4HTTPSpoolProducesCompleteObjects$ -test.count=1',[(4,'8g'),(2,'4g')]),('cover','validation.test','-test.run=^Test2997CoverCandidatesAtBoundaryDecodeAllThree$ -test.count=1',[(4,'8g'),(1,'2g')]),('timeline','validation.test','-test.run=^TestPreviewRealNonzeroTimestampAndPartialTail$ -test.count=1',[(4,'8g'),(1,'2g')])]
results=[]
try:
 for case,binary,args,tiers in cases:
  for cpu,memory in tiers:
   for repeat in range(a.repeats):
    name='hhc-event-bench-'+uuid.uuid4().hex[:10]
    script=f'/bench/{binary} {args}; result=$?; echo MEMORY_PEAK_BYTES; cat /sys/fs/cgroup/memory.peak; echo MEMORY_EVENTS; cat /sys/fs/cgroup/memory.events; exit "$result"'
    started=time.monotonic()
    try:
     r=subprocess.run(['docker','run','--name',name,'--cpus',str(cpu),'--memory',memory,'--memory-swap',memory,'-e','HHC_REQUIRE_MEDIA_TESTS=1','-v',str(a.artifacts)+':/bench:ro','--entrypoint','/bin/sh',image_id,'-c',script],capture_output=True,text=True,timeout=900)
     elapsed=time.monotonic()-started
     state=json.loads(subprocess.check_output(['docker','inspect',name,'--format','{{json .State}}'],text=True))
     log=a.artifacts/f'{case}-{cpu}-{repeat}.log';log.write_text(r.stdout+r.stderr)
     peak=None
     if 'MEMORY_PEAK_BYTES\n' in r.stdout:
      peak=int(r.stdout.split('MEMORY_PEAK_BYTES\n',1)[1].splitlines()[0])
     results.append(dict(case=case,cpu=cpu,memory=memory,repeat=repeat,wallSeconds=elapsed,peakBytes=peak,exitCode=r.returncode,oomKilled=state.get('OOMKilled'),image=image_id,log=log.name,labOnly=True))
     if r.returncode:raise RuntimeError(f'{case}: fixture failed; see {log}')
    finally:subprocess.run(['docker','rm','-f',name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
finally:
 (a.artifacts/'tier-results.json').write_text(json.dumps({'limitations':['Short synthetic Linux fixtures, not Azure performance or billing','Encoding fixture is one rendition; not full source-HLS pipeline','Wall includes startup and fixture preparation','No production 24h idle or 2.5h resource acceptance claimed'],'results':results},indent=2)+'\n')
print('Saved tier-results.json; no cloud acceptance or savings percentage claimed.')
