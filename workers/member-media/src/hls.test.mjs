import assert from 'node:assert/strict';
import { test } from 'node:test';
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import worker from './index.ts';

const origin = 'https://www.alive.org.tw';
const pkg = '0123456789abcdef0123456789abcdef';
const path = `/videos/rec-1/packages/${pkg}/sessions/scope-1/`;
const prefix = `recordings/packages/${pkg}/final/attempt-1/`;
const encode = value => Buffer.from(JSON.stringify(value)).toString('base64url');

async function fixture() {
  const keys = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign', 'verify']);
  const sign = async payload => {
    const input = `${encode({ alg: 'ES256', kid: 'test-key', typ: 'JWT' })}.${encode(payload)}`;
    const signature = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, keys.privateKey, new TextEncoder().encode(input));
    return `${input}.${Buffer.from(signature).toString('base64url')}`;
  };
  const scope = { iss: 'hhc-media-test', aud: 'hhc-media', recordingId: 'rec-1', packageId: pkg, scopeId: 'scope-1' };
  const now = Math.floor(Date.now() / 1000);
  const playback = await sign({ ...scope, typ: 'playback', exp: now + 3600, prefix });
  const exchange = await sign({ ...scope, typ: 'exchange', exp: now + 60, playback });
  const calls = [];
  const env = {
    MEDIA_ISSUER: scope.iss, ALLOWED_ORIGINS: origin,
    MEDIA_PUBLIC_KEYS: JSON.stringify({ 'test-key': await crypto.subtle.exportKey('jwk', keys.publicKey) }),
    MEDIA_BUCKET: {
      async head(key) { calls.push(['head', key]); return { size: 6, httpEtag: '"v1"' }; },
      async get(key, options) {
        calls.push(['get', key]);
        const body = options?.range ? 'ab' : 'abcdef';
        return { body: new Response(body).body, size: 6, httpEtag: '"v1"' };
      },
    },
  };
  const get = (object, headers = {}, method = 'GET') => worker.fetch(new Request(`https://media.alive.org.tw${path}${object}`, { method, headers }), env);
  return { env, calls, sign, scope, now, playback, exchange, get };
}

test('every HLS object and metadata read requires the package-scoped cookie', async () => {
  const f = await fixture();
  for (const object of ['master.m3u8', '720p/index.m3u8', '720p/init.mp4', '720p/seg-000000.m4s', 'previews/index.vtt', 'previews/seg-000000.jpg']) {
    for (const method of ['GET', 'HEAD']) {
      const response = await f.get(object, { Range: 'bytes=0-1' }, method);
      assert.equal(response.status, 401);
      assert.equal(response.headers.get('ETag'), null);
    }
  }
  assert.equal(f.calls.length, 0);
});

test('retired single-file routes reject even valid legacy grants without reading R2', async () => {
  const f = await fixture();
  const playback = await f.sign({ iss: f.scope.iss, aud: 'hhc-media', typ: 'playback', exp: f.now + 3600,
    recordingId: 'rec-1', assetVersionId: 'file-1', scopeId: 'scope-1', objectKey: 'recordings/file-1.mp4' });
  const credential = await f.sign({ iss: f.scope.iss, aud: 'hhc-media', typ: 'exchange', exp: f.now + 60,
    recordingId: 'rec-1', assetVersionId: 'file-1', scopeId: 'scope-1', playback });
  for (const [method, resource] of [['GET', 'content'], ['HEAD', 'content'], ['POST', 'cookie']]) {
    const response = await worker.fetch(new Request(`https://media.alive.org.tw/videos/rec-1/files/file-1/sessions/scope-1/${resource}`, {
      method, headers: { Origin: origin, Cookie: `hhc_media=${playback}` },
      ...(method === 'POST' ? { body: JSON.stringify({ credential }) } : {}),
    }), f.env);
    assert.equal(response.status, 404);
  }
  assert.equal(f.calls.length, 0);
});

