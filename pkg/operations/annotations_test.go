package operations

import (
	"testing"
)

func TestPodHasRequiredAnnotations(t *testing.T) {
	tests := []struct {
		name                string
		podAnnotations      map[string]string
		requiredAnnotations []string
		expected            bool
	}{
		{
			name: "exact match",
			podAnnotations: map[string]string{
				"app":     "myapp",
				"version": "v1.0",
			},
			requiredAnnotations: []string{"app=myapp", "version=v1.0"},
			expected:            true,
		},
		{
			name: "partial match",
			podAnnotations: map[string]string{
				"app":     "myapp",
				"version": "v1.0",
			},
			requiredAnnotations: []string{"app=myapp"},
			expected:            true,
		},
		{
			name: "no match",
			podAnnotations: map[string]string{
				"app":     "myapp",
				"version": "v1.0",
			},
			requiredAnnotations: []string{"app=myapp", "version=v2.0"},
			expected:            false,
		},
		{
			name: "missing annotation",
			podAnnotations: map[string]string{
				"app": "myapp",
			},
			requiredAnnotations: []string{"app=myapp", "version=v1.0"},
			expected:            false,
		},
		{
			name: "empty required annotations",
			podAnnotations: map[string]string{
				"app": "myapp",
			},
			requiredAnnotations: []string{},
			expected:            true,
		},
		{
			name: "istio canary example",
			podAnnotations: map[string]string{
				"app":          "myapp",
				"istio.io/rev": "canary",
			},
			requiredAnnotations: []string{"istio.io/rev=canary"},
			expected:            true,
		},
		{
			name: "istio default example",
			podAnnotations: map[string]string{
				"app":          "myapp",
				"istio.io/rev": "default",
			},
			requiredAnnotations: []string{"istio.io/rev=default"},
			expected:            true,
		},
		{
			name: "multiple annotations match",
			podAnnotations: map[string]string{
				"app":          "myapp",
				"version":      "v1.0",
				"istio.io/rev": "canary",
				"env":          "production",
			},
			requiredAnnotations: []string{"app=myapp", "istio.io/rev=canary", "env=production"},
			expected:            true,
		},
		{
			name: "invalid annotation format",
			podAnnotations: map[string]string{
				"app": "myapp",
			},
			requiredAnnotations: []string{"invalid-format", "app=myapp"},
			expected:            true, // Should ignore invalid format and match valid ones
		},
		{
			name: "kubernetes annotations example",
			podAnnotations: map[string]string{
				"kubernetes.io/created-by":          `{"kind":"SerializedReference","apiVersion":"v1","reference":{"kind":"ReplicaSet","namespace":"default","name":"myapp-123","uid":"abc123"}}`,
				"deployment.kubernetes.io/revision": "1",
			},
			requiredAnnotations: []string{"deployment.kubernetes.io/revision=1"},
			expected:            true,
		},
		{
			name: "prometheus annotations example",
			podAnnotations: map[string]string{
				"prometheus.io/scrape": "true",
				"prometheus.io/port":   "8080",
				"prometheus.io/path":   "/metrics",
			},
			requiredAnnotations: []string{"prometheus.io/scrape=true", "prometheus.io/port=8080"},
			expected:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock DeploymentOperations with required annotations
			ops := &DeploymentOperations{
				podAnnotations: tt.requiredAnnotations,
			}

			result := ops.podHasRequiredAnnotations(tt.podAnnotations)
			if result != tt.expected {
				t.Errorf("podHasRequiredAnnotations() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestPodHasRequiredAnnotationsStatefulSet(t *testing.T) {
	tests := []struct {
		name                string
		podAnnotations      map[string]string
		requiredAnnotations []string
		expected            bool
	}{
		{
			name: "exact match for StatefulSet",
			podAnnotations: map[string]string{
				"app":     "mydb",
				"version": "v1.0",
			},
			requiredAnnotations: []string{"app=mydb", "version=v1.0"},
			expected:            true,
		},
		{
			name: "no match for StatefulSet",
			podAnnotations: map[string]string{
				"app":     "mydb",
				"version": "v1.0",
			},
			requiredAnnotations: []string{"app=mydb", "version=v2.0"},
			expected:            false,
		},
		{
			name: "statefulset specific annotations",
			podAnnotations: map[string]string{
				"statefulset.kubernetes.io/pod-name": "mydb-0",
				"app":                                "mydb",
			},
			requiredAnnotations: []string{"statefulset.kubernetes.io/pod-name=mydb-0"},
			expected:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock StatefulSetOperations with required annotations
			ops := &StatefulSetOperations{
				podAnnotations: tt.requiredAnnotations,
			}

			result := ops.podHasRequiredAnnotations(tt.podAnnotations)
			if result != tt.expected {
				t.Errorf("podHasRequiredAnnotations() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestPodHasRequiredLabelsAndAnnotations(t *testing.T) {
	tests := []struct {
		name                string
		podLabels           map[string]string
		podAnnotations      map[string]string
		requiredLabels      []string
		requiredAnnotations []string
		expected            bool
	}{
		{
			name: "both labels and annotations match",
			podLabels: map[string]string{
				"app": "myapp",
			},
			podAnnotations: map[string]string{
				"istio.io/rev": "canary",
			},
			requiredLabels:      []string{"app=myapp"},
			requiredAnnotations: []string{"istio.io/rev=canary"},
			expected:            true,
		},
		{
			name: "labels match but annotations don't",
			podLabels: map[string]string{
				"app": "myapp",
			},
			podAnnotations: map[string]string{
				"istio.io/rev": "default",
			},
			requiredLabels:      []string{"app=myapp"},
			requiredAnnotations: []string{"istio.io/rev=canary"},
			expected:            false,
		},
		{
			name: "annotations match but labels don't",
			podLabels: map[string]string{
				"app": "myapp",
			},
			podAnnotations: map[string]string{
				"istio.io/rev": "canary",
			},
			requiredLabels:      []string{"app=otherapp"},
			requiredAnnotations: []string{"istio.io/rev=canary"},
			expected:            false,
		},
		{
			name: "neither match",
			podLabels: map[string]string{
				"app": "myapp",
			},
			podAnnotations: map[string]string{
				"istio.io/rev": "default",
			},
			requiredLabels:      []string{"app=otherapp"},
			requiredAnnotations: []string{"istio.io/rev=canary"},
			expected:            false,
		},
		{
			name: "no requirements",
			podLabels: map[string]string{
				"app": "myapp",
			},
			podAnnotations: map[string]string{
				"istio.io/rev": "canary",
			},
			requiredLabels:      []string{},
			requiredAnnotations: []string{},
			expected:            true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock DeploymentOperations with required labels and annotations
			ops := &DeploymentOperations{
				podLabels:      tt.requiredLabels,
				podAnnotations: tt.requiredAnnotations,
			}

			labelsMatch := ops.podHasRequiredLabels(tt.podLabels)
			annotationsMatch := ops.podHasRequiredAnnotations(tt.podAnnotations)
			result := labelsMatch && annotationsMatch

			if result != tt.expected {
				t.Errorf("podHasRequiredLabelsAndAnnotations() = %v, want %v (labels: %v, annotations: %v)",
					result, tt.expected, labelsMatch, annotationsMatch)
			}
		})
	}
}
