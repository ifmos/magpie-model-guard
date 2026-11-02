// Manual integration acceptance for Magpie routing-group synchronization.
//
// Usage:
//   node scripts/verify-auto-routing.mjs
//
// Environment:
//   MAGPIE_TEST_BINARY        real magpie executable (default: PATH, ~/.local/bin, /Applications)
//   MODEL_GUARD_TEST_BINARY   model-guard backend (default: bin/model-guard)
//
// Everything runs against a throwaway XDG_CONFIG_HOME / XDG_STATE_HOME under
// os.tmpdir(). It never reads or writes the user's real Magpie configuration,
// never prints provider keys or control tokens, and always kills every child
// process and removes the temporary directory.

import assert from 'node:assert/strict';
import { spawn, execFile } from 'node:child_process';
import { constants } from 'node:fs';
import { access, chmod, mkdir, copyFile, mkdtemp, readFile, readdir, rename, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { randomUUID } from 'node:crypto';
import { createInterface } from 'node:readline';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);
const root = fileURLToPath(new URL('../', import.meta.url));

const MAGPIE_KEY = 'magpie';
const MODEL = 'test-model';
const BAD_MODEL = 'guard-bad/test-model';
const GOOD_MODEL = 'guard-good/test-model';
const GROUP = 'group/guard-order';
const AUTO = 'auto/test-model';
const AUTO_ID = 'auto-test-model';
const PROMPT_ID = 'candy';
const BUSINESS_MARK = 'guard-business';
const ROUTE_SETTLE_MS = 2500;

const secrets = new Set();

function addSecret(value) {
  if (value) secrets.add(String(value));
}

function sanitize(value) {
  let out = String(value ?? '');
  for (const secret of secrets) out = out.split(secret).join('***');
  out = out.replace(/"(control|key|token|authorization)"\s*:\s*"[^"]*"/gi, '"$1":"***"');
  out = out.replace(/Bearer\s+[A-Za-z0-9._~+/=-]+/gi, 'Bearer ***');
  out = out.replace(/\b(key|token|authorization)\s*[=:]\s*("[^"]*"|'[^']*'|[^\s,;]+)/gi, '$1=***');
  return out;
}

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

async function waitFor(label, fn, { timeout = 30000, interval = 250 } = {}) {
  const started = Date.now();
  let last;
  while (Date.now() - started < timeout) {
    try {
      const value = await fn();
      if (value) return value;
    } catch (error) {
      last = error;
    }
    await sleep(interval);
  }
  throw new Error(`timed out after ${timeout}ms waiting for ${label}${last ? `: ${last.message}` : ''}`);
}

async function isExecutable(candidate) {
  if (!candidate) return false;
  try {
    await access(candidate, constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

async function findOnPath(name) {
  const names = process.platform === 'win32' ? [`${name}.exe`, `${name}.cmd`, name] : [name];
  for (const dir of (process.env.PATH || '').split(path.delimiter).filter(Boolean)) {
    for (const file of names) {
      const candidate = path.join(dir, file);
      if (await isExecutable(candidate)) return candidate;
    }
  }
  return null;
}

async function resolveMagpieBinary() {
  const explicit = process.env.MAGPIE_TEST_BINARY;
  if (explicit) {
    assert.ok(await isExecutable(explicit), `MAGPIE_TEST_BINARY is not executable: ${explicit}`);
    return explicit;
  }
  const candidates = [
    await findOnPath('magpie'),
    path.join(os.homedir(), '.local', 'bin', 'magpie'),
    '/Applications/magpie.app/Contents/MacOS/magpie',
  ];
  for (const candidate of candidates) {
    if (await isExecutable(candidate)) return candidate;
  }
  throw new Error('magpie was not found; set MAGPIE_TEST_BINARY or put it on PATH');
}

async function resolveGuardBinary() {
  const explicit = process.env.MODEL_GUARD_TEST_BINARY;
  if (explicit) {
    assert.ok(await isExecutable(explicit), `MODEL_GUARD_TEST_BINARY is not executable: ${explicit}`);
    return explicit;
  }
  const platform = { win32: 'windows' }[process.platform] || process.platform;
  const arch = { x64: 'amd64' }[process.arch] || process.arch;
  const suffix = platform === 'windows' ? '.exe' : '';
  const candidates = [
    path.join(root, 'bin', `model-guard-${platform}-${arch}${suffix}`),
    path.join(root, 'bin', `model-guard${suffix}`),
  ];
  for (const candidate of candidates) {
    if (await isExecutable(candidate)) return candidate;
  }
  throw new Error('model-guard backend was not found; run sh scripts/build.sh or set MODEL_GUARD_TEST_BINARY');
}

async function freePort() {
  const server = createNetServer();
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const { port } = server.address();
  await new Promise(resolve => server.close(resolve));
  return port;
}

// One local OpenAI-compatible upstream. It classifies requests by prompt
// content so probe traffic and business traffic are counted separately, and
// answers with a distinguishable model name plus the expected number (21) or
// a wrong number (0).
async function startUpstream(name) {
  const counts = { business: 0, probe: 0, other: 0 };
  let mode = 'ok';
  const server = createServer((req, res) => {
    let raw = '';
    req.on('data', chunk => {
      if (raw.length < 4 * 1024 * 1024) raw += chunk;
    });
    req.on('end', () => {
      let url;
      try {
        url = new URL(req.url, 'http://127.0.0.1');
      } catch {
        res.writeHead(400).end();
        return;
      }
      if (url.pathname === '/__control') {
        const next = url.searchParams.get('mode');
        if (next === 'ok' || next === 'wrong') mode = next;
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ name, mode, counts }));
        return;
      }
      if (url.pathname.endsWith('/models')) {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({
          object: 'list',
          data: [{ id: MODEL, object: 'model', owned_by: name.toLowerCase() }],
        }));
        return;
      }
      if (url.pathname.includes('/chat/completions')) {
        let body = {};
        try {
          body = JSON.parse(raw);
        } catch {
          res.writeHead(400, { 'content-type': 'application/json' });
          res.end(JSON.stringify({ error: { message: 'invalid json' } }));
          return;
        }
        const text = JSON.stringify(body.messages ?? body.prompt ?? '');
        if (text.includes(BUSINESS_MARK)) counts.business++;
        else if (text.includes('糖果') || text.includes('不使用任何外部工具')) counts.probe++;
        else counts.other++;
        const content = `${name} ${mode === 'wrong' ? '0' : '21'}`;
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({
          id: `mock-${name.toLowerCase()}`,
          object: 'chat.completion',
          model: MODEL,
          choices: [{ index: 0, message: { role: 'assistant', content }, finish_reason: 'stop' }],
          usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
        }));
        return;
      }
      res.writeHead(404, { 'content-type': 'application/json' });
      res.end(JSON.stringify({ error: { message: 'not found' } }));
    });
  });
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const { port } = server.address();
  return {
    name,
    port,
    counts: () => ({ ...counts }),
    setMode: next => { mode = next; },
    close: () => new Promise(resolve => {
      server.closeAllConnections?.();
      server.close(() => resolve());
    }),
  };
}

