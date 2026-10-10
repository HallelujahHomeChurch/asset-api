type R2Object = {
  size: number;
  httpEtag: string;
  range?: { offset: number; length: number };
  body?: ReadableStream<Uint8Array>;
};

type Env = {
  MEDIA_ISSUER: string;
  MEDIA_PUBLIC_KEYS: string;
  ALLOWED_ORIGINS: string;
  MEDIA_BUCKET: {
    head(key: string): Promise<R2Object | null>;
    get(key: string, options?: { range?: { offset?: number; length?: number; suffix?: number } }): Promise<R2Object | null>;
  };
  DENY_ALL?: string;
};

type Claims = {
  iss: string;
  aud: string;
  typ: 'exchange' | 'playback';
  exp: number;
  nbf?: number;
  recordingId: string;
  assetVersionId?: never;
  packageId?: string;
  captureId?: string;
  prefix?: string;
  scopeId: string;
  objectKey?: never;
  playback?: string;
  broadcastEpoch?: number;
  rangeRevision?: number;
  purpose?: 'member' | 'staff-preview';
};

const cookieName = 'hhc_media';
const packageRoute = /^\/videos\/([a-zA-Z0-9-]{1,80})\/packages\/([a-f0-9]{32})\/sessions\/([a-zA-Z0-9-]{1,80})\/(cookie|master\.m3u8|previews\/(?:index\.vtt|seg-\d{6}\.jpg)|(?:480p|720p|1080p)\/(?:index\.m3u8|init\.mp4|seg-\d{6}\.m4s))$/;
const finalPrefix = /^recordings\/packages\/([a-f0-9]{32})\/final\/[a-zA-Z0-9-]{1,80}\/$/;
const liveRoute = /^\/videos\/([a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12})\/captures\/([a-f0-9]{32})\/sessions\/([a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12})\/(cookie|master\.m3u8|(?:480p|720p|1080p)\/(?:index\.m3u8|init\.mp4|seg-\d{6}\.m4s))$/;
const previewRoute = new RegExp(liveRoute.source.replace('sessions', 'previews'));
const livePrefix = /^recordings\/captures\/([a-f0-9]{32})\/final\/$/;
const encoder = new TextEncoder();

type BroadcastPolicy = { recordingId: string; epoch: number; rangeRevision: number; startSequence: number | null; endSequenceExclusive: number | null; revoked: boolean; memberState?: 'live' | 'blocked' | 'vod' };

async function boundedText(env: Env, key: string, limit: number): Promise<string | null> {
  const object = await env.MEDIA_BUCKET.get(key);
  if (!object) return null;
  if (!object.body || !Number.isSafeInteger(object.size) || object.size < 1 || object.size > limit) throw new Error('Invalid control object');
  const reader = object.body.getReader();
  const bytes = new Uint8Array(limit); let size = 0;
  for (;;) {
    const {done, value} = await reader.read(); if (done) break;
    if (size + value.length > limit) {await reader.cancel(); throw new Error('Oversized control object');}
    bytes.set(value, size); size += value.length;
  }
  return new TextDecoder().decode(bytes.subarray(0, size));
}

async function broadcastPolicy(env: Env, capture: string): Promise<BroadcastPolicy | null> {
  const raw = await boundedText(env, `recordings/captures/${capture}/final/broadcast.json`, 1024);
  if (raw === null) return null;
  const p = JSON.parse(raw) as BroadcastPolicy;
  const boundary = (n: number | null) => n === null || Number.isSafeInteger(n) && n >= 0 && n <= 2147483647;
  if (!p || !['endSequenceExclusive,epoch,rangeRevision,recordingId,revoked,startSequence','endSequenceExclusive,epoch,memberState,rangeRevision,recordingId,revoked,startSequence'].includes(Object.keys(p).sort().join(',')) ||
      typeof p.recordingId !== 'string' || !Number.isSafeInteger(p.epoch) || p.epoch < 1 || !Number.isSafeInteger(p.rangeRevision) || p.rangeRevision < 1 ||
      typeof p.revoked !== 'boolean' || p.memberState !== undefined && !['live','blocked','vod'].includes(p.memberState) || !boundary(p.startSequence) || !boundary(p.endSequenceExclusive) ||
      p.endSequenceExclusive !== null && (p.startSequence === null || p.endSequenceExclusive <= p.startSequence)) throw new Error('Invalid broadcast policy');
  p.memberState ??= 'blocked';
  return p;
}

