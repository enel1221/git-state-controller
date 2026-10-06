package manifest

import (
	"bytes"
	"encoding/json"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRender(t *testing.T) {
	input := []byte(`{"kind":"Unknown","apiVersion":"test.example/v1","metadata":{"name":"x","uid":"drop","resourceVersion":"2","generation":3,"creationTimestamp":"today","managedFields":[],"labels":{"authored":"yes"}},"status":{"ready":true},"spec":{"unknown":{"big":9007199254740993,"list":[1,true,"x"]}}}`)
	out, hash, err := Render(input)
	if err != nil {
		t.Fatal(err)
	}
	again, hash2, err := Render(input)
	if err != nil || !bytes.Equal(out, again) || hash != hash2 {
		t.Fatal("nondeterministic render")
	}
	b, err := yaml.YAMLToJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err = json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["status"]; ok {
		t.Fatal("status retained")
	}
	if bytes.Contains(obj["metadata"], []byte("uid")) {
		t.Fatal("bookkeeping retained")
	}
	if !bytes.Contains(obj["spec"], []byte("9007199254740993")) {
		t.Fatalf("unknown field lost precision: %s", b)
	}
	if !bytes.Contains(obj["metadata"], []byte("authored")) {
		t.Fatal("authored metadata dropped")
	}
}
func TestRejectMalformed(t *testing.T) {
	for _, s := range []string{`null`, `[]`, `"yaml"`, `{}`, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{}}`} {
		if _, _, err := Render([]byte(s)); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func TestManagedAnnotationsAndForeignUID(t *testing.T) {
	raw := []byte(`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"example","annotations":{"gitops.example.io/source-uid":"forged","other":"keep"}},"spec":{"largeInteger":9007199254740993},"status":{"drop":true}}`)
	content, hash, err := RenderManaged(raw, "demo", "wrapper", "real-uid", "managed")
	if err != nil {
		t.Fatal(err)
	}
	namespace, name, uid, state := Ownership(content)
	if namespace != "demo" || name != "wrapper" || uid != "real-uid" || state != "managed" || hash != Hash(content) {
		t.Fatal("reserved annotations not authoritative")
	}
	object, err := Decode(content)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(object["spec"], []byte("9007199254740993")) || object["status"] != nil {
		t.Fatal("authored spec changed or status persisted")
	}
	_, _, foreign, _ := Ownership([]byte(`{"metadata":{"annotations":{"gitops.example.io/source-uid":"foreign","bad-external-annotation":true}}}`))
	if foreign != "foreign" {
		t.Fatal("unrelated malformed annotation hid a foreign owner")
	}
}
