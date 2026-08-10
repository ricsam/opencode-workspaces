package kube

import (
	"testing"

	"github.com/ricsam/opencode-workspaces/internal/config"
	"github.com/ricsam/opencode-workspaces/internal/model"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWorkspacePodSecurity(t *testing.T) {
	c := &Controller{Client: fake.NewSimpleClientset(), Config: config.Config{Workspace: config.Workspace{Image: "workspace:test", ImagePullPolicy: "IfNotPresent", CPURequest: "100m", MemoryRequest: "128Mi", CPULimit: "1", MemoryLimit: "1Gi", Port: 4096, NodeSelector: map[string]string{"kubernetes.io/hostname": "worker-1"}}}}
	spec := c.podSpec(model.Workspace{ResourceName: "ws-test", UserID: "user-test"})
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("workspace service account token must be disabled")
	}
	container := spec.Containers[0]
	if container.SecurityContext.Privileged == nil || *container.SecurityContext.Privileged {
		t.Fatal("workspace must not be privileged")
	}
	if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatal("privilege escalation must be denied")
	}
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].SubPath != "" {
		t.Fatal("workspace must mount complete PVC without subPath")
	}
	if len(spec.InitContainers) != 1 || len(spec.InitContainers[0].VolumeMounts) != 2 {
		t.Fatal("workspace bootstrap must copy managed files from a secret into the PVC")
	}
	if spec.NodeSelector["kubernetes.io/hostname"] != "worker-1" {
		t.Fatal("workspace node selector was not propagated")
	}
}
