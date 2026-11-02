// Manual acceptance for the native Magpie provider composition.
//
// Usage:
//   MAGPIE_TEST_BINARY=/Applications/magpie.app/Contents/MacOS/magpie \
//   MODEL_GUARD_TEST_BINARY=bin/model-guard \
//   node scripts/verify-native-provider.mjs
//
// Everything runs against throwaway XDG/HOME folders under os.tmpdir(). The
// script never reads the user's Magpie configuration, never prints keys or
// control tokens, and always kills every child process it started.

import assert from 'node:assert/strict';
import { spawn, execFile } from 'node:child_process';
import { constants } from 'node:fs';
import { access, copyFile, cp, mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { createServer as createNetServer } from 'node:net';
import { createInterface } from 'node:readline';
import { randomUUID } from 'node:crypto';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';

const execFileAsync = promisify(execFile);
const root = fileURLToPath(new URL('../', import.meta.url));

const PROVIDER = 'fixture';
const BACKUP = 'backup';
const CHAT = 'fixture/fixture-chat';
const OTHER = 'fixture/fixture-other';
const TOOLS = 'fixture/fixture-tools';
const DRAW = 'fixture/fixture-gpt-image-1';
const FILM = 'fixture/fixture-sora-video-1';
const DECIDE = 'fixture/fixture-decide';
const BACKUP_MODEL = 'backup/backup-model';
const PROVIDER_KEY = 'provider-key';
const GATEWAY_KEY = 'restricted-gateway-key';
const LOCAL_GATEWAY_KEY = 'local-gateway-key';

const secrets = new Set();
const addSecret = value => { if (value) secrets.add(String(value)); };
function sanitize(value) {
  let out = String(value ?? '');
  for (const secret of secrets) out = out.split(secret).join('***');
  out = out.replace(/"(control|key|token|authorization)"\s*:\s*"[^"]*"/gi, '"$1":"***"');
  out = out.replace(/Bearer\s+[A-Za-z0-9._~+/=-]+/gi, 'Bearer ***');
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
  try { await access(candidate, constants.X_OK); return true; } catch { return false; }
}

async function resolveMagpieBinary() {
  const explicit = process.env.MAGPIE_TEST_BINARY;
  if (explicit) {
    assert.ok(await isExecutable(explicit), `MAGPIE_TEST_BINARY is not executable: ${explicit}`);
    return explicit;
  }
  const candidates = [
    '/Applications/magpie.app/Contents/MacOS/magpie',
    path.join(os.homedir(), '.local', 'bin', 'magpie'),
  ];
  for (const candidate of candidates) if (await isExecutable(candidate)) return candidate;
  throw new Error('magpie was not found; set MAGPIE_TEST_BINARY');
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
  for (const candidate of [path.join(root, 'bin', `model-guard-${platform}-${arch}${suffix}`), path.join(root, 'bin', `model-guard${suffix}`)]) {
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

// --- fixture vendor -------------------------------------------------------

// One server answers the four native protocols, the fallback provider and the
// media/aux routes. It records every request so the acceptance can assert what
// Magpie actually forwarded, including headers and extraBody expansion.
async function startFixture() {
  const state = {
    counts: {},
    requests: [],
    modelMode: 'alias',
    failPrimary: false,
    failAll: false,
    refuseAll: false,
    failPrimaryProvider: false,
    holdFirstMs: 0,
    holdBadMs: 0,
    holdMs: 0,
    badFirstUsed: false,
    badFirst: new Set(),
  };
  const seen = {};
  const hint = (kind, body) => {
    const key = `${kind}:${randomUUID()}`;
    seen[key] = body;
    return key;
  };
  const server = createServer((req, res) => {
    const chunks = [];
    req.on('data', chunk => { if (chunks.reduce((n, b) => n + b.length, 0) < 64 << 20) chunks.push(chunk); });
    req.on('end', () => {
      const raw = Buffer.concat(chunks);
      const url = new URL(req.url, 'http://127.0.0.1');
      state.requests.push({ method: req.method, path: url.pathname, query: url.search, headers: { ...req.headers }, body: raw.toString('latin1') });
      state.counts[url.pathname] = (state.counts[url.pathname] || 0) + 1;
      if (url.pathname === '/__state') {
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end(JSON.stringify({ counts: state.counts, requests: state.requests.slice(-200) }));
        return;
      }
      if (url.pathname === '/__control') {
        if (url.searchParams.has('modelMode')) state.modelMode = url.searchParams.get('modelMode');
        if (url.searchParams.has('failPrimary')) state.failPrimary = url.searchParams.get('failPrimary') === '1';
        if (url.searchParams.has('failAll')) state.failAll = url.searchParams.get('failAll') === '1';
        if (url.searchParams.has('refuseAll')) state.refuseAll = url.searchParams.get('refuseAll') === '1';
        if (url.searchParams.has('failPrimaryProvider')) state.failPrimaryProvider = url.searchParams.get('failPrimaryProvider') === '1';
        if (url.searchParams.has('holdFirstMs')) state.holdFirstMs = Number(url.searchParams.get('holdFirstMs')) || 0;
        if (url.searchParams.has('holdBadMs')) state.holdBadMs = Number(url.searchParams.get('holdBadMs')) || 0;
        if (url.searchParams.has('holdMs')) state.holdMs = Number(url.searchParams.get('holdMs')) || 0;
        if (url.searchParams.has('resetBadFirst')) state.badFirstUsed = false;
        if (url.searchParams.has('badFirst')) state.badFirst.add(url.searchParams.get('badFirst'));
        res.writeHead(200, { 'content-type': 'application/json' });
        res.end('{"ok":true}');
        return;
      }
      if (state.failAll || (state.failPrimaryProvider && /^\/(chat|responses|anthropic|gemini)\//.test(url.pathname))) {
        res.writeHead(503, { 'content-type': 'application/json' });
        res.end('{"error":{"message":"fixture down"}}');
        return;
      }
      const auth = req.headers.authorization || '';
      if (state.failPrimary && auth.includes('primary') && !auth.includes('backup')) {
        res.writeHead(429, { 'content-type': 'application/json', 'retry-after': '0' });
        res.end('{"error":{"message":"primary key limited"}}');
        return;
      }
      res.setHeader('X-Oneapi-Request-Id', 'fixture-request-id');
      let body = {};
      if (raw.length && (req.headers['content-type'] || '').includes('json')) {
        try { body = JSON.parse(raw.toString('utf8')); } catch { body = {}; }
      }
      const want = typeof body.model === 'string' ? body.model : '';
      let model;
      if (state.modelMode === 'bad-then-good') {
        model = state.badFirstUsed ? 'fixture-good' : 'fixture-bad';
        state.badFirstUsed = true;
      } else {
        model = state.modelMode === 'bad' ? 'fixture-bad' : (want || 'fixture-good');
      }
      const json = (value, status = 200, headers = {}) => {
        res.writeHead(status, { 'content-type': 'application/json', ...headers });
        res.end(JSON.stringify(value));
      };
      const tool = (body.tools || []).length > 0 && /function_call_output|tool_result/.test(JSON.stringify(body.input ?? body.messages ?? ''));
      if (state.refuseAll) { json({ error: { message: 'fixture refuses this request', type: 'fixture_error' } }, 400); return; }
      if (url.pathname === '/chat/v1/models') {
        json({ object: 'list', data: [
          { id: 'fixture-chat', object: 'model' }, { id: 'fixture-other', object: 'model' },
          { id: 'fixture-tools', object: 'model' }, { id: 'fixture-gpt-image-1', object: 'model', kind: 'image' },
          { id: 'fixture-sora-video-1', object: 'model', kind: 'video' }, { id: 'fixture-decide', object: 'model', kind: 'decision' },
        ] });
        return;
      }
      if (url.pathname === '/backup/v1/models') {
        json({ object: 'list', data: [{ id: 'backup-model', object: 'model' }] });
        return;
      }
      if (url.pathname.endsWith('/chat/completions')) {
        if (body.stream) {
          res.writeHead(200, { 'content-type': 'text/event-stream' });
          res.write(`data: ${JSON.stringify({ id: 'chatcmpl_fixture', object: 'chat.completion.chunk', model, choices: [{ index: 0, delta: { content: 'ok' }, finish_reason: null }] })}\n\n`);
          const done = () => { if (res.writableEnded) return; res.write('data: [DONE]\n\n'); res.end(); };
          if (state.holdBadMs > 0 && model === 'fixture-bad') setTimeout(done, state.holdBadMs);
          else done();
          return;
        }
        json({ id: 'chatcmpl_fixture', object: 'chat.completion', model, choices: [{ index: 0, message: { role: 'assistant', content: tool ? 'tool-ok' : 'ok' }, finish_reason: 'stop' }], usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 } });
        return;
      }
      if (url.pathname.endsWith('/responses')) {
        const id = `resp_${(state.counts[url.pathname] || 1)}`;
        if (body.stream || /text\/event-stream/.test(req.headers.accept || '')) {
          res.writeHead(200, { 'content-type': 'text/event-stream' });
          res.write(`event: response.created\ndata: ${JSON.stringify({ type: 'response.created', response: { id, object: 'response', model, status: 'in_progress', output: [] } })}\n\n`);
          const finish = () => {
            if (res.writableEnded) return;
            res.write(`event: response.output_text.delta\ndata: ${JSON.stringify({ type: 'response.output_text.delta', item_id: 'msg_1', output_index: 0, content_index: 0, delta: 'ok' })}\n\n`);
            res.write(`event: response.completed\ndata: ${JSON.stringify({ type: 'response.completed', response: { id, object: 'response', model, status: 'completed', output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'ok' }] }] } })}\n\n`);
            res.end();
          };
          if (state.holdMs > 0) {
            setTimeout(finish, state.holdMs);
          } else if (state.holdBadMs > 0 && model === 'fixture-bad') {
            setTimeout(finish, state.holdBadMs);
          } else if ((state.counts[url.pathname] || 1) === 1 && state.holdFirstMs > 0) {
            setTimeout(finish, state.holdFirstMs);
          } else {
            finish();
          }
          return;
        }
        json({ id, object: 'response', model, status: 'completed', output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'ok' }] }], usage: { input_tokens: 3, output_tokens: 2, total_tokens: 5 } });
        return;
      }
      if (url.pathname.endsWith('/messages') || url.pathname.endsWith('/messages/count_tokens')) {
        if (url.pathname.endsWith('/count_tokens')) { json({ input_tokens: 3 }); return; }
        if (body.stream || /text\/event-stream/.test(req.headers.accept || '')) {
          res.writeHead(200, { 'content-type': 'text/event-stream' });
          res.write(`event: message_start\ndata: ${JSON.stringify({ type: 'message_start', message: { id: 'msg_fixture', type: 'message', role: 'assistant', model, content: [], usage: { input_tokens: 3, output_tokens: 0 } } })}\n\n`);
          res.write(`event: content_block_delta\ndata: ${JSON.stringify({ type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'ok' } })}\n\n`);
          res.write(`event: message_stop\ndata: ${JSON.stringify({ type: 'message_stop' })}\n\n`);
          res.end();
          return;
        }
        json({ id: 'msg_fixture', type: 'message', role: 'assistant', model, content: [{ type: 'text', text: 'ok' }], usage: { input_tokens: 3, output_tokens: 2 } });
        return;
      }
      if (url.pathname.includes('/v1beta/models')) {
        const modelFromPath = decodeURIComponent(url.pathname.split('/models/')[1] || '').split(':')[0] || want;
        if (url.pathname.endsWith('/models')) { json({ models: [{ name: `models/${modelFromPath}` }] }); return; }
        const payload = { candidates: [{ content: { role: 'model', parts: [{ text: 'ok' }] }, finishReason: 'STOP' }], modelVersion: model, usageMetadata: { promptTokenCount: 3, candidatesTokenCount: 2 } };
        if (url.pathname.endsWith(':streamGenerateContent')) {
          res.writeHead(200, { 'content-type': 'text/event-stream' });
          res.write(`data: ${JSON.stringify(payload)}\n\n`);
          res.end();
          return;
        }
        json(payload);
        return;
      }
      if (url.pathname.endsWith('/images/generations')) { json({ created: 1, data: [{ b64_json: 'aW1n' }] }); return; }
      if (url.pathname.endsWith('/images/edits')) { json({ created: 1, data: [{ b64_json: 'ZWRpdA==' }] }); return; }
      if (url.pathname.endsWith('/embeddings')) { json({ object: 'list', model, data: [{ object: 'embedding', index: 0, embedding: [0.1, 0.2] }] }); return; }
      if (url.pathname.endsWith('/rerank')) { json({ model, results: [{ index: 0, relevance_score: 0.9 }] }); return; }
      if (url.pathname.endsWith('/audio/transcriptions') || url.pathname.endsWith('/audio/translations')) { json({ text: 'fixture audio' }); return; }
      if (url.pathname.endsWith('/audio/speech')) {
        res.writeHead(200, { 'content-type': 'audio/mpeg' });
        res.end('AUDIO');
        return;
      }
      if (url.pathname.endsWith('/videos') && req.method === 'POST') { json({ id: 'vid_fixture', object: 'video', status: 'queued', model }); return; }
      if (/\/videos\/[^/]+\/content$/.test(url.pathname)) {
        res.writeHead(200, { 'content-type': 'video/mp4' });
        res.end('MP4DATA');
        return;
      }
      if (/\/videos\/[^/]+$/.test(url.pathname)) { json({ id: 'vid_fixture', object: 'video', status: 'completed', model }); return; }
      if (url.pathname.endsWith('/videos/vid_fixture/content')) {
        res.writeHead(200, { 'content-type': 'video/mp4' });
        res.end('MP4DATA');
        return;
      }
      if (url.pathname.endsWith('/systemone')) { json({ model: 'fixture-decide', decision: { choice: 'fixture/fixture-chat' } }); return; }
      if (url.pathname.endsWith('/alpha/search')) { json({ results: [{ title: 'fixture', url: 'https://example.invalid' }] }); return; }
      if (url.pathname.endsWith('/api/tags')) { json({ models: [{ name: 'fixture-chat', model: 'fixture-chat' }] }); return; }
      if (url.pathname.endsWith('/api/chat')) {
        res.writeHead(200, { 'content-type': 'application/x-ndjson' });
        res.write(`${JSON.stringify({ model: want, message: { role: 'assistant', content: 'ok' }, done: false })}\n`);
        res.write(`${JSON.stringify({ model: want, done: true })}\n`);
        res.end();
        return;
      }
      if (url.pathname.endsWith('/api/generate')) {
        res.writeHead(200, { 'content-type': 'application/x-ndjson' });
        res.write(`${JSON.stringify({ model: want, response: 'ok', done: true })}\n`);
        res.end();
        return;
      }
      json({ error: { message: `fixture has no route for ${url.pathname}` } }, 404);
    });
  });
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const { port } = server.address();
  return {
    base: `http://127.0.0.1:${port}`,
    port,
    state,
    seen,
    hint,
    counts: () => ({ ...state.counts }),
    requests: () => state.requests.slice(),
    close: () => new Promise(resolve => { server.closeAllConnections?.(); server.close(() => resolve()); }),
  };
}

