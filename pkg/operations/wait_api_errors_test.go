package operations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func podsForbidden() error {
	return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("pods list denied"))
}

// Codex #1 (Kafka): a pod List error while waiting for cluster a trips the
// run-wide stop at once; a is still waited for (its pods get replaced) and
// the error is returned although the rotation completed; b is not started.
func TestRestartKafkaClusters_WaitAPIErrorStopsRun(t *testing.T) {
	fastPolling(t)
	srv := newCRTestServer(t)
	srv.json("GET", "/apis/kafka.strimzi.io", 200, apiGroupV1Only)
	srv.json("GET", "/apis/core.strimzi.io", 200, apiGroupV1Only)
	crs := "/apis/kafka.strimzi.io/v1/namespaces/ns/kafkas"
	sets := "/apis/core.strimzi.io/v1/namespaces/ns/strimzipodsets"
	srv.json("GET", crs, 200, `{"items":[{"metadata":{"name":"a"}},{"metadata":{"name":"b"}}]}`)
	srv.handle("GET", sets, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Query().Get("labelSelector"), "strimzi.io/cluster=")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[{"metadata":{"name":%q,"labels":{"strimzi.io/cluster":%q}}}]}`, name+"-pool", name)
	})
	for _, name := range []string{"a", "b"} {
		srv.json("GET", sets+"/"+name+"-pool", 200, `{"spec":{"pods":[{}]}}`)
		srv.json("GET", crs+"/"+name, 200, `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
		srv.handle("PATCH", sets+"/"+name+"-pool", okJSON)
	}

	rs := NewRunStop()
	var (
		aLists            atomic.Int32
		stoppedBeforeDone atomic.Bool
	)
	client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
		name := strings.TrimPrefix(opts.LabelSelector, "strimzi.io/cluster=")
		owner := metav1.OwnerReference{Kind: "StrimziPodSet", Name: name + "-pool"}
		uid := "old-" + name
		if name == "a" {
			switch n := aLists.Add(1); {
			case n == 2: // first poll after the PATCH
				return nil, podsForbidden()
			case n == 3: // the pod has been replaced
				stoppedBeforeDone.Store(rs.Stopped())
				uid = "new-a"
			case n > 3:
				uid = "new-a"
			}
		}
		return &corev1.PodList{Items: []corev1.Pod{testPod(name+"-0", uid, boolPtr(true), owner)}}, nil
	}}
	k := NewKafkaOperations(client, 1, 2, false, nil, false, "", "")
	k.SetRunStop(rs)
	err := k.RestartKafkaClusters(context.Background(), []string{"ns"})
	if err == nil || !strings.Contains(err.Error(), "pods list denied") {
		t.Fatalf("the pod List error while waiting must be returned, got %v", err)
	}
	if !strings.Contains(err.Error(), "completed nevertheless") {
		t.Errorf("cluster a must still be waited for until its rotation completed: %v", err)
	}
	if !stoppedBeforeDone.Load() {
		t.Error("the API error must trip the run-wide stop before the rotation completes")
	}
	if n := len(srv.bodiesFor("PATCH", sets+"/b-pool")); n != 0 {
		t.Errorf("cluster b started after an API error while waiting for a: %d PATCH", n)
	}
	if !strings.Contains(err.Error(), "kafka ns/b: not started due to earlier failure") {
		t.Errorf("cluster b not reported as not started: %v", err)
	}
}