test('preview routes resolve only a bounded private attempt and authorize before cache', async () => {
  const f = await fixture();
  let pointer = '{"attempt":"preview-1"}';
  const original = f.env.MEDIA_BUCKET.get;
  f.env.MEDIA_BUCKET.get = async (key, options) => key.endsWith('/current.json')
    ? { size: Buffer.byteLength(pointer), body: new Response(pointer).body, httpEtag: '"pointer"' }
    : original(key, options);
  for (const [name, mime] of [['index.vtt', 'text/vtt'], ['seg-000000.jpg', 'image/jpeg']]) {
    const response = await f.get(`previews/${name}`, { Cookie: `hhc_media=${f.playback}`, Origin: origin });
    assert.equal(response.status, 200);
    assert.equal(response.headers.get('Content-Type'), mime);
    assert.equal(response.headers.get('Access-Control-Allow-Credentials'), 'true');
    assert.equal(response.headers.get('Cache-Control'), 'private, no-store');
    assert.ok(f.calls.some(([op, key]) => op === 'get' && key === prefix + `previews/preview-1/${name}`));
  }
  for (const bad of ['{"attempt":"../../other"}', '{"attempt":"preview-1","prefix":"other"}', 'x'.repeat(129)]) {
    pointer = bad;
    assert.equal((await f.get('previews/index.vtt', { Cookie: `hhc_media=${f.playback}` })).status, 503);
  }
  pointer = '{"attempt":"preview-1"}';
  const oldCaches = globalThis.caches;
  let hits = 0;
  globalThis.caches = { default: { async match() { hits++; return new Response('cached'); } } };
  try {
    for (const object of ['previews/index.vtt', 'previews/seg-000000.jpg', '480p/seg-000000.m4s']) {
      for (const claims of [{ ...f.scope, exp: f.now - 1 }, { ...f.scope, exp: f.now + 60, scopeId: 'other' }, { ...f.scope, exp: f.now + 60, packageId: 'a'.repeat(32) }]) {
        const token = await f.sign({ ...claims, typ: 'playback', prefix });
        assert.equal((await f.get(object, { Cookie: `hhc_media=${token}` })).status, 401);
      }
    }
    assert.equal(hits, 0);
    assert.equal((await f.get('previews/index.vtt', { Cookie: `hhc_media=${f.playback}` })).status, 200);
    assert.equal(hits, 1);
  } finally { globalThis.caches = oldCaches; }
  f.env.MEDIA_BUCKET.head = async () => ({ size: (1 << 20) + 1, httpEtag: '"too-large"' });
  assert.equal((await f.get('previews/seg-000000.jpg', { Cookie: `hhc_media=${f.playback}` })).status, 503);
  for (const name of ['current.json', 'preview-1/index.vtt', '../master.m3u8']) {
    assert.notEqual((await f.get(`previews/${name}`, { Cookie: `hhc_media=${f.playback}` })).status, 200);
  }
});

test('HLS exchange and renewal retain the exact package/session cookie path', async () => {
  const f = await fixture();
  for (let i = 0; i < 2; i++) {
    const response = await worker.fetch(new Request(`https://media.alive.org.tw${path}cookie`, {
      method: 'POST', headers: { Origin: origin }, body: JSON.stringify({ credential: f.exchange }),
    }), f.env);
    assert.equal(response.status, 204);
    assert.ok(response.headers.get('Set-Cookie').includes(`Path=${path};`));
    assert.match(response.headers.get('Set-Cookie'), /Secure; HttpOnly; SameSite=Strict/);
  }
});

test('authorized HLS requests select only immutable final objects with correct MIME and Range', async () => {
  const f = await fixture();
  const cookie = `hhc_media=${f.playback}`;
  for (const [object, mime] of [['master.m3u8', 'application/vnd.apple.mpegurl'], ['480p/index.m3u8', 'application/vnd.apple.mpegurl'], ['480p/init.mp4', 'video/mp4'], ['480p/seg-000000.m4s', 'video/iso.segment'], ['1080p/index.m3u8', 'application/vnd.apple.mpegurl'], ['720p/init.mp4', 'video/mp4'], ['720p/seg-000000.m4s', 'video/iso.segment']]) {
    const response = await f.get(object, { Cookie: cookie, Origin: origin });
    assert.equal(response.status, 200);
    assert.equal(response.headers.get('Content-Type'), mime);
    assert.equal(response.headers.get('Cache-Control'), 'private, no-store');
    assert.equal(response.headers.get('Access-Control-Allow-Origin'), origin);
    assert.equal(await response.text(), 'abcdef');
    assert.deepEqual(f.calls.at(-1), ['get', prefix + object]);
  }
  const response = await f.get('720p/seg-000000.m4s', { Cookie: cookie, Range: 'bytes=0-1' });
  assert.equal(response.status, 206);
  assert.equal(response.headers.get('Content-Range'), 'bytes 0-1/6');
  assert.equal(await response.text(), 'ab');
});