function stopChild(child) {
  return new Promise(resolve => {
    if (!child || child.exitCode !== null || child.signalCode !== null) {
      resolve();
      return;
    }
    const force = setTimeout(() => {
      try { child.kill('SIGKILL'); } catch {}
    }, 4000);
    const finish = setTimeout(() => {
      clearTimeout(force);
      resolve();
    }, 7000);
    child.once('exit', () => {
      clearTimeout(force);
      clearTimeout(finish);
      resolve();
    });
    try {
      child.kill('SIGTERM');
    } catch {
      clearTimeout(force);
      clearTimeout(finish);
      resolve();
    }
  });
}

async function cleanupProcesses(tempDir, rootPids = []) {
  if (!tempDir && (!rootPids || rootPids.length === 0)) return;

  // 1. Terminate all descendant processes of known parents (e.g. magpie serve, guard backend)
  for (const rootPid of rootPids) {
    if (!rootPid) continue;
    try {
      const { stdout } = await execFileAsync('ps', ['-ax', '-o', 'pid,ppid']);
      const parentMap = new Map();
      for (const line of stdout.split('\n')) {
        const parts = line.trim().split(/\s+/);
        if (parts.length >= 2) {
          const pid = parseInt(parts[0], 10);
          const ppid = parseInt(parts[1], 10);
          if (!isNaN(pid) && !isNaN(ppid)) {
            if (!parentMap.has(ppid)) parentMap.set(ppid, []);
            parentMap.get(ppid).push(pid);
          }
        }
      }
      const queue = [rootPid];
      const descendants = [];
      while (queue.length > 0) {
        const curr = queue.shift();
        const children = parentMap.get(curr) || [];
        for (const ch of children) {
          if (ch !== process.pid && ch !== process.ppid && !descendants.includes(ch)) {
            descendants.push(ch);
            queue.push(ch);
          }
        }
      }
      for (const pid of descendants) {
        try { process.kill(pid, 'SIGTERM'); } catch {}
      }
      if (descendants.length > 0) {
        await sleep(150);
        for (const pid of descendants) {
          try { process.kill(pid, 'SIGKILL'); } catch {}
        }
      }
    } catch {}
  }

  // 2. Terminate any lingering process whose command line references tempDir (including CLI-derived hosts)
  if (tempDir) {
    try {
      const { stdout } = await execFileAsync('ps', ['-ax', '-o', 'pid,command']);
      const toKill = [];
      for (const line of stdout.split('\n')) {
        const trimmed = line.trim();
        const match = trimmed.match(/^(\d+)\s+(.*)$/);
        if (match) {
          const pid = parseInt(match[1], 10);
          const cmd = match[2];
          if (pid !== process.pid && pid !== process.ppid && cmd.includes(tempDir)) {
            toKill.push(pid);
          }
        }
      }
      for (const pid of toKill) {
        try { process.kill(pid, 'SIGTERM'); } catch {}
      }
      if (toKill.length > 0) {
        await sleep(150);
        for (const pid of toKill) {
          try { process.kill(pid, 'SIGKILL'); } catch {}
        }
      }
    } catch {}
  }
}

function normalizeGroups(groups) {
  return (groups || [])
    .map(group => ({
      id: group.id,
      name: group.name,
      members: [...(group.members || [])],
      routing: group.routing,
      off: [...(group.off || [])].sort(),
    }))
    .sort((a, b) => String(a.id).localeCompare(String(b.id)));
}

function offOf(group) {
  return Array.isArray(group?.off) ? group.off : [];
}

class Guard {
  constructor(child, info) {
    this.child = child;
    this.origin = info.origin;
    this.control = info.control;
  }

