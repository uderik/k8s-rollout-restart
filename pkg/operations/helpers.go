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