test('cross-package credentials, staging prefixes and arbitrary paths fail closed', async () => {
  const f = await fixture();
  for (const changes of [{ packageId: 'other' }, { scopeId: 'other' }, { prefix: prefix.replace(pkg, 'other') }, { prefix: `recordings/packages/${pkg}/staging/` }, { assetVersionId: 'file-1' }]) {
    const token = await f.sign({ ...f.scope, typ: 'playback', exp: f.now + 3600, prefix, ...changes });
    assert.equal((await f.get('master.m3u8', { Cookie: `hhc_media=${token}` })).status, 401);
  }
  for (const object of ['inventory.json', '720p/seg-1.m4s', '360p/index.m3u8', '720p/%2finit.mp4', '720p/INIT.mp4', 'master.m3u8?token=anything']) {
    assert.equal((await f.get(object, { Cookie: `hhc_media=${f.playback}` })).status, 404);
  }
  assert.equal(f.calls.length, 0);
});

test('HLS denies unknown origins, methods, duplicate ranges and storage failures', async () => {
  const f = await fixture();
  const headers = { Cookie: `hhc_media=${f.playback}` };
  assert.equal((await f.get('master.m3u8', { ...headers, Origin: 'https://untrusted.example' })).status, 403);
  assert.equal((await f.get('master.m3u8', headers, 'POST')).status, 405);
  for (const range of ['bytes=8-10', 'bytes=0-1,4-5', 'bytes=-0', 'bytes=9007199254740992-']) {
    assert.equal((await f.get('720p/init.mp4', { ...headers, Range: range })).status, 416);
  }
  const preflight = await f.get('720p/seg-000000.m4s', { Origin: origin }, 'OPTIONS');
  assert.equal(preflight.status, 204);
  assert.equal(preflight.headers.get('Access-Control-Allow-Methods'), 'GET, HEAD, OPTIONS');
  f.env.MEDIA_BUCKET.head = async () => { throw new Error('provider failed with private diagnostic'); };
  const failed = await f.get('master.m3u8', headers);
  assert.equal(failed.status, 503);
  assert.equal(await failed.text(), '');
  assert.equal(failed.headers.get('Cache-Control'), 'private, no-store');
});

test('an internal cache hit never bypasses authorization and its public TTL is not sent to viewers', async () => {
  const f = await fixture();
  const cacheCalls = [];
  const oldCaches = globalThis.caches;
  globalThis.caches = { default: {
    async match(request) {
      cacheCalls.push(request);
      return new Response('cached', { headers: { 'Content-Length': '6', 'Content-Type': 'video/iso.segment', 'Cache-Control': 'public, max-age=86400', ETag: '"v1"' } });
    },
  } };
  try {
    assert.equal((await f.get('720p/seg-000000.m4s')).status, 401);
    const expired = await f.sign({ ...f.scope, typ: 'playback', exp: f.now - 1, prefix });
    assert.equal((await f.get('720p/seg-000000.m4s', { Cookie: `hhc_media=${expired}` })).status, 401);
    assert.equal(cacheCalls.length, 0);
    const response = await f.get('720p/seg-000000.m4s', { Cookie: `hhc_media=${f.playback}` });
    assert.equal(response.status, 200);
    assert.equal(await response.text(), 'cached');
    assert.equal(response.headers.get('Cache-Control'), 'private, no-store');
    assert.equal(cacheCalls.length, 1);
    assert.equal(cacheCalls[0].headers.has('Cookie'), false);
    assert.ok(cacheCalls[0].url.endsWith(prefix + '720p/seg-000000.m4s'));
    assert.equal(f.calls.length, 0);
  } finally { globalThis.caches = oldCaches; }
});

