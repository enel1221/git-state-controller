import { defineConfig } from 'vite';
// @ts-expect-error Small Node helper is shared with the development proxy tests.
import { permitted, sameOrigin } from './scripts/access.mjs';
export default defineConfig({
  server: {
    host: '127.0.0.1', port: 5173, strictPort: true, cors: false,
    allowedHosts: ['127.0.0.1'], origin: 'http://127.0.0.1:5173',
    fs: { strict: true, allow: [import.meta.dirname] },
    proxy: { '/kube': { target: 'http://127.0.0.1:8001', rewrite: path => path.slice(5) } },
  },
  plugins: [{ name: 'restricted-kubernetes-client', configureServer(server) {
    server.middlewares.use((req, res, next) => {
      if (!req.url?.startsWith('/kube')) return next();
      const path = new URL(req.url, 'http://127.0.0.1:5173').pathname.slice(5);
      if (!permitted(path, req.method) || !sameOrigin(req.headers, req.method, 'http://127.0.0.1:5173')) {
        res.statusCode = 403; res.end('This local client does not permit that request.'); return;
      }
      next();
    });
  } }],
});
