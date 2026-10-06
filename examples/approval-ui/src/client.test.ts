import test from 'node:test';
import assert from 'node:assert/strict';
import {API,annotations,enabledNamespaces,compatible,changeInput,guardedPatch,approvalPatch} from './client.ts';
import type {ObjectRecord} from './client.ts';
const object=(name='one'):ObjectRecord=>({metadata:{name,namespace:'demo',uid:'uid',resourceVersion:'1',generation:1},spec:{manifest:{value:1}},status:{approval:{request:'opaque-reviewed-request'}}});
test('enabled namespaces are literal and nonterminating; optional attribution starts empty',()=>{
 const yes=object('yes');yes.metadata.annotations={[annotations.enabled]:'true'};
 const no=object('no');no.metadata.annotations={[annotations.enabled]:'TRUE'};
 const terminating=structuredClone(yes);terminating.metadata.deletionTimestamp='now';
 assert.deepEqual(enabledNamespaces([yes,no,terminating]).map(n=>n.metadata.name),['yes']);
 assert.equal(changeInput('','',''),null);
 assert.deepEqual(changeInput('','','','Delete'),{action:'Delete'});
 assert.throws(()=>changeInput('','Name',''),/both/);
});
test('guarded edits tolerate status-only versions and preserve request-bound review',()=>{
 const review=object(),current=structuredClone(review);current.metadata.resourceVersion='2';current.status!.extra='status change';
 assert.equal(approvalPatch(review,current).metadata.resourceVersion,'2');
 assert.deepEqual(approvalPatch(review,current).metadata.annotations,{[annotations.approved]:'opaque-reviewed-request',[annotations.approvedBy]:null});
 const replacement=structuredClone(current);replacement.metadata.uid='new';assert.throws(()=>guardedPatch(review,replacement),/Stale/);
 current.spec!.manifest={value:2};assert.throws(()=>guardedPatch(review,current),/Stale/);
 current.spec=review.spec;current.status!.approval.request='new-request';assert.throws(()=>approvalPatch(review,current),/Stale/);
});
test('v0.2 schema cannot enable mutations',()=>{
 assert.equal(compatible({spec:{versions:[{name:'v1alpha1',served:true,schema:{openAPIV3Schema:{properties:{spec:{properties:{change:{properties:{message:{type:'string'}}}}}}}}}]}}),false);
});
test('API sends conditional patches, does not retry stale approval and bounds requests',async t=>{
 const original=globalThis.fetch;t.after(()=>globalThis.fetch=original);
 const reviewed=object();const bodies:any[]=[];
 globalThis.fetch=async(_input,init)=>{if(init?.method==='PATCH')bodies.push(JSON.parse(String(init.body)));return new Response(JSON.stringify(reviewed),{status:200});};
 await new API().approve(reviewed);assert.equal(bodies[0].metadata.uid,'uid');assert.equal(bodies[0].metadata.resourceVersion,'1');
 globalThis.fetch=async()=>new Response(JSON.stringify({...reviewed,status:{approval:{request:'new'}}}),{status:200});
 await assert.rejects(new API().approve(reviewed),/Stale/);assert.equal(bodies.length,1);
 globalThis.fetch=async(_input,init)=>new Promise((_resolve,reject)=>init!.signal!.addEventListener('abort',()=>reject(init!.signal!.reason)));
 // AbortSignal.timeout's timer is unref'd; this timer keeps the isolated test alive.
 const keepAlive=setTimeout(()=>{},50);await assert.rejects(new API(5).request('/api/v1/namespaces'));clearTimeout(keepAlive);
});

test('approver attribution is optional, validated and bound to the reviewed request',()=>{
 const review=object();
 const attrs=approvalPatch(review,review,'Jamie','jamie@example.com').metadata.annotations!;
 assert.deepEqual(JSON.parse(attrs[annotations.approvedBy]!),{request:'opaque-reviewed-request',name:'Jamie',email:'jamie@example.com'});
 assert.throws(()=>approvalPatch(review,review,'Jamie',''),/both/);
 assert.throws(()=>approvalPatch(review,review,'Jamie\nInjected','jamie@example.com'),/usable/);
 assert.throws(()=>approvalPatch(review,review,'Jamie','bad'),/usable/);
 const newer=structuredClone(review);newer.metadata.generation=2;assert.throws(()=>approvalPatch(review,newer,'Jamie','jamie@example.com'),/Stale/);
});