// --- lifecycle ------------------------------------------------------------

function stopChild(child) {
  return new Promise(resolve => {
    if (!child || child.exitCode !== null || child.signalCode !== null) { resolve(); return; }
    const force = setTimeout(() => { try { child.kill('SIGKILL'); } catch {} }, 4000);
    const finish = setTimeout(() => { clearTimeout(force); resolve(); }, 7000);
    child.once('exit', () => { clearTimeout(force); clearTimeout(finish); resolve(); });
    try { child.kill('SIGTERM'); } catch { clearTimeout(force); clearTimeout(finish); resolve(); }
  });
}

async function startHost(tempRoot, pluginDir, options) {
  const script = `import { pathToFileURL } from 'node:url';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
const pluginIndexPath = process.argv[2];
const options = JSON.parse(process.argv[3]);
const { default: plugin } = await import(pathToFileURL(pluginIndexPath).href);
const hooks = await plugin.server({}, options);
await hooks.config({});
const entry = await hooks.auth.methods[0].authorize();
const openUrl = new URL(entry.url);
const redeemed = await fetch(openUrl, { redirect: 'manual', signal: AbortSignal.timeout(5000) });
const cookies = typeof redeemed.headers.getSetCookie === 'function' ? redeemed.headers.getSetCookie() : [redeemed.headers.get('set-cookie') || ''];
let session = '';
for (const cookie of cookies) {
  const match = /(?:^|[;\\s])mg_session=([^;]+)/.exec(cookie || '');
  if (match) session = match[1];
}
if (!session) throw new Error('console ticket did not return a session');
const probe = await fetch(openUrl.origin + '/api/state', { headers: { authorization: 'Bearer ' + session }, signal: AbortSignal.timeout(3000) });
if (!probe.ok) throw new Error('console session cannot read management state');
const hook = JSON.parse(await readFile(path.join(options.dataDir, 'response-hook.json'), 'utf8'));
console.log(JSON.stringify({ origin: openUrl.origin, control: session, hookControl: hook.control }));
process.stdin.resume();
process.stdin.on('end', () => process.exit(0));
`;
  const runner = path.join(tempRoot, 'guard-host.mjs');
  await writeFile(runner, script, 'utf8');
  const child = spawn(process.execPath, [runner, path.join(pluginDir, 'plugin', 'index.mjs'), JSON.stringify(options)], {
    cwd: root,
    env: { ...process.env, MAGPIE_MODEL_GUARD_DISCOVERY: '' },
    stdio: ['pipe', 'pipe', 'pipe'],
  });
  child.stdin.on('error', () => {});
  let stderr = '';
  child.stderr.on('data', chunk => { stderr = (stderr + chunk).slice(-8192); });
  const lines = createInterface({ input: child.stdout });
  try {
    const line = await new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error(`guard startup timed out: ${sanitize(stderr)}`)), 40000);
      lines.once('line', value => { clearTimeout(timer); resolve(value); });
      child.once('exit', code => { clearTimeout(timer); reject(new Error(`guard exited (${code}): ${sanitize(stderr)}`)); });
    });
    lines.close();
    const info = JSON.parse(line);
    addSecret(info.control);
    addSecret(info.hookControl);
    return { child, info };
  } catch (error) {
    lines.close();
    await stopChild(child);
    throw error;
  }
}