test('Cloudflare local runtime serves private R2 HLS and cached Range only after cookie authentication', async () => {
  // Use Wrangler's pinned runtime, not a second independently versioned simulator.
  const { Miniflare, convertV4MiniflareOptions } = createRequire(import.meta.resolve('wrangler'))('miniflare');
  const ts = createRequire(import.meta.url)('typescript');
  const f = await fixture();
  const script = ts.transpileModule(readFileSync(new URL('./index.ts', import.meta.url), 'utf8'), {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.ESNext },
  }).outputText;
  const mf = new Miniflare(convertV4MiniflareOptions({ modules: true, script, compatibilityDate: '2026-09-27',
    r2Buckets: ['MEDIA_BUCKET'], bindings: {
      MEDIA_ISSUER: f.env.MEDIA_ISSUER, MEDIA_PUBLIC_KEYS: f.env.MEDIA_PUBLIC_KEYS, ALLOWED_ORIGINS: origin,
    },
  }));
  try {
    const bucket = await mf.getR2Bucket('MEDIA_BUCKET');
    await bucket.put(prefix + '720p/seg-000000.m4s', 'abcdef');
    const url = `https://media.alive.org.tw${path}720p/seg-000000.m4s`;
    assert.equal((await mf.dispatchFetch(url)).status, 401);
    const setup = await mf.dispatchFetch(`https://media.alive.org.tw${path}cookie`, {
      method: 'POST', headers: { Origin: origin }, body: JSON.stringify({ credential: f.exchange }),
    });
    assert.equal(setup.status, 204);
    const cookie = setup.headers.get('Set-Cookie').split(';')[0];
    const response = await mf.dispatchFetch(url, { headers: { Cookie: cookie } });
    assert.equal(response.status, 200);
    assert.equal(await response.text(), 'abcdef');
    const staleRange = await mf.dispatchFetch(url, { headers: { Cookie: cookie, Range: 'bytes=0-1', 'If-Range': '"old"' } });
    assert.equal(staleRange.status, 200);
    assert.equal(await staleRange.text(), 'abcdef');
    const caches = await mf.getCaches();
    const key = `https://media.alive.org.tw/__hhc_media_cache/${prefix}720p/seg-000000.m4s`;
    const cached = await caches.default.match(key);
    assert.ok(cached, 'the full immutable response must reach internal cache');
    assert.equal(cached.headers.get('Cache-Control'), 'public, max-age=86400');
    await bucket.delete(prefix + '720p/seg-000000.m4s');
    const range = await mf.dispatchFetch(url, { headers: { Cookie: cookie, Range: 'bytes=0-1' } });
    assert.equal(range.status, 206);
    assert.equal(range.headers.get('Content-Range'), 'bytes 0-1/6');
    assert.equal(range.headers.get('Cache-Control'), 'private, no-store');
    assert.equal(await range.text(), 'ab');
    const notModified = await mf.dispatchFetch(url, { headers: { Cookie: cookie, 'If-None-Match': range.headers.get('ETag') } });
    assert.equal(notModified.status, 304);
    assert.equal(notModified.headers.get('Cache-Control'), 'private, no-store');
    const head = await mf.dispatchFetch(url, { method: 'HEAD', headers: { Cookie: cookie } });
    assert.equal(head.status, 200);
    assert.equal(await head.text(), '');
    assert.equal((await mf.dispatchFetch(url)).status, 401);
    const expired = await f.sign({ ...f.scope, typ: 'playback', exp: f.now - 1, prefix });
    assert.equal((await mf.dispatchFetch(url, { headers: { Cookie: `hhc_media=${expired}` } })).status, 401);
    assert.equal((await mf.dispatchFetch(`https://media.alive.org.tw/__hhc_media_cache/${prefix}720p/seg-000000.m4s`)).status, 404);
  } finally { await mf.dispose(); }
});
