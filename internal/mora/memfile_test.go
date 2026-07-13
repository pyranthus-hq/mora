package mora

import (
	"testing"
)

func TestContentHash(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: "14650fb0739d0383", // stable behavior of custom FNV-like hash
		},
		{
			name:     "hello world",
			input:    "hello world",
			expected: "e1d7a701437f78f9",
		},
		{
			name:     "mora",
			input:    "mora",
			expected: "a2b99a1a9e480182",
		},
		{
			name:     "newline character",
			input:    "test\nstring",
			expected: "d8488a1e7a73a4b2",
		},
		{
			name:     "longer string",
			input:    "longer string with some more content to hash",
			expected: "b9dea82c1af697e6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ContentHash(tt.input)
			if got != tt.expected {
				t.Errorf("ContentHash(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}
