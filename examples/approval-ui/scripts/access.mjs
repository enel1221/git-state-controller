const name = '[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?';
export const acceptedPaths = `^/(?:api/v1/namespaces(?:/${name})?|apis/gitops\\.example\\.io/v1alpha1/(?:gitresources|namespaces/${name}/gitresources(?:/${name})?)|apis/apiextensions\\.k8s\\.io/v1/customresourcedefinitions/gitresources\\.gitops\\.example\\.io)$`;
export function permitted(path, method) {
  if (!new RegExp(acceptedPaths).test(path)) return false;
  if (path === '/api/v1/namespaces') return ['GET', 'POST'].includes(method);
  if (path.startsWith('/api/v1/namespaces/')) return ['GET', 'PATCH'].includes(method);
  if (path.startsWith('/apis/apiextensions.') || path === '/apis/gitops.example.io/v1alpha1/gitresources') return method === 'GET';
  return path.endsWith('/gitresources') ? ['GET', 'POST'].includes(method) : ['GET', 'PATCH'].includes(method);
}
export function sameOrigin(headers, method, origin) {
  if (headers.host !== new URL(origin).host) return false;
  if (headers['sec-fetch-site'] && !['same-origin', 'none'].includes(headers['sec-fetch-site'])) return false;
  if (headers.origin && headers.origin !== origin) return false;
  return method === 'GET' || headers.origin === origin;
}
