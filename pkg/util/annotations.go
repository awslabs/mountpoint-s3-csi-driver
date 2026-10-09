package util

import (
	"encoding/json"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ParseAnnotations parses a JSON string into a map of annotations and validates their keys.
// Returns an empty map if the input is empty or invalid JSON.
func ParseAnnotations(annotationsJSON string, log logr.Logger) map[string]string {
	if annotationsJSON == "" || annotationsJSON == "{}" || annotationsJSON == "null" {
		return map[string]string{}
	}

	var annotations map[string]string
	if err := json.Unmarshal([]byte(annotationsJSON), &annotations); err != nil {
		log.Error(err, "Failed to parse annotations JSON, ignoring", "json", annotationsJSON)
		return map[string]string{}
	}

	validAnnotations := make(map[string]string)
	for key, value := range annotations {
		if strings.HasPrefix(key, ReservedLabelPrefix) {
			log.Info("Skipping annotation with reserved prefix", "key", key, "prefix", ReservedLabelPrefix)
			continue
		}

		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			log.Info("Skipping annotation with invalid key", "key", key, "errors", strings.Join(errs, "; "))
			continue
		}

		validAnnotations[key] = value
	}

	return validAnnotations
}
