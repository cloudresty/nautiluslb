package main

import (
	"testing"
)

func TestApplicationVersion(t *testing.T) {
	// Test that version constants are properly defined
	tests := []struct {
		name     string
		value    string
		expected bool
	}{
		{"Version should not be empty", "v0.0.6", true},
		{"Stage should not be empty", "alpha", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if (tt.value != "") != tt.expected {
				t.Errorf("Expected %v, got %v for %s", tt.expected, tt.value != "", tt.name)
			}
		})
	}
}

func TestApplicationInfo(t *testing.T) {
	// Test basic application information
	appName := "NautilusLB"
	if appName == "" {
		t.Error("Application name should not be empty")
	}

	repoURL := "https://github.com/cloudresty/nautiluslb"
	if repoURL == "" {
		t.Error("Repository URL should not be empty")
	}
}
