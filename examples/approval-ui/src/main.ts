import { API, annotations, enabledNamespaces, compatible, changeInput } from './client.ts';
import type { ObjectRecord } from './client.ts';
import './style.css';
const api=new API();
const app=document.querySelector<HTMLDivElement>('#app')!;
app.innerHTML=`
<header><span class="mark">GS</span><div><h1>Git State</h1><p>Local approval workspace</p></div><span class="local">LOCAL · k3d-git-state-dev</span></header>
<main><p id="compatibility">Checking v0.3 API compatibility…</p><p id="notice" role="status"></p>
<section><div class="section-title"><div><h2>Namespaces</h2><p>Enabled namespaces accept new requests here.</p></div><button id="refresh">Refresh</button></div>
<form id="namespace-form"><label>New namespace<input name="namespace" required pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?" maxlength="63"></label><label class="check"><input name="approval" type="checkbox">Require approval</label><button data-mutation>Create namespace</button></form>
<div class="toolbar"><label>Namespace<select id="namespace-select"></select></label><span id="policy"></span><button data-mutation id="toggle-policy">Toggle approval policy</button><button data-mutation id="create">New resource</button></div></section>
<section><div class="section-title"><div><h2>Resources</h2><p>Proposals and the native status of the running deployment.</p></div><span id="count"></span></div>
<div class="table-wrap"><table><thead><tr><th>Namespace / name</th><th>Approved</th><th>Published</th><th>Argo sync / health</th><th>Ready reason</th><th>GitDrift</th><th>Actions</th></tr></thead><tbody id="resources"></tbody></table></div></section>
<section id="details" hidden><h2>Resource details</h2><pre id="detail-json"></pre></section>
<dialog id="editor"><form id="resource-form"><h2 id="editor-title"></h2><p id="editor-help"></p>
<div class="grid"><label>Resource name<input name="name" required pattern="[a-z0-9]([-a-z0-9.]*[a-z0-9])?"></label><label>Deletion policy<select name="policy"><option>Delete</option><option>Orphan</option></select></label></div>
<div id="destination" class="grid"><label>Repository URL<input name="repo" value="http://forgejo.forgejo.svc.cluster.local:3000/demo/resources.git"></label><label>Branch<input name="branch" value="main"></label><label>File path<input name="path"></label></div>
<label id="manifest-label">Manifest JSON<textarea name="manifest" rows="12" spellcheck="false"></textarea></label>
<div class="grid"><label>Author name (optional)<input name="authorName" maxlength="128"></label><label>Author email (optional)<input name="authorEmail" type="email" maxlength="254"></label></div><label>Message (optional)<input name="message" maxlength="4096"></label>
<p class="hint">Blank attribution uses the configured bot. These inputs apply once.</p><p id="form-error" role="alert"></p><div class="dialog-actions"><button type="button" id="cancel-editor">Cancel</button><button data-mutation id="submit-resource">Submit proposal</button></div></form></dialog>
<dialog id="review"><h2>Review approval</h2><p>This approves only the advertised request shown below.</p><pre id="review-json"></pre><div class="grid"><label>Approver name (optional)<input id="approver-name" maxlength="128"></label><label>Approver email (optional)<input id="approver-email" type="email" maxlength="254"></label></div><p class="hint">Recorded in the Git footer as supplied attribution; identity is not verified.</p><p id="review-error" role="alert"></p><div class="dialog-actions"><button id="cancel-review">Cancel</button><button data-mutation id="confirm-approval">Approve reviewed request</button></div></dialog>
</main><footer>Trusted local demonstration. Attribution is not authentication; approval annotations are coordination evidence.</footer>`;
const el=<T extends HTMLElement=HTMLElement>(id:string)=>document.getElementById(id) as T;
let namespaces:ObjectRecord[]=[],resources:ObjectRecord[]=[],canMutate=false,refreshing=false,stopped=false;
let editing:ObjectRecord|undefined,mode:'create'|'edit'|'delete'='create',reviewing:ObjectRecord|undefined,selected:string|undefined;
const form=el<HTMLFormElement>('resource-form');
const input=(name:string)=>form.elements.namedItem(name) as HTMLInputElement;
function notice(message:string,error=false){el('notice').textContent=message;el('notice').className=error?'error':'';}
async function run(action:()=>Promise<any>){try{await action();notice('Request saved. The controller will report its progress.');await refresh();}catch(e){notice((e as Error).message,true);}}
function condition(cr:ObjectRecord,name:string){return cr.status?.conditions?.find((c:any)=>c.type===name);}
function mutations(){document.querySelectorAll<HTMLButtonElement>('[data-mutation]').forEach(b=>b.disabled=!canMutate);}
function render(){
 const select=el<HTMLSelectElement>('namespace-select'),previous=select.value;
 const enabled=enabledNamespaces(namespaces);
 const names=enabled.map(n=>n.metadata.name);
 if (Array.from(select.options).map(o=>o.value).join()!==names.join()) {select.replaceChildren(...names.map(name=>{const o=document.createElement('option');o.value=name;o.textContent=name;return o;}));if(names.includes(previous))select.value=previous;}
 const ns=enabled.find(n=>n.metadata.name===select.value);
 el('policy').textContent=ns?`Approval policy: ${ns.metadata.annotations?.[annotations.required] ?? 'false'}`:'No enabled namespaces';
 el<HTMLButtonElement>('create').disabled=!canMutate||!ns;
 el<HTMLButtonElement>('toggle-policy').disabled=!canMutate||!ns;
 const tbody=el('resources');tbody.replaceChildren();el('count').textContent=`${resources.length} resources`;
 for(const cr of resources){
  const tr=document.createElement('tr');tr.dataset.resource=cr.metadata.name;tr.dataset.generation=String(cr.metadata.generation);tr.dataset.request=cr.status?.approval?.request??'';
  const cells=[`${cr.metadata.namespace} / ${cr.metadata.name}${cr.metadata.deletionTimestamp?' · deleting':''}${cr.metadata.annotations?.[annotations.paused]==='true'?' · paused':''}`,condition(cr,'Approved')?.reason??'Pending',condition(cr,'Published')?.status??'Pending',`${cr.status?.argoCD?.status?.sync?.status??'—'} / ${cr.status?.argoCD?.status?.health?.status??'—'}`,condition(cr,'Ready')?.reason??'Pending',condition(cr,'GitDrift')?.reason??'—'];
  for(const value of cells){const td=document.createElement('td');td.textContent=value;tr.append(td);}
  const actions=document.createElement('td');actions.className='actions';
  const button=(label:string,action:()=>void,mutation=false,disabled=false)=>{const b=document.createElement('button');b.textContent=label;b.disabled=disabled||mutation&&!canMutate;b.onclick=action;actions.append(b);};
  button('Details',()=>{selected=cr.metadata.uid;el('details').hidden=false;el('detail-json').textContent=JSON.stringify(cr,null,2);});
  button('Edit',()=>openEditor('edit',cr),true,!!cr.metadata.deletionTimestamp);
  if(cr.status?.approval?.request && condition(cr,'Approved')?.status!=='True')button('Approve',()=>{reviewing=structuredClone(cr);el<HTMLInputElement>('approver-name').value='';el<HTMLInputElement>('approver-email').value='';el('review-json').textContent=JSON.stringify(reviewing,null,2);el('review-error').textContent='';el<HTMLDialogElement>('review').showModal();},true);
  button(cr.metadata.annotations?.[annotations.paused]==='true'?'Resume':'Pause',()=>void run(()=>api.edit(cr,undefined,{[annotations.paused]:cr.metadata.annotations?.[annotations.paused]==='true'?null:'true'})),true);
  if(!cr.metadata.deletionTimestamp){
   button('Request deletion',()=>openEditor('delete',cr),true);
   if(cr.spec?.change?.action==='Delete')button('Cancel deletion',()=>{if(confirm('Cancel before Kubernetes accepts deletion? Approval can start deletion immediately.'))void run(()=>api.edit(cr,{change:null}));},true);
   button('Adopt',()=>{if(confirm('Request explicit adoption? Namespace approval still applies.'))void run(()=>api.edit(cr,undefined,{[annotations.adopt]:'true'}));},true);
  }
  tr.append(actions);tbody.append(tr);
 }
 if(selected){const cr=resources.find(c=>c.metadata.uid===selected);el('detail-json').textContent=cr?JSON.stringify(cr,null,2):'The wrapper has been deleted. There is no retained CR audit record.';}
}
async function refresh(){
 if(refreshing||stopped)return;refreshing=true;
 try{
  const [ns,cr]=await Promise.all([api.request('/api/v1/namespaces'),api.request('/apis/gitops.example.io/v1alpha1/gitresources')]);
  namespaces=ns.items;resources=cr.items;render();
 }catch(e){notice((e as Error).message,true);}finally{refreshing=false;}
}
function openEditor(nextMode:typeof mode,cr?:ObjectRecord){
 mode=nextMode;editing=cr?structuredClone(cr):undefined;form.reset();el('form-error').textContent='';
 el('editor-title').textContent=mode==='create'?'Create GitResource':mode==='edit'?'Edit proposal':'Request deletion';
 el('editor-help').textContent=mode==='delete'?'The controller handles approval and Kubernetes deletion. An accepted deletion cannot be canceled.':'Submit the manifest and fresh one-shot metadata together.';
 input('name').value=cr?.metadata.name??'';input('name').disabled=mode!=='create';input('policy').value=cr?.spec?.deletionPolicy??'Delete';
 el('destination').hidden=mode!=='create';el('manifest-label').hidden=mode==='delete';
 input('manifest').value=JSON.stringify(cr?.spec?.manifest??{apiVersion:'v1',kind:'ConfigMap',metadata:{name:'example'},data:{greeting:'hello'}},null,2);
 input('path').value='';el<HTMLDialogElement>('editor').showModal();
}
el('create').onclick=()=>openEditor('create');el('refresh').onclick=()=>void refresh();el<HTMLSelectElement>('namespace-select').onchange=()=>render();
el('cancel-editor').onclick=()=>el<HTMLDialogElement>('editor').close();el('cancel-review').onclick=()=>el<HTMLDialogElement>('review').close();
form.onsubmit=async e=>{
 e.preventDefault();if(!canMutate)return;
 try{
  const change=changeInput(input('message').value,input('authorName').value,input('authorEmail').value,mode==='delete'?'Delete':undefined);
  const spec:Record<string,any>={change,deletionPolicy:input('policy').value};
  if(mode!=='delete'){spec.manifest=JSON.parse(input('manifest').value);if(!spec.manifest||Array.isArray(spec.manifest)||typeof spec.manifest!=='object')throw new Error('Manifest must be one JSON object.');}
  if(mode==='create'){
   const namespace=el<HTMLSelectElement>('namespace-select').value;
   if(!enabledNamespaces(namespaces).some(n=>n.metadata.name===namespace))throw new Error('Select an enabled namespace.');
   spec.repository={url:input('repo').value,branch:input('branch').value,path:input('path').value||`${namespace}/${input('name').value}.yaml`};if(!change)delete spec.change;
   await api.request(`/apis/gitops.example.io/v1alpha1/namespaces/${namespace}/gitresources`,'POST',{apiVersion:'gitops.example.io/v1alpha1',kind:'GitResource',metadata:{namespace,name:input('name').value},spec});
  }else await api.edit(editing!,spec);
  el<HTMLDialogElement>('editor').close();notice('Proposal submitted.');await refresh();
 }catch(e){el('form-error').textContent=(e as Error).message;}
};
el('confirm-approval').onclick=async()=>{try{if(!canMutate)return;await api.approve(reviewing!,el<HTMLInputElement>('approver-name').value,el<HTMLInputElement>('approver-email').value);el<HTMLDialogElement>('review').close();notice('Reviewed request approved.');await refresh();}catch(e){el('review-error').textContent=(e as Error).message;}};
el<HTMLFormElement>('namespace-form').onsubmit=e=>{e.preventDefault();if(!canMutate)return;const f=e.currentTarget as HTMLFormElement;const values=new FormData(f);void run(async()=>{await api.request('/api/v1/namespaces','POST',{apiVersion:'v1',kind:'Namespace',metadata:{name:values.get('namespace'),annotations:{[annotations.enabled]:'true',[annotations.required]:String(values.has('approval'))}}});f.reset();});};
el('toggle-policy').onclick=()=>{const ns=namespaces.find(n=>n.metadata.name===el<HTMLSelectElement>('namespace-select').value);if(ns&&confirm('Changing policy affects pending work. Disabling approval releases pending requests.'))void run(()=>api.policy(ns,ns.metadata.annotations?.[annotations.required]!=='true'));};
mutations();
try{canMutate=compatible(await api.request('/apis/apiextensions.k8s.io/v1/customresourcedefinitions/gitresources.gitops.example.io'));el('compatibility').textContent=canMutate?'v0.3 schema available · install the matching controller to enforce approvals.':'Mutations disabled: install the v0.3 CRD and matching controller.';}catch(e){el('compatibility').textContent=`Mutations disabled: ${(e as Error).message}`;}
mutations();await refresh();
const timer=setInterval(()=>{if(!document.hidden)void refresh();},3_000);
document.addEventListener('visibilitychange',()=>{if(!document.hidden)void refresh();});
window.addEventListener('pagehide',()=>{stopped=true;clearInterval(timer);});
