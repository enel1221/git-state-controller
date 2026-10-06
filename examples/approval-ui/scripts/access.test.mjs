import test from 'node:test';
import assert from 'node:assert/strict';
import {permitted,sameOrigin} from './access.mjs';
test('API allowlist excludes secrets, status, target changes and namespace deletion',()=>{
 for(const [path,method,want] of [
 ['/api/v1/namespaces','POST',true],['/api/v1/namespaces/demo','PATCH',true],['/api/v1/namespaces/demo','DELETE',false],
 ['/api/v1/namespaces/demo/secrets','GET',false],['/api/v1/namespaces/demo/configmaps/x','PATCH',false],
 ['/apis/gitops.example.io/v1alpha1/namespaces/demo/gitresources/x','PATCH',true],
 ['/apis/gitops.example.io/v1alpha1/namespaces/demo/gitresources/x/status','PATCH',false],
 ['/apis/gitops.example.io/v1alpha1/namespaces/demo/gitresources/x/proxy','GET',false],
 ['/apis/apiextensions.k8s.io/v1/customresourcedefinitions/gitresources.gitops.example.io','GET',true],
 ['/apis/apiextensions.k8s.io/v1/customresourcedefinitions/others','GET',false],
 ])assert.equal(permitted(path,method),want,`${method} ${path}`);
});
test('host and same-origin checks guard browser reads and mutations',()=>{
 const origin='http://127.0.0.1:5173',host='127.0.0.1:5173';
 assert.equal(sameOrigin({host},'GET',origin),true);
 assert.equal(sameOrigin({host},'PATCH',origin),false);
 assert.equal(sameOrigin({host,origin,'sec-fetch-site':'same-origin'},'PATCH',origin),true);
 assert.equal(sameOrigin({host,origin:'http://evil.invalid','sec-fetch-site':'cross-site'},'GET',origin),false);
 assert.equal(sameOrigin({host:'evil.invalid',origin},'GET',origin),false);
});
