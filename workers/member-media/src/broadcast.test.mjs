import assert from 'node:assert/strict';
import {test} from 'node:test';
import worker from './index.ts';
const recording='11111111-1111-4111-8111-111111111111',capture='a'.repeat(32),scope='22222222-2222-4222-8222-222222222222';
const prefix=`recordings/captures/${capture}/final/`,path=`/videos/${recording}/captures/${capture}/sessions/${scope}/`;
const encode=x=>Buffer.from(JSON.stringify(x)).toString('base64url');
async function fixture(){
 const keys=await crypto.subtle.generateKey({name:'ECDSA',namedCurve:'P-256'},true,['sign','verify']);
 const now=Math.floor(Date.now()/1000),calls=[];
 const claims={iss:'test',aud:'hhc-media',typ:'playback',exp:now+300,recordingId:recording,captureId:capture,scopeId:scope,prefix,broadcastEpoch:1,rangeRevision:3,purpose:'member'};
 const sign=async c=>{const input=`${encode({alg:'ES256',typ:'JWT',kid:'test'})}.${encode(c)}`;return `${input}.${Buffer.from(await crypto.subtle.sign({name:'ECDSA',hash:'SHA-256'},keys.privateKey,new TextEncoder().encode(input))).toString('base64url')}`;};
 let policy={recordingId:recording,epoch:1,rangeRevision:3,startSequence:8,endSequenceExclusive:20,revoked:false,memberState:'live'};
 let lastSequence=23;
 const playlist='#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:31\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:EVENT\n#EXT-X-MAP:URI="init.mp4"\n'+Array.from({length:24},(_,i)=>`#EXTINF:30.030000,\nseg-${String(i).padStart(6,'0')}.m4s\n`).join('')+'#EXT-X-ENDLIST\n';
 const env={MEDIA_ISSUER:'test',MEDIA_PUBLIC_KEYS:JSON.stringify({test:await crypto.subtle.exportKey('jwk',keys.publicKey)}),ALLOWED_ORIGINS:'https://www.alive.org.tw',MEDIA_BUCKET:{
 async get(key){calls.push(key);const value=key.endsWith('broadcast.json')?policy===null?null:JSON.stringify(policy):key.endsWith('current.json')?JSON.stringify({revision:25,lastSequence}):key.endsWith('index.m3u8')?playlist.split('#EXTINF:').slice(0,lastSequence+2).join('#EXTINF:'):'abcdef';return value===null?null:{size:Buffer.byteLength(value),httpEtag:'"media"',body:new Response(value).body};},
 async head(key){calls.push(key);return {size:6,httpEtag:'"media"'};}}};
 const token=await sign(claims);
 const get=(resource,credential=token,mode='sessions')=>worker.fetch(new Request(`https://media.alive.org.tw${path.replace('/sessions/',`/${mode}/`)}${resource}`,{headers:{Cookie:`hhc_media=${credential}`}}),env);
 return {env,calls,claims,sign,get,setPolicy:p=>{policy=p;},setLastSequence:n=>{lastSequence=n;},policy};
}
test('TestMemberCannotReadPreStartOrPostEndSegment',async()=>{const f=await fixture();for(const i of [0,7,20,23]) {assert.equal((await f.get(`720p/seg-${String(i).padStart(6,'0')}.m4s`)).status,404);}for(const i of [8,19]) assert.equal((await f.get(`720p/seg-${String(i).padStart(6,'0')}.m4s`)).status,200);const r=await f.get('720p/index.m3u8');assert.equal(r.status,200);const body=await r.text();assert.ok(body.includes('#EXT-X-MEDIA-SEQUENCE:8'));assert.equal((body.match(/#EXTINF:/g)||[]).length,12);assert.ok(body.includes('#EXT-X-PLAYLIST-TYPE:VOD'));assert.ok(body.includes('#EXT-X-ENDLIST'));assert.ok(!body.includes('seg-000007'));});
test('TestCachedBytesStillRequireCurrentRange',async()=>{const f=await fixture();const old=globalThis.caches;let hits=0;globalThis.caches={default:{async match(){hits++;return new Response('cached');}}};try {assert.equal((await f.get('720p/seg-000008.m4s')).status,200);assert.equal(hits,1);f.setPolicy({...f.policy,rangeRevision:4,revoked:true});assert.equal((await f.get('720p/seg-000008.m4s')).status,403);assert.equal(hits,1);}finally {globalThis.caches=old;}});
test('TestPreviewGrantCannotReadMemberScope',async()=>{const f=await fixture();const token=await f.sign({...f.claims,purpose:'staff-preview'});assert.equal((await f.get('720p/seg-000000.m4s',token)).status,401);assert.equal((await f.get('720p/seg-000000.m4s',token,'previews')).status,200);assert.equal((await f.get('720p/seg-000008.m4s',await f.sign(f.claims),'previews')).status,401);});
test('broadcast authority fails closed on missing malformed and stale policy',async()=>{const f=await fixture();for(const p of [null,{...f.policy,epoch:2},{...f.policy,rangeRevision:2},{...f.policy,startSequence:null},{...f.policy,extra:'x'}]){f.setPolicy(p);assert.notEqual((await f.get('720p/seg-000008.m4s')).status,200);}assert.equal(f.calls.some(k=>k.includes('seg-')),false);});
test('raw package routes cannot bypass capture authority',async()=>{const f=await fixture();f.setPolicy({...f.policy,memberState:'vod'});const token=await f.sign({...f.claims,captureId:undefined,packageId:capture,prefix:`recordings/packages/${capture}/final/attempt/`});for(const name of ['480p/seg-000007.m4s','480p/seg-000020.m4s','previews/seg-000007.jpg']){const r=await worker.fetch(new Request(`https://media.alive.org.tw/videos/${recording}/packages/${capture}/sessions/${scope}/${name}`,{headers:{Cookie:`hhc_media=${token}`}}),f.env);assert.equal(r.status,404);}assert.equal(f.calls.some(k=>k.includes('seg-')),false);});
test('member VTT and sprite cues remain inside the public range and normalize to zero',async()=>{
 const f=await fixture();f.setPolicy({...f.policy,memberState:'vod'});const original=f.env.MEDIA_BUCKET.get;
 f.env.MEDIA_BUCKET.get=async key=>{
  let body;
  if(key===`recordings/packages/${capture}/final/attempt/previews/current.json`) body='{"attempt":"sprites"}';
  if(key.endsWith('/previews/sprites/index.vtt')) body='WEBVTT\n\n00:03:30.210 --> 00:03:35.210\nseg-000007.jpg#xywh=0,0,160,90\n\n00:04:00.240 --> 00:04:05.240\nseg-000008.jpg#xywh=0,0,160,90\n\n00:10:00.600 --> 00:10:05.600\nseg-000020.jpg#xywh=0,0,160,90\n';
  return body?{size:Buffer.byteLength(body),body:new Response(body).body,httpEtag:'"vtt"'}:original(key);
 };
 const token=await f.sign({...f.claims,captureId:undefined,packageId:capture,prefix:`recordings/packages/${capture}/final/attempt/`});
 const r=await worker.fetch(new Request(`https://media.alive.org.tw/videos/${recording}/packages/${capture}/sessions/${scope}/previews/index.vtt`,{headers:{Cookie:`hhc_media=${token}`}}),f.env);
 assert.equal(r.status,200);const body=await r.text();assert.ok(body.includes('00:00:00.000 --> 00:00:05.000'));assert.ok(body.includes('seg-000008.jpg'));assert.ok(!body.includes('seg-000007.jpg'));assert.ok(!body.includes('seg-000020.jpg'));
});
test('preview cookie exchange cannot cross member purpose',async()=>{
 const f=await fixture();const playback=await f.sign({...f.claims,purpose:'staff-preview'});const exchange=await f.sign({...f.claims,purpose:'staff-preview',typ:'exchange',exp:Math.floor(Date.now()/1000)+60,playback});
 const send=mode=>worker.fetch(new Request(`https://media.alive.org.tw${path.replace('/sessions/',`/${mode}/`)}cookie`,{method:'POST',headers:{Origin:'https://www.alive.org.tw'},body:JSON.stringify({credential:exchange})}),f.env);
 assert.equal((await send('sessions')).status,401);const r=await send('previews');assert.equal(r.status,204);assert.ok(r.headers.get('Set-Cookie').includes('/previews/'));
});

test('end pending serves verified public prefix until the end marker arrives',async()=>{const f=await fixture();f.setLastSequence(14);const r=await f.get('720p/index.m3u8');assert.equal(r.status,200);const body=await r.text();assert.ok(body.includes('#EXT-X-PLAYLIST-TYPE:EVENT'));assert.ok(!body.includes('#EXT-X-ENDLIST'));assert.equal((body.match(/#EXTINF:/g)||[]).length,7);assert.equal((await f.get('720p/seg-000014.m4s')).status,200);assert.equal((await f.get('720p/seg-000015.m4s')).status,404);});
test('member admission blocks existing live and VOD grants before cache and admits only published VOD',async()=>{const f=await fixture();const token=await f.sign({...f.claims,captureId:undefined,packageId:capture,prefix:`recordings/packages/${capture}/final/attempt/`});const vod=()=>worker.fetch(new Request(`https://media.alive.org.tw/videos/${recording}/packages/${capture}/sessions/${scope}/480p/seg-000008.m4s`,{headers:{Cookie:`hhc_media=${token}`}}),f.env);assert.equal((await vod()).status,403);f.setPolicy({...f.policy,memberState:'vod'});assert.equal((await vod()).status,200);f.setPolicy({...f.policy,memberState:'blocked'});assert.equal((await f.get('480p/seg-000008.m4s')).status,403);assert.equal((await vod()).status,403);const preview=await f.sign({...f.claims,purpose:'staff-preview'});assert.equal((await f.get('480p/seg-000000.m4s',preview,'previews')).status,200);});

test('existing member credential survives end revision while current authority still governs bytes',async()=>{const f=await fixture();f.setLastSequence(14);f.setPolicy({...f.policy,rangeRevision:4,endSequenceExclusive:20});assert.equal((await f.get('480p/seg-000010.m4s')).status,200);assert.equal((await f.get('480p/seg-000020.m4s')).status,404);f.setPolicy({...f.policy,rangeRevision:5,memberState:'blocked'});assert.equal((await f.get('480p/seg-000010.m4s')).status,403);f.setPolicy({...f.policy,rangeRevision:6,revoked:true});assert.equal((await f.get('480p/seg-000010.m4s')).status,403);});
