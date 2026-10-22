package cmd

import (
	"bufio"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	strimziGroupV1 = `{"kind":"APIGroup","apiVersion":"v1","versions":[{"groupVersion":"x/v1","version":"v1"}]}`
)

// withRoutes returns baseFakeRoutes with extra routes added or replaced.
func withRoutes(extra map[string]fakeRoute) map[string]fakeRoute {
	routes := baseFakeRoutes()
	for k, v := range extra {
		routes[k] = v
	}
	return routes
}

// podSetDiscoveryForbidden: the Kafka API is served without any Kafka
// cluster, StrimziPodSet discovery is forbidden, and there is one
// PostgreSQL cluster app/pg.
func podSetDiscoveryForbidden() map[string]fakeRoute {
	return withRoutes(map[string]fakeRoute{
		"GET /apis/kafka.strimzi.io":                               {body: strimziGroupV1},
		"GET /apis/kafka.strimzi.io/v1/namespaces/app/kafkas":      {body: `{"items":[]}`},
		"GET /apis/core.strimzi.io":                                statusRoute(403, "Forbidden"),
		"GET /apis/acid.zalan.do/v1/namespaces/app/postgresqls":    {body: `{"items":[{"metadata":{"name":"pg","creationTimestamp":"2020-01-01T00:00:00Z"}}]}`},
		"GET /apis/acid.zalan.do/v1/namespaces/app/postgresqls/pg": {body: `{"spec":{"numberOfInstances":1},"status":{"PostgresClusterStatus":"Running"}}`},
	})
}

// planLines returns the messages of the "Restart plan:" block of a -o json
// run.
func planLines(t *testing.T, out string) []string {
	t.Helper()
	var (
		lines  []string
		inPlan bool
	)
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var entry struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &entry) != nil {
			continue
		}
		switch {
		case entry.Message == "Restart plan:":
			inPlan = true
		case inPlan && strings.HasPrefix(entry.Message, "  "):
			lines = append(lines, entry.Message)
		default:
			inPlan = false
		}
	}
	return lines
}

// Codex #2: a discovery failure of a later resource type (Kafka) must stop
// the run before anything is mutated: no cordon, no Deployment PATCH, even
// with --skip-wait; every planned target is reported as not started.
func TestRunMaintenance_PlanFailureBeforeAnyMutation(t *testing.T) {
	api := newFakeAPI(t, withRoutes(map[string]fakeRoute{
		"GET /apis/kafka.strimzi.io": statusRoute(403, "Forbidden"),
	}))
	out, err := runCLI(t, api.URL, "--execute", "-n", "app", "-o", "json",
		"--resources", "all", "--skip-wait", "--cordon", "--timeout", "5")
	if err == nil {
		t.Fatalf("expected the Kafka discovery failure to fail the run\n%s", out)
	}
	if !strings.Contains(err.Error(), "failed to plan Kafka clusters") ||
		!strings.Contains(err.Error(), "deployment app/web: not started due to earlier failure") {
		t.Errorf("plan failure / not-started Deployment not reported: %v", err)
	}
	if n := api.count("PATCH", "/apis/apps/"); n != 0 {
		t.Errorf("Deployment patched %d time(s) although planning failed", n)
	}
	if n := api.count("PATCH", "/api/v1/nodes"); n != 0 {
		t.Errorf("node cordoned %d time(s) although planning failed", n)
	}
	if n := api.count("PATCH", "/"); n != 0 {
		t.Errorf("%d PATCH request(s) although planning failed", n)
	}
}

// Codex #3: a StrimziPodSet discovery error is a failure even when there is
// no Kafka cluster; PostgreSQL (and every other type) is not started.
func TestRunMaintenance_PodSetDiscoveryErrorWithoutKafkaClusters(t *testing.T) {
	api := newFakeAPI(t, podSetDiscoveryForbidden())
	out, err := runCLI(t, api.URL, "--execute", "-n", "app", "-o", "json",
		"--resources", "all", "--skip-wait", "--timeout", "5")
	if err == nil {
		t.Fatalf("expected the StrimziPodSet discovery failure to fail the run\n%s", out)
	}
	if !strings.Contains(err.Error(), "StrimziPodSet API") {
		t.Errorf("StrimziPodSet discovery error not reported: %v", err)
	}
	if !strings.Contains(err.Error(), "postgresql app/pg: not started due to earlier failure") {
		t.Errorf("PostgreSQL cluster not reported as not started: %v", err)
	}
	if n := api.count("PATCH", "/"); n != 0 {
		t.Errorf("%d PATCH request(s) after the StrimziPodSet discovery failure", n)
	}
}

// Dry-run and execute build the plan with the same function: same targets,
// same planning errors.
func TestRunPlan_DryRunEqualsExecute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		routes  func() map[string]fakeRoute
		want    []string
		wantErr bool
	}{
		{
			name:   "healthy",
			routes: baseFakeRoutes,
			want: []string{
				"  - deployments: 1 to restart", "    * app/web",
				"  - statefulsets: none to restart",
				"  - Kafka clusters: none to restart",
				"  - PostgreSQL clusters: none to restart",
			},
		},
		{
			name:   "podset discovery forbidden",
			routes: podSetDiscoveryForbidden,
			want: []string{
				"  - deployments: 1 to restart", "    * app/web",
				"  - statefulsets: none to restart",
				"  - Kafka clusters: none to restart",
				"  - PostgreSQL clusters: 1 to restart", "    * app/pg",
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plans := map[string][]string{}
			for _, mode := range []string{"--dry-run", "--execute"} {
				api := newFakeAPI(t, tc.routes())
				out, err := runCLI(t, api.URL, mode, "-n", "app", "-o", "json", "--resources", "all", "--timeout", "30")
				if (err != nil) != tc.wantErr {
					t.Fatalf("%s: err=%v, want error=%v\n%s", mode, err, tc.wantErr, out)
				}
				plans[mode] = planLines(t, out)
				if mode == "--execute" && tc.wantErr {
					if n := api.count("PATCH", "/"); n != 0 {
						t.Errorf("execute patched %d time(s) after a planning failure", n)
					}
				}
			}
			if !reflect.DeepEqual(plans["--dry-run"], tc.want) {
				t.Errorf("dry-run plan = %q, want %q", plans["--dry-run"], tc.want)
			}
			if !reflect.DeepEqual(plans["--dry-run"], plans["--execute"]) {
				t.Errorf("dry-run plan %q differs from execute plan %q", plans["--dry-run"], plans["--execute"])
			}
		})
	}
}
