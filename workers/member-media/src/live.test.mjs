import assert from 'node:assert/strict';
import {test} from 'node:test';
import worker from './index.ts';
const recording='018f47d2-e5d1-4f3f-8f18-6c8e32621f71',capture='a'.repeat(32),scope='018f47d2-e5d1-4f3f-8f18-6c8e32621f72';
const prefix=`recordings/captures/${capture}/final/`,path=`/videos/${recording}/captures/${capture}/sessions/${scope}/`;
const encode=value=>Buffer.from(JSON.stringify(value)).toString('base64url');
async function fixture(){
 const keys=await crypto.subtle.generateKey({name:'ECDSA',namedCurve:'P-256'},true,['sign','verify']);
 const now=Math.floor(Date.now()/1000),calls=[];
 const sign=async payload=>{const input=`${encode({alg:'ES256',typ:'JWT',kid:'test'})}.${encode(payload)}`;return `${input}.${Buffer.from(await crypto.subtle.sign({name:'ECDSA',hash:'SHA-256'},keys.privateKey,new TextEncoder().encode(input))).toString('base64url')}`;};
 const claims={iss:'test',aud:'hhc-media',typ:'playback',exp:now+300,recordingId:recording,captureId:capture,scopeId:scope,prefix};
 let pointer={revision:3,lastSequence:2};
 const env={MEDIA_ISSUER:'test',ALLOWED_ORIGINS:'https://www.alive.org.tw',MEDIA_PUBLIC_KEYS:JSON.stringify({test:await crypto.subtle.exportKey('jwk',keys.publicKey)}),MEDIA_BUCKET:{
  async head(key){calls.push(key);return {size:6,httpEtag:'"media"'};},
  async get(key){calls.push(key);const body=key===prefix+'current.json'?JSON.stringify(pointer):'abcdef';return {size:Buffer.byteLength(body),httpEtag:'"media"',body:new Response(body).body};}
 }};
 const token=await sign(claims);
 return {env,token,calls,claims,now,sign,setPointer:value=>{pointer=value;},get:(resource,credential=token)=>worker.fetch(new Request(`https://media.alive.org.tw${path}${resource}`,{headers:{Cookie:`hhc_media=${credential}`}}),env)};
}
test('live objects require capture-bound short grants before R2 access',async()=>{
 const f=await fixture();
 for(const claims of [{...f.claims,exp:f.now+301},{...f.claims,captureId:'b'.repeat(32)},{...f.claims,packageId:capture},{...f.claims,prefix:`recordings/packages/${capture}/staging/`},{...f.claims,exp:f.now-1}]) assert.equal((await f.get('720p/seg-000000.m4s',await f.sign(claims))).status,401);
 assert.equal(f.calls.length,0);
 assert.equal((await f.get('720p/seg-000000.m4s')).status,200);
});
test('live manifests resolve revisions and segments cannot pass the common waterline',async()=>{
 const f=await fixture();
 assert.equal((await f.get('720p/seg-000003.m4s')).status,404);
 assert.equal(f.calls.some(key=>key.endsWith('seg-000003.m4s')),false);
 const response=await f.get('720p/index.m3u8');
 assert.equal(response.status,200);
 assert.equal(response.headers.get('Cache-Control'),'private, no-store');
 assert.ok(f.calls.includes(prefix+'playlists/3/720p/index.m3u8'));
 f.setPointer({revision:4,lastSequence:3});
 assert.equal((await f.get('720p/seg-000003.m4s')).status,200);
 assert.equal((await f.get('master.m3u8')).status,200);
 assert.ok(f.calls.includes(prefix+'playlists/4/master.m3u8'));
});
test('live pointer rejects invalid fields and staging routes',async()=>{
 const f=await fixture();
 for(const pointer of [{revision:'../other',lastSequence:2},{revision:0,lastSequence:2},{revision:2,lastSequence:1440},{revision:2,lastSequence:2,extra:'x'},{revision:2,lastSequence:-1}]) {f.setPointer(pointer);assert.equal((await f.get('master.m3u8')).status,503);}
 assert.equal((await f.get('staging/720p/init.mp4')).status,404);
});

test('live exchange binds capture scope and grants no more than five minutes',async()=>{
 const f=await fixture();
 const exchange={...f.claims,typ:'exchange',exp:f.now+60,playback:f.token};delete exchange.prefix;
 const request=credential=>worker.fetch(new Request(`https://media.alive.org.tw${path}cookie`,{method:'POST',headers:{Origin:'https://www.alive.org.tw','Content-Type':'application/json'},body:JSON.stringify({credential})}),f.env);
 const response=await request(await f.sign(exchange));
 assert.equal(response.status,204);
 assert.ok(response.headers.get('Set-Cookie').includes(`Path=${path}`));
 assert.match(response.headers.get('Set-Cookie'),/Max-Age=(?:300|299);/);
 assert.equal((await request(await f.sign({...exchange,captureId:'b'.repeat(32)}))).status,401);
 assert.equal(f.calls.length,0);
});
test('live cache never bypasses expired authorization',async()=>{
 const f=await fixture();let reads=0;
 const original=globalThis.caches;
 globalThis.caches={default:{async match(){reads++;return new Response('cached');}}};
 try {
  assert.equal((await f.get('720p/seg-000000.m4s')).status,200);
  assert.equal(reads,1);
  f.calls.length=0;
  assert.equal((await f.get('720p/seg-000000.m4s',await f.sign({...f.claims,exp:f.now-1}))).status,401);
  assert.equal(reads,1);assert.equal(f.calls.length,0);
 } finally {globalThis.caches=original;}
});
