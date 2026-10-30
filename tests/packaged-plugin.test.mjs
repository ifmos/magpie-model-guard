import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { cp, mkdir, mkdtemp, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';
import test from 'node:test';

const exec = promisify(execFile);
const root = fileURLToPath(new URL('../', import.meta.url));

// Exercise the distributed package with no Go toolchain or development binary.
test('GitHub package starts its bundled backend and opens the console without Go', async () => {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'model-guard-package-'));
  try {
    const platform = { win32: 'windows' }[process.platform] || process.platform;
    const arch = { x64: 'amd64' }[process.arch] || process.arch;
    const name = `model-guard-${platform}-${arch}${platform === 'windows' ? '.exe' : ''}`;
    await cp(path.join(root, 'plugin'), path.join(dir, 'plugin'), { recursive: true });
    await cp(path.join(root, 'package.json'), path.join(dir, 'package.json'));
    await mkdir(path.join(dir, 'bin'));
    await cp(path.join(root, 'bin', name), path.join(dir, 'bin', name));
    const script = `
      import assert from 'node:assert/strict';
      import plugin from './plugin/index.mjs';
      const hooks = await plugin.server({}, { dataDir: './state', magpieConfigDir: './config/magpie' });
      const cfg = {};
      await hooks.config(cfg);
      const entry = await hooks.auth.methods[0].authorize();
      const url = new URL(entry.url);
      const token = new URLSearchParams(url.hash.slice(1)).get('token');
      assert.ok(token);
      assert.equal((await fetch(url.origin)).status, 200);
      const headers = { authorization: 'Bearer ' + token, 'content-type': 'application/json' };
      assert.equal((await fetch(url.origin + '/api/state')).status, 401);
      const response = await fetch(url.origin + '/api/state', { headers });
      assert.equal(response.status, 200);
      const state = await response.json();
      assert.ok(state.prompts.some(p => p.id === 'candy'));
      const target = await fetch(url.origin + '/api/targets', {
        method: 'POST', headers,
        body: JSON.stringify({ model: 'example/model', enabled: false, prompt_ids: ['candy'], interval_min: 30 }),
      });
      assert.equal(target.status, 200, await target.text());
      console.log('packaged backend, console, authentication, candy bank and new target: OK');
      process.exit(0);
    `;
    const { stdout } = await exec(process.execPath, ['--input-type=module', '-e', script], {
      cwd: dir,
      env: { ...process.env, PATH: '' },
      timeout: 25_000,
    });
    assert.match(stdout, /packaged backend, console, authentication, candy bank and new target: OK/);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});
