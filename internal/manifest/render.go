// Package manifest renders a single authored object without server bookkeeping.
package manifest

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"sigs.k8s.io/yaml"
)

func Render(raw []byte) ([]byte, string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, "", fmt.Errorf("manifest must be an object")
	}
	for _, key := range []string{"apiVersion", "kind"} {
		var value string
		if json.Unmarshal(object[key], &value) != nil || value == "" {
			return nil, "", fmt.Errorf("manifest requires %s", key)
		}
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(object["metadata"], &metadata) != nil || metadata == nil {
		return nil, "", fmt.Errorf("manifest requires metadata")
	}
	var name string
	if json.Unmarshal(metadata["name"], &name) != nil || name == "" {
		return nil, "", fmt.Errorf("manifest requires metadata.name")
	}
	delete(object, "status")
	for _, key := range []string{"uid", "resourceVersion", "generation", "managedFields", "creationTimestamp", "deletionTimestamp", "deletionGracePeriodSeconds", "selfLink"} {
		delete(metadata, key)
	}
	cleanMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, "", err
	}
	object["metadata"] = cleanMetadata
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, "", err
	}
	output, err := yaml.JSONToYAML(canonical)
	if err != nil {
		return nil, "", err
	}
	return output, fmt.Sprintf("sha256:%x", sha256.Sum256(output)), nil
}