  async api(pathname, { method = 'GET', body, timeout = 40000 } = {}) {
    const response = await fetch(this.origin + pathname, {
      method,
      headers: {
        authorization: `Bearer ${this.control}`,
        'content-type': 'application/json',
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(timeout),
    });
    const text = await response.text();
    let json;
    try {
      json = JSON.parse(text);
    } catch {}
    if (!response.ok) {
      throw new Error(`${method} ${pathname} -> HTTP ${response.status}: ${json?.error || text.slice(0, 800)}`);
    }
    return json;
  }

  async stop() {
    await stopChild(this.child);
  }
}

let guardBinary;
let magpieBinary;
let tempRoot;
let configHome;
let stateHome;
let providersPath;
let gatePath;
let pluginPkgDir;
let guardHostCreated = false;
let bad;
let good;
let magpieChild;
let magpieLog = '';
let guard;
let gatewayPort;
let gatewayUrl;
let baseEnv;

async function readProviders() {
  return JSON.parse(await readFile(providersPath, 'utf8'));
}

async function writeProviders(parsed) {
  const temporary = `${providersPath}.verify-${process.pid}.tmp`;
  await writeFile(temporary, `${JSON.stringify(parsed, null, 2)}\n`, { mode: 0o600 });
  await rename(temporary, providersPath);
}

// Markers stand for user-owned configuration the guard must never drop.
function assertUserFields(parsed, label) {
  assert.equal(parsed?.['x-verify-root']?.keep, 'me', `${label}: unknown providers.json root field was dropped`);
  const group = findGroup(parsed, 'guard-order');
  assert.ok(group, `${label}: explicit group disappeared`);
  assert.equal(group['x-verify-group']?.keep, 'me', `${label}: unknown group field was dropped`);
}

function findGroup(parsed, id) {
  return (parsed.groups || []).find(group => group.id === id);
}

async function waitGroups(label, predicate, timeout = 30000) {
  return waitFor(label, async () => {
    try {
      const parsed = await readProviders();
      return predicate(parsed) ? parsed : false;
    } catch {
      return false;
    }
  }, { timeout });
}

async function magpie(args, { timeout = 60000 } = {}) {
  try {
    const { stdout, stderr } = await execFileAsync(magpieBinary, args, {
      cwd: root,
      env: baseEnv,
      timeout,
      maxBuffer: 8 * 1024 * 1024,
      encoding: 'utf8',
    });
    return String(stdout) + String(stderr);
  } catch (error) {
    // Never echo argv: provider keys travel as CLI arguments.
    const output = `${error.stdout || ''}${error.stderr || ''}`;
    throw new Error(`magpie CLI failed (exit ${error.code ?? 'unknown'}): ${sanitize(output).slice(0, 1500)}`);
  }
}

async function startGuard() {
  const hostScriptPath = path.join(tempRoot, 'guard-host-runner.mjs');
  if (!guardHostCreated) {
    const scriptCode = `import { pathToFileURL } from 'node:url';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
const pluginIndexPath = process.argv[2];
const options = JSON.parse(process.argv[3]);
const { default: plugin } = await import(pathToFileURL(pluginIndexPath).href);
const hooks = await plugin.server({}, options);
await hooks.config({});
const entry = await hooks.auth.methods[0].authorize();
const openUrl = new URL(entry.url);
// The authorize URL carries a one-time ticket, not a session token. Redeem it
// without following the redirect so the Set-Cookie session can be kept.
const redeemed = await fetch(openUrl, { redirect: 'manual', signal: AbortSignal.timeout(5000) });
const cookies = typeof redeemed.headers.getSetCookie === 'function'
  ? redeemed.headers.getSetCookie()
  : [redeemed.headers.get('set-cookie') || ''];
let session = '';
for (const cookie of cookies) {
  const match = /(?:^|[;\s])mg_session=([^;]+)/.exec(cookie || '');
  if (match) session = match[1];
}
if (!session) throw new Error('console ticket did not return a session');
// Management access must be proven before the fixture is used.
const probe = await fetch(openUrl.origin + '/api/state', {
  headers: { authorization: 'Bearer ' + session },
  signal: AbortSignal.timeout(3000),
});
if (!probe.ok) throw new Error('console session cannot read management state');
// The control token comes from the protected handshake file, not stdout.
const hook = JSON.parse(await readFile(path.join(options.dataDir, 'response-hook.json'), 'utf8'));
console.log(JSON.stringify({ origin: openUrl.origin, control: session, hookControl: hook.control }));
process.stdin.resume();
process.stdin.on('end', () => process.exit(0));
`;
    await writeFile(hostScriptPath, scriptCode, 'utf8');
    guardHostCreated = true;
  }

  const hostEnv = { ...baseEnv };
  delete hostEnv.MAGPIE_MODEL_GUARD_DISCOVERY;

  const pluginIndexPath = path.join(pluginPkgDir, 'plugin', 'index.mjs');
  const hostOptions = {
    dataDir: path.join(tempRoot, 'guard'),
    magpieConfigDir: path.join(configHome, 'magpie'),
    gatewayURL: gatewayUrl,
    magpiePath: magpieBinary,
    binaryPath: guardBinary,
  };

  const child = spawn(process.execPath, [
    hostScriptPath,
    pluginIndexPath,
    JSON.stringify(hostOptions),
  ], {
    cwd: root,
    env: hostEnv,
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  child.stdin.on('error', () => {});
  let stderr = '';
  child.stderr.on('data', chunk => {
    stderr = (stderr + chunk).slice(-8192);
  });

  const lines = createInterface({ input: child.stdout });
  try {
    const line = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`model-guard startup timed out${stderr ? `: ${sanitize(stderr)}` : ''}`)), 40000);
      lines.once('line', value => {
        clearTimeout(timer);
        resolve(value);
      });
      child.once('exit', code => {
        clearTimeout(timer);
        reject(new Error(`model-guard exited before startup (${code})${stderr ? `: ${sanitize(stderr)}` : ''}`));
      });
    });
    lines.close();

    let info;
    try {
      info = JSON.parse(line);
    } catch {
      throw new Error(`invalid model-guard startup line: ${sanitize(String(line).slice(0, 500))}`);
    }
    addSecret(info.control);
    if (info.hookControl) addSecret(info.hookControl);
    assert.ok(info.origin && info.control, 'model-guard startup line is missing origin/control');
    return new Guard(child, info);
  } catch (error) {
    lines.close();
    await stopChild(child);
    throw error;
  }
}

