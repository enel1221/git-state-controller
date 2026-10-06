export const annotations = {
  enabled: 'gitops.example.io/enabled', required: 'gitops.example.io/approval-required',
  approved: 'gitops.example.io/approved-request', approvedBy: 'gitops.example.io/approved-by', paused: 'gitops.example.io/paused', adopt: 'gitops.example.io/adopt-existing',
};
export interface ObjectRecord {
  metadata: { name: string; namespace?: string; uid: string; resourceVersion: string; generation?: number; deletionTimestamp?: string; annotations?: Record<string,string> };
  spec?: Record<string, any>; status?: Record<string, any>;
  [key: string]: any;
}
export function enabledNamespaces(list: ObjectRecord[]) {
  return list.filter(n => n.metadata.annotations?.[annotations.enabled] === 'true' && !n.metadata.deletionTimestamp);
}
export function compatible(crd: any) {
  const schema = crd.spec?.versions?.find((v: any) => v.name === 'v1alpha1' && v.served)?.schema?.openAPIV3Schema?.properties;
  return !!(schema?.spec?.properties?.change?.properties?.author && schema.spec.properties.change.properties.action && schema?.status?.properties?.approval?.properties?.request && schema?.status?.properties?.cleanup?.properties?.request);
}
export function changeInput(message: string, name: string, email: string, action?: 'Delete') {
  name=name.trim(); email=email.trim(); message=message.trim();
  if (!!name !== !!email) throw new Error('Provide both author name and email, or leave both blank.');
  const change: Record<string,any> = {};
  if (message) change.message=message;
  if (name) change.author={name,email};
  if (action) change.action=action;
  return Object.keys(change).length ? change : null;
}
export function guardedPatch(review: ObjectRecord, current: ObjectRecord, spec?: Record<string,any>, attrs?: Record<string,string|null>) {
  if (review.metadata.uid !== current.metadata.uid || JSON.stringify(review.spec) !== JSON.stringify(current.spec)) throw new Error('Stale request: the resource or proposal changed. Refresh and review again.');
  return { metadata: { uid: current.metadata.uid, resourceVersion: current.metadata.resourceVersion, ...(attrs ? {annotations:attrs} : {}) }, ...(spec ? {spec} : {}) };
}
export function approvalPatch(review: ObjectRecord, current: ObjectRecord, name='', email='') {
  const request=review.status?.approval?.request;
  if (!request || request !== current.status?.approval?.request || review.metadata.generation !== current.metadata.generation) throw new Error('Stale approval: review the current advertised request.');
  const identity=changeInput('',name,email)?.author;
  if (identity && (identity.name.length>128 || identity.email.length>254 || /[<>\r\n\x00-\x1f\x7f]/.test(identity.name) || !/^[^<>\s@]+@[^<>\s@]+$/.test(identity.email))) throw new Error('Provide a usable approver name and email without header characters.');
  return guardedPatch(review,current,undefined,{[annotations.approved]:request,[annotations.approvedBy]:identity?JSON.stringify({request,...identity}):null});
}
export class API {
  private timeout: number;
  constructor(timeout=8_000) { this.timeout=timeout; }
  async request(path: string, method='GET', body?: any) {
    const response=await fetch('/kube'+path,{method,headers:body ? {'Content-Type':method==='PATCH'?'application/merge-patch+json':'application/json'}:{},body:body ? JSON.stringify(body):undefined,signal:AbortSignal.timeout(this.timeout)});
    if (!response.ok) {
      let message=`Kubernetes returned ${response.status}`;
      try { message=(await response.json()).message || message; } catch { /* A proxy failure may be plain text. */ }
      throw new Error(`${response.status === 409 ? 'Conflict: refresh and review again. ' : ''}${message}`);
    }
    return response.json();
  }
  path(cr: ObjectRecord) { return `/apis/gitops.example.io/v1alpha1/namespaces/${cr.metadata.namespace}/gitresources/${cr.metadata.name}`; }
  async edit(review: ObjectRecord, spec?: Record<string,any>, attrs?: Record<string,string|null>) {
    const current=await this.request(this.path(review));
    if (current.metadata.deletionTimestamp && spec) throw new Error('Deletion has started; normal spec edits are disabled.');
    return this.request(this.path(review),'PATCH',guardedPatch(review,current,spec,attrs));
  }
  async approve(review: ObjectRecord, name='', email='') {
    const current=await this.request(this.path(review));
    return this.request(this.path(review),'PATCH',approvalPatch(review,current,name,email));
  }
  async policy(review: ObjectRecord, required: boolean) {
    const path='/api/v1/namespaces/'+review.metadata.name;
    const current=await this.request(path);
    if (current.metadata.uid!==review.metadata.uid || current.metadata.annotations?.[annotations.required]!==review.metadata.annotations?.[annotations.required]) throw new Error('Namespace policy changed; refresh and review again.');
    return this.request(path,'PATCH',{metadata:{uid:current.metadata.uid,resourceVersion:current.metadata.resourceVersion,annotations:{[annotations.required]:String(required)}}});
  }
}
