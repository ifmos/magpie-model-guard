import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { mkdtemp, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import test from 'node:test';

const root = fileURLToPath(new URL('../', import.meta.url));

// A crashed host cannot run JavaScript exit hooks; its stdin pipe still closes.
test('plugin-owned backend exits when its host pipe closes', { timeout: 10000 }, async t => {
  const dir = await mkdtemp(path.join(os.tmpdir(), 'guard-owner-lifecycle-'));
  const platform = { win32: 'windows', darwin: 'darwin', linux: 'linux' }[process.platform] || process.platform;
  const arch = { x64: 'amd64' }[process.arch] || process.arch;
  const binary = path.join(root, 'bin', `model-guard-${platform}-${arch}${process.platform === 'win32' ? '.exe' : ''}`);
  const child = spawn(binary, [
    '-data', dir,
    '-gate', path.join(dir, 'gate.middleware.js'),
    '-owned',
  ], { stdio: ['pipe', 'pipe', 'pipe'] });
  const exited = once(child, 'exit');
  t.after(async () => {
    if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
    await exited;
    await rm(dir, { recursive: true, force: true });
  });
  const info = await new Promise((resolve, reject) => {
    let output = '';
    child.once('error', reject);
    child.once('exit', () => reject(new Error('backend exited before its startup handshake')));
    child.stdout.on('data', chunk => {
      output += chunk;
      const end = output.indexOf('\n');
      if (end >= 0) {
        try { resolve(JSON.parse(output.slice(0, end))); }
        catch { reject(new Error('backend startup handshake was invalid')); }
      }
    });
  });
  const headers = { authorization: `Bearer ${info.control}` };
  assert.equal((await fetch(info.origin + '/api/state', { headers })).status, 200);
  child.stdin.end();
  const [code, signal] = await exited;
  assert.equal(signal, null);
  assert.equal(code, 0, 'host loss must stop the scheduler and HTTP server cleanly');
  await assert.rejects(fetch(info.origin + '/api/state', { headers }), 'management listener must close');
});