function guardAPI(info) {
  return async function api(route, { method = 'GET', body, gatewayKey } = {}) {
    const headers = { authorization: `Bearer ${info.control}` };
    if (body !== undefined) headers['content-type'] = 'application/json';
    if (gatewayKey) headers['x-gateway-key'] = gatewayKey;
    const response = await fetch(info.origin + route, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(15000),
    });
    const text = await response.text();
    if (!response.ok) throw new Error(`${route} -> ${response.status}: ${sanitize(text).slice(0, 500)}`);
    try { return JSON.parse(text); } catch { return text; }
  };
}

function newContext(tempRoot, fixture, magpieBinary, guardBinary, gatewayPort) {
  const configHome = path.join(tempRoot, 'config');
  const stateHome = path.join(tempRoot, 'state');
  const home = path.join(tempRoot, 'home');
  const guardDir = path.join(tempRoot, 'guard');
  const pkgDir = path.join(tempRoot, 'pkg');
  return {
    tempRoot, fixture, magpieBinary, guardBinary, configHome, stateHome, home, guardDir, pkgDir,
    gatewayPort,
    gatewayUrl: `http://127.0.0.1:${gatewayPort}`,
    magpieChild: null,
    guard: null,
    info: null,
  };
}

async function preparePackage(ctx) {
  await cp(path.join(root, 'plugin'), path.join(ctx.pkgDir, 'plugin'), { recursive: true });
  await mkdir(path.join(ctx.pkgDir, 'bin'), { recursive: true });
  const name = path.basename(ctx.guardBinary);
  await copyFile(ctx.guardBinary, path.join(ctx.pkgDir, 'bin', name));
  await writeFile(path.join(ctx.pkgDir, 'package.json'), JSON.stringify({ name: 'model-guard-fixture', type: 'module', main: './plugin/index.mjs' }));
  return path.join(ctx.pkgDir, 'plugin', 'index.mjs');
}

async function prepareMagpie(ctx) {
  const dir = path.join(ctx.configHome, 'magpie');
  await mkdir(dir, { recursive: true });
  await mkdir(ctx.stateHome, { recursive: true });
  await mkdir(ctx.home, { recursive: true });
  const base = ctx.fixture.base;
  const providers = {
    providers: [
      {
        id: PROVIDER, name: 'Fixture', key: 'primary',
        keys: [{ name: 'secondary', key: 'secondary' }],
        chat: `${base}/chat/v1`, responses: `${base}/responses/v1`,
        anthropic: `${base}/anthropic`, gemini: `${base}/gemini/v1beta`,
        routing: 'order', affinity: 'off',
        headers: { 'X-Fixture-Session': '${sessionId}' },
        extraBody: { fixture_extra: 'base', '!fixture_replace': 'override' },
        replyHeaders: ['X-Oneapi-Request-Id'],
        models: ['fixture-chat', 'fixture-other', 'fixture-tools', 'fixture-gpt-image-1', 'fixture-sora-video-1', 'fixture-decide'],
        fallback: [BACKUP_MODEL],
      },
      {
        id: BACKUP, name: 'Backup', key: 'backup-key',
        chat: `${base}/backup/v1`,
        models: ['backup-model'],
      },
    ],
    order: [PROVIDER, BACKUP],
  };
  await writeFile(path.join(dir, 'providers.json'), JSON.stringify(providers, null, 2), { mode: 0o600 });
}

