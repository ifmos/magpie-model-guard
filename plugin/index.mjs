import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import os from 'node:os';

const root = fileURLToPath(new URL('../', import.meta.url));
let child, ready, stopDiscovery;

// Start the Go backend; resolves with {origin, control} startup info.
function start(options = {}) {
  if (ready) return ready;
  ready = new Promise((resolve, reject) => {
    const platform = { win32: 'windows', darwin: 'darwin', linux: 'linux' }[process.platform] || process.platform;
    const arch = { x64: 'amd64', arm64: 'arm64' }[process.arch] || process.arch;
    const suffix = platform === 'windows' ? '.exe' : '';
    const bundled = path.join(root, 'bin', `model-guard-${platform}-${arch}${suffix}`);
    const fallback = path.join(root, 'bin', `model-guard${suffix}`);
    const binary = options.binaryPath || (existsSync(bundled) ? bundled : existsSync(fallback) ? fallback : undefined);
    if (!binary) {
      reject(new Error('找不到 Go 后台二进制：请先运行 go build -o bin/model-guard ./cmd/model-guard'));
      return;
    }
    const args = [
      '-data', options.dataDir || path.join(os.homedir(), '.config', 'magpie-model-guard'),
      '-gate', path.join(root, 'plugin', 'gate.middleware.js'),
      '-owned',
    ];
    child = spawn(binary, args, { stdio: ['pipe', 'pipe', 'pipe'], windowsHide: true });
    child.stdin.on('error', () => {});
    let output = '';
    const timer = setTimeout(() => { child.kill(); reject(new Error('model-guard 后台启动超时')); }, 15000);
    child.stdout.on('data', b => {
      output += b;
      const i = output.indexOf('\n');
      if (i >= 0) {
        try {
          const v = JSON.parse(output.slice(0, i));
          clearTimeout(timer);
          resolve(v);
        } catch { clearTimeout(timer); reject(new Error('后台启动信息无效')); }
      }
    });
    child.stderr.on('data', () => {}); // never log raw payloads
    child.once('error', e => { clearTimeout(timer); ready = undefined; reject(new Error(`无法启动 Go 后台：${e.message}`)); });
    child.once('exit', () => { clearTimeout(timer); stopDiscovery?.(); stopDiscovery = undefined; ready = undefined; });
  });
  return ready;
}
process.once('exit', () => child?.kill());

// Discover the local magpie gateway (settings.json port / MAGPIE_ADDR).
async function discoverGateway(options = {}, env = process.env, fetcher = fetch) {
  let url = options.gatewayURL;
  const key = options.gatewayKey || 'magpie';
  if (!url) {
    let port = 3425;
    try {
      const { readFile } = await import('node:fs/promises');
      const settings = JSON.parse(await readFile(
        path.join(env.XDG_CONFIG_HOME || path.join(os.homedir(), '.config'), 'magpie', 'settings.json'), 'utf8'));
      if (Number.isInteger(settings.port) && settings.port > 0 && settings.port < 65536) port = settings.port;
    } catch {}
    let address = env.MAGPIE_ADDR || `127.0.0.1:${port}`;
    if (/^0\.0\.0\.0:\d+$/.test(address)) address = address.replace('0.0.0.0', '127.0.0.1');
    if (/^\[::\]:\d+$/.test(address)) address = address.replace('[::]', '[::1]');
    if (/^:\d+$/.test(address)) address = '127.0.0.1' + address;
    // Discovery never sends the default key to a non-loopback host.
    if (!/^(?:127\.0\.0\.1|localhost|\[::1\]):\d+$/.test(address)) return undefined;
    url = `http://${address}`;
  }
  try {
    const parsed = new URL(url);
    if (!['http:', 'https:'].includes(parsed.protocol) || parsed.username || parsed.password || parsed.search || parsed.hash) return undefined;
    if (!['127.0.0.1', 'localhost', '[::1]'].includes(parsed.hostname) && !options.gatewayKey) return undefined;
    const base = url.replace(/\/$/, '');
    const response = await fetcher(base + (base.endsWith('/v1') ? '' : '/v1') + '/models', {
      headers: { authorization: `Bearer ${key}` }, redirect: 'error', signal: AbortSignal.timeout(3000),
    });
    if (!response.ok) return undefined;
    const data = await response.json();
    if (!Array.isArray(data.data)) return undefined;
    return { gateway_url: url, gateway_key: key };
  } catch { return undefined; }
}

// Keep pushing discovery into the backend until it has a gateway configured.
function startGatewayDiscovery(service, options = {}) {
  let stopped = false, timer;
  const headers = { authorization: `Bearer ${service.control}`, 'content-type': 'application/json' };
  async function attempt() {
    try {
      const response = await fetch(service.origin + '/api/state', { headers, signal: AbortSignal.timeout(3000) });
      if (!response.ok) throw Error('state unavailable');
      const state = await response.json();
      if (state.config?.gateway_url) return;
      const cfg = await discoverGateway(options);
      if (cfg && !stopped) {
        await fetch(service.origin + '/api/config/initial', {
          method: 'POST', headers, body: JSON.stringify(cfg), signal: AbortSignal.timeout(3000),
        });
      }
    } catch {}
    if (!stopped) {
      timer = setTimeout(attempt, 5000);
      timer.unref?.();
    }
  }
  timer = setTimeout(attempt, 0);
  timer.unref?.();
  return () => { stopped = true; clearTimeout(timer); };
}

export default {
  id: 'magpie-model-guard',
  server: async (_input, options = {}) => ({
    auth: {
      provider: 'magpie-model-guard',
      methods: [{
        type: 'oauth',
        label: '打开 Model Guard 控制台',
        async authorize() {
          const v = await start(options);
          const url = `${v.origin}/#token=${v.control}`;
          return {
            url,
            instructions: `完整管理链接：${url}\n复制链接后在浏览器打开。此入口不是供应商登录，无需提交验证码；使用后可以取消此窗口。`,
            method: 'code',
            callback: async () => ({ type: 'failed' }),
          };
        },
      }],
      async loader() { await start(options); return {}; },
    },
    async config(cfg) {
      // Start on host configuration so persisted monitoring survives restart.
      const service = await start(options);
      stopDiscovery ??= startGatewayDiscovery(service, options);
      cfg.provider ??= {};
      cfg.provider['magpie-model-guard'] = { name: 'Model Guard', models: {} };
    },
  }),
};
