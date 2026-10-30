import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { readFile, mkdtemp, rm } from 'node:fs/promises';
import { createInterface } from 'node:readline';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const root = fileURLToPath(new URL('../', import.meta.url));

test('manual block survives monitoring pause, probe bypass and restart', { timeout: 30000 }, async () => {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'model-guard-gate-'));
  const gate = path.join(dir, 'gate.middleware.js');
  let child;
  let info;
  async function start() {
    child = spawn(path.join(root, 'bin', 'model-guard'), ['-data', dir, '-gate', gate, '-owned'], { stdio: ['pipe', 'pipe', 'pipe'] });
    const lines = createInterface({ input: child.stdout });
    const [line] = await once(lines, 'line', { signal: AbortSignal.timeout(10000) });
    info = JSON.parse(line);
    lines.close();
  }
  async function stop() {
    const exited = once(child, 'exit');
    child.kill();
    await exited;
  }
  async function post(route, body) {
    const res = await fetch(info.origin + route, { method: 'POST', headers: { authorization: 'Bearer ' + info.control, 'content-type': 'application/json' }, body: JSON.stringify(body) });
    assert.equal(res.status, 200, await res.text());
  }
  async function middleware() {
    const code = await readFile(gate, 'utf8');
    return { module: await import('data:text/javascript;base64,' + Buffer.from(code).toString('base64')), alias: JSON.parse(code.match(/const alias=(.*);/)[1]) };
  }
  function request(module, body, model) {
    let status = 200;
    const out = module.onRequest(body, { model, reject(code) { status = code; } });
    return { status, out };
  }
  try {
    await start();
    await post('/api/targets', { model: 'vendor/model', enabled: true, prompt_ids: ['candy'] });
    await post('/api/targets/action', { model: 'vendor/model', action: 'disable' });
    await post('/api/targets/action', { model: 'vendor/model', action: 'monitor-off' });
    let { module, alias } = await middleware();
    assert.equal(request(module, { model: 'vendor/model' }).status, 403);
    assert.equal(request(module, {}, 'vendor/model').status, 403);
    assert.equal(request(module, { model: 'magpie/vendor/model' }).status, 403);
    assert.equal(request(module, { model: 'vendor/other' }).status, 200);
    assert.deepEqual(request(module, null), { status: 200, out: null });
    assert.deepEqual(request(module, { model: 'model-guard-probe/' + alias + '/vendor/model' }), { status: 200, out: { model: 'vendor/model' } });
    await stop();
    await start();
    ({ module } = await middleware());
    assert.equal(request(module, { model: 'vendor/model' }).status, 403);
    await post('/api/targets/action', { model: 'vendor/model', action: 'allow' });
    ({ module } = await middleware());
    assert.equal(request(module, { model: 'vendor/model' }).status, 200);
    await post('/api/targets/action', { model: 'vendor/model', action: 'disable' });
    ({ module } = await middleware());
    assert.equal(request(module, { model: 'vendor/model' }).status, 403);
    await post('/api/targets/action', { model: 'vendor/model', action: 'enable' });
    ({ module } = await middleware());
    assert.equal(request(module, { model: 'vendor/model' }).status, 200);
  } finally {
    if (child && child.exitCode === null && child.signalCode === null) await stop();
    await rm(dir, { recursive: true, force: true });
  }
});