function magpieEnv(ctx) {
  return {
    ...process.env,
    HOME: ctx.home,
    XDG_CONFIG_HOME: ctx.configHome,
    XDG_STATE_HOME: ctx.stateHome,
    MAGPIE_MODEL_GUARD_DISCOVERY: '1',
  };
}

async function magpieCLI(ctx, args, { timeout = 60000 } = {}) {
  try {
    const { stdout, stderr } = await execFileAsync(ctx.magpieBinary, args, {
      cwd: root, env: magpieEnv(ctx), timeout, maxBuffer: 8 << 20, encoding: 'utf8',
    });
    return String(stdout) + String(stderr);
  } catch (error) {
    throw new Error(`magpie ${args[0]} failed (${error.code ?? 'unknown'}): ${sanitize(`${error.stdout || ''}${error.stderr || ''}`).slice(0, 1500)}`);
  }
}

async function startMagpie(ctx) {
  ctx.magpieChild = spawn(ctx.magpieBinary, ['serve'], {
    cwd: root,
    env: { ...magpieEnv(ctx), MAGPIE_ADDR: `127.0.0.1:${ctx.gatewayPort}` },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  ctx.magpieLog = '';
  const capture = chunk => { ctx.magpieLog = (ctx.magpieLog + chunk).slice(-32768); };
  ctx.magpieChild.stdout.on('data', capture);
  ctx.magpieChild.stderr.on('data', capture);
  await waitFor('gateway ready', async () => {
    if (ctx.magpieChild.exitCode !== null) throw new Error(`magpie serve exited: ${sanitize(ctx.magpieLog.slice(-1000))}`);
    try {
      const response = await fetch(`${ctx.gatewayUrl}/v1/models`, { headers: { authorization: 'Bearer magpie' }, signal: AbortSignal.timeout(1000) });
      return response.ok;
    } catch { return false; }
  }, { timeout: 30000, interval: 300 });
}

async function gatewayKeyID(ctx, name) {
  const lines = (await magpieCLI(ctx, ['gateway-key', 'list'])).split('\n');
  for (const line of lines) {
    const columns = line.trim().split(/\s+/);
    if (columns.length >= 2 && columns[1] === name) return columns[0];
  }
  throw new Error(`gateway key ${name} was not found`);
}

async function configureProviders(ctx) {
  await magpieCLI(ctx, ['provider', 'refresh', PROVIDER], { timeout: 30000 });
  await magpieCLI(ctx, ['provider', 'refresh', BACKUP], { timeout: 30000 });
  await magpieCLI(ctx, ['gateway-key', 'add', 'local', '--key', LOCAL_GATEWAY_KEY]);
  await magpieCLI(ctx, ['gateway-key', 'add', 'verify', '--key', GATEWAY_KEY]);
  const restricted = await gatewayKeyID(ctx, 'verify');
  await magpieCLI(ctx, ['gateway-key', 'models', restricted,
    'fixture/fixture-chat', 'fixture/fixture-other', 'fixture/fixture-tools',
    'fixture/fixture-gpt-image-1', 'fixture/fixture-sora-video-1', 'fixture/fixture-decide']);
}

async function guardConfig(ctx, api, extra = {}) {
  await api('/api/config', {
    method: 'POST',
    body: {
      gateway_url: ctx.gatewayUrl,
      gateway_key: LOCAL_GATEWAY_KEY,
      provider_enabled: true,
      provider_key: PROVIDER_KEY,
      provider_attempts: 2,
      ...extra,
    },
  });
}

// --- HTTP acceptance ------------------------------------------------------

function providerFetch(ctx, route, { method = 'GET', body, headers = {}, key = PROVIDER_KEY, timeout = 30000 } = {}) {
  const base = ctx.info.origin + '/provider';
  const finalHeaders = { ...headers };
  if (key) finalHeaders.authorization = `Bearer ${key}`;
  if (body !== undefined && !(body instanceof FormData) && !finalHeaders['content-type']) finalHeaders['content-type'] = 'application/json';
  return fetch(base + route, {
    method,
    headers: finalHeaders,
    body: body === undefined ? undefined : (typeof body === 'string' || body instanceof FormData ? body : JSON.stringify(body)),
    signal: AbortSignal.timeout(timeout),
  });
}

async function magpieFetch(ctx, route, { method = 'GET', body, headers = {}, key = 'magpie' } = {}) {
  const finalHeaders = { ...headers };
  if (key) finalHeaders.authorization = `Bearer ${key}`;
  if (body !== undefined) finalHeaders['content-type'] = 'application/json';
  return fetch(ctx.gatewayUrl + route, {
    method, headers: finalHeaders,
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(30000),
  });
}

async function jsonOf(response) {
  const text = await response.text();
  try { return JSON.parse(text); } catch { return { raw: text }; }
}

async function setTarget(ctx, api, model, rule) {
  await api('/api/targets/delete', { method: 'POST', body: { model } }).catch(() => {});
  return api('/api/targets', {
    method: 'POST',
    body: {
      model, enabled: true, interval_min: 30, fail_recheck_min: 60, schedule_mode: 'fixed',
      next_check_at: '2099-01-01T00:00:00Z',
      response_rule_mode: rule ? 'custom' : 'inherit',
      response_model_rules: rule ? [rule] : [],
    },
  });
}

function modelOf(body) {
  return body?.model ?? body?.response?.model ?? body?.message?.model ?? body?.modelVersion ?? '';
}

async function fixtureControl(ctx, query) {
  await fetch(`${ctx.fixture.base}/__control?${query}`, { signal: AbortSignal.timeout(5000) });
}

async function requestCount(ctx, pathname) {
  const state = await (await fetch(`${ctx.fixture.base}/__state`, { signal: AbortSignal.timeout(5000) })).json();
  return state.counts[pathname] || 0;
}

async function lastFixtureRequest(ctx, suffix) {
  const state = await (await fetch(`${ctx.fixture.base}/__state`, { signal: AbortSignal.timeout(5000) })).json();
  return [...state.requests].reverse().find(entry => entry.path.endsWith(suffix));
}

async function httpAcceptance(ctx, api) {
  await fixtureControl(ctx, 'modelMode=alias');
  const dir = await jsonOf(await providerFetch(ctx, '/v1/models'));
  assert.ok(Array.isArray(dir.data) && dir.data.length > 0, 'guarded model directory is empty');
  const directDir = await jsonOf(await magpieFetch(ctx, '/v1/models'));
  const ids = new Set(dir.data.map(entry => entry.id));
  for (const model of [CHAT, OTHER, TOOLS]) assert.ok(ids.has(model), `directory lost ${model}`);
  for (const entry of directDir.data || []) {
    if (ids.has(entry.id)) {
      const guarded = dir.data.find(item => item.id === entry.id);
      for (const key of ['context_window', 'modalities', 'native_endpoints', 'pricing', 'service_tiers', 'future_field']) {
        assert.deepEqual(guarded[key], entry[key], `directory field ${key} changed for ${entry.id}`);
      }
    }
  }

  const chat = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [{ role: 'user', content: 'hello' }] } });
  const chatBody = await jsonOf(chat);
  assert.equal(chat.status, 200);
  // Without an active rule the path is transparent: Magpie keeps its default
  // naming (the requested model id). Vendor naming is asserted below with a rule.
  assert.equal(chatBody.model, CHAT, 'transparent chat must keep the requested model');
  assert.equal(chat.headers.get('x-oneapi-request-id'), 'fixture-request-id', 'reply header lost');
  const seen = await lastFixtureRequest(ctx, '/chat/completions');
  assert.ok(seen.headers['x-fixture-session'] && seen.headers['x-fixture-session'] !== '${sessionId}', 'session header was not expanded');
  assert.match(seen.body, /"fixture_extra":"base"/);
  assert.match(seen.body, /"fixture_replace":"override"/);
  assert.equal(JSON.parse(seen.body).model, 'fixture-chat', 'provider model id was not plain');
  assert.equal(JSON.parse(seen.body).fixture_replace, 'override');

  const chatStream = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, stream: true, messages: [{ role: 'user', content: 'hello' }] } });
  const chatStreamText = await chatStream.text();
  assert.equal(chatStream.status, 200);
  assert.match(chatStreamText, /data: .*"fixture\/fixture-chat"/);
  assert.match(chatStreamText, /\[DONE\]/);

  await fixtureControl(ctx, 'modelMode=alias&failPrimary=1');
  // Native key failover: the ordered provider keys try the secondary after a 429.
  const failoverMarker = `failover-${Date.now()}-${Math.random()}`;
  const failover = await providerFetch(ctx, '/v1/chat/completions', {
    method: 'POST', timeout: 8000,
    body: { model: CHAT, messages: [{ role: 'user', content: failoverMarker }] },
  });
  assert.equal(failover.status, 200, 'key failover failed');
  const failoverState = await (await fetch(`${ctx.fixture.base}/__state`)).json();
  const failoverKeys = failoverState.requests
    .filter(r => r.path === '/chat/v1/chat/completions' && r.body.includes(failoverMarker))
    .map(r => r.headers.authorization);
  assert.ok(failoverKeys.includes('Bearer primary') && failoverKeys.includes('Bearer secondary'), `key failover used ${JSON.stringify(failoverKeys)}`);

  await fixtureControl(ctx, 'failPrimary=0');
  // Native errors are not disguised as success.
  await fixtureControl(ctx, 'refuseAll=1');
  const unknown = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [] } });
  await fixtureControl(ctx, 'refuseAll=0');
  assert.ok([400, 401, 403, 404].includes(unknown.status), `native error status ${unknown.status}`);
  assert.ok(!(await jsonOf(unknown)).model, 'unknown model must not answer a successful model');


  const directVendor = await jsonOf(await magpieFetch(ctx, '/v1/chat/completions', {
    method: 'POST', key: LOCAL_GATEWAY_KEY,
    headers: { 'x-magpie-response-model': 'vendor' },
    body: { model: CHAT, messages: [{ role: 'user', content: 'hello' }] },
  }));
  assert.equal(directVendor.model, 'fixture-chat', 'native vendor naming is unavailable');

  const responses = await providerFetch(ctx, '/v1/responses', { method: 'POST', body: { model: CHAT, input: 'hello' } });
  const responsesText = await responses.text();
  assert.equal(responses.status, 200);
  assert.match(responsesText, /fixture\/fixture-chat/, `responses body: ${responsesText.slice(0, 300)}`);
  const responsesStream = await providerFetch(ctx, '/v1/responses', { method: 'POST', body: { model: CHAT, input: 'hello', stream: true } });
  assert.match(await responsesStream.text(), /response\.completed/);

  const anthropic = await providerFetch(ctx, '/v1/messages', { method: 'POST', body: { model: CHAT, max_tokens: 16, messages: [{ role: 'user', content: 'hello' }] } });
  const anthropicText = await anthropic.text();
  assert.equal(anthropic.status, 200);
  assert.match(anthropicText, /fixture\/fixture-chat/, `anthropic body: ${anthropicText.slice(0, 300)}`);
  const anthropicStream = await providerFetch(ctx, '/v1/messages', { method: 'POST', body: { model: CHAT, max_tokens: 16, stream: true, messages: [{ role: 'user', content: 'hello' }] } });
  assert.match(await anthropicStream.text(), /message_stop/);

  const gemini = await providerFetch(ctx, '/v1beta/models/fixture%2Ffixture-chat:generateContent', { method: 'POST', body: { contents: [{ role: 'user', parts: [{ text: 'hello' }] }] } });
  const geminiText = await gemini.text();
  assert.equal(gemini.status, 200);
  assert.match(geminiText, /fixture\/fixture-chat/, `gemini body: ${geminiText.slice(0, 300)}`);
  const geminiStream = await providerFetch(ctx, '/v1beta/models/fixture%2Ffixture-chat:streamGenerateContent?alt=sse', { method: 'POST', body: { contents: [] } });
  assert.match(await geminiStream.text(), /fixture\/fixture-chat/);

  // Retry: the vendor returns fixture-bad first, fixture-good on the second call.
  await setTarget(ctx, api, CHAT, { id: 'r-bad', match: 'contains', pattern: 'fixture-bad', action: 'disable' });
  await fixtureControl(ctx, 'modelMode=bad-then-good&resetBadFirst=1');
  const before = await requestCount(ctx, '/chat/v1/chat/completions');
  const retried = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [{ role: 'user', content: 'hello' }] } });
  const retriedBody = await jsonOf(retried);
  assert.equal(retried.status, 200, `retry failed: ${JSON.stringify(retriedBody)}`);
  assert.equal(retriedBody.model, 'fixture-good');
  assert.equal(await requestCount(ctx, '/chat/v1/chat/completions'), before + 2, 'guard must retry once');
  assert.notEqual((await api('/api/targets')).find(t => t.model === CHAT)?.state, 'failed', 'good retry disabled the target');

  await fixtureControl(ctx, 'modelMode=bad');
  const exhausted = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [{ role: 'user', content: 'hello' }] } });
  assert.equal(exhausted.status, 503, 'exhausted disable must answer 503');
  assert.equal(exhausted.headers.get('retry-after'), '1');
  assert.equal((await api('/api/targets')).find(t => t.model === CHAT)?.state, 'failed', 'exhausted disable must disable the target');

  // record returns the original response and does not retry.
  await setTarget(ctx, api, CHAT, { id: 'r-record', match: 'contains', pattern: 'fixture-bad', action: 'record' });
  await fixtureControl(ctx, 'modelMode=bad');
  const recordBefore = await requestCount(ctx, '/chat/v1/chat/completions');
  const recorded = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [{ role: 'user', content: 'hello' }] } });
  assert.equal(recorded.status, 200);
  assert.equal((await jsonOf(recorded)).model, 'fixture-bad');
  assert.equal(await requestCount(ctx, '/chat/v1/chat/completions'), recordBefore + 1, 'record must not retry');
  await setTarget(ctx, api, CHAT, null);

  await fixtureControl(ctx, 'failPrimary=0&failPrimaryProvider=1');
  const fallback = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [{ role: 'user', content: 'hello' }] } });
  assert.equal(fallback.status, 200, 'provider fallback failed');
  await fixtureControl(ctx, 'failPrimaryProvider=0');

  // Media and auxiliary native routes must reach the fixture unchanged.
  const image = await providerFetch(ctx, '/v1/images/generations', { method: 'POST', body: { model: DRAW, prompt: 'a cat' } });
  const imageText = await image.text();
  assert.equal(image.status, 200, `image: ${imageText.slice(0, 500)}`);
  assert.equal(await requestCount(ctx, '/chat/v1/images/generations'), 1);
  const form = new FormData();
  form.set('model', DRAW);
  form.set('prompt', 'edit');
  form.set('image', new Blob(['PNGDATA'], { type: 'image/png' }), 'in.png');
  const edit = await providerFetch(ctx, '/v1/images/edits', { method: 'POST', body: form });
  const editText = await edit.text();
  assert.equal(edit.status, 200, `edit: ${editText.slice(0, 500)}`);
  const editSeen = await lastFixtureRequest(ctx, '/images/edits');
  assert.match(editSeen.headers['content-type'], /multipart\/form-data; boundary=/);
  assert.match(editSeen.body, /PNGDATA/);

  const embedding = await providerFetch(ctx, '/v1/embeddings', { method: 'POST', body: { model: CHAT, input: 'hello' } });
  assert.equal(embedding.status, 200);
  const rerank = await providerFetch(ctx, '/v1/rerank', { method: 'POST', body: { model: CHAT, query: 'q', documents: ['d'] } });
  assert.equal(rerank.status, 200);
  const transcription = new FormData();
  transcription.set('model', CHAT);
  transcription.set('file', new Blob(['AUDIODATA'], { type: 'audio/wav' }), 'a.wav');
  const transcribed = await providerFetch(ctx, '/v1/audio/transcriptions', { method: 'POST', body: transcription });
  assert.equal(transcribed.status, 200);
  const speech = await providerFetch(ctx, '/v1/audio/speech', { method: 'POST', body: { model: CHAT, input: 'hi' } });
  assert.equal(speech.status, 200);
  assert.equal(await speech.text(), 'AUDIO');
  assert.match(speech.headers.get('content-type'), /audio\/mpeg/);
  const video = await providerFetch(ctx, '/v1/videos', { method: 'POST', body: { model: FILM, prompt: 'a clip' } });
  const videoText = await video.text();
  assert.equal(video.status, 200, `video: ${videoText.slice(0, 500)}`);
  const videoID = JSON.parse(videoText).id;
  assert.ok(videoID, 'video create did not return an id');
  const poll = await providerFetch(ctx, `/v1/videos/${encodeURIComponent(videoID)}`);
  const pollText = await poll.text();
  assert.equal(poll.status, 200, `video poll: ${pollText.slice(0, 500)}`);
  const content = await providerFetch(ctx, `/v1/videos/${encodeURIComponent(videoID)}/content`);
  assert.equal(await content.text(), 'MP4DATA');

  const tokens = await providerFetch(ctx, '/v1/messages/count_tokens', { method: 'POST', body: { model: CHAT, messages: [] } });
  const tokensText = await tokens.text();
  assert.equal(tokens.status, 200, `count tokens: ${tokensText.slice(0, 500)}`);
  // Rare native APIs need their own Magpie providers; the guard must at least
  // preserve the gateway's answer byte for byte rather than substituting one.
  for (const [route, body] of [['/v1/systemone', { model: DECIDE }], ['/v1/alpha/search', { query: 'x' }]]) {
    const guarded = await providerFetch(ctx, route, { method: 'POST', body });
    const guardedText = await guarded.text();
    const direct = await magpieFetch(ctx, route, { method: 'POST', body, key: LOCAL_GATEWAY_KEY });
    const directText = await direct.text();
    assert.equal(guarded.status, direct.status, `${route}: guard ${guarded.status}, native ${direct.status}`);
    assert.equal(guardedText, directText, `${route}: guard altered the native answer`);
    assert.ok(!guardedText.includes('model_guard_error'), `${route}: guard substituted its own error`);
  }
  const tags = await providerFetch(ctx, '/api/tags');
  const tagsText = await tags.text();
  assert.equal(tags.status, 200, `tags: ${tagsText.slice(0, 300)}`);
  const ollama = await providerFetch(ctx, '/api/chat', { method: 'POST', body: { model: CHAT, messages: [] } });
  const ollamaText = await ollama.text();
  assert.equal(ollama.status, 200, `ollama: ${ollamaText.slice(0, 300)}`);
  assert.match(ollamaText, /"done":true/);


  // CORS preflight follows the native policy.
  const directPreflight = await magpieFetch(ctx, '/v1/chat/completions', { method: 'OPTIONS', headers: { origin: 'https://example.invalid', 'access-control-request-method': 'POST' }, key: LOCAL_GATEWAY_KEY });
  const guardPreflight = await fetch(ctx.info.origin + '/provider/v1/chat/completions', {
    method: 'OPTIONS', headers: { origin: 'https://example.invalid', 'access-control-request-method': 'POST' }, signal: AbortSignal.timeout(15000),
  });
  assert.equal(guardPreflight.status, directPreflight.status, 'guarded preflight changed the native CORS status');
  assert.equal(guardPreflight.headers.get('access-control-allow-origin'), directPreflight.headers.get('access-control-allow-origin'));

  // A named gateway key's model allowlist is not bypassed by the guard.
  await guardConfig(ctx, api, { gateway_key: GATEWAY_KEY });
  const denied = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: BACKUP_MODEL, messages: [] } });
  assert.ok([403, 404].includes(denied.status), `gateway key model allowlist not enforced: ${denied.status}`);
  await guardConfig(ctx, api, { gateway_key: LOCAL_GATEWAY_KEY });
}

