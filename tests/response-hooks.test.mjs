import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { once } from 'node:events';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../', import.meta.url));
const hook = await import(path.join(root, 'plugin', 'response.hooks.mjs'));

async function fixture(reply, ctx, enabled = true) {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'model-guard-hook-'));
  let received = null;
  const server = createServer((req, res) => {
    let body = '';
    req.on('data', chunk => { body += chunk; });
    req.on('end', () => {
      received = { url: req.url, auth: req.headers.authorization, body: JSON.parse(body) };
      res.writeHead(202, { 'content-type': 'application/json' });
      res.end('{"accepted":true}');
    });
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  const origin = `http://127.0.0.1:${server.address().port}`;
  await writeFile(path.join(dir, 'response-hook.json'), JSON.stringify({ origin, control: 'test-control', enabled }));
  try {
    await hook.onResponse(reply, { ...ctx, options: { ...(ctx.options || {}), dataDir: dir } });
    const requested = String(ctx.request?.model || '').replace(/^magpie\//, '');
    if (enabled && ctx.success && !requested.startsWith('model-guard-probe/')) {
      const deadline = Date.now() + 2000;
      while (!received && Date.now() < deadline) await new Promise(r => setTimeout(r, 20));
    }
    return received;
  } finally {
    server.close();
    await rm(dir, { recursive: true, force: true });
  }
}

test('reports the real model from a chat reply', async () => {
  const got = await fixture(
    { model: 'vendor/real-model' },
    { success: true, request: { model: 'vendor/asked' }, protocol: 'chat', agent: 'omp' },
  );
  assert.ok(got, 'hook must call the backend');
  assert.equal(got.url, '/api/response-model');
  assert.equal(got.auth, 'Bearer test-control');
  assert.deepEqual(got.body, {
    requested_model: 'vendor/asked',
    actual_model: 'vendor/real-model',
    response_id: '',
    protocol: 'chat',
    agent: 'omp',
    success: true,
  });
});

test('reports a reply id from each native protocol shape', async () => {
  const cases = [
    [{ id: 'chatcmpl-1', model: 'vendor/real' }, 'chatcmpl-1'],
    [{ id: 'resp_1', model: 'vendor/real' }, 'resp_1'],
    [{ response: { id: 'resp_2', model: 'vendor/real' } }, 'resp_2'],
    [{ message: { id: 'msg_1', model: 'vendor/real' } }, 'msg_1'],
    [{ responseId: 'gem-1', modelVersion: 'gemini-3-flash' }, 'gem-1'],
  ];
  for (const [reply, expected] of cases) {
    const got = await fixture(reply, { success: true, request: { model: 'vendor/asked' } });
    assert.equal(got.body.response_id, expected, JSON.stringify(reply));
  }
});

test('reads modelVersion from a gemini reply', async () => {
  const got = await fixture(
    { candidates: [{ modelVersion: 'gemini-3-flash' }] },
    { success: true, request: { model: 'gemini-3-pro' }, protocol: 'gemini' },
  );
  assert.equal(got.body.actual_model, 'gemini-3-flash');
});

test('does nothing when disabled, unsuccessful, or a probe', async () => {
  assert.equal(await fixture({ model: 'vendor/x' }, { success: true, request: { model: 'vendor/a' } }, false), null);
  assert.equal(await fixture({ model: 'vendor/x' }, { success: false, request: { model: 'vendor/a' } }), null);
  assert.equal(await fixture({ model: 'vendor/x' }, { success: true, request: { model: 'model-guard-probe/a/vendor/a' } }), null);
  assert.equal(await fixture({ model: 'vendor/x' }, { success: true, request: { model: 'magpie/model-guard-probe/a/vendor/a' } }), null);
});
