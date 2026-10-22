package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/pflag"
)

// fakeRoute is the canned answer of the fake API server for one
// "METHOD /path" key.
type fakeRoute struct {
	code int // HTTP status (200 when zero)
	body string
}

// statusRoute answers with a Kubernetes Status error of the given code.
func statusRoute(code int, reason string) fakeRoute {
	return fakeRoute{code: code, body: fmt.Sprintf(
		`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":%q,"code":%d,"message":"%s (test)"}`, reason, code, reason)}
}

// baseFakeRoutes is the minimal Kubernetes API a run of the tool needs: one
// node, one namespace "app" with one ready Deployment and its pod, the
// Zalando PostgreSQL API without clusters, and no Strimzi API.
func baseFakeRoutes() map[string]fakeRoute {
	const (
		node = `{"kind":"Node","apiVersion":"v1","metadata":{"name":"n1","uid":"uid-n1","resourceVersion":"1"},` +
			`"status":{"conditions":[{"type":"Ready","status":"True"}]}}`
		pod = `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"web-1","namespace":"app","uid":"uid-pod"},` +
			`"spec":{"nodeName":"n1","containers":[{"name":"c","image":"nginx"}]},"status":{"phase":"Running"}}`
		deployment = `{"kind":"Deployment","apiVersion":"apps/v1","metadata":{"name":"web","namespace":"app","uid":"uid-web",` +
			`"generation":2,"creationTimestamp":"2020-01-01T00:00:00Z"},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"web"}},` +
			`"template":{"metadata":{"labels":{"app":"web"}},"spec":{"containers":[{"name":"c","image":"nginx"}]}}},` +
			`"status":{"observedGeneration":2,"replicas":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}`
	)
	list := func(kind, apiVersion string, items ...string) string {
		return fmt.Sprintf(`{"kind":%q,"apiVersion":%q,"metadata":{},"items":[%s]}`, kind, apiVersion, strings.Join(items, ","))
	}
	routes := map[string]string{
		"GET /api/v1/nodes":                                     list("NodeList", "v1", node),
		"PATCH /api/v1/nodes/n1":                                node,
		"GET /api/v1/pods":                                      list("PodList", "v1", pod),
		"GET /api/v1/namespaces/app/pods":                       list("PodList", "v1", pod),
		"GET /api/v1/persistentvolumes":                         list("PersistentVolumeList", "v1"),
		"GET /apis/apps/v1/namespaces/app/deployments":          list("DeploymentList", "apps/v1", deployment),
		"GET /apis/apps/v1/namespaces/app/deployments/web":      deployment,
		"PATCH /apis/apps/v1/namespaces/app/deployments/web":    deployment,
		"GET /apis/apps/v1/namespaces/app/statefulsets":         list("StatefulSetList", "apps/v1"),
		"GET /apis/apps/v1/namespaces/app/replicasets":          list("ReplicaSetList", "apps/v1"),
		"GET /apis/acid.zalan.do":                               `{"kind":"APIGroup","apiVersion":"v1","name":"acid.zalan.do","versions":[{"groupVersion":"acid.zalan.do/v1","version":"v1"}]}`,
		"GET /apis/acid.zalan.do/v1/namespaces/app/postgresqls": `{"items":[]}`,
	}
	out := make(map[string]fakeRoute, len(routes))
	for k, v := range routes {
		out[k] = fakeRoute{body: v}
	}
	return out
}

// fakeAPI is a fake Kubernetes API server serving canned routes; it counts
// every request by "METHOD /path". Unknown routes answer 404.
type fakeAPI struct {
	*httptest.Server
	mu    sync.Mutex
	calls map[string]int
}

func newFakeAPI(t *testing.T, routes map[string]fakeRoute) *fakeAPI {
	t.Helper()
	api := &fakeAPI{calls: map[string]int{}}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		api.mu.Lock()
		api.calls[key]++
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		route, ok := routes[key]
		if !ok {
			route = statusRoute(http.StatusNotFound, "NotFound")
		}
		if route.code != 0 {
			w.WriteHeader(route.code)
		}
		_, _ = w.Write([]byte(route.body))
	}))
	t.Cleanup(api.Close)
	return api
}

// count returns the number of requests with the given method whose path
// starts with prefix.
func (a *fakeAPI) count(method, prefix string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for key, c := range a.calls {
		if strings.HasPrefix(key, method+" "+prefix) {
			n += c
		}
	}
	return n
}

// fakeAPIServer serves baseFakeRoutes.
func fakeAPIServer(t *testing.T) *httptest.Server {
	return newFakeAPI(t, baseFakeRoutes()).Server
}

// resetRootFlags restores every flag of rootCmd (and its bound global) to
// its default value and marks it unchanged.
func resetRootFlags(t *testing.T) {
	t.Helper()
	rootCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			def := strings.Trim(f.DefValue, "[]")
			vals := []string{}
			if def != "" {
				vals = strings.Split(def, ",")
			}
			if err := sv.Replace(vals); err != nil {
				t.Fatalf("reset --%s: %v", f.Name, err)
			}
		} else if err := f.Value.Set(f.DefValue); err != nil {
			t.Fatalf("reset --%s: %v", f.Name, err)
		}
		f.Changed = false
	})
}

// runCLI runs the root command against the API server at serverURL (through
// a kubeconfig with the context "fake-ctx") and returns its stdout. All flags
// are reset to their defaults before and after the run.
func runCLI(t *testing.T, serverURL string, args ...string) (string, error) {
	t.Helper()
	kubeconfig := filepath.Join(t.TempDir(), "config")
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake-ctx
  context:
    cluster: fake
    user: fake
users:
- name: fake
  user: {}
current-context: fake-ctx
`, serverURL)
	if err := os.WriteFile(kubeconfig, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	resetRootFlags(t)
	t.Cleanup(func() {
		resetRootFlags(t)
		rootCmd.SetArgs(nil)
	})
	rootCmd.SetArgs(append([]string{"--context", "fake-ctx"}, args...))
	var err error
	out := captureStdout(t, func() { err = rootCmd.Execute() })
	return out, err
}

// captureStdout runs fn with os.Stdout redirected and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	var (
		buf strings.Builder
		wg  sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(&buf, r)
	}()
	defer func() {
		os.Stdout = orig
	}()
	fn()
	_ = w.Close()
	wg.Wait()
	return buf.String()
}

// #8: with -o json every stdout line of a real command run (dry-run and
// execute, all resource handlers, cordon, reporter) must be valid JSON.
func TestRootCommand_JSONOutputIsJSONOnly(t *testing.T) {
	srv := fakeAPIServer(t)
	for _, mode := range []string{"--dry-run", "--execute"} {
		t.Run(mode, func(t *testing.T) {
			out, err := runCLI(t, srv.URL, mode, "-n", "app", "-o", "json",
				"--resources", "all", "--cordon", "--timeout", "30")
			if err != nil {
				t.Fatalf("run failed: %v\noutput:\n%s", err, out)
			}
			lines := 0
			sc := bufio.NewScanner(strings.NewReader(out))
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			for sc.Scan() {
				line := sc.Text()
				if strings.TrimSpace(line) == "" {
					continue
				}
				lines++
				if !json.Valid([]byte(line)) {
					t.Errorf("non-JSON stdout line in -o json mode: %q", line)
				}
			}
			if lines < 5 {
				t.Fatalf("expected the run to log, got %d lines:\n%s", lines, out)
			}
			for _, want := range []string{"deployment", "PostgreSQL", "Kafka", "n1"} {
				if !strings.Contains(out, want) {
					t.Errorf("output does not mention %q:\n%s", want, out)
				}
			}
		})
	}
}