// --- Responses WebSocket --------------------------------------------------

const WS_HELPER = `package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"
)

type readSpec struct {
	TimeoutMs int    \`json:"timeoutMs"\`
	Count     int    \`json:"count"\`
	Until     string \`json:"until"\`
}

type action struct {
	Send      json.RawMessage \`json:"send"\`
	Read      *readSpec       \`json:"read"\`
	Interrupt string          \`json:"interrupt"\`
	SleepMs   int             \`json:"sleepMs"\`
}

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: wshelper <url> <token> <origin> <actions>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[4])
	if err != nil { panic(err) }
	var actions []action
	if err := json.Unmarshal(raw, &actions); err != nil { panic(err) }
	header := map[string][]string{"Authorization": {"Bearer " + os.Args[2]}}
	if os.Args[3] != "" { header["Origin"] = []string{os.Args[3]} }
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, os.Args[1], &websocket.DialOptions{HTTPHeader: header})
	if err != nil { fmt.Printf("{\\"error\\":%q}\\n", err.Error()); return }
	defer c.CloseNow()
	c.SetReadLimit(512 << 20)
	emit := func(kind string, value any) {
		b, _ := json.Marshal(map[string]any{kind: value})
		fmt.Println(string(b))
	}
	lastID := ""
	for _, act := range actions {
		if len(act.Send) > 0 {
			payload := act.Send
			if lastID != "" {
				replacement := append([]byte{'"'}, lastID...)
				replacement = append(replacement, '"')
				payload = bytes.Replace(payload, []byte{'"', '$', 'l', 'a', 's', 't', '"'}, replacement, 1)
			}
			if err := c.Write(ctx, websocket.MessageText, payload); err != nil { emit("error", err.Error()); return }
		}
		if act.SleepMs > 0 { time.Sleep(time.Duration(act.SleepMs) * time.Millisecond) }
		if act.Interrupt != "" {
			id := act.Interrupt
			if id == "$last" { id = lastID }
			frame, _ := json.Marshal(map[string]string{"type": "response.interrupt", "response_id": id})
			if err := c.Write(ctx, websocket.MessageText, frame); err != nil { emit("error", err.Error()); return }
		}
		if act.Read == nil { continue }
		// Reading under a child context that is canceled after a successful
		// read can close the socket and break the next turn; the helper's
		// overall context bounds the run.
		readCtx := ctx
		read := 0
		for {
			_, data, err := c.Read(readCtx)
			if err != nil {
				code := websocket.CloseStatus(err)
				if code != -1 {
					emit("close", map[string]any{"code": int(code), "reason": err.Error()})
				} else {
					emit("error", err.Error())
				}
				return
			}
			read++
			var seen struct {
				Type string \`json:"type"\`
				Response struct{ ID string \`json:"id"\` } \`json:"response"\`
			}
			_ = json.Unmarshal(data, &seen)
			if seen.Type == "response.created" && seen.Response.ID != "" { lastID = seen.Response.ID }
			fmt.Println(string(mustFrame(data)))
			if act.Read.Until != "" {
				var probe struct{ Type string \`json:"type"\` }
				_ = json.Unmarshal(data, &probe)
				if probe.Type == act.Read.Until { break }
			}
			if act.Read.Count > 0 && read >= act.Read.Count { break }
		}
	}
}

func mustFrame(data []byte) []byte {
	out, _ := json.Marshal(map[string]json.RawMessage{"frame": data})
	return out
}
`;

