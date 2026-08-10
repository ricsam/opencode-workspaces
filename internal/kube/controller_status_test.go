package kube

import (
	"context"
	"testing"
	"time"

	"github.com/ricsam/opencode-workspaces/internal/config"
	"github.com/ricsam/opencode-workspaces/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestStatusesListsWorkspaceResourcesInBulk(t *testing.T) {
	now := metav1.NewTime(time.Now())
	stoppedReplicas, runningReplicas := int32(0), int32(1)
	labelsFor := func(name string) map[string]string {
		return map[string]string{"app.kubernetes.io/managed-by": managedBy, "opencode.workspaces/name": name}
	}
	client := fake.NewSimpleClientset(
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ws-stopped", Namespace: "workspaces", Labels: labelsFor("ws-stopped")}, Spec: appsv1.DeploymentSpec{Replicas: &stoppedReplicas}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "ws-running", Namespace: "workspaces", Labels: labelsFor("ws-running")}, Spec: appsv1.DeploymentSpec{Replicas: &runningReplicas}, Status: appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "ws-running-pod", Namespace: "workspaces", Labels: labelsFor("ws-running")}, Status: corev1.PodStatus{StartTime: &now}},
	)
	controller := &Controller{Client: client, Config: config.Config{Namespace: "workspaces"}}
	workspaces := []model.Workspace{{ResourceName: "ws-missing"}, {ResourceName: "ws-stopped"}, {ResourceName: "ws-running"}}

	statuses, err := controller.Statuses(context.Background(), workspaces)
	if err != nil {
		t.Fatal(err)
	}
	if statuses["ws-missing"].Phase != "stopped" || statuses["ws-missing"].Exists {
		t.Fatalf("unexpected missing status: %#v", statuses["ws-missing"])
	}
	if statuses["ws-stopped"].Phase != "stopped" || !statuses["ws-stopped"].Exists {
		t.Fatalf("unexpected stopped status: %#v", statuses["ws-stopped"])
	}
	running := statuses["ws-running"]
	if running.Phase != "running" || !running.Ready || running.PodName != "ws-running-pod" || running.StartedAt.IsZero() {
		t.Fatalf("unexpected running status: %#v", running)
	}
}
