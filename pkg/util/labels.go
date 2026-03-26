package util

import (
	"encoding/json"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ReservedLabelPrefix is the prefix reserved for driver-managed labels.
const ReservedLabelPrefix = "s3.csi.aws.com/"

// ParseLabels parses a JSON string into a map of labels and validates them.
// Returns an empty map if the input is empty, invalid JSON, or contains invalid labels.
func ParseLabels(labelsJSON string, log logr.Logger) map[string]string {
	if labelsJSON == "" || labelsJSON == "{}" || labelsJSON == "null" {
		return map[string]string{}
	}

	var labels map[string]string
	if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
		log.Error(err, "Failed to parse labels JSON, ignoring", "json", labelsJSON)
		return map[string]string{}
	}

	// Validate and filter out invalid labels
	validLabels := make(map[string]string)
	for key, value := range labels {
		if strings.HasPrefix(key, ReservedLabelPrefix) {
			log.Info("Skipping label with reserved prefix", "key", key, "prefix", ReservedLabelPrefix)
			continue
		}

		// Validate key and value
		if errs := validation.IsQualifiedName(key); len(errs) > 0 {
			log.Info("Skipping label with invalid key", "key", key, "errors", strings.Join(errs, "; "))
			continue
		}
		if errs := validation.IsValidLabelValue(value); len(errs) > 0 {
			log.Info("Skipping label with invalid value", "key", key, "value", value, "errors", strings.Join(errs, "; "))
			continue
		}

		validLabels[key] = value
	}

	return validLabels
}
