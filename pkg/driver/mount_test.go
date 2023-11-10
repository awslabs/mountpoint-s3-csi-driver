package driver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestUserAgentPrefix(t *testing.T) {
	testCases := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "success: add user agent prefix to mount call",
			input:    []string{"--read-only"},
			expected: []string{"--read-only", userAgentPrefix + " " + csiDriverPrefix + GetVersion().DriverVersion},
		},
		{
			name:     "success: replacing customer user agent prefix",
			input:    []string{"--read-only", "--user-agent-prefix testing"},
			expected: []string{"--read-only", userAgentPrefix + " " + csiDriverPrefix + GetVersion().DriverVersion},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, addUserAgentToOptions(tc.input), tc.expected)
		})
	}
}
