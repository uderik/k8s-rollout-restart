package operations

import "strings"

// ParseLabelsOrAnnotations parses a slice of "key=value" strings into a map
func ParseLabelsOrAnnotations(items []string) map[string]string {
	result := make(map[string]string)
	for _, item := range items {
		parts := strings.SplitN(item, "=", 2)
		if len(parts) == 2 {
			result[parts[0]] = parts[1]
		}
	}
	return result
}

// isNotFoundErr reports whether err indicates that an API group/version or
// resource does not exist on the cluster. The raw REST client used for custom
// resources returns plain errors (not typed apierrors), so we match on the
// well-known "not found" phrasings the apiserver produces.
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "could not find") ||
		strings.Contains(msg, "the server could not find the requested resource")
}

// MatchesRequirements checks if given labels/annotations match all required ones
func MatchesRequirements(actual, required map[string]string) bool {
	if len(required) == 0 {
		return true
	}
	for key, value := range required {
		if actual[key] != value {
			return false
		}
	}
	return true
}