function startMagpieServe() {
  magpieChild = spawn(magpieBinary, ['serve'], {
    cwd: root,
    env: { ...baseEnv, MAGPIE_ADDR: `127.0.0.1:${gatewayPort}`, MAGPIE_MODEL_GUARD_DISCOVERY: '1' },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const capture = chunk => {
    magpieLog = (magpieLog + chunk).slice(-32768);
  };
  magpieChild.stdout.on('data', capture);
  magpieChild.stderr.on('data', capture);
  return magpieChild;
}

async function waitGatewayReady() {
  await waitFor('magpie gateway serving both providers and the explicit group', async () => {
    if (magpieChild.exitCode !== null) return false;
    try {
      const response = await fetch(`${gatewayUrl}/v1/models`, {
        headers: { authorization: `Bearer ${MAGPIE_KEY}` },
        signal: AbortSignal.timeout(1500),
      });
      if (!response.ok) return false;
      const payload = await response.json();
      const ids = new Set((payload.data || []).map(model => model.id));
      return ids.has(BAD_MODEL) && ids.has(GOOD_MODEL) && ids.has(GROUP);
    } catch {
      return false;
    }
  }, { timeout: 30000, interval: 300 });
  if (magpieChild.exitCode !== null) {
    throw new Error(`magpie serve exited (${magpieChild.exitCode}): ${sanitize(magpieLog.slice(-2000))}`);
  }
}

async function chat(model, marker, timeout = 20000) {
  const started = Date.now();
  const response = await fetch(`${gatewayUrl}/v1/chat/completions`, {
    method: 'POST',
    headers: {
      authorization: `Bearer ${MAGPIE_KEY}`,
      'content-type': 'application/json',
    },
    body: JSON.stringify({
      model,
      stream: false,
      messages: [{ role: 'user', content: `${marker} ${BUSINESS_MARK} ${randomUUID()}` }],
    }),
    signal: AbortSignal.timeout(timeout),
  });
  const text = await response.text();
  let payload;
  try {
    payload = JSON.parse(text);
  } catch {}
  return {
    status: response.status,
    ok: response.ok,
    answer: payload?.choices?.[0]?.message?.content || '',
    text,
    elapsed: Date.now() - started,
  };
}

async function targets() {
  const list = await guard.api('/api/targets');
  assert.ok(Array.isArray(list), '/api/targets must return an array');
  return list;
}

async function target(model) {
  return (await targets()).find(entry => entry.model === model);
}

async function waitTarget(label, model, predicate, timeout = 40000) {
  return waitFor(label, async () => {
    const entry = await target(model);
    return entry && predicate(entry) ? entry : false;
  }, { timeout, interval: 300 });
}

async function action(model, name) {
  for (let attempt = 0; attempt < 40; attempt++) {
    try {
      return await guard.api('/api/targets/action', { method: 'POST', body: { model, action: name } });
    } catch (error) {
      if (attempt < 39 && /检测进行中/.test(error.message)) {
        await sleep(250);
        continue;
      }
      throw error;
    }
  }
  throw new Error('unreachable');
}

async function assertConsoleShape() {
  const state = await guard.api('/api/state');
  assert.ok(state && typeof state === 'object', '/api/state must return an object');
  assert.equal(state.config?.gateway_url, gatewayUrl);
  assert.equal(state.config?.gateway_key, '', 'gateway key must be stripped from the snapshot');
  if ('schedule_mode' in (state.config || {})) {
    assert.equal(state.config.schedule_mode, 'fixed', 'the isolated config must stay on the fixed schedule');
  }
  assert.ok(Array.isArray(state.targets), 'state.targets must be an array');
  assert.ok(Array.isArray(state.prompts), 'state.prompts must be an array');
  assert.equal(typeof state.health, 'string', 'state.health must be a string');
  assert.ok(state.routing && typeof state.routing === 'object', 'state.routing must be an object');
  assert.equal(state.routing.enabled, true, 'routing must be enabled with -magpie-config');
  assert.equal(typeof state.routing.managed_groups, 'number', 'routing.managed_groups must be a number');
  assert.ok(!state.routing.error, `routing.error must be empty: ${state.routing.error || ''}`);

  const candy = state.prompts.find(prompt => prompt.id === 'candy');
  assert.ok(candy, 'builtin candy prompt must exist');
  assert.equal(candy.automatic, true);
  assert.equal(candy.grader, 'number');
  assert.equal(candy.expect, '21');

  const badTarget = state.targets.find(entry => entry.model === BAD_MODEL);
  assert.ok(badTarget, 'bad target must exist');
  for (const key of ['model', 'enabled', 'prompt_ids', 'state', 'manual_hold', 'blocked', 'checked_at', 'fail_streak', 'interval_min']) {
    assert.ok(key in badTarget, `target JSON is missing ${key}`);
  }
  assert.equal(typeof badTarget.blocked, 'boolean');
}

async function main() {
  magpieBinary = await resolveMagpieBinary();
  guardBinary = await resolveGuardBinary();

  tempRoot = await mkdtemp(path.join(os.tmpdir(), 'model-guard-auto-routing-'));
  configHome = path.join(tempRoot, 'config');
  stateHome = path.join(tempRoot, 'state');
  providersPath = path.join(configHome, 'magpie', 'providers.json');
  await mkdir(configHome, { recursive: true });
  await mkdir(stateHome, { recursive: true });
  await mkdir(path.join(tempRoot, 'guard'), { recursive: true });

  baseEnv = {
    ...process.env,
    XDG_CONFIG_HOME: configHome,
    XDG_STATE_HOME: stateHome,
    MAGPIE_NO_ACCOUNTS: '1',
    MAGPIE_MODEL_GUARD_DISCOVERY: '1',
  };
  for (const key of ['MAGPIE_ADDR', 'MAGPIE_GATEWAY_KEY', 'MAGPIE_WEB_KEY', 'MAGPIE_WEB_URL', 'MAGPIE_PUBLIC_URL']) {
    delete baseEnv[key];
  }

  bad = await startUpstream('BAD');
  good = await startUpstream('GOOD');
  const badKey = `sk-guard-bad-${randomUUID()}`;
  const goodKey = `sk-guard-good-${randomUUID()}`;
  addSecret(badKey);
  addSecret(goodKey);
  gatewayPort = await freePort();
  gatewayUrl = `http://127.0.0.1:${gatewayPort}`;

  await magpie(['provider', 'add', 'Guard Bad',
    `url=http://127.0.0.1:${bad.port}/v1`, `key=${badKey}`, `models=${MODEL}`, 'id=guard-bad']);
  await magpie(['provider', 'add', 'Guard Good',
    `url=http://127.0.0.1:${good.port}/v1`, `key=${goodKey}`, `models=${MODEL}`, 'id=guard-good']);
  const groupOut = await magpie(['group', 'add', 'Guard Order',
    `models=${BAD_MODEL},${GOOD_MODEL}`, 'routing=order']);
  assert.match(groupOut, /group\/guard-order/, 'explicit order group must be created');

  const groupsOut = await magpie(['groups']);
  const refs = groupsOut.split(/\r?\n/)
    .map(line => line.trim())
    .filter(Boolean)
    .map(line => line.split(/\s{2,}/)[1])
    .filter(Boolean);
  assert.ok(refs.includes(GROUP), `magpie groups must list ${GROUP}: ${sanitize(groupsOut)}`);
  assert.ok(refs.includes(AUTO), `default auto group must exist: ${sanitize(groupsOut)}`);

  // User-owned fields, unknown to the guard, must survive every rewrite.
  const seeded = await readProviders();
  seeded['x-verify-root'] = { keep: 'me' };
  const seededGroup = seeded.groups.find(group => group.id === 'guard-order');
  assert.ok(seededGroup, 'explicit group must be present after CLI setup');
  seededGroup['x-verify-group'] = { keep: 'me' };
  await writeProviders(seeded);

  // Package a complete plugin installation under tempRoot:
  // package.json + index.mjs + real Go binary + gate middleware.
  pluginPkgDir = path.join(tempRoot, 'plugin-package');
  const pluginBinDir = path.join(pluginPkgDir, 'bin');
  const pluginSrcDir = path.join(pluginPkgDir, 'plugin');
  await mkdir(pluginBinDir, { recursive: true });
  await mkdir(pluginSrcDir, { recursive: true });

  await copyFile(path.join(root, 'package.json'), path.join(pluginPkgDir, 'package.json'));
  for (const name of await readdir(path.join(root, 'plugin'))) {
    await copyFile(path.join(root, 'plugin', name), path.join(pluginSrcDir, name));
  }

  const platform = { win32: 'windows', darwin: 'darwin', linux: 'linux' }[process.platform] || process.platform;
  const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch] || process.arch;
  const suffix = platform === 'windows' ? '.exe' : '';
  const bundledName = `model-guard-${platform}-${arch}${suffix}`;
  const fallbackName = `model-guard${suffix}`;

  const targetBundled = path.join(pluginBinDir, bundledName);
  const targetFallback = path.join(pluginBinDir, fallbackName);
  await copyFile(guardBinary, targetBundled);
  await copyFile(guardBinary, targetFallback);
  if (process.platform !== 'win32') {
    await chmod(targetBundled, 0o755);
    await chmod(targetFallback, 0o755);
  }

  gatePath = path.join(pluginSrcDir, 'gate.middleware.js');

  await magpie(['plugin', 'add', pluginPkgDir]);

  // Explicitly configure isolated plugin options so host execution never touches user configuration.
  const pluginOptions = {
    dataDir: path.join(tempRoot, 'guard'),
    magpieConfigDir: path.join(configHome, 'magpie'),
    gatewayURL: gatewayUrl,
    magpiePath: magpieBinary,
  };
  await magpie(['plugin', 'options', pluginPkgDir, JSON.stringify(pluginOptions)]);

  // Verify magpie groups runs cleanly with full plugin installed without recursive spawn
  const postInstallGroups = await magpie(['groups']);
  assert.ok(postInstallGroups.includes(GROUP), `magpie groups after full plugin install must list ${GROUP}`);
  assert.ok(postInstallGroups.includes(AUTO), `magpie groups after full plugin install must list ${AUTO}`);

  const initial = await readProviders();
  const originalGroups = normalizeGroups(initial.groups);
  assert.deepEqual(originalGroups.map(group => group.id), ['guard-order'], 'only the explicit group is stored before the guard runs');
  const originalProviders = initial.providers.map(provider => ({
    id: provider.id,
    name: provider.name,
    chat: provider.chat,
    models: [...(provider.models || [])],
  }));

  startMagpieServe();
  await waitGatewayReady();

  guard = await startGuard();
  await guard.api('/api/config', {
    method: 'POST',
    body: { gateway_url: gatewayUrl, gateway_key: MAGPIE_KEY, schedule_mode: 'fixed' },
  });
  const badTarget = await guard.api('/api/targets', {
    method: 'POST',
    body: {
      model: BAD_MODEL, enabled: false, prompt_ids: [PROMPT_ID], interval_min: 30, fail_recheck_min: 60,
      schedule_mode: 'fixed',
    },
  });
  assert.equal(badTarget.enabled, false, 'bad target starts with monitoring off');
  if ('schedule_mode' in badTarget) assert.equal(badTarget.schedule_mode, 'fixed', 'bad target must use the fixed schedule');
  const goodTarget = await guard.api('/api/targets', {
    method: 'POST',
    body: {
      model: GOOD_MODEL, enabled: false, prompt_ids: [PROMPT_ID], interval_min: 30, fail_recheck_min: 60,
      schedule_mode: 'fixed',
    },
  });
  assert.equal(goodTarget.enabled, false, 'good target starts with monitoring off');

  await assertConsoleShape();
  console.log('PASS console JSON exposes config/targets/prompts/health/routing');

  await action(BAD_MODEL, 'disable');
  const held = await target(BAD_MODEL);
  assert.equal(held.manual_hold, true, 'manual disable must set manual_hold');
  assert.equal(held.blocked, true, 'manual disable must block the model');
  assert.equal(held.enabled, false, 'monitoring stays off after manual disable');

  await waitGroups('explicit group excludes the blocked member', parsed => offOf(findGroup(parsed, 'guard-order')).includes(BAD_MODEL));
  await waitGroups('auto group is materialized with the blocked member excluded', parsed => {
    const auto = findGroup(parsed, AUTO_ID);
    return auto
      && Array.isArray(auto.members) && auto.members.length === 2
      && offOf(auto).includes(BAD_MODEL) && !offOf(auto).includes(GOOD_MODEL);
  });
  const managed = await guard.api('/api/state');
  assert.ok(managed.routing.managed_groups >= 1, 'managed_groups must count the synchronized groups');
  assertUserFields(await readProviders(), 'after the first guard rewrite');
  console.log(`PASS blocked member excluded from explicit and auto groups (managed ${managed.routing.managed_groups})`);

  // Direct request to the blocked model ID must be rejected with 403 by gate middleware
  const directBlocked = await chat(BAD_MODEL, 'direct blocked model request');
  assert.equal(directBlocked.status, 403, `direct request to blocked model must return 403, got ${directBlocked.status}`);
  assert.match(directBlocked.text, /MODEL_GUARD_BLOCKED/i, 'direct request must return MODEL_GUARD_BLOCKED');
  console.log('PASS direct request to blocked model ID returns 403');

  await sleep(ROUTE_SETTLE_MS);
  const badBusinessBefore = bad.counts().business;
  const goodBusinessBefore = good.counts().business;
  for (let round = 0; round < 3; round++) {
    const groupAnswer = await chat(GROUP, `group round ${round}`);
    assert.equal(groupAnswer.status, 200, `group request failed: ${sanitize(groupAnswer.text.slice(0, 500))}`);
    assert.match(groupAnswer.answer, /^GOOD\b/, `group request must reach the healthy member, got ${groupAnswer.answer}`);
    const autoAnswer = await chat(AUTO, `auto round ${round}`);
    assert.equal(autoAnswer.status, 200, `auto request failed: ${sanitize(autoAnswer.text.slice(0, 500))}`);
    assert.match(autoAnswer.answer, /^GOOD\b/, `auto request must reach the healthy member, got ${autoAnswer.answer}`);
  }
  assert.equal(bad.counts().business, badBusinessBefore, 'blocked member must receive zero business traffic');
  assert.ok(good.counts().business - goodBusinessBefore >= 6, 'healthy member must serve every business request');
  console.log('PASS explicit and auto groups serve only the healthy member');

  const badProbeBefore = bad.counts().probe;
  const goodProbeBefore = good.counts().probe;
  const businessDuringProbe = bad.counts().business;
  const probe = await guard.api('/api/probe', {
    method: 'POST',
    body: { model: BAD_MODEL, prompt_id: PROMPT_ID },
    timeout: 40000,
  });
  assert.match(String(probe.answer || ''), /^BAD\b/, `probe must be answered by the blocked model, got ${probe.answer}`);
  assert.equal(probe.verdict, 'passed', 'the candy probe answer is graded and passes');
  assert.equal(bad.counts().probe - badProbeBefore, 1, 'blocked model must receive exactly one probe request');
  assert.equal(good.counts().probe, goodProbeBefore, 'probe must not be substituted by the healthy member');
  assert.equal(bad.counts().business, businessDuringProbe, 'probe must not count as business traffic');
  console.log('PASS /api/probe bypasses the gate and reaches the blocked model');

  await action(BAD_MODEL, 'recheck');
  const rechecked = await waitTarget('manual recheck completes', BAD_MODEL, entry => entry.state === 'passed');
  assert.equal(rechecked.manual_hold, true, 'recheck under manual disable must keep manual_hold');
  assert.equal(rechecked.blocked, true, 'recheck under manual disable must stay blocked');
  await sleep(ROUTE_SETTLE_MS);
  const badBusinessHeld = bad.counts().business;
  const stillHeld = await chat(GROUP, 'manual hold still active');
  assert.match(stillHeld.answer, /^GOOD\b/, 'manual hold must keep routing to the healthy member');
  assert.equal(bad.counts().business, badBusinessHeld, 'manual hold must not leak business traffic');
  console.log('PASS manual_hold survives recheck and keeps the member out of business routing');

  await action(BAD_MODEL, 'enable');
  const enabled = await waitTarget('manual enable', BAD_MODEL, entry => !entry.manual_hold && !entry.blocked);
  assert.equal(enabled.manual_hold, false);
  await waitGroups('explicit group off cleared after manual enable', parsed => {
    const group = findGroup(parsed, 'guard-order');
    return group && offOf(group).length === 0;
  });
  await waitGroups('materialized auto group removed after manual enable', parsed => !findGroup(parsed, AUTO_ID));
  await sleep(ROUTE_SETTLE_MS);
  const badBusinessEnabled = bad.counts().business;
  const routedAfterEnable = await chat(GROUP, 'manual enable routes again');
  assert.equal(routedAfterEnable.status, 200, `enabled group request failed: ${sanitize(routedAfterEnable.text.slice(0, 500))}`);
  assert.match(routedAfterEnable.answer, /^BAD\b/, 'order routing must use the first healthy member again');
  assert.equal(bad.counts().business - badBusinessEnabled, 1, 're-enabled member must receive business traffic');
  console.log('PASS enable restores the original group fields and routing to the formerly blocked member');

  bad.setMode('wrong');
  const failureProbeBase = bad.counts().probe;
  await action(BAD_MODEL, 'monitor-on');
  const failedOnce = await waitTarget('first failed detection round', BAD_MODEL, entry => entry.state === 'failed');
  assert.ok(failedOnce.fail_streak >= 1, 'a failed round must raise the failure streak');
  assert.ok(bad.counts().probe - failureProbeBase >= 2, 'a failed round probes twice (fail + retry)');
  await sleep(400);
  await action(BAD_MODEL, 'recheck');
  const failedTwice = await waitTarget('second failed detection round', BAD_MODEL,
    entry => entry.state === 'failed' && entry.fail_streak > failedOnce.fail_streak);
  assert.ok(bad.counts().probe - failureProbeBase >= 4, 'two failed rounds must produce at least four probes');
  assert.equal(failedTwice.manual_hold, false, 'automatic failure must not become a manual hold');

  await waitGroups('failed member excluded from groups', parsed => {
    const group = findGroup(parsed, 'guard-order');
    const auto = findGroup(parsed, AUTO_ID);
    return group && auto && offOf(group).includes(BAD_MODEL) && offOf(auto).includes(BAD_MODEL);
  });
  await sleep(ROUTE_SETTLE_MS);
  const badBusinessFailed = bad.counts().business;
  for (const model of [GROUP, AUTO]) {
    const answer = await chat(model, 'failed member excluded');
    assert.equal(answer.status, 200, `request to ${model} failed: ${sanitize(answer.text.slice(0, 500))}`);
    assert.match(answer.answer, /^GOOD\b/, `${model} must route only to the healthy member`);
  }
  assert.equal(bad.counts().business, badBusinessFailed, 'failed model must receive no business traffic');
  console.log('PASS automatic failure blocks the member after two failed rounds');

  bad.setMode('ok');
  await sleep(200);
  await action(BAD_MODEL, 'recheck');
  const recovered = await waitTarget('recheck passes after the model answers 21', BAD_MODEL,
    entry => entry.state === 'passed' && entry.blocked === false);
  assert.equal(recovered.manual_hold, false);
  await waitGroups('explicit group off cleared after recovery', parsed => {
    const group = findGroup(parsed, 'guard-order');
    return group && offOf(group).length === 0;
  });
  await waitGroups('auto derived record removed after recovery', parsed => !findGroup(parsed, AUTO_ID));
  await sleep(ROUTE_SETTLE_MS);
  const badBusinessRecovered = bad.counts().business;
  const recoveredAnswer = await chat(GROUP, 'recovered routing');
  assert.match(recoveredAnswer.answer, /^BAD\b/, 'recovered member must be routable again');
  assert.equal(bad.counts().business - badBusinessRecovered, 1, 'recovered member must receive business traffic');
  const recoveredState = await guard.api('/api/state');
  assert.ok(!recoveredState.routing.error, `routing.error after recovery: ${recoveredState.routing.error || ''}`);
  assertUserFields(await readProviders(), 'after recovery');
  console.log('PASS passing recheck clears blocked state and restores routing');

  await action(BAD_MODEL, 'disable');
  await action(GOOD_MODEL, 'disable');
  await waitGroups('all members excluded from both groups', parsed => {
    const group = findGroup(parsed, 'guard-order');
    const auto = findGroup(parsed, AUTO_ID);
    return group && auto
      && [BAD_MODEL, GOOD_MODEL].every(model => offOf(group).includes(model) && offOf(auto).includes(model));
  });
  await sleep(ROUTE_SETTLE_MS);
  const badCountsBlocked = bad.counts();
  const goodCountsBlocked = good.counts();
  let blockedResponse;
  try {
    blockedResponse = await chat(AUTO, 'all members blocked', 12000);
  } catch (error) {
    if (error.name === 'TimeoutError' || error.name === 'AbortError') {
      throw new Error('all-blocked auto group request hung instead of returning an error');
    }
    throw error;
  }
  assert.ok(blockedResponse.status >= 400,
    `all-blocked auto group must return an error, got HTTP ${blockedResponse.status}: ${sanitize(blockedResponse.text.slice(0, 300))}`);
  assert.ok(blockedResponse.elapsed < 12000, 'all-blocked auto group must not loop or wait');
  assert.deepEqual(bad.counts(), badCountsBlocked, 'all-blocked auto group must not send anything upstream (bad)');
  assert.deepEqual(good.counts(), goodCountsBlocked, 'all-blocked auto group must not send anything upstream (good)');
  console.log('PASS all-blocked auto group fails fast without upstream traffic');

  await action(BAD_MODEL, 'enable');
  await waitGroups('one healthy member is enough for the auto group', parsed => {
    const auto = findGroup(parsed, AUTO_ID);
    const group = findGroup(parsed, 'guard-order');
    return auto && group
      && offOf(auto).includes(GOOD_MODEL) && !offOf(auto).includes(BAD_MODEL)
      && offOf(group).includes(GOOD_MODEL) && !offOf(group).includes(BAD_MODEL);
  });
  await sleep(ROUTE_SETTLE_MS);
  const badBusinessRestored = bad.counts().business;
  const restoredAnswer = await chat(AUTO, 'single recovered member');
  assert.equal(restoredAnswer.status, 200, `auto group after one recovery failed: ${sanitize(restoredAnswer.text.slice(0, 500))}`);
  assert.match(restoredAnswer.answer, /^BAD\b/, 'the only unblocked member must serve the auto group');
  assert.equal(bad.counts().business - badBusinessRestored, 1);
  console.log('PASS restoring one member makes the auto group usable again');

  const groupsBeforeRestart = normalizeGroups((await readProviders()).groups);
  const targetsBeforeRestart = await targets();
  assert.ok(groupsBeforeRestart.some(group => group.id === AUTO_ID), 'auto group record must exist before restart');

  await guard.stop();
  guard = await startGuard();
  const restartedState = await guard.api('/api/state');
  assert.equal(restartedState.routing.enabled, true, 'routing must stay enabled after restart');
  assert.ok(!restartedState.routing.error, `routing.error after restart: ${restartedState.routing.error || ''}`);
  assert.ok(restartedState.routing.managed_groups >= 1, 'restart must restore managed groups');
  assert.equal(restartedState.config.gateway_url, gatewayUrl, 'restart must keep the isolated gateway config');
  assert.equal(restartedState.targets.length, targetsBeforeRestart.length, 'restart must keep the isolated targets');
  const restartedGood = restartedState.targets.find(entry => entry.model === GOOD_MODEL);
  assert.equal(restartedGood.manual_hold, true, 'manual hold must survive restart');
  assert.equal(restartedGood.blocked, true, 'blocked state must survive restart');

  await waitGroups('original group fields and materialized auto record after restart', parsed => {
    return JSON.stringify(normalizeGroups(parsed.groups)) === JSON.stringify(groupsBeforeRestart);
  });
  assertUserFields(await readProviders(), 'after restart');
  await assertConsoleShape();
  await sleep(ROUTE_SETTLE_MS);
  const badBusinessRestart = bad.counts().business;
  const restartAnswer = await chat(GROUP, 'routing after restart');
  assert.equal(restartAnswer.status, 200, `group request after restart failed: ${sanitize(restartAnswer.text.slice(0, 500))}`);
  assert.match(restartAnswer.answer, /^BAD\b/, 'the remaining healthy member must serve after restart');
  assert.equal(bad.counts().business - badBusinessRestart, 1);
  console.log('PASS restart keeps isolation, original group fields and the derived auto record');

  await action(GOOD_MODEL, 'enable');
  await waitGroups('guard changes fully reverted', parsed => {
    if (findGroup(parsed, AUTO_ID)) return false;
    return JSON.stringify(normalizeGroups(parsed.groups)) === JSON.stringify(originalGroups);
  });
  assertUserFields(await readProviders(), 'after final cleanup');
  const finalProviders = await readProviders();
  const finalSelected = finalProviders.providers.map(provider => ({
    id: provider.id,
    name: provider.name,
    chat: provider.chat,
    models: [...(provider.models || [])],
  })).sort((a, b) => a.id.localeCompare(b.id));
  assert.deepEqual(finalSelected, [...originalProviders].sort((a, b) => a.id.localeCompare(b.id)),
    'providers must keep their original url/models after all guard edits');
  for (const provider of finalProviders.providers) {
    assert.equal(typeof provider.key, 'string');
    assert.ok(provider.key.length > 0, 'provider credentials must be preserved');
  }
  await assertConsoleShape();
  console.log('PASS all guard edits removed and the original group fields are restored');

  return { managed: managed.routing.managed_groups };
}

let failure;
let summary;
const VERIFY_TIMEOUT_MS = 240000;
try {
  summary = await Promise.race([
    main(),
    new Promise((_, reject) => {
      const timer = setTimeout(() => {
        reject(new Error(`auto routing acceptance timed out after ${VERIFY_TIMEOUT_MS}ms`));
      }, VERIFY_TIMEOUT_MS);
      timer.unref?.();
    }),
  ]);
} catch (error) {
  failure = error;
} finally {
  await guard?.stop();
  await stopChild(magpieChild);
  await cleanupProcesses(tempRoot, [magpieChild?.pid, guard?.child?.pid]);
  if (bad) await bad.close();
  if (good) await good.close();
  if (tempRoot) await rm(tempRoot, { recursive: true, force: true });
}

if (failure) {
  console.error('auto routing acceptance: FAIL');
  console.error(sanitize(failure.message || String(failure)));
  if (magpieLog) {
    const tail = sanitize(magpieLog.trim()).slice(-1200);
    if (tail) console.error(`magpie log tail: ${tail}`);
  }
  process.exitCode = 1;
} else {
  console.log(`auto routing acceptance: PASS (${summary?.managed ?? 0} managed groups at the blocked checkpoint)`);
}