async function buildWSHelper(ctx) {
  const dir = path.join(root, `.verify-native-ws-${process.pid}-${Date.now()}`);
  await mkdir(dir, { recursive: true });
  await writeFile(path.join(dir, 'main.go'), WS_HELPER, 'utf8');
  const binary = path.join(ctx.tempRoot, 'wshelper');
  await execFileAsync('go', ['build', '-o', binary, '.'], { cwd: dir, env: process.env, timeout: 120000 });
  await rm(dir, { recursive: true, force: true });
  return binary;
}

async function runWS(ctx, helper, actions, origin = '') {
  const file = path.join(ctx.tempRoot, `ws-actions-${randomUUID()}.json`);
  await writeFile(file, JSON.stringify(actions));
  const url = `ws://127.0.0.1:${new URL(ctx.info.origin).port}/provider/v1/responses`;
  const child = spawn(helper, [url, PROVIDER_KEY, origin, file], { stdio: ['ignore', 'pipe', 'pipe'] });
  let stdout = '', stderr = '';
  child.stdout.on('data', chunk => { stdout += chunk; });
  child.stderr.on('data', chunk => { stderr += chunk; });
  const code = await new Promise(resolve => child.once('exit', resolve));
  await rm(file, { force: true });
  const frames = [];
  let close = null;
  let error = null;
  for (const line of stdout.split('\n').filter(Boolean)) {
    let value;
    try { value = JSON.parse(line); } catch { continue; }
    if (value.frame) frames.push(value.frame);
    if (value.close) close = value.close;
    if (value.error) error = value.error;
  }
  if (code !== 0 || (!frames.length && error)) throw new Error(`ws helper failed (${code}): ${sanitize(stderr || error || stdout).slice(0, 800)}`);
  return { frames, close, error };
}

