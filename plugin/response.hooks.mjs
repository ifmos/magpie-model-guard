// Magpie async gateway hook: report the real model of a completed reply to
// the Model Guard backend. It observes only; it never rewrites a reply.
import { readFile, stat } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

const cache = new Map(); // config path -> {stamp, value}

export function onRequest() {
  // Required by Magpie's hook loader; request context is unchanged.
  return undefined;
}

function dataDir(options) {
  if (options && typeof options.dataDir === 'string' && options.dataDir) {
    return options.dataDir;
  }
  return path.join(os.homedir(), '.config', 'magpie-model-guard');
}

function localOrigin(value) {
  if (typeof value !== 'string') return '';
  try {
    const u = new URL(value);
    if (u.protocol !== 'http:' || u.username || u.password || u.search || u.hash) return '';
    if (!['127.0.0.1', 'localhost', '[::1]', '::1'].includes(u.hostname)) return '';
    return u.origin;
  } catch {
    return '';
  }
}

async function hookConfig(options) {
  const file = path.join(dataDir(options), 'response-hook.json');
  let info;
  try {
    info = await stat(file);
  } catch {
    cache.delete(file);
    return null;
  }
  const stamp = info.mtimeMs + ':' + info.size;
  const hit = cache.get(file);
  if (hit && hit.stamp === stamp) return hit.value;
  let value = null;
  try {
    value = JSON.parse(await readFile(file, 'utf8'));
  } catch {
    value = null;
  }
  cache.set(file, { stamp, value });
  return value;
}

function requestedModel(ctx) {
  const candidates = [ctx?.request?.model, ctx?.model];
  for (const value of candidates) {
    if (typeof value === 'string' && value.trim()) return value.trim();
  }
  return '';
}

function actualModel(reply) {
  if (!reply || typeof reply !== 'object') return '';
  const candidates = [
    reply.model,
    reply.modelVersion,
    reply.model_name,
    reply.response?.model,
    reply.response?.modelVersion,
    reply.message?.model,
  ];
  const first = reply.candidates?.[0];
  if (first && typeof first === 'object') {
    candidates.push(first.model, first.modelVersion);
  }
  for (const value of candidates) {
    if (typeof value === 'string' && value.trim()) return value.trim();
  }
  return '';
}

// replyID is the id the client reply carries. Magpie renders streams into one
// whole reply before the hook sees it, so the translated protocol's id field
// is the one to read; the vendor-side id is prefix-normalized by the backend.
function replyID(reply) {
  if (!reply || typeof reply !== 'object') return '';
  const candidates = [
    reply.id,
    reply.responseId,
    reply.response?.id,
    reply.response?.responseId,
    reply.message?.id,
  ];
  for (const value of candidates) {
    if (typeof value === 'string' && value.trim()) return value.trim();
  }
  return '';
}

export async function onResponse(reply, ctx) {
  try {
    if (!ctx || ctx.success !== true) return;
    const cfg = await hookConfig(ctx.options);
    if (!cfg || cfg.enabled !== true) return;
    const origin = localOrigin(cfg.origin);
    const control = typeof cfg.control === 'string' ? cfg.control : '';
    if (!origin || !control) return;
    let requested = requestedModel(ctx).replace(/^magpie\//, '');
    if (!requested || requested.startsWith('model-guard-probe/')) return;
    const actual = actualModel(reply);
    if (!actual) return;
    await fetch(origin + '/api/response-model', {
      method: 'POST',
      headers: {
        authorization: 'Bearer ' + control,
        'content-type': 'application/json',
      },
      body: JSON.stringify({
        requested_model: requested,
        actual_model: actual,
        response_id: replyID(reply),
        protocol: typeof ctx.protocol === 'string' ? ctx.protocol : '',
        agent: typeof ctx.agent === 'string' ? ctx.agent : '',
        success: true,
      }),
      signal: AbortSignal.timeout(800),
    });
  } catch {
    // Best-effort observation; never affect the client reply.
  }
}
