import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import { createInterface } from 'node:readline';
import { copyFile, mkdir, mkdtemp, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
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

const hostRuntimes = [{ name: 'Node', binary: process.execPath }];
if (process.env.MAGPIE_BUN_TEST_BINARY) {
  hostRuntimes.push({ name: 'Bun', binary: process.env.MAGPIE_BUN_TEST_BINARY });
}

for (const runtime of hostRuntimes) {
  test(`plugin stops its backend on RPC host EOF while the host remains alive (${runtime.name})`, { timeout: 15000 }, async t => {
    const dir = await mkdtemp(path.join(os.tmpdir(), 'guard-rpc-lifecycle-'));
    const entry = path.join(dir, 'plugin', 'index.mjs');
    await mkdir(path.dirname(entry));
    await copyFile(path.join(root, 'plugin', 'index.mjs'), entry);
    const platform = { win32: 'windows', darwin: 'darwin', linux: 'linux' }[process.platform] || process.platform;
    const arch = { x64: 'amd64' }[process.arch] || process.arch;
    const binary = path.join(root, 'bin', `model-guard-${platform}-${arch}${process.platform === 'win32' ? '.exe' : ''}`);
    const options = {
      binaryPath: binary,
      dataDir: path.join(dir, 'guard'),
      magpieConfigDir: path.join(dir, 'config', 'magpie'),
      gatewayURL: 'http://127.0.0.1:1',
      gatewayKey: 'fixture',
    };
    const script = `
      import { createInterface } from 'node:readline';
      const rl = createInterface({ input: process.stdin, terminal: false });
      // Keep the host alive after EOF, so process exit cannot hide missing plugin cleanup.
      setInterval(() => {}, 1000);
      rl.on('line', async line => {
        const message = JSON.parse(line);
        const { default: plugin } = await import(${JSON.stringify(pathToFileURL(entry).href)});
        const hooks = await plugin.server({}, ${JSON.stringify(options)});
        await hooks.config({});
        const entry = await hooks.auth.methods[0].authorize();
        console.log(JSON.stringify({ id: message.id, result: entry.url }));
      });
    `;
    const env = { ...process.env };
    delete env.MAGPIE_MODEL_GUARD_DISCOVERY;
    const host = spawn(runtime.binary, ['--input-type=module', '-e', script], {
      env, stdio: ['pipe', 'pipe', 'pipe'],
    });
    const exited = once(host, 'exit');
    const replies = createInterface({ input: host.stdout });
    t.after(async () => {
      if (host.exitCode === null && host.signalCode === null) host.kill('SIGKILL');
      await exited;
      replies.close();
      await rm(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 50 });
    });
    function request(message) {
      return new Promise((resolve, reject) => {
        const cleanup = () => {
          replies.off('line', onLine);
          host.off('exit', onExit);
          host.off('error', onError);
        };
        const onLine = line => {
          const reply = JSON.parse(line);
          if (reply.id !== message.id) return;
          cleanup();
          resolve(reply.result);
        };
        const onExit = () => { cleanup(); reject(new Error('RPC host exited unexpectedly')); };
        const onError = error => { cleanup(); reject(error); };
        replies.on('line', onLine);
        host.once('exit', onExit);
        host.once('error', onError);
        // A split message exercises the shared stream without an extra plugin reader.
        const wire = JSON.stringify(message) + '\n';
        const middle = Math.floor(wire.length / 2);
        host.stdin.write(wire.slice(0, middle));
        host.stdin.write(wire.slice(middle));
      });
    }
    const url = new URL(await request({ id: 1, method: 'load' }));
    const token = new URLSearchParams(url.hash.slice(1)).get('token');
    const headers = { authorization: `Bearer ${token}` };
    assert.equal((await fetch(url.origin + '/api/state', { headers })).status, 200);
    const nextUrl = new URL(await request({ id: 2, method: 'authorize' }));
    const nextToken = new URLSearchParams(nextUrl.hash.slice(1)).get('token');
    assert.equal((await fetch(nextUrl.origin + '/api/state', {
      headers: { authorization: `Bearer ${nextToken}` },
    })).status, 200);
    host.stdin.end();
    let listenerClosed = false;
    const deadline = Date.now() + 3000;
    while (Date.now() < deadline) {
      try {
        await fetch(url.origin + '/api/state', { headers, signal: AbortSignal.timeout(250) });
      } catch (error) {
        if (error.name === 'TypeError') { listenerClosed = true; break; }
        if (error.name !== 'TimeoutError') throw error;
      }
      await new Promise(resolve => setTimeout(resolve, 25));
    }
    assert.equal(listenerClosed, true, 'the plugin must stop its backend after RPC input closes');
    assert.equal(host.exitCode, null, 'the test host must remain alive after EOF');
    assert.equal(host.signalCode, null);
  });
}
