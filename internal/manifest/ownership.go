package manifest

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/yaml"
)

const StateAnnotation = "gitops.example.io/management-state"
const NamespaceAnnotation = "gitops.example.io/source-namespace"
const NameAnnotation = "gitops.example.io/source-name"
const UIDAnnotation = "gitops.example.io/source-uid"

func Hash(content []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(content)) }

// RenderManaged overwrites reserved metadata without touching the authored spec.
func RenderManaged(raw []byte, namespace, name, uid, state string) ([]byte, string, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, "", err
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(obj["metadata"], &metadata); err != nil || metadata == nil {
		return nil, "", fmt.Errorf("manifest requires metadata")
	}
	annotations := map[string]json.RawMessage{}
	if v, ok := metadata["annotations"]; ok {
		if err := json.Unmarshal(v, &annotations); err != nil || annotations == nil {
			return nil, "", fmt.Errorf("annotations must be an object")
		}
	}
	for k, v := range map[string]string{StateAnnotation: state, NamespaceAnnotation: namespace, NameAnnotation: name, UIDAnnotation: uid} {
		annotations[k], _ = json.Marshal(v)
	}
	metadata["annotations"], _ = json.Marshal(annotations)
	obj["metadata"], _ = json.Marshal(metadata)
	updated, err := json.Marshal(obj)
	if err != nil {
		return nil, "", err
	}
	return Render(updated)
}
func Decode(content []byte) (map[string]json.RawMessage, error) {
	raw, err := yaml.YAMLToJSON(content)
	if err != nil {
		return nil, err
	}
	var o map[string]json.RawMessage
	err = json.Unmarshal(raw, &o)
	return o, err
}
func Ownership(content []byte) (namespace, name, uid, state string) {
	o, err := Decode(content)
	if err != nil {
		return
	}
	var m struct {
		Annotations map[string]json.RawMessage `json:"annotations"`
	}
	if json.Unmarshal(o["metadata"], &m) != nil {
		return
	}
	_ = json.Unmarshal(m.Annotations[NamespaceAnnotation], &namespace)
	_ = json.Unmarshal(m.Annotations[NameAnnotation], &name)
	_ = json.Unmarshal(m.Annotations[UIDAnnotation], &uid)
	_ = json.Unmarshal(m.Annotations[StateAnnotation], &state)
	return
}
func Orphan(content []byte, namespace, name, uid string) ([]byte, string, error) {
	raw, err := yaml.YAMLToJSON(content)
	if err != nil {
		return nil, "", err
	}
	return RenderManaged(raw, namespace, name, uid, "orphaned")
}
