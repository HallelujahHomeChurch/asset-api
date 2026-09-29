import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';

test('Pages bundle rejects media reads without a scoped cookie', async () => {
  execFileSync('npm', ['run', 'build'], { cwd: new URL('..', import.meta.url), stdio: 'pipe' });
  const { default: media } = await import(new URL('../pages/test/dist/_worker.js', import.meta.url));
  const url = 'https://hhc-member-media-test.pages.dev/videos/rec-1/files/file-1/sessions/scope-1/content';
  const env = { ALLOWED_ORIGINS: 'https://www-test.alive.org.tw' };
  for (const method of ['GET', 'HEAD']) {
    const response = await media.fetch(new Request(url, { method }), env);
    assert.equal(response.status, 401);
    assert.equal(response.headers.get('Content-Length'), null);
    assert.equal(response.headers.get('ETag'), null);
  }
  const range = await media.fetch(new Request(url, { headers: { Range: 'bytes=0-1' } }), env);
  assert.equal(range.status, 401);
  assert.equal(range.headers.get('Content-Range'), null);
});

test('test and production Pages deployments bind separate private media', () => {
  assert.deepEqual(
    readFileSync(new URL('../pages/test/dist/_worker.js', import.meta.url)),
    readFileSync(new URL('../pages/production/dist/_worker.js', import.meta.url)),
  );
  const expected = [
    ['test', 'hhc-member-media-test', 'hhc-member-recordings-test', 'hhc-media-test', 'https://www-test.alive.org.tw'],
    ['production', 'hhc-member-media', 'hhc-member-recordings-prod', 'hhc-media-prod', 'https://www.alive.org.tw'],
  ];
  for (const [environment, name, bucket, issuer, origin] of expected) {
    const config = JSON.parse(readFileSync(new URL(`../pages/${environment}/wrangler.jsonc`, import.meta.url), 'utf8'));
    assert.equal(config.name, name);
    assert.equal(config.pages_build_output_dir, './dist');
    assert.deepEqual(config.r2_buckets, [{ binding: 'MEDIA_BUCKET', bucket_name: bucket }]);
    assert.deepEqual(config.vars, { MEDIA_ISSUER: issuer, ALLOWED_ORIGINS: origin });
    assert.equal(config.routes, undefined);
    assert.equal(config.workers_dev, undefined);
  }
});
