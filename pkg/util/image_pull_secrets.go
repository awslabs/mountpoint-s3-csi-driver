package util

import (
	"encoding/json"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ParseImagePullSecrets parses a JSON array of Secret names into Kubernetes references.
// Invalid input is logged and omitted so the controller can continue running.
func ParseImagePullSecrets(value string, log logr.Logger) []corev1.LocalObjectReference {
	if value == "" || value == "[]" || value == "null" {
		return nil
	}

	var names []string
	if err := json.Unmarshal([]byte(value), &names); err != nil {
		log.Error(err, "Failed to parse image pull secrets JSON, ignoring", "json", value)
		return nil
	}

	references := make([]corev1.LocalObjectReference, 0, len(names))
	for _, name := range names {
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			log.Info("Skipping invalid image pull secret name", "name", name, "errors", strings.Join(errs, "; "))
			continue
		}
		references = append(references, corev1.LocalObjectReference{Name: name})
	}
	return references
}
