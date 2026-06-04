package operations

import "testing"

func TestNewKafkaOperations_APIPaths(t *testing.T) {
	tests := []struct {
		name           string
		kafkaVersion   string
		podSetVersion  string
		wantKafkaPath  string
		wantPodSetPath string
	}{
		{
			name:           "defaults when empty",
			kafkaVersion:   "",
			podSetVersion:  "",
			wantKafkaPath:  "/apis/" + DefaultKafkaAPIVersion,
			wantPodSetPath: "/apis/" + DefaultStrimziPodSetAPIVersion,
		},
		{
			name:           "custom versions",
			kafkaVersion:   "kafka.strimzi.io/v1beta3",
			podSetVersion:  "core.strimzi.io/v1beta3",
			wantKafkaPath:  "/apis/kafka.strimzi.io/v1beta3",
			wantPodSetPath: "/apis/core.strimzi.io/v1beta3",
		},
		{
			name:           "tolerates leading /apis/ prefix",
			kafkaVersion:   "/apis/kafka.strimzi.io/v1beta2",
			podSetVersion:  "/apis/core.strimzi.io/v1beta2",
			wantKafkaPath:  "/apis/kafka.strimzi.io/v1beta2",
			wantPodSetPath: "/apis/core.strimzi.io/v1beta2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := NewKafkaOperations(nil, 1, 1, false, nil, false, tt.kafkaVersion, tt.podSetVersion)
			if k.kafkaAPIPath != tt.wantKafkaPath {
				t.Errorf("kafkaAPIPath = %q, want %q", k.kafkaAPIPath, tt.wantKafkaPath)
			}
			if k.podSetAPIPath != tt.wantPodSetPath {
				t.Errorf("podSetAPIPath = %q, want %q", k.podSetAPIPath, tt.wantPodSetPath)
			}
		})
	}
}

func TestIsNotFoundErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "unrelated error", err: errStr("connection refused"), want: false},
		{name: "server could not find", err: errStr(`the server could not find the requested resource (get kafkas.kafka.strimzi.io)`), want: true},
		{name: "could not find", err: errStr("could not find the requested resource"), want: true},
		{name: "plain not found", err: errStr(`kafkas.kafka.strimzi.io "x" not found`), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotFoundErr(tt.err); got != tt.want {
				t.Errorf("isNotFoundErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// errStr is a tiny error type for table tests.
type errStr string

func (e errStr) Error() string { return string(e) }