// Codex #1 (PostgreSQL): a pod List error while waiting for the rotation of
// acid-main trips the run-wide stop at once; the rotation is still waited
// for and the error is returned although it completed; acid-next is not
// started.
func TestRestartPostgresqlClusters_WaitAPIErrorStopsRun(t *testing.T) {
	fastPolling(t)
	srv := newPGTestServer(t)
	srv.json("GET", pgListPath, 200, `{"items":[{"metadata":{"name":"acid-main"}},{"metadata":{"name":"acid-next"}}]}`)
	var mainTriggers, nextTriggers atomic.Int32
	for _, name := range []string{"acid-main", "acid-next"} {
		srv.json("GET", pgListPath+"/"+name, 200, pgRunning2)
		for _, ordinal := range []string{"0", "1"} {
			srv.handle("PATCH", pgPodPath+name+"-"+ordinal, okJSON)
		}
	}
	srv.handle("PATCH", pgCRPath, func(w http.ResponseWriter, r *http.Request) {
		mainTriggers.Add(1)
		okJSON(w, r)
	})
	srv.handle("PATCH", pgListPath+"/acid-next", func(w http.ResponseWriter, r *http.Request) {
		nextTriggers.Add(1)
		okJSON(w, r)
	})

	rs := NewRunStop()
	var (
		polls             atomic.Int32
		stoppedBeforeDone atomic.Bool
	)
	client := &crTestClient{rc: srv.restClient(), pods: func(_ string, opts metav1.ListOptions) (*corev1.PodList, error) {
		name := strings.TrimPrefix(opts.LabelSelector, postgresqlPodSelector)
		triggered := nextTriggers.Load() > 0
		replaced := false
		if name == "acid-main" {
			triggered = mainTriggers.Load() > 0
			if triggered {
				switch polls.Add(1) {
				case 1:
					return nil, podsForbidden()
				case 2:
					stoppedBeforeDone.Store(rs.Stopped())
					replaced = true
				default:
					replaced = true
				}
			}
		}
		var pods []corev1.Pod
		for _, ordinal := range []string{"0", "1"} {
			uid := name + "-old-" + ordinal
			if replaced {
				uid = name + "-new-" + ordinal
			}
			pod := testPod(name+"-"+ordinal, uid, boolPtr(true))
			if triggered && !replaced {
				pod.Annotations = map[string]string{postgresqlRollingUpdateAnnotation: "true", PostgresqlMarkedByAnnotation: testPGRunID}
			}
			pods = append(pods, pod)
		}
		return &corev1.PodList{Items: pods}, nil
	}}
	p := newTestPG(client, 2, false)
	p.SetRunStop(rs)
	err := p.RestartPostgresqlClusters(context.Background(), []string{"db"})
	if err == nil || !strings.Contains(err.Error(), "pods list denied") ||
		!strings.Contains(err.Error(), "the rotation completed nevertheless") {
		t.Fatalf("the pod List error must be returned after the completed rotation, got %v", err)
	}
	if !stoppedBeforeDone.Load() {
		t.Error("the API error must trip the run-wide stop before the rotation completes")
	}
	if !rs.Stopped() || rs.Cause() == nil || !strings.Contains(rs.Cause().Error(), "postgresql db/acid-main") {
		t.Errorf("run stop not tripped by acid-main: stopped=%v, cause=%v", rs.Stopped(), rs.Cause())
	}
	if n := nextTriggers.Load(); n != 0 {
		t.Errorf("acid-next triggered after an API error while waiting for acid-main: %d PATCH", n)
	}
	if !strings.Contains(err.Error(), "postgresql db/acid-next: not started due to earlier failure") {
		t.Errorf("acid-next not reported as not started: %v", err)
	}
}

// Codex #3: a StrimziPodSet discovery error fails the Kafka phase even
// without any Kafka cluster, and through the shared stop PostgreSQL is not
// started. A PodSet API group that is simply not installed is harmless when
// there is no Kafka cluster.
func TestRestartKafkaClusters_PodSetDiscoveryErrorWithoutClusters(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		body    string
		wantErr bool
	}{
		{"forbidden", 403, forbidden, true},
		{"unavailable", 503, serverUnavailable, true},
		{"not installed", 404, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newPGTestServer(t)
			srv.json("GET", "/apis/kafka.strimzi.io", 200, apiGroupV1Only)
			srv.json("GET", "/apis/kafka.strimzi.io/v1/namespaces/db/kafkas", 200, `{"items":[]}`)
			srv.json("GET", "/apis/core.strimzi.io", tc.code, tc.body)
			srv.json("GET", pgCRPath, 200, pgRunning2)
			srv.handle("PATCH", pgPodPath+"acid-main-0", okJSON)
			srv.handle("PATCH", pgPodPath+"acid-main-1", okJSON)
			srv.handle("PATCH", pgCRPath, okJSON)
			client := &crTestClient{rc: srv.restClient(), pods: func(string, metav1.ListOptions) (*corev1.PodList, error) {
				return &corev1.PodList{Items: []corev1.Pod{
					testPod("acid-main-0", "old-0", boolPtr(true)), testPod("acid-main-1", "old-1", boolPtr(true))}}, nil
			}}
			rs := NewRunStop()
			k := NewKafkaOperations(client, 1, 1, false, nil, true, "", "")
			k.SetRunStop(rs)
			kafkaErr := k.RestartKafkaClusters(context.Background(), []string{"db"})
			p := newTestPG(client, 1, true)
			p.SetRunStop(rs)
			pgErr := p.RestartPostgresqlClusters(context.Background(), []string{"db"})

			if !tc.wantErr {
				if kafkaErr != nil || pgErr != nil || rs.Stopped() {
					t.Fatalf("missing PodSet API without Kafka clusters must be harmless: kafka=%v, pg=%v", kafkaErr, pgErr)
				}
				return
			}
			if kafkaErr == nil || !strings.Contains(kafkaErr.Error(), "StrimziPodSet API") {
				t.Fatalf("StrimziPodSet discovery error must fail the Kafka phase, got %v", kafkaErr)
			}
			if !rs.Stopped() {
				t.Error("the discovery error must trip the run-wide stop")
			}
			if n := srv.methodCount("PATCH"); n != 0 {
				t.Errorf("%d PATCH request(s) after the discovery error", n)
			}
			if pgErr == nil || !strings.Contains(pgErr.Error(), "postgresql db/acid-main: not started due to earlier failure") {
				t.Errorf("PostgreSQL cluster not reported as not started: %v", pgErr)
			}
		})
	}
}
