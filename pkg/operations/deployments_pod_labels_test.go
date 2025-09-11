package operations

import (
	"testing"
)

func TestPodHasRequiredLabels(t *testing.T) {
	tests := []struct {
		name           string
		podLabels      map[string]string
		requiredLabels []string
		expected       bool
	}{
		{
			name: "exact match",
			podLabels: map[string]string{
				"app":     "myapp",
				"version": "v1.0",
			},
			requiredLabels: []string{"app=myapp", "version=v1.0"},
			expected:       true,
		},
		{
			name: "partial match",
			podLabels: map[string]string{
				"app":     "myapp",
				"version": "v1.0",
			},
			requiredLabels: []string{"app=myapp"},
			expected:       true,
		},
		{
			name: "no match",
			podLabels: map[string]string{
				"app":     "myapp",
				"version": "v1.0",
			},
			requiredLabels: []string{"app=myapp", "version=v2.0"},
			expected:       false,
		},
		{
			name: "missing label",
			podLabels: map[string]string{
				"app": "myapp",
			},
			requiredLabels: []string{"app=myapp", "version=v1.0"},
			expected:       false,
		},
		{
			name: "empty required labels",
			podLabels: map[string]string{
				"app": "myapp",
			},
			requiredLabels: []string{},
			expected:       true,
		},
		{
			name: "istio canary example",
			podLabels: map[string]string{
				"app":          "myapp",
				"istio.io/rev": "canary",
			},
			requiredLabels: []string{"istio.io/rev=canary"},
			expected:       true,
		},
		{
			name: "istio default example",
			podLabels: map[string]string{
				"app":          "myapp",
				"istio.io/rev": "default",
			},
			requiredLabels: []string{"istio.io/rev=default"},
			expected:       true,
		},
		{
			name: "multiple labels match",
			podLabels: map[string]string{
				"app":          "myapp",
				"version":      "v1.0",
				"istio.io/rev": "canary",
				"env":          "production",
			},
			requiredLabels: []string{"app=myapp", "istio.io/rev=canary", "env=production"},
			expected:       true,
		},
		{
			name: "invalid label format",
			podLabels: map[string]string{
				"app": "myapp",
			},
			requiredLabels: []string{"invalid-format", "app=myapp"},
			expected:       true, // Should ignore invalid format and match valid ones
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock DeploymentOperations with required labels
			ops := &DeploymentOperations{
				podLabels: tt.requiredLabels,
			}

			result := ops.podHasRequiredLabels(tt.podLabels)
			if result != tt.expected {
				t.Errorf("podHasRequiredLabels() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestPodHasRequiredLabelsStatefulSet(t *testing.T) {
	tests := []struct {
		name           string
		podLabels      map[string]string
		requiredLabels []string
		expected       bool
	}{
		{
			name: "exact match for StatefulSet",
			podLabels: map[string]string{
				"app":     "mydb",
				"version": "v1.0",
			},
			requiredLabels: []string{"app=mydb", "version=v1.0"},
			expected:       true,
		},
		{
			name: "no match for StatefulSet",
			podLabels: map[string]string{
				"app":     "mydb",
				"version": "v1.0",
			},
			requiredLabels: []string{"app=mydb", "version=v2.0"},
			expected:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock StatefulSetOperations with required labels
			ops := &StatefulSetOperations{
				podLabels: tt.requiredLabels,
			}

			result := ops.podHasRequiredLabels(tt.podLabels)
			if result != tt.expected {
				t.Errorf("podHasRequiredLabels() = %v, want %v", result, tt.expected)
			}
		})
	}
}
