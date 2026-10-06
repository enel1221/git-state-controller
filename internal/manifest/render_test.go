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