async function wsAcceptance(ctx, api, helper) {
  await fixtureControl(ctx, 'modelMode=alias&failPrimary=0&failAll=0&failPrimaryProvider=0');
  const basic = await runWS(ctx, helper, [
    { send: { type: 'response.create', model: CHAT, input: [{ type: 'message', role: 'user', content: 'hello' }] }, read: { timeoutMs: 8000, until: 'response.completed' } },
  ]);
  const types = basic.frames.map(frame => frame.type);
  assert.ok(types.includes('response.created') && types.includes('response.completed'), `ws frames: ${types.join(',')}`);
  assert.ok(types.indexOf('response.output_text.delta') < types.indexOf('response.completed'), 'delta must arrive before the terminal frame');

  await setTarget(ctx, api, CHAT, { id: 'r-ws-bad', match: 'contains', pattern: 'fixture-bad', action: 'disable' });
  await fixtureControl(ctx, 'modelMode=bad-then-good&resetBadFirst=1&holdBadMs=800');
  const retried = await runWS(ctx, helper, [
    { send: { type: 'response.create', model: CHAT, input: [{ type: 'message', role: 'user', content: 'retry please' }] }, read: { timeoutMs: 12000, until: 'response.completed' } },
  ]);
  const models = retried.frames.filter(frame => frame.type === 'response.created').map(frame => frame.response?.model);
  if (models.length > 0) {
    assert.deepEqual(models, ['fixture-good'], `ws retry delivered ${JSON.stringify(models)}`);
  } else {
    // A vendor stream that finishes before it can be interrupted is blocked,
    // never delivered. The interrupt/retry unit test covers the replay path.
    const blocked = retried.frames.find(frame => frame.type === 'error' && frame.error?.code === 'model_guard_blocked');
    assert.ok(blocked, `ws bad turn neither retried nor blocked: ${JSON.stringify(retried.frames)}`);
    assert.equal(retried.close?.code, 1011, `blocked close=${JSON.stringify(retried.close)}`);
  }

  await setTarget(ctx, api, CHAT, null);
  await fixtureControl(ctx, 'modelMode=alias');
  const continued = await runWS(ctx, helper, [
    { send: { type: 'response.create', model: CHAT, input: [{ type: 'message', role: 'user', content: 'first turn' }] }, read: { timeoutMs: 8000, until: 'response.completed' } },
    { send: { type: 'response.create', model: CHAT, previous_response_id: '$last', input: [{ type: 'function_call_output', call_id: 'call_1', output: 'first turn' }] }, read: { timeoutMs: 8000, until: 'response.completed' } },
  ]);
  assert.equal(continued.frames.filter(frame => frame.type === 'response.completed').length, 2, 'two ws turns did not complete');
  const second = await lastFixtureRequest(ctx, '/responses/v1/responses');
  assert.match(second.body, /first turn/, 'previous_response_id history was not carried by the native socket');
  assert.match(second.body, /function_call_output/);

  await fixtureControl(ctx, 'modelMode=alias&holdMs=5000');
  const interrupted = await runWS(ctx, helper, [
    { send: { type: 'response.create', model: CHAT, input: [{ type: 'message', role: 'user', content: 'interrupt me' }] }, read: { timeoutMs: 8000, count: 1 } },
    { interrupt: '$last', read: { timeoutMs: 8000, until: 'response.incomplete' } },
  ]);
  const interruptedTypes = interrupted.frames.map(frame => frame.type);
  assert.ok(interruptedTypes.includes('response.interrupt.accepted'), 'native interrupt was not accepted');
  assert.ok(interruptedTypes.includes('response.incomplete'));
  await fixtureControl(ctx, 'holdMs=0');

  const warmBefore = await requestCount(ctx, '/responses/v1/responses');
  const warm = await runWS(ctx, helper, [
    { send: { type: 'response.create', model: CHAT, generate: false, input: [] }, read: { timeoutMs: 8000, count: 2 } },
  ]);
  assert.deepEqual(warm.frames.map(frame => frame.type), ['response.created', 'response.completed'], 'warmup was judged or forwarded');
  assert.equal(await requestCount(ctx, '/responses/v1/responses'), warmBefore, 'warmup reached the provider');

  await setTarget(ctx, api, CHAT, { id: 'r-ws-terminal', match: 'contains', pattern: 'fixture-bad', action: 'disable' });
  await fixtureControl(ctx, 'modelMode=bad');
  const terminal = await runWS(ctx, helper, [
    { send: { type: 'response.create', model: CHAT, input: [{ type: 'message', role: 'user', content: 'terminal bad' }] }, read: { timeoutMs: 8000, until: 'error' } },
    { read: { timeoutMs: 3000 } },
  ]);
  assert.equal(terminal.frames[0]?.type, 'error', 'terminal bad reply must not be delivered');
  assert.equal(terminal.frames[0]?.error?.code, 'model_guard_blocked');
  assert.equal(terminal.close?.code, 1011, `bad terminal close=${JSON.stringify(terminal.close)}`);
  await setTarget(ctx, api, CHAT, null);
}

