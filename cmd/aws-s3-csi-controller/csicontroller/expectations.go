package csicontroller

import (
	"sort"
	"strings"
	"sync"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Expectations is a structure that manages pending expectations for Kubernetes resources.
// It uses field filters as keys to track resources that are expected to be created
// helping to reduce unnecessary processing and API server load.
type Expectations struct {
	pending sync.Map
}

// NewExpectations creates and returns a new Expectations instance.
func NewExpectations() *Expectations {
	return &Expectations{}
}

// SetPending marks a resource as pending based on the given field filters.
// This is typically used when a create operation is initiated.
func (e *Expectations) SetPending(fieldFilters client.MatchingFields) {
	key := deriveExpectationKeyFromFilters(fieldFilters)
	e.pending.Store(key, struct{}{})
}

// IsPending checks if a resource is marked as pending based on the given field filters.
// Returns true if the resource is pending, false otherwise.
func (e *Expectations) IsPending(fieldFilters client.MatchingFields) bool {
	key := deriveExpectationKeyFromFilters(fieldFilters)
	_, ok := e.pending.Load(key)
	return ok
}

// Clear removes the pending mark for a resource based on the given field filters.
// This is typically called when an expected operation has been confirmed as completed.
func (e *Expectations) Clear(fieldFilters client.MatchingFields) {
	key := deriveExpectationKeyFromFilters(fieldFilters)
	e.pending.Delete(key)
}

// deriveExpectationKeyFromFilters generates a deterministic string key from a map of field filters.
// It creates a consistent string representation of the filters by:
// 1. Sorting the filter keys alphabetically
// 2. Concatenating each key-value pair in the format "key=value;"
//
// For example, given filters {"foo": "bar", "baz": "qux"}, it will produce "baz=qux;foo=bar;"
//
// Parameters:
//   - fieldFilters: A map of field names to their values used for filtering Kubernetes resources
//
// Returns:
//   - A string that uniquely represents the combination of field filters
func deriveExpectationKeyFromFilters(fieldFilters client.MatchingFields) string {
	keys := make([]string, 0, len(fieldFilters))
	for k := range fieldFilters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(fieldFilters[k])
		sb.WriteString(";")
	}
	return sb.String()
}
