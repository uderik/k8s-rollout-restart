package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/uderik/k8s-rollout-restart/pkg/logger"
	"github.com/uderik/k8s-rollout-restart/pkg/operations"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// stopTestClient serves one Deployment whose restart PATCH is rejected and
// one StatefulSet whose PATCH is recorded.
type stopTestClient struct {
	operations.K8sClient
	stsPatched *int
}

func (c stopTestClient) AppsV1() operations.AppsV1Interface { return stopTestApps(c) }

type stopTestApps struct {
	operations.K8sClient
	stsPatched *int
}

func (stopTestApps) Deployments(string) operations.DeploymentInterface { return stopTestDeployments{} }
func (a stopTestApps) StatefulSets(string) operations.StatefulSetInterface {
	return stopTestStatefulSets{patched: a.stsPatched}
}
func (stopTestApps) ReplicaSets(string) operations.ReplicaSetInterface { return nil }

type stopTestDeployments struct{ operations.DeploymentInterface }

func (stopTestDeployments) List(context.Context, metav1.ListOptions) (*appsv1.DeploymentList, error) {
	return &appsv1.DeploymentList{Items: []appsv1.Deployment{{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "app"}}}}, nil
}

func (stopTestDeployments) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions) (*appsv1.Deployment, error) {
	return nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "web", errors.New("denied"))
}

type stopTestStatefulSets struct {
	operations.StatefulSetInterface
	patched *int
}

func (stopTestStatefulSets) List(context.Context, metav1.ListOptions) (*appsv1.StatefulSetList, error) {
	return &appsv1.StatefulSetList{Items: []appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app"}}}}, nil
}

func (s stopTestStatefulSets) Patch(context.Context, string, types.PatchType, []byte, metav1.PatchOptions) (*appsv1.StatefulSet, error) {
	*s.patched++
	return nil, errors.New("must not be called")
}

// A Deployment failure must not end the run silently: the StatefulSets of
// the later resource type are not started and are reported as such.
func TestRunMaintenance_LaterResourceTypesReportedNotStarted(t *testing.T) {
	patched := 0
	c := stopTestClient{stsPatched: &patched}
	log := logger.NewLogger(false)
	rs := operations.NewRunStop()
	deploymentOps := operations.NewDeploymentOperations(c, 1, 1, true, false, nil, nil, nil, nil, false)
	deploymentOps.SetLogger(log)
	deploymentOps.SetRunStop(rs)
	statefulSetOps := operations.NewStatefulSetOperations(c, 1, 1, true, false, nil, nil, nil, nil, false)
	statefulSetOps.SetLogger(log)
	statefulSetOps.SetRunStop(rs)
	ops := maintenanceOps{deployment: deploymentOps, statefulSet: statefulSetOps}

	var err error
	captureStdout(t, func() {
		err = runMaintenance(context.Background(), log, []string{"app"},
			restartFlags{deployments: true, statefulSets: true}, ops, nil, nil)
	})
	if err == nil {
		t.Fatal("expected Deployment failure")
	}
	if !strings.Contains(err.Error(), "denied") {
		t.Errorf("deployment error missing: %v", err)
	}
	if !strings.Contains(err.Error(), "statefulset app/db: not started due to earlier failure") ||
		!errors.Is(err, operations.ErrNotStartedEarlierFailure) {
		t.Errorf("remaining StatefulSet not reported as not started; err=%v", err)
	}
	if patched != 0 {
		t.Errorf("StatefulSet patched %d time(s) after an earlier failure", patched)
	}
}
