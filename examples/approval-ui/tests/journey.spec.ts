import {test,expect} from '@playwright/test';
import {execFileSync} from 'node:child_process';
import {resolve} from 'node:path';
const root=resolve(import.meta.dirname,'../../..');
const base='/apis/gitops.example.io/v1alpha1';
const namespaces:string[]=[];
const k=(args:string[])=>execFileSync('kubectl',['--kubeconfig',resolve(root,'.dev/kubeconfig'),'--context','k3d-git-state-dev',...args],{encoding:'utf8',timeout:200_000});
test.afterAll(()=>{
 for(const ns of namespaces){
  k(['annotate','namespace',ns,'gitops.example.io/approval-required=false','--overwrite']);
  k(['delete','gitresources','--all','-n',ns,'--timeout=180s']);
  k(['delete','namespace',ns,'--timeout=60s']);
 }
});
test('real approval create/update/delete journey and automatic namespace smoke',async({page,request})=>{
 page.on('dialog',dialog=>void dialog.accept());await page.goto('/');
 await expect(page.getByText('v0.3 schema available',{exact:false})).toBeVisible();
 for(const required of [true,false]){
  const ns=`ui-e2e-${Date.now()}-${required?'review':'auto'}`;const name='example';namespaces.push(ns);
  await page.locator('#namespace-form input[name=namespace]').fill(ns);
  await page.locator('#namespace-form input[name=approval]').setChecked(required);
  await page.getByRole('button',{name:'Create namespace',exact:true}).click();
  await expect(page.locator(`#namespace-select option[value="${ns}"]`)).toBeAttached();await page.locator('#namespace-select').selectOption(ns);
  await page.getByRole('button',{name:'New resource',exact:true}).click();
  await page.locator('#resource-form input[name=name]').fill(name);
  const manifest={apiVersion:'v1',kind:'ConfigMap',metadata:{name},data:{greeting:'browser'}};
  await page.locator('#resource-form textarea[name=manifest]').fill(JSON.stringify(manifest));
  if(required){await page.locator('#resource-form input[name=authorName]').fill('Browser Editor');await page.locator('#resource-form input[name=authorEmail]').fill('editor@example.invalid');}
  await page.getByRole('button',{name:'Submit proposal',exact:true}).click();await expect(page.locator('#editor')).not.toBeVisible();
  const path=`/kube${base}/namespaces/${ns}/gitresources/${name}`;
  const get=async()=>{const response=await request.get(path);expect(response.ok()).toBeTruthy();return response.json();};
  const row=page.locator('tr').filter({hasText:`${ns} / ${name}`});
  const approve=async()=>{
   const reviewedRequest=await row.getAttribute('data-request');
   await row.getByRole('button',{name:'Approve',exact:true}).click();await expect(page.locator('#approver-name')).toHaveValue('');await expect(page.locator('#approver-email')).toHaveValue('');
   await page.locator('#approver-name').fill('Browser Reviewer');await page.locator('#approver-email').fill('reviewer@example.invalid');
   const submitted=page.waitForRequest(r=>r.method()==='PATCH'&&r.url().endsWith(path));
   await page.getByRole('button',{name:'Approve reviewed request',exact:true}).click();
   const patch=(await submitted).postDataJSON();
   expect(patch.metadata.uid).toBeTruthy();expect(patch.metadata.resourceVersion).toBeTruthy();
   expect(patch.metadata.annotations['gitops.example.io/approved-request']).toBe(reviewedRequest);
   expect(JSON.parse(patch.metadata.annotations['gitops.example.io/approved-by'])).toEqual({request:reviewedRequest,name:'Browser Reviewer',email:'reviewer@example.invalid'});
   await expect(page.locator('#review')).not.toBeVisible();
  };
  const ready=async()=>{await expect.poll(async()=>{const cr=await get();const c=cr.status?.conditions?.find((c:any)=>c.type==='Ready');return c?.status==='True'&&c.observedGeneration===cr.metadata.generation;},{timeout:120_000}).toBe(true);const current=await get();await page.locator('#refresh').click();await expect(row).toHaveAttribute('data-generation',String(current.metadata.generation));await expect(row).toHaveAttribute('data-request','');};
  if(required){await expect(row).toContainText('ApprovalPending');expect((await get()).status.lastPublishedRevision).toBeUndefined();await approve();}
  await ready();let cr=await get();expect(cr.spec.change).toBeUndefined();expect(cr.metadata.annotations?.['gitops.example.io/approved-request']).toBeUndefined();expect(cr.metadata.annotations?.['gitops.example.io/approved-by']).toBeUndefined();
  if(required){
   await row.getByRole('button',{name:'Edit',exact:true}).click();await expect(page.locator('#resource-form input[name=authorName]')).toHaveValue('');await expect(page.locator('#resource-form input[name=message]')).toHaveValue('');await expect(page.locator('#destination')).not.toBeVisible();
   manifest.data.greeting='updated';await page.locator('#resource-form textarea[name=manifest]').fill(JSON.stringify(manifest));await page.getByRole('button',{name:'Submit proposal',exact:true}).click();await expect(page.locator('#editor')).not.toBeVisible();
   await expect(row).toContainText('ApprovalPending');const proposal=await get();expect(proposal.status.lastPublishedRevision).toBe(cr.status.lastPublishedRevision);
   // Leave a reviewed dialog open while another client changes the intent.
   await row.getByRole('button',{name:'Approve',exact:true}).click();
   cr=await get();manifest.data.greeting='newer';const response=await request.patch(path,{data:{metadata:{uid:cr.metadata.uid,resourceVersion:cr.metadata.resourceVersion},spec:{manifest}},headers:{'Content-Type':'application/merge-patch+json'}});expect(response.ok()).toBeTruthy();
   await page.getByRole('button',{name:'Approve reviewed request',exact:true}).click();await expect(page.locator('#review-error')).toContainText('Stale');await page.locator('#cancel-review').click();
   await expect.poll(async()=>{const now=await get();return now.status?.approval?.generation===now.metadata.generation;}).toBe(true);const latest=await get();await page.locator('#refresh').click();await expect(row).toHaveAttribute('data-request',latest.status.approval.request);await approve();await ready();
  }
  await row.getByRole('button',{name:'Request deletion',exact:true}).click();await page.getByRole('button',{name:'Submit proposal',exact:true}).click();await expect(page.locator('#editor')).not.toBeVisible();
  if(required){await expect(row).toContainText('ApprovalPending');expect((await get()).metadata.deletionTimestamp).toBeUndefined();await approve();}
  await expect.poll(async()=>(await request.get(path)).status(),{timeout:120_000}).toBe(404);
 }
 // Proxy never permits source secrets, target operations or foreign origins.
 expect((await request.get('/kube/api/v1/namespaces/git-state-system/secrets')).status()).toBe(403);
 expect((await request.get('/kube/api/v1/namespaces',{headers:{Origin:'http://evil.invalid'}})).status()).toBe(403);
 expect((await request.get('/@fs'+resolve(root,'.dev/kubeconfig'))).status()).toBe(403);
});
