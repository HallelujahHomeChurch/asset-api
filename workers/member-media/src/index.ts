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
  assetVersionId: string;
  scopeId: string;
  objectKey?: string;
  playback?: string;
};

const cookieName = 'hhc_media';
const route = /^\/videos\/([a-zA-Z0-9-]{1,80})\/files\/([a-zA-Z0-9-]{1,80})\/sessions\/([a-zA-Z0-9-]{1,80})\/(cookie|content)$/;
const encoder = new TextEncoder();

function failure(status: number): Response {
  return new Response(null, { status, headers: { 'Cache-Control': 'private, no-store' } });
}

function decode64(value: string): Uint8Array<ArrayBuffer> {
  if (!/^[a-zA-Z0-9_-]+$/.test(value)) throw new Error('invalid base64url');
  const raw = atob(value.replace(/-/g, '+').replace(/_/g, '/'));
  return new Uint8Array([...raw].map(char => char.charCodeAt(0)));
}

async function verify(token: string, type: Claims['typ'], env: Env, now: number): Promise<Claims | null> {
  if (token.length > 3072) return null;
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
    if (claims.iss !== env.MEDIA_ISSUER || claims.aud !== 'hhc-media' || claims.typ !== type || !Number.isInteger(claims.exp) || claims.exp <= now || claims.exp > now + (type === 'exchange' ? 60 : 3600)) return null;
    if (claims.nbf !== undefined && (!Number.isInteger(claims.nbf) || claims.nbf > now)) return null;
    return claims;
  } catch {
    return null;
  }
}

function scoped(claims: Claims, ids: string[]): boolean {
  return claims.recordingId === ids[0] && claims.assetVersionId === ids[1] && claims.scopeId === ids[2];
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

async function handle(request: Request, env: Env): Promise<Response> {
  if (env.DENY_ALL === 'true') return failure(503);
  const url = new URL(request.url);
  const match = route.exec(url.pathname);
  if (!match) return failure(404);
  const ids = match.slice(1, 4);
  const path = url.pathname.slice(0, url.pathname.lastIndexOf('/') + 1);
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
    if (!exchange || !scoped(exchange, ids) || typeof exchange.playback !== 'string') return failure(401);
    const playback = await verify(exchange.playback, 'playback', env, now);
    if (!playback || !scoped(playback, ids) || !playback.objectKey?.startsWith('recordings/') || playback.exp < exchange.exp || exchange.playback.length > 3072) return failure(401);
    const response = withCors(failure(204), origin!);
    response.headers.set('Set-Cookie', `${cookieName}=${exchange.playback}; Path=${path}; Max-Age=${playback.exp - now}; Secure; HttpOnly; SameSite=Strict`);
    return response;
  }
  if (request.method !== 'GET' && request.method !== 'HEAD') return failure(405);
  const cookie = request.headers.get('Cookie')?.split(';').map(value => value.trim()).find(value => value.startsWith(`${cookieName}=`))?.slice(cookieName.length + 1);
  const playback = cookie && await verify(cookie, 'playback', env, Math.floor(Date.now() / 1000));
  if (!playback || !scoped(playback, ids) || !playback.objectKey?.startsWith('recordings/')) return failure(401);
  const object = await env.MEDIA_BUCKET.head(playback.objectKey);
  if (!object) return failure(404);
  const headers = new Headers({ 'Cache-Control': 'private, no-store', 'Content-Type': 'video/mp4', 'Accept-Ranges': 'bytes', ETag: object.httpEtag });
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
  const data = await env.MEDIA_BUCKET.get(playback.objectKey, range ? { range } : undefined);
  if (!data?.body) return failure(503);
  const response = new Response(data.body, { status: range ? 206 : 200, headers });
  return origin ? withCors(response, origin) : response;
}

export default { fetch: handle };
