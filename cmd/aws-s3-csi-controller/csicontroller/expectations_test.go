package csicontroller

import (
	"testing"

	"github.com/awslabs/aws-s3-csi-driver/pkg/util/testutil/assert"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestDeriveExpectationKeyFromFilters(t *testing.T) {
	tests := []struct {
		name         string
		fieldFilters client.MatchingFields
		want         string
	}{
		{
			name:         "empty filters",
			fieldFilters: client.MatchingFields{},
			want:         "",
		},
		{
			name: "single filter",
			fieldFilters: client.MatchingFields{
				"key1": "value1",
			},
			want: "key1=value1;",
		},
		{
			name: "multiple filters",
			fieldFilters: client.MatchingFields{
				"key2": "value2",
				"key1": "value1",
				"key3": "value3",
			},
			want: "key1=value1;key2=value2;key3=value3;",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deriveExpectationKeyFromFilters(tt.fieldFilters)
			assert.Equals(t, tt.want, got)
		})
	}
}

func TestExpectations(t *testing.T) {
	tests := []struct {
		name         string
		fieldFilters client.MatchingFields
		operations   func(*expectations)
		wantPending  bool
	}{
		{
			name: "set and check pending",
			fieldFilters: client.MatchingFields{
				"key1": "value1",
			},
			operations: func(e *expectations) {
				e.setPending(client.MatchingFields{"key1": "value1"})
			},
			wantPending: true,
		},
		{
			name: "set and clear pending",
			fieldFilters: client.MatchingFields{
				"key1": "value1",
			},
			operations: func(e *expectations) {
				e.setPending(client.MatchingFields{"key1": "value1"})
				e.clear(client.MatchingFields{"key1": "value1"})
			},
			wantPending: false,
		},
		{
			name: "check non-existent pending",
			fieldFilters: client.MatchingFields{
				"key1": "value1",
			},
			operations:  func(e *expectations) {},
			wantPending: false,
		},
		{
			name: "multiple operations",
			fieldFilters: client.MatchingFields{
				"key1": "value1",
				"key2": "value2",
			},
			operations: func(e *expectations) {
				e.setPending(client.MatchingFields{"key1": "value1", "key2": "value2"})
				e.clear(client.MatchingFields{"key1": "value1"}) // Different key, shouldn't affect the test
			},
			wantPending: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newExpectations()
			tt.operations(e)
			got := e.isPending(tt.fieldFilters)
			assert.Equals(t, tt.wantPending, got)
		})
	}
}
