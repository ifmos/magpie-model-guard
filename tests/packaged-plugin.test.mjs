import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { cp, mkdir, mkdtemp, readdir, rm } from 'node:fs/promises';
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
      assert.match(url.pathname, /^\\/open\\//);
      const login = await fetch(url, { redirect: 'manual' });
      assert.equal(login.status, 303);
      const session = login.headers.get('set-cookie')?.split(';')[0];
      assert.ok(session);
      assert.equal((await fetch(url.origin + '/api/state')).status, 401);
      assert.equal((await fetch(url.origin + '/api/health', { headers: { cookie: session } })).status, 200);
      const response = await fetch(url.origin + '/api/state', { headers: { cookie: session } });
      assert.equal(response.status, 200);
      const state = await response.json();
      assert.ok(state.prompts.some(p => p.id === 'candy'));
      const target = await fetch(url.origin + '/api/targets', {
        method: 'POST', headers: { cookie: session, 'content-type': 'application/json' },
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

// The npm package must carry every resource the remote console needs: the
// cloudflared auto-installer copies third_party/cloudflared/LICENSE next to
// the binary it downloads.
test('npm package files include the cloudflared third-party license', async () => {
  const packDir = await mkdtemp(path.join(os.tmpdir(), 'model-guard-pack-'));
  try {
    await exec('npm', ['pack', '-q', '--pack-destination', packDir], { cwd: root });
    const [archive] = await readdir(packDir);
    const listing = await exec('tar', ['-tzf', path.join(packDir, archive)]);
    assert.match(listing.stdout, /package\/third_party\/cloudflared\/LICENSE$/m);
    assert.match(listing.stdout, /package\/plugin\/cloudflared-runtime\.mjs$/m);
    assert.match(listing.stdout, /package\/plugin\/console-readiness\.mjs$/m);
  } finally {
    await rm(packDir, { recursive: true, force: true });
  }
});