// --- console and lifecycle ------------------------------------------------

async function consoleFetch(ctx, route, { method = 'GET', body } = {}) {
  const response = await fetch(ctx.info.origin + route, {
    method,
    headers: { cookie: `mg_session=${ctx.info.control}`, ...(body === undefined ? {} : { 'content-type': 'application/json' }) },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(15000),
  });
  const text = await response.text();
  return { status: response.status, text, json: (() => { try { return JSON.parse(text); } catch { return null; } })() };
}

async function consoleAcceptance(ctx, api) {
  const html = await consoleFetch(ctx, '/');
  assert.equal(html.status, 200);
  for (const id of ['cfg-provider-url', 'cfg-provider-url-native', 'cfg-provider-url-gemini', 'cfg-provider-attempts', 'cfg-provider-key']) {
    assert.match(html.text, new RegExp(`id="${id}"`), `console lost ${id}`);
  }
  assert.match(html.text, /OpenAI \/ Responses/);
  assert.match(html.text, /record 首次命中即记录，不重试/);

  const state = await api('/api/state');
  assert.ok(state.provider_base?.endsWith('/provider/v1'), 'provider_base missing');
  assert.ok(state.provider_root?.endsWith('/provider'), 'provider_root missing');
  assert.equal(state.provider_base, state.provider_root + '/v1');

  const config = { ...state.config, gateway_url: ctx.gatewayUrl, gateway_key: LOCAL_GATEWAY_KEY };
  delete config.provider_enabled;
  await api('/api/config', { method: 'POST', body: config });
  let after = await api('/api/state');
  assert.equal(after.config.provider_enabled, true, 'omitted provider_enabled must keep the provider on');
  const missing = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [] } });
  assert.equal(missing.status, 200);

  await api('/api/config', { method: 'POST', body: { ...config, provider_enabled: false } });
  after = await api('/api/state');
  assert.equal(after.config.provider_enabled, false);
  const off = await providerFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [] } });
  assert.equal(off.status, 404, 'disabled provider must return 404');
  await api('/api/config', { method: 'POST', body: { ...config, provider_enabled: true } });
}

async function restartAcceptance(ctx, pluginIndex, options) {
  const oldOrigin = ctx.info.origin;
  const oldKey = (await consoleFetch(ctx, '/api/state')).json.config.provider_key;
  await stopChild(ctx.guard);
  ctx.guard = null;

  const restarted = await startHost(ctx.tempRoot, ctx.pkgDir, options);
  ctx.guard = restarted.child;
  ctx.info = restarted.info;
  assert.equal(ctx.info.origin, oldOrigin, 'guard did not reuse its persisted listen address');
  const state = await (await fetch(ctx.info.origin + '/api/state', { headers: { authorization: `Bearer ${ctx.info.control}` }, signal: AbortSignal.timeout(5000) })).json();
  assert.equal(state.config.provider_key, oldKey, 'provider key changed across restart');

  // The saved port must not silently drift when it is occupied.
  await stopChild(ctx.guard);
  ctx.guard = null;
  const port = Number(new URL(oldOrigin).port);
  const blocker = createNetServer();
  await new Promise((resolve, reject) => { blocker.once('error', reject); blocker.listen(port, '127.0.0.1', resolve); });
  let failed = false;
  try {
    const broken = await startHost(ctx.tempRoot, ctx.pkgDir, options);
    await stopChild(broken.child);
  } catch {
    failed = true;
  }
  await new Promise(resolve => blocker.close(resolve));
  assert.equal(failed, true, 'guard started on another port instead of failing on an occupied one');

  const replacement = await freePort();
  const recovered = await startHost(ctx.tempRoot, ctx.pkgDir, { ...options, backendAddr: `127.0.0.1:${replacement}` });
  ctx.guard = recovered.child;
  ctx.info = recovered.info;
  assert.equal(Number(new URL(ctx.info.origin).port), replacement, 'backendAddr did not recover the listener');
}

async function main() {
  const tempRoot = await mkdtemp(path.join(os.tmpdir(), 'model-guard-native-'));
  const fixture = await startFixture();
  const magpieBinary = await resolveMagpieBinary();
  const guardBinary = await resolveGuardBinary();
  const gatewayPort = await freePort();
  const ctx = newContext(tempRoot, fixture, magpieBinary, guardBinary, gatewayPort);
  addSecret(PROVIDER_KEY);
  addSecret(GATEWAY_KEY);
  addSecret(LOCAL_GATEWAY_KEY);
  let helper = null;
  try {
    await prepareMagpie(ctx);
    await startMagpie(ctx);
    await configureProviders(ctx);
    const pluginIndex = await preparePackage(ctx);
    const options = {
      dataDir: ctx.guardDir,
      magpieConfigDir: path.join(ctx.configHome, 'magpie'),
      gatewayURL: ctx.gatewayUrl,
      gatewayKey: 'magpie',
      binaryPath: guardBinary,
    };
    const host = await startHost(tempRoot, ctx.pkgDir, options);
    ctx.guard = host.child;
    ctx.info = host.info;
    addSecret(ctx.info.control);
    addSecret(ctx.info.hookControl);
    const api = guardAPI(ctx.info);
    await guardConfig(ctx, api);

    await httpAcceptance(ctx, api);
    // The HTTP suite forces provider fails and rests; a fresh gateway keeps the
    // WebSocket turns on the primary provider they name.
    await stopChild(ctx.magpieChild);
    await startMagpie(ctx);
    helper = await buildWSHelper(ctx);
    await wsAcceptance(ctx, api, helper);
    await consoleAcceptance(ctx, api);
    await restartAcceptance(ctx, pluginIndex, options);

    assert.ok(helper, 'ws helper was not built');
    console.log('native provider acceptance passed');
  } finally {
    await stopChild(ctx.guard);
    await stopChild(ctx.magpieChild);
    await fixture.close();
    await rm(tempRoot, { recursive: true, force: true });
  }
}

main().catch(error => {
  console.error('native provider acceptance failed:', sanitize(error?.stack || error));
  process.exitCode = 1;
});
