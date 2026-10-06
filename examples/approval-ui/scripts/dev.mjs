import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { createServer as netServer } from 'node:net';
import { resolve } from 'node:path';
import { createServer } from 'vite';
import { acceptedPaths } from './access.mjs';
const root = resolve(import.meta.dirname, '../../..');
const kubeconfig = resolve(root, '.dev/kubeconfig');
let proxy, vite;
async function stop() {
  await vite?.close();
  proxy?.kill('SIGTERM');
}
async function vacant(port) {
  const server = netServer();
  await new Promise((resolve, reject) => { server.once('error', reject); server.listen(port, '127.0.0.1', resolve); });
  await new Promise(resolve => server.close(resolve));
}
try {
  if (!existsSync(kubeconfig)) throw new Error('Run make up; the explicit project .dev/kubeconfig is required.');
  await vacant(8001); await vacant(5173);
  proxy = spawn('kubectl', ['--kubeconfig', kubeconfig, '--context', 'k3d-git-state-dev', 'proxy', '--address=127.0.0.1', '--port=8001', `--accept-paths=${acceptedPaths}`, '--reject-methods=^(DELETE|PUT|CONNECT|TRACE|OPTIONS|HEAD)$'], { stdio: ['ignore', 'pipe', 'inherit'] });
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error('Kubernetes proxy did not start.')), 10_000);
    proxy.once('error', reject);
    proxy.once('exit', code => reject(new Error(`Kubernetes proxy exited (${code}).`)));
    proxy.stdout.on('data', data => { if (data.toString().includes('Starting to serve')) { clearTimeout(timeout); resolve(); } });
  });
  vite = await createServer({ root: resolve(root, 'examples/approval-ui'), configFile: resolve(root, 'examples/approval-ui/vite.config.ts') });
  await vite.listen(); vite.printUrls();
  proxy.once('exit', () => { void stop().then(() => process.exit(1)); });
  for (const signal of ['SIGINT', 'SIGTERM']) process.once(signal, () => { void stop().then(() => process.exit(0)); });
} catch (error) {
  console.error(error.message); await stop(); process.exitCode = 1;
}
process.once('exit', () => proxy?.kill('SIGTERM'));
