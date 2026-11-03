// Manual acceptance for supplier protection: the Guard takes over the vendor
// endpoints Magpie already saved, so clients keep Magpie's address, keys and
// model ids.
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
const BACKUP_MODEL = 'backup/backup-model';
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
    omitReplyID: false,
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
        if (url.searchParams.has('omitReplyID')) state.omitReplyID = url.searchParams.get('omitReplyID') === '1';
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
          res.write(`data: ${JSON.stringify({ ...(state.omitReplyID ? {} : { id: 'chatcmpl_fixture' }), object: 'chat.completion.chunk', model, choices: [{ index: 0, delta: { content: 'ok' }, finish_reason: null }] })}\n\n`);
          const done = () => { if (res.writableEnded) return; res.write('data: [DONE]\n\n'); res.end(); };
          if (state.holdBadMs > 0 && model === 'fixture-bad') setTimeout(done, state.holdBadMs);
          else done();
          return;
        }
        json({ ...(state.omitReplyID ? {} : { id: 'chatcmpl_fixture' }), object: 'chat.completion', model, choices: [{ index: 0, message: { role: 'assistant', content: tool ? 'tool-ok' : 'ok' }, finish_reason: 'stop' }], usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 } });
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
  return async function api(route, { method = 'GET', body, control = false } = {}) {
    const headers = { authorization: `Bearer ${control ? info.hookControl : info.control}` };
    if (body !== undefined) headers['content-type'] = 'application/json';
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

async function configureProviders(ctx) {
  await magpieCLI(ctx, ['provider', 'refresh', PROVIDER], { timeout: 30000 });
  await magpieCLI(ctx, ['provider', 'refresh', BACKUP], { timeout: 30000 });
  // Loopback clients need no key; the named key is what the Guard's own probes
  // send to Magpie.
  await magpieCLI(ctx, ['gateway-key', 'add', 'local', '--key', LOCAL_GATEWAY_KEY]);
}

async function guardConfig(ctx, api, extra = {}) {
  await api('/api/config', {
    method: 'POST',
    body: {
      gateway_url: ctx.gatewayUrl,
      gateway_key: LOCAL_GATEWAY_KEY,
      provider_attempts: 2,
      ...extra,
    },
  });
}

// --- supplier acceptance helpers ------------------------------------------

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

// --- supplier takeover ----------------------------------------------------

// Supplier protection keeps every client request on the Magpie gateway.
async function supplierAcceptance(ctx, api, options) {
  await guardConfig(ctx, api, { response_model_mode: 'off' });
  const file = path.join(ctx.configHome, 'magpie', 'providers.json');
  const before = JSON.parse(await readFile(file, 'utf8'));
  const original = before.providers.find(p => p.id === PROVIDER);
  const list = await api('/api/suppliers');
  assert.ok(list.configured && list.providers.some(p => p.id === PROVIDER));
  await api('/api/suppliers/protection', { method: 'POST', body: { provider_id: PROVIDER, enabled: true } });
  const managed = JSON.parse(await readFile(file, 'utf8')).providers.find(p => p.id === PROVIDER);
  assert.ok(managed.chat.startsWith(ctx.info.origin + '/upstream/'));
  assert.equal(managed.key, original.key);
  assert.deepEqual(managed.keys, original.keys);
  assert.deepEqual(managed.models, original.models);
  await setTarget(ctx, api, CHAT, { id: 'supplier-disable', match: 'exact', pattern: 'fixture-bad', action: 'disable' });
  await fixtureControl(ctx, 'modelMode=bad-then-good&resetBadFirst=1');
  const count = await requestCount(ctx, '/chat/v1/chat/completions');
  const response = await magpieFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [{ role: 'user', content: 'supplier protection' }] } });
  const text = await response.text();
  assert.equal(response.status, 200, text);
  assert.equal(await requestCount(ctx, '/chat/v1/chat/completions'), count + 2, 'supplier rule did not retry before Magpie delivered the reply');
  assert.ok(!text.includes('fixture-bad'), 'rejected vendor model leaked to the client');
  const seen = await lastFixtureRequest(ctx, '/chat/completions');
  assert.ok(seen.headers.authorization?.startsWith('Bearer '));
  assert.equal(seen.headers['x-model-guard-session'], undefined, 'internal probe marker leaked');
  assert.equal(JSON.parse(seen.body).model, 'fixture-chat');
  assert.equal(JSON.parse(seen.body).fixture_replace, 'override');

  for (const [route, body, vendorPath] of [
    ['/v1/messages', { model: CHAT, max_tokens: 128, messages: [{ role: 'user', content: 'hello' }] }, '/anthropic/v1/messages'],
    ['/v1/responses', { model: CHAT, input: 'hello', stream: true }, '/responses/v1/responses'],
    [`/v1beta/models/${encodeURIComponent(CHAT)}:generateContent`, { contents: [{ role: 'user', parts: [{ text: 'hello' }] }] }, '/gemini/v1beta/models/fixture-chat:streamGenerateContent'],
  ]) {
    const settingsFile = path.join(ctx.configHome, 'magpie', 'settings.json');
    const settingsBefore = JSON.parse(await readFile(settingsFile, 'utf8').catch(error => { if (error.code === 'ENOENT') return '{}'; throw error; }));
    if (route.startsWith('/v1beta/')) {
      await writeFile(settingsFile, JSON.stringify({ ...settingsBefore, modelAPIs: { ...settingsBefore.modelAPIs, [CHAT]: 'gemini' } }));
    }
    await fixtureControl(ctx, 'modelMode=bad-then-good&resetBadFirst=1');
    const n = await requestCount(ctx, vendorPath);
    const reply = await magpieFetch(ctx, route, { method: 'POST', body });
    const content = await reply.text();
    assert.equal(reply.status, 200, `${route}: ${content}`);
    const afterCount = await requestCount(ctx, vendorPath);
    if (afterCount !== n + 2) {
      const trace = await (await fetch(ctx.fixture.base + '/__state')).json();
      throw new Error(`${route} supplier retries: expected ${n + 2}, got ${afterCount}; recent vendor paths: ${trace.requests.slice(-10).map(r => r.path).join(', ')}`);
    }
    assert.ok(!content.includes('fixture-bad'), `${route} leaked the blocked model`);
    if (route.startsWith('/v1beta/')) await writeFile(settingsFile, JSON.stringify(settingsBefore));
  }

  // Install only the generated middleware, avoiding a second plugin host.
  await magpieCLI(ctx, ['plugin', 'add', path.join(ctx.pkgDir, 'plugin', 'gate.middleware.js')]);
  await api('/api/prompts', { method: 'POST', body: { id: 'supplier-probe', name: 'Supplier recovery', text: 'hello', grader: 'exact', expect: 'ok', automatic: false } });
  await api('/api/targets/action', { method: 'POST', body: { model: CHAT, action: 'disable' } });
  const blockedCount = await requestCount(ctx, '/chat/v1/chat/completions');
  const blocked = await magpieFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [] } });
  assert.equal(blocked.status, 403);
  await blocked.text();
  assert.equal(await requestCount(ctx, '/chat/v1/chat/completions'), blockedCount);
  const probe = await api('/api/probe', { method: 'POST', body: { model: CHAT, prompt_id: 'supplier-probe' } });
  assert.equal(probe.verdict, 'passed', 'protected blocked model must still receive its recovery probe');
  assert.equal(await requestCount(ctx, '/chat/v1/chat/completions'), blockedCount + 1);
  await api('/api/targets/action', { method: 'POST', body: { model: CHAT, action: 'enable' } });
  await fixtureControl(ctx, 'modelMode=alias');
  await waitFor('middleware release after enable', async () => {
    const released = await magpieFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [] } });
    await released.text();
    return released.status === 200;
  });

  await fixtureControl(ctx, 'modelMode=bad');
  await setTarget(ctx, api, CHAT, { id: 'supplier-record', match: 'exact', pattern: 'fixture-bad', action: 'record' });
  const recordCount = await requestCount(ctx, '/chat/v1/chat/completions');
  const recorded = await magpieFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [] } });
  assert.equal(recorded.status, 200);
  await recorded.text();
  assert.equal(await requestCount(ctx, '/chat/v1/chat/completions'), recordCount + 1);
  const records = await api('/api/results?model=' + encodeURIComponent(CHAT));
  assert.ok(records.some(r => r.rule_id === 'supplier-record' && r.actual_model === 'fixture-bad'));

  // A vendor reply without an id acquires a fresh id during native translation.
  // The async hook must still leave ownership with supplier protection.
  const noIDSettingsFile = path.join(ctx.configHome, 'magpie', 'settings.json');
  const noIDSettingsBefore = JSON.parse(await readFile(noIDSettingsFile, 'utf8'));
  await writeFile(noIDSettingsFile, JSON.stringify({ ...noIDSettingsBefore, modelAPIs: { ...noIDSettingsBefore.modelAPIs, [CHAT]: 'chat' } }));
  await fixtureControl(ctx, 'modelMode=alias&omitReplyID=1');
  await setTarget(ctx, api, CHAT, { id: 'direct-duplicate', match: 'exact', pattern: CHAT, action: 'disable' });
  const noIDResponse = await magpieFetch(ctx, '/v1/responses', { method: 'POST', body: { model: CHAT, input: 'vendor reply without id' } });
  const noIDReply = await jsonOf(noIDResponse);
  assert.equal(noIDResponse.status, 200);
  assert.ok(noIDReply.id, 'native translation did not generate a reply id');
  assert.notEqual(noIDReply.id, 'chatcmpl_fixture');
  await api('/api/response-model', { method: 'POST', body: { requested_model: CHAT, actual_model: CHAT, response_id: noIDReply.id, success: true }, control: true });
  const noIDTarget = (await api('/api/targets')).find(t => t.model === CHAT);
  assert.notEqual(noIDTarget.state, 'failed', 'generated id caused a duplicate async disable');
  assert.ok(!(await api('/api/results?model=' + encodeURIComponent(CHAT))).some(r => r.rule_id === 'direct-duplicate'), 'generated id caused a duplicate async result');
  await fixtureControl(ctx, 'omitReplyID=0');
  await writeFile(noIDSettingsFile, JSON.stringify(noIDSettingsBefore));
  await setTarget(ctx, api, CHAT, { id: 'supplier-record', match: 'exact', pattern: 'fixture-bad', action: 'record' });

  const groupedConfig = JSON.parse(await readFile(file, 'utf8'));
  groupedConfig.groups = [...(groupedConfig.groups || []), { id: 'supplier-check', name: 'Supplier check', members: [CHAT], routing: 'order' }];
  await writeFile(file, JSON.stringify(groupedConfig));
  await guardConfig(ctx, api, { response_model_mode: 'on', response_rules: [{ id: 'group-duplicate', match: 'exact', pattern: 'group/supplier-check', action: 'record' }] });
  const grouped = await magpieFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: 'group/supplier-check', messages: [] } });
  const groupedReply = await jsonOf(grouped);
  assert.equal(grouped.status, 200);
  await api('/api/response-model', { method: 'POST', body: { requested_model: 'group/supplier-check', actual_model: 'group/supplier-check', response_id: groupedReply.id, success: true }, control: true });
  const duplicates = await api('/api/results?model=group%2Fsupplier-check');
  assert.ok(!duplicates.some(r => r.rule_id === 'group-duplicate'), 'group reply was judged twice');
  await guardConfig(ctx, api, { response_model_mode: 'off', response_rules: [] });

  const oldOrigin = ctx.info.origin;
  await stopChild(ctx.guard);
  ctx.guard = null;
  const restarted = await startHost(ctx.tempRoot, ctx.pkgDir, options);
  ctx.guard = restarted.child;
  ctx.info = restarted.info;
  assert.equal(ctx.info.origin, oldOrigin);
  const restartedAPI = guardAPI(ctx.info);
  assert.ok((await restartedAPI('/api/suppliers')).providers.find(p => p.id === PROVIDER).protected);
  await restartedAPI('/api/suppliers/protection', { method: 'POST', body: { provider_id: PROVIDER, enabled: false } });
  const restored = JSON.parse(await readFile(file, 'utf8')).providers.find(p => p.id === PROVIDER);
  for (const key of ['chat', 'responses', 'anthropic', 'gemini', 'proxy', 'accountProxies', 'headers']) {
    assert.deepEqual(restored[key], original[key], `${key} was not restored`);
  }
  await setTarget(ctx, restartedAPI, CHAT, null);
  await fixtureControl(ctx, 'modelMode=alias');
  const unprotected = await magpieFetch(ctx, '/v1/chat/completions', { method: 'POST', body: { model: CHAT, messages: [] } });
  assert.equal(unprotected.status, 200);
  await unprotected.text();
  console.log('supplier protection acceptance passed (unchanged client URL/key/models; four native protocols; retry, record, restart, restore)');
}

async function main() {
  const tempRoot = await mkdtemp(path.join(os.tmpdir(), 'model-guard-native-'));
  const fixture = await startFixture();
  const magpieBinary = await resolveMagpieBinary();
  const guardBinary = await resolveGuardBinary();
  const gatewayPort = await freePort();
  const ctx = newContext(tempRoot, fixture, magpieBinary, guardBinary, gatewayPort);
  addSecret(LOCAL_GATEWAY_KEY);
  try {
    await prepareMagpie(ctx);
    await startMagpie(ctx);
    await configureProviders(ctx);
    await preparePackage(ctx);
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
    await supplierAcceptance(ctx, guardAPI(ctx.info), options);
  } finally {
    await stopChild(ctx.guard);
    await stopChild(ctx.magpieChild);
    await fixture.close();
    await rm(tempRoot, { recursive: true, force: true });
  }
}

main().catch(error => {
  console.error('supplier protection acceptance failed:', sanitize(error?.stack || error));
  process.exitCode = 1;
});
