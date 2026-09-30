import assert from 'node:assert/strict';
import { before, test } from 'node:test';
import worker from './index.ts';

const path = '/videos/rec-1/files/file-1/sessions/scope-1/';
const origin = 'https://www.alive.org.tw';
let keys;
let env;

const encode = value => Buffer.from(JSON.stringify(value)).toString('base64url');

async function sign(payload) {
  const input = `${encode({ alg: 'ES256', kid: 'test-key', typ: 'JWT' })}.${encode(payload)}`;
  const signature = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, keys.privateKey, new TextEncoder().encode(input));
  return `${input}.${Buffer.from(signature).toString('base64url')}`;
}

async function credentials(exp = Math.floor(Date.now() / 1000) + 1800) {
  const scope = { iss: 'hhc-media-test', aud: 'hhc-media', recordingId: 'rec-1', assetVersionId: 'file-1', scopeId: 'scope-1' };
  const playback = await sign({ ...scope, typ: 'playback', exp, objectKey: 'recordings/file-1.mp4' });
  return sign({ ...scope, typ: 'exchange', exp: Math.floor(Date.now() / 1000) + 60, playback });
}

async function playback(exp, scopeId = 'scope-1') {
  return sign({ iss: 'hhc-media-test', aud: 'hhc-media', typ: 'playback', exp,
    recordingId: 'rec-1', assetVersionId: 'file-1', scopeId, objectKey: 'recordings/file-1.mp4' });
}

before(async () => {
  keys = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, true, ['sign', 'verify']);
  env = {
    MEDIA_ISSUER: 'hhc-media-test',
    MEDIA_PUBLIC_KEYS: JSON.stringify({ 'test-key': await crypto.subtle.exportKey('jwk', keys.publicKey) }),
    ALLOWED_ORIGINS: origin,
    MEDIA_BUCKET: {
      async get(key, options) {
        assert.equal(key, 'recordings/file-1.mp4');
        const range = options?.range;
        const bytes = range?.offset === 0 && range?.length === 2 ? 'ab' : 'abcdef';
        return { body: new Response(bytes).body, size: 6, httpEtag: '"v1"', range };
      },
      async head() { return { size: 6, httpEtag: '"v1"' }; },
    },
  };
});

test('shared content URL without a scoped cookie cannot read bytes or metadata', async () => {
  for (const method of ['GET', 'HEAD']) {
    const response = await worker.fetch(new Request(`https://media.alive.org.tw${path}content`, { method }), env);
    assert.equal(response.status, 401);
    assert.equal(response.headers.get('Content-Length'), null);
  }
});

test('exchange sets a path-scoped cookie and permits streamed Range reads', async () => {
  const exchange = await credentials();
  const setup = await worker.fetch(new Request(`https://media.alive.org.tw${path}cookie`, {
    method: 'POST', headers: { Origin: origin, 'Content-Type': 'application/json' }, body: JSON.stringify({ credential: exchange }),
  }), env);
  assert.equal(setup.status, 204);
  const cookie = setup.headers.get('Set-Cookie');
  assert.match(cookie, /HttpOnly/);
  assert.match(cookie, /SameSite=Strict/);
  assert.match(cookie, /Path=\/videos\/rec-1\/files\/file-1\/sessions\/scope-1\//);
  assert.doesNotMatch(cookie, /Domain=/);
  const response = await worker.fetch(new Request(`https://media.alive.org.tw${path}content`, {
    headers: { Cookie: cookie.split(';')[0], Range: 'bytes=0-1', Origin: origin },
  }), env);
  assert.equal(response.status, 206);
  assert.equal(response.headers.get('Content-Range'), 'bytes 0-1/6');
  assert.equal(await response.text(), 'ab');
});

test('expired playback cookie cannot authorize HEAD or Range', async () => {
  const expired = await playback(Math.floor(Date.now() / 1000) - 1);
  const cookie = `hhc_media=${expired}`;
  for (const method of ['HEAD', 'GET']) {
    const response = await worker.fetch(new Request(`https://media.alive.org.tw${path}content`, {
      method, headers: { Cookie: cookie, Range: 'bytes=0-1' },
    }), env);
    assert.equal(response.status, 401);
  }
});

test('another page scope cannot reuse a valid cookie', async () => {
  const token = await playback(Math.floor(Date.now() / 1000) + 1800, 'scope-2');
  const response = await worker.fetch(new Request(`https://media.alive.org.tw${path}content`, {
    headers: { Cookie: `hhc_media=${token}` },
  }), env);
  assert.equal(response.status, 401);
});

test('invalid and multiple byte ranges are rejected after authorization', async () => {
  const token = await playback(Math.floor(Date.now() / 1000) + 1800);
  for (const range of ['bytes=8-10', 'bytes=0-1,4-5']) {
    const response = await worker.fetch(new Request(`https://media.alive.org.tw${path}content`, {
      headers: { Cookie: `hhc_media=${token}`, Range: range },
    }), env);
    assert.equal(response.status, 416);
    assert.equal(response.headers.get('Content-Range'), 'bytes */6');
  }
});