function playlistSegments(raw: string): {sequence: number; duration: number}[] {
  if (!raw.startsWith('#EXTM3U\n')) throw new Error('Invalid playlist');
  const segments = [...raw.matchAll(/#EXTINF:(\d+(?:\.\d+)?),\nseg-(\d{6})\.m4s\n/g)].map(m => ({duration:Number(m[1]),sequence:Number(m[2])}));
  if (!segments.length || segments.length > 1440 || segments.some((s,i) => s.sequence !== i || s.duration <= 0 || s.duration > 31)) throw new Error('Invalid playlist timeline');
  return segments;
}

function projectPlaylist(raw: string, policy: BroadcastPolicy): string {
  const segments = playlistSegments(raw);
  const start = policy.startSequence!; const end = Math.min(policy.endSequenceExclusive ?? segments.length, segments.length);
  if (start >= end) throw new Error('Unverified broadcast range');
  const ended = policy.endSequenceExclusive !== null && policy.endSequenceExclusive <= segments.length;
  let out = `#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:31\n#EXT-X-MEDIA-SEQUENCE:${start}\n#EXT-X-PLAYLIST-TYPE:${ended ? 'VOD' : 'EVENT'}\n#EXT-X-MAP:URI="init.mp4"\n`;
  for (const segment of segments.slice(start,end)) out += `#EXTINF:${segment.duration.toFixed(6)},\nseg-${String(segment.sequence).padStart(6,'0')}.m4s\n`;
  if (ended) out += '#EXT-X-ENDLIST\n';
  return out;
}

function projectVtt(raw: string, playlist: string, policy: BroadcastPolicy): string {
  if (!raw.startsWith('WEBVTT\n')) throw new Error('Invalid preview');
  const origin = playlistSegments(playlist).slice(0,policy.startSequence!).reduce((sum,s) => sum+s.duration,0);
  const seconds = (s: string) => s.split(':').reduce((sum,n) => sum*60+Number(n),0);
  const timestamp = (s: number) => {const ms=Math.max(0,Math.round(s*1000));return `${String(Math.floor(ms/3600000)).padStart(2,'0')}:${String(Math.floor(ms/60000)%60).padStart(2,'0')}:${String(Math.floor(ms/1000)%60).padStart(2,'0')}.${String(ms%1000).padStart(3,'0')}`;};
  let out = 'WEBVTT\n\n';
  for (const m of raw.matchAll(/(\d{2}:\d{2}:\d{2}\.\d{3}) --> (\d{2}:\d{2}:\d{2}\.\d{3})\n(seg-(\d{6})\.jpg#xywh=\d+,0,160,90)\n/g)) {
    const sequence=Number(m[4]);if (sequence < policy.startSequence! || policy.endSequenceExclusive !== null && sequence >= policy.endSequenceExclusive) continue;
    const start=seconds(m[1])-origin,end=seconds(m[2])-origin;
    if (start < -0.001 || end <= start) throw new Error('Invalid preview timeline');
    out += `${timestamp(start)} --> ${timestamp(end)}\n${m[3]}\n\n`;
  }
  return out;
}

function failure(status: number): Response {
  return new Response(null, { status, headers: { 'Cache-Control': 'private, no-store' } });
}

function decode64(value: string): Uint8Array<ArrayBuffer> {
  if (!/^[a-zA-Z0-9_-]+$/.test(value)) throw new Error('invalid base64url');
  const raw = atob(value.replace(/-/g, '+').replace(/_/g, '/'));
  return new Uint8Array([...raw].map(char => char.charCodeAt(0)));
}

async function verify(token: string, type: Claims['typ'], env: Env, now: number): Promise<Claims | null> {
  if (token.length > (type === 'exchange' ? 8192 : 3072)) return null;
  try {
    const parts = token.split('.');
    if (parts.length !== 3) return null;
    const header = JSON.parse(new TextDecoder().decode(decode64(parts[0])));
    if (header.alg !== 'ES256' || header.typ !== 'JWT' || typeof header.kid !== 'string') return null;
    const jwk = JSON.parse(env.MEDIA_PUBLIC_KEYS)[header.kid];
    if (!jwk || jwk.kty !== 'EC' || jwk.crv !== 'P-256') return null;
    const key = await crypto.subtle.importKey('jwk', jwk, { name: 'ECDSA', namedCurve: 'P-256' }, false, ['verify']);
    const valid = await crypto.subtle.verify({ name: 'ECDSA', hash: 'SHA-256' }, key, decode64(parts[2]), encoder.encode(`${parts[0]}.${parts[1]}`));
    if (!valid) return null;
    const claims = JSON.parse(new TextDecoder().decode(decode64(parts[1]))) as Claims;
    if (claims.iss !== env.MEDIA_ISSUER || claims.aud !== 'hhc-media' || claims.typ !== type || !Number.isInteger(claims.exp) || claims.exp <= now || claims.exp > now + (type === 'exchange' ? 60 : claims.captureId !== undefined ? 300 : 3600)) return null;
    if (claims.nbf !== undefined && (!Number.isInteger(claims.nbf) || claims.nbf > now)) return null;
    return claims;
  } catch {
    return null;
  }
}

function scoped(claims: Claims, ids: string[], live: boolean, preview = false): boolean {
  if (preview ? claims.purpose !== 'staff-preview' || !Number.isSafeInteger(claims.broadcastEpoch) : claims.purpose === 'staff-preview') return false;
  return claims.recordingId === ids[0] && claims.scopeId === ids[2] && claims.assetVersionId === undefined &&
    (live ? claims.captureId === ids[1] && claims.packageId === undefined : claims.packageId === ids[1] && claims.captureId === undefined);
}

function validResource(claims: Claims, live: boolean): boolean {
  return typeof claims.prefix === 'string' && claims.objectKey === undefined &&
    (live ? livePrefix.exec(claims.prefix)?.[1] === claims.captureId : finalPrefix.exec(claims.prefix)?.[1] === claims.packageId);
}

async function livePointer(env: Env, prefix: string): Promise<{revision: number; lastSequence: number} | null> {
  const object = await env.MEDIA_BUCKET.get(prefix + 'current.json');
  if (!object) return null;
  if (!object.body || !Number.isSafeInteger(object.size) || object.size < 1 || object.size > 128) throw new Error('Invalid live pointer');
  const reader = object.body.getReader();
  const bytes = new Uint8Array(128);
  let size = 0;
  for (;;) {
    const {done, value} = await reader.read();
    if (done) break;
    if (size + value.length > 128) {await reader.cancel(); throw new Error('Invalid live pointer');}
    bytes.set(value, size); size += value.length;
  }
  const value = JSON.parse(new TextDecoder().decode(bytes.subarray(0, size)));
  if (!value || Object.keys(value).sort().join(',') !== 'lastSequence,revision' || !Number.isSafeInteger(value.revision) || value.revision < 1 || value.revision > 1441 || !Number.isSafeInteger(value.lastSequence) || value.lastSequence < 0 || value.lastSequence > 1439) throw new Error('Invalid live pointer');
  return value;
}

function withCors(response: Response, origin: string): Response {
  response.headers.set('Access-Control-Allow-Origin', origin);
  response.headers.set('Access-Control-Allow-Credentials', 'true');
  response.headers.set('Access-Control-Expose-Headers', 'Accept-Ranges, Content-Length, Content-Range, ETag');
  response.headers.set('Vary', 'Origin');
  return response;
}

async function limitedBody(request: Request): Promise<string | null> {
  const reader = request.body?.getReader();
  if (!reader) return null;
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > 8192) { await reader.cancel(); return null; }
    chunks.push(value);
  }
  const bytes = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
  return new TextDecoder().decode(bytes);
}

function parseRange(value: string | null, size: number): { offset?: number; length?: number; suffix?: number } | null | false {
  if (!value) return null;
  const match = /^bytes=(\d*)-(\d*)$/.exec(value);
  if (!match || (!match[1] && !match[2])) return false;
  const start = match[1] ? Number(match[1]) : undefined;
  const end = match[2] ? Number(match[2]) : undefined;
  if ((start !== undefined && !Number.isSafeInteger(start)) || (end !== undefined && !Number.isSafeInteger(end))) return false;
  if (start === undefined) return end! > 0 ? { suffix: Math.min(end!, size) } : false;
  if (start >= size || (end !== undefined && end < start)) return false;
  return { offset: start, length: Math.min(end ?? size - 1, size - 1) - start + 1 };
}

type ExecutionContext = { waitUntil(promise: Promise<unknown>): void };

async function handle(request: Request, env: Env, ctx?: ExecutionContext): Promise<Response> {
  if (env.DENY_ALL === 'true') return failure(503);
  const url = new URL(request.url);
  const preview = previewRoute.test(url.pathname);
  const live = preview || liveRoute.test(url.pathname);
  const match = preview ? previewRoute.exec(url.pathname) : live ? liveRoute.exec(url.pathname) : packageRoute.exec(url.pathname);
  if (!match) return failure(404);
  if (url.search) return failure(404);
  const ids = match.slice(1, 4);
  const path = url.pathname.slice(0, -match[4].length);
  const origin = request.headers.get('Origin');
  const allowed = env.ALLOWED_ORIGINS.split(',').map(value => value.trim()).includes(origin ?? '');
  if (origin && !allowed) return failure(403);
  if (request.method === 'OPTIONS') {
    if (!allowed) return failure(403);
    return withCors(new Response(null, { status: 204, headers: {
      'Access-Control-Allow-Methods': match[4] === 'cookie' ? 'POST, DELETE, OPTIONS' : 'GET, HEAD, OPTIONS',
      'Access-Control-Allow-Headers': 'Content-Type, Range',
      'Cache-Control': 'private, no-store',
    } }), origin!);
  }
  if (match[4] === 'cookie') {
    if (!allowed) return failure(403);
    if (request.method === 'DELETE') {
      const response = withCors(failure(204), origin!);
      response.headers.set('Set-Cookie', `${cookieName}=; Path=${path}; Max-Age=0; Secure; HttpOnly; SameSite=Strict`);
      return response;
    }
    if (request.method !== 'POST') return failure(405);
    const body = await limitedBody(request);
    if (!body) return failure(400);
    let credential: string;
    try { credential = JSON.parse(body).credential; } catch { return failure(400); }
    if (typeof credential !== 'string') return failure(400);
    const now = Math.floor(Date.now() / 1000);
    const exchange = await verify(credential, 'exchange', env, now);
    if (!exchange || !scoped(exchange, ids, live, preview) || typeof exchange.playback !== 'string') return failure(401);
    const playback = await verify(exchange.playback, 'playback', env, now);
    if (!playback || !scoped(playback, ids, live, preview) || !validResource(playback, live) || playback.exp < exchange.exp || exchange.playback.length > 3072) return failure(401);
    const response = withCors(failure(204), origin!);
    response.headers.set('Set-Cookie', `${cookieName}=${exchange.playback}; Path=${path}; Max-Age=${playback.exp - now}; Secure; HttpOnly; SameSite=Strict`);
    return response;
  }
  if (request.method !== 'GET' && request.method !== 'HEAD') return failure(405);
  const cookie = request.headers.get('Cookie')?.split(';').map(value => value.trim()).find(value => value.startsWith(`${cookieName}=`))?.slice(cookieName.length + 1);
  const playback = cookie && await verify(cookie, 'playback', env, Math.floor(Date.now() / 1000));
  if (!playback || !scoped(playback, ids, live, preview) || !validResource(playback, live)) return failure(401);
  const policy = await broadcastPolicy(env, ids[1]);
  if (!policy && playback.broadcastEpoch !== undefined) return failure(503);
  if (policy) {
    if (policy.recordingId !== ids[0] || playback.broadcastEpoch !== policy.epoch) return failure(403);
    // Older grants retain continuity across End; current authority still controls every byte.
    if (!preview && (policy.revoked || policy.memberState === 'blocked' ||
        !live && policy.memberState !== 'vod' || playback.purpose !== 'member' ||
        !Number.isSafeInteger(playback.rangeRevision) || playback.rangeRevision! < 1 ||
        playback.rangeRevision! > policy.rangeRevision || policy.startSequence === null)) return failure(403);
    const segment = /seg-(\d{6})\.(?:m4s|jpg)$/.exec(match[4]);
    if (!preview && segment && (Number(segment[1]) < policy.startSequence! || policy.endSequenceExclusive !== null && Number(segment[1]) >= policy.endSequenceExclusive)) return failure(404);
  }
  let objectKey = playback.prefix! + match[4];
  if (live || policy) {
    const rawPrefix = policy ? `recordings/captures/${ids[1]}/final/` : playback.prefix!;
    const pointer = await livePointer(env, rawPrefix);
    if (!pointer) return failure(404);
    const sequence = /seg-(\d{6})\.m4s$/.exec(match[4]);
    if (sequence && Number(sequence[1]) > pointer.lastSequence) return failure(404);
    if (policy && !preview && policy.startSequence! > pointer.lastSequence) return failure(503);
    if (!match[4].startsWith('previews/')) objectKey = rawPrefix + match[4];
    if (match[4].endsWith('.m3u8')) {
      objectKey = rawPrefix + `playlists/${pointer.revision}/` + match[4];
      if (policy && !preview) {
        const raw = await boundedText(env, objectKey, 1 << 20);
        if (raw === null) return failure(404);
        const body = match[4] === 'master.m3u8' ? raw : projectPlaylist(raw, policy);
        const response = new Response(request.method === 'HEAD' ? null : body, {headers:{'Cache-Control':'private, no-store','Content-Type':'application/vnd.apple.mpegurl','X-Content-Type-Options':'nosniff'}});
        return origin ? withCors(response, origin) : response;
      }
    }
  }
  if (match[4].startsWith('previews/')) {
    const pointer = await env.MEDIA_BUCKET.get(playback.prefix! + 'previews/current.json');
    if (!pointer) return failure(404);
    if (!pointer.body || !Number.isSafeInteger(pointer.size) || pointer.size <= 0 || pointer.size > 128) return failure(503);
    const reader = pointer.body.getReader();
    const chunks: Uint8Array[] = [];
    let size = 0;
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > 128) { await reader.cancel(); return failure(503); }
      chunks.push(value);
    }
    const bytes = new Uint8Array(size);
    let offset = 0;
    for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.byteLength; }
    const attempt = /^\{"attempt":"([a-zA-Z0-9-]{1,80})"\}$/.exec(new TextDecoder().decode(bytes))?.[1];
    if (!attempt) return failure(503);
    objectKey = playback.prefix! + `previews/${attempt}/` + match[4].slice('previews/'.length);
  }
  if (policy && match[4] === 'previews/index.vtt') {
    const rawPrefix = `recordings/captures/${ids[1]}/final/`;
    const pointer = await livePointer(env, rawPrefix);if (!pointer) return failure(503);
    const playlist = await boundedText(env, rawPrefix + `playlists/${pointer.revision}/480p/index.m3u8`, 1 << 20);
    const raw = await boundedText(env, objectKey, 1 << 20);if (playlist === null || raw === null) return failure(404);
    const body = projectVtt(raw, playlist, policy);
    const response = new Response(request.method === 'HEAD' ? null : body, {headers:{'Cache-Control':'private, no-store','Content-Type':'text/vtt','X-Content-Type-Options':'nosniff'}});
    return origin ? withCors(response, origin) : response;
  }
  const cache = (globalThis.caches as CacheStorage & { default?: Cache } | undefined)?.default;
  const cacheKey = new Request(`${url.origin}/__hhc_media_cache/${objectKey}`);
  if (cache && !request.headers.has('If-Range')) {
    const headers = new Headers();
    for (const name of ['Range', 'If-None-Match']) {
      const value = request.headers.get(name);
      if (value) headers.set(name, value);
    }
    const cached = await cache.match(new Request(cacheKey, { headers })).catch(() => undefined);
    if (cached) {
      const response = new Response(request.method === 'HEAD' ? null : cached.body, cached);
      response.headers.set('Cache-Control', 'private, no-store');
      return origin ? withCors(response, origin) : response;
    }
  }
  const object = await env.MEDIA_BUCKET.head(objectKey);
  if (!object) return failure(404);
  if (!Number.isSafeInteger(object.size) || object.size <= 0 || object.size > (match[4].endsWith('.m3u8') || match[4].startsWith('previews/') ? 1 << 20 : 128 << 20)) return failure(503);
  const mime = match[4].endsWith('.vtt') ? 'text/vtt' : match[4].endsWith('.jpg') ? 'image/jpeg' : match[4].endsWith('.m3u8') ? 'application/vnd.apple.mpegurl' : match[4].endsWith('.m4s') ? 'video/iso.segment' : 'video/mp4';
  const headers = new Headers({ 'Cache-Control': 'private, no-store', 'Content-Type': mime, 'X-Content-Type-Options': 'nosniff', 'Accept-Ranges': 'bytes', ETag: object.httpEtag });
  if (request.headers.get('If-None-Match') === object.httpEtag) {
    const response = new Response(null, { status: 304, headers });
    return origin ? withCors(response, origin) : response;
  }
  const range = parseRange(request.headers.get('If-Range') && request.headers.get('If-Range') !== object.httpEtag ? null : request.headers.get('Range'), object.size);
  if (range === false) {
    headers.set('Content-Range', `bytes */${object.size}`);
    const response = new Response(null, { status: 416, headers });
    return origin ? withCors(response, origin) : response;
  }
  const length = range ? (range.suffix ?? range.length!) : object.size;
  if (range) headers.set('Content-Range', `bytes ${range.offset ?? object.size - length}-${(range.offset ?? object.size - length) + length - 1}/${object.size}`);
  headers.set('Content-Length', String(length));
  if (request.method === 'HEAD') {
    const response = new Response(null, { status: range ? 206 : 200, headers });
    return origin ? withCors(response, origin) : response;
  }
  const data = await env.MEDIA_BUCKET.get(objectKey, range ? { range } : undefined);
  if (!data?.body) return failure(503);
  const response = new Response(data.body, { status: range ? 206 : 200, headers });
  if (cache && ctx && !range) {
    const internal = response.clone();
    internal.headers.set('Cache-Control', 'public, max-age=86400');
    ctx.waitUntil(cache.put(cacheKey, internal).catch(() => undefined));
  }
  return origin ? withCors(response, origin) : response;
}

export default { fetch: async (request: Request, env: Env, ctx?: ExecutionContext) => {
  try { return await handle(request, env, ctx); } catch { return failure(503); }
} };
