import assert from 'node:assert/strict';
import {test} from 'node:test';
import worker from './index.ts';

const pkg='0123456789abcdef0123456789abcdef';
const path=`/videos/rec-1/packages/${pkg}/sessions/scope-1/`;
const prefix=`recordings/packages/${pkg}/final/attempt/`;
const audio='audio/aac-hls-v1/';
async function fixture(){
 const keys=await crypto.subtle.generateKey({name:'ECDSA',namedCurve:'P-256'},true,['sign','verify']);
 const encode=x=>Buffer.from(JSON.stringify(x)).toString('base64url');
 const claims={iss:'test',aud:'hhc-media',typ:'playback',exp:Math.floor(Date.now()/1000)+3600,recordingId:'rec-1',packageId:pkg,scopeId:'scope-1',prefix,broadcastEpoch:1,rangeRevision:1,purpose:'member'};
 const sign=async c=>{const input=encode({alg:'ES256',kid:'test',typ:'JWT'})+'.'+encode(c);return input+'.'+Buffer.from(await crypto.subtle.sign({name:'ECDSA',hash:'SHA-256'},keys.privateKey,new TextEncoder().encode(input))).toString('base64url')};
 const token=await sign(claims);
 let policy={recordingId:'rec-1',epoch:1,rangeRevision:1,startSequence:1,endSequenceExclusive:3,revoked:false,memberState:'vod'};
 const playlist='#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:11\n#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n'+Array.from({length:4},(_,i)=>`#EXTINF:10.005333,\nseg-${String(i).padStart(6,'0')}.ts\n`).join('')+'#EXT-X-ENDLIST\n';
 const calls=[];
 const env={MEDIA_ISSUER:'test',ALLOWED_ORIGINS:'https://www.alive.org.tw',MEDIA_PUBLIC_KEYS:JSON.stringify({test:await crypto.subtle.exportKey('jwk',keys.publicKey)}),MEDIA_BUCKET:{
  async head(key){calls.push(key);return {size:6,httpEtag:'"one"'}},
  async get(key,options){calls.push(key);let body;
   if(key.endsWith('/broadcast.json')){if(!policy)return null;body=JSON.stringify(policy)}
   else if(key===prefix+audio+'current.json')body='{"attempt":"audio-one"}';
   else if(key===prefix+audio+'audio-one/index.m3u8')body=playlist;
   else if(key.startsWith(prefix+audio+'audio-one/seg-'))body=options?.range?'ab':'abcdef';
   else throw new Error('unexpected object key '+key);
   return {size:Buffer.byteLength(body),body:new Response(body).body,httpEtag:'"one"'};
  }
 }};
 const get=(name,credential=token,method='GET',headers={})=>worker.fetch(new Request('https://media.alive.org.tw'+path+name,{method,headers:{...(credential?{Cookie:'hhc_media='+credential}:{}),...headers}}),env);
 return {get,calls,claims,sign,token,setPolicy:p=>{policy=p},policy};
}

test('AAC HLS uses the existing grant and own immutable prefix with current broadcast range',async()=>{
 const f=await fixture();const response=await f.get(audio+'index.m3u8');assert.equal(response.status,200);
 const body=await response.text();assert.ok(body.includes('#EXT-X-VERSION:3'));assert.ok(body.includes('#EXT-X-MEDIA-SEQUENCE:1'));assert.ok(!body.includes('EXT-X-MAP'));assert.ok(body.includes('seg-000001.ts'));assert.ok(!body.includes('seg-000000.ts'));assert.ok(!body.includes('seg-000003.ts'));
 assert.equal((await f.get(audio+'seg-000000.ts')).status,404);assert.equal((await f.get(audio+'seg-000003.ts')).status,404);
 const segment=await f.get(audio+'seg-000001.ts');assert.equal(segment.status,200);assert.equal(segment.headers.get('Content-Type'),'video/mp2t');
 assert.equal((await f.get(audio+'seg-000001.ts',f.token,'HEAD')).status,200);
 assert.equal((await f.get(audio+'seg-000001.ts',f.token,'GET',{Range:'bytes=0-1'})).status,206);
 assert.ok(f.calls.some(k=>k===prefix+audio+'audio-one/seg-000001.ts'));
});

test('audio rejects unauthenticated, expired, foreign session and exact-path neighbors',async()=>{
 const f=await fixture();assert.equal((await f.get(audio+'index.m3u8',null)).status,401);assert.equal(f.calls.length,0);
 for(const change of [{exp:1},{scopeId:'other'},{packageId:'ffffffffffffffffffffffffffffffff'},{exp:Math.floor(Date.now()/1000)+7200},{purpose:'staff-preview'}])assert.equal((await f.get(audio+'seg-000001.ts',await f.sign({...f.claims,...change}))).status,401);
 for(const name of ['audio/aac-hls-v2/index.m3u8',audio+'current.json',audio+'init.mp4',audio+'seg-1.ts',audio+'audio-one/seg-000001.ts',audio+'seg-000001.ts?token=x'])assert.equal((await f.get(name)).status,404);
 assert.equal((await f.get(audio+'seg-000001.ts',f.token,'POST')).status,405);
});

test('cached audio still checks revocation and epoch before any cache access',async()=>{
 const f=await fixture();const previous=globalThis.caches;let hits=0;
 globalThis.caches={default:{async match(){hits++;return new Response('cached',{headers:{'Content-Type':'video/mp2t'}})}}};
 try{assert.equal((await f.get(audio+'seg-000001.ts')).status,200);assert.equal(hits,1);
 f.setPolicy({...f.policy,revoked:true});assert.equal((await f.get(audio+'seg-000001.ts')).status,403);assert.equal(hits,1);
 f.setPolicy({...f.policy,epoch:2});assert.equal((await f.get(audio+'seg-000001.ts')).status,403);assert.equal(hits,1);
 f.setPolicy({...f.policy,memberState:'blocked'});assert.equal((await f.get(audio+'index.m3u8')).status,403);
 }finally{globalThis.caches=previous}
});
