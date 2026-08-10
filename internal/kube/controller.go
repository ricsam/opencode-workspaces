package kube

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ricsam/opencode-workspaces/internal/config"
	"github.com/ricsam/opencode-workspaces/internal/database"
	"github.com/ricsam/opencode-workspaces/internal/model"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const managedBy = "opencode-workspaces"

type Controller struct {
	Client   kubernetes.Interface
	Store    *database.Store
	Config   config.Config
	OwnerUID types.UID
	log      *slog.Logger
}

func New(ctx context.Context, store *database.Store, cfg config.Config, logger *slog.Logger) (*Controller, error) {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		restConfig, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	}
	if err != nil {
		return nil, err
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	owner, err := client.CoreV1().ConfigMaps(cfg.Namespace).Get(ctx, cfg.OwnerName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get workspace owner anchor: %w", err)
	}
	return &Controller{Client: client, Store: store, Config: cfg, OwnerUID: owner.UID, log: logger}, nil
}

func (c *Controller) Run(ctx context.Context) {
	ticker := time.NewTicker(c.Config.Workspace.ReconcileInterval)
	defer ticker.Stop()
	c.reconcileAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.reconcileAll(ctx)
		}
	}
}
func (c *Controller) reconcileAll(ctx context.Context) {
	if c.Config.Workspace.IdleTimeout > 0 {
		if ids, err := c.Store.IdleWorkspaces(ctx, time.Now().Add(-c.Config.Workspace.IdleTimeout)); err == nil {
			for _, id := range ids {
				c.log.Info("workspace idled", "user_id", id)
			}
		}
	}
	items, err := c.Store.ListWorkspaces(ctx)
	if err != nil {
		c.log.Error("list workspaces", "error", err)
		return
	}
	for _, w := range items {
		if err := c.Reconcile(ctx, w); err != nil {
			c.log.Error("reconcile workspace", "workspace", w.ResourceName, "error", err)
		}
	}
}

func (c *Controller) Reconcile(ctx context.Context, w model.Workspace) error {
	if err := c.ensurePVC(ctx, w); err != nil {
		return err
	}
	if err := c.ensureSecret(ctx, w); err != nil {
		return err
	}
	if err := c.ensureService(ctx, w); err != nil {
		return err
	}
	return c.ensureDeployment(ctx, w)
}
func (c *Controller) Start(ctx context.Context, userID string) error {
	if err := c.Store.SetWorkspaceState(ctx, userID, userID, "running", "", false); err != nil {
		return err
	}
	w, err := c.Store.Workspace(ctx, userID)
	if err != nil {
		return err
	}
	return c.Reconcile(ctx, w)
}
func (c *Controller) Stop(ctx context.Context, actor, userID, remote string) error {
	if err := c.Store.SetWorkspaceState(ctx, actor, userID, "stopped", remote, false); err != nil {
		return err
	}
	w, err := c.Store.Workspace(ctx, userID)
	if err != nil {
		return err
	}
	return c.Reconcile(ctx, w)
}
func (c *Controller) Restart(ctx context.Context, actor, userID, remote string) error {
	if err := c.Store.SetWorkspaceState(ctx, actor, userID, "running", remote, true); err != nil {
		return err
	}
	w, err := c.Store.Workspace(ctx, userID)
	if err != nil {
		return err
	}
	return c.Reconcile(ctx, w)
}
func (c *Controller) Purge(ctx context.Context, actor, userID, remote string) error {
	w, err := c.Store.Workspace(ctx, userID)
	if err != nil {
		return err
	}
	if err := c.Stop(ctx, actor, userID, remote); err != nil {
		return err
	}
	policy := metav1.DeletePropagationForeground
	if err := c.Client.CoreV1().PersistentVolumeClaims(c.Config.Namespace).Delete(ctx, w.ResourceName, metav1.DeleteOptions{PropagationPolicy: &policy}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return c.Store.DeleteWorkspaceDataRecord(ctx, actor, userID, remote)
}

func (c *Controller) Status(ctx context.Context, w model.Workspace) (model.WorkspaceStatus, error) {
	deployment, err := c.Client.AppsV1().Deployments(c.Config.Namespace).Get(ctx, w.ResourceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return model.WorkspaceStatus{Phase: "stopped"}, nil
	}
	if err != nil {
		return model.WorkspaceStatus{}, err
	}
	status := model.WorkspaceStatus{Exists: true, Replicas: deployment.Status.Replicas, ReadyReplicas: deployment.Status.ReadyReplicas, Ready: deployment.Status.ReadyReplicas > 0, Phase: "starting"}
	if deployment.Spec.Replicas != nil && *deployment.Spec.Replicas == 0 {
		status.Phase = "stopped"
	} else if status.Ready {
		status.Phase = "running"
	}
	pods, err := c.Client.CoreV1().Pods(c.Config.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{"app.kubernetes.io/managed-by": managedBy, "opencode.workspaces/name": w.ResourceName}.String()})
	if err == nil && len(pods.Items) > 0 {
		status.PodName = pods.Items[0].Name
		if pods.Items[0].Status.StartTime != nil {
			status.StartedAt = pods.Items[0].Status.StartTime.Time
		}
		if !status.Ready {
			for _, condition := range pods.Items[0].Status.Conditions {
				if condition.Type == corev1.PodReady && condition.Message != "" {
					status.Message = condition.Message
				}
			}
		}
	}
	return status, nil
}

func (c *Controller) Backend(ctx context.Context, userID string) (string, string, error) {
	w, err := c.Store.Workspace(ctx, userID)
	if err != nil {
		return "", "", err
	}
	status, err := c.Status(ctx, w)
	if err != nil || !status.Ready {
		return "", "", errors.New("workspace is not ready")
	}
	secret, err := c.Client.CoreV1().Secrets(c.Config.Namespace).Get(ctx, w.ResourceName+"-auth", metav1.GetOptions{})
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("http://%s.%s.svc:%d", w.ResourceName, c.Config.Namespace, c.Config.Workspace.Port), string(secret.Data["password"]), nil
}

func (c *Controller) owner() []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: c.Config.OwnerName, UID: c.OwnerUID, Controller: ptr(true), BlockOwnerDeletion: ptr(true)}}
}
func (c *Controller) meta(w model.Workspace) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: w.ResourceName, Namespace: c.Config.Namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": managedBy, "opencode.workspaces/name": w.ResourceName, "opencode.workspaces/user-id": w.UserID}, OwnerReferences: c.owner()}
}
func (c *Controller) ensurePVC(ctx context.Context, w model.Workspace) error {
	_, err := c.Client.CoreV1().PersistentVolumeClaims(c.Config.Namespace).Get(ctx, w.ResourceName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: c.meta(w), Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: &c.Config.Workspace.StorageClass, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(c.Config.Workspace.StorageSize)}}}}
	// PVC deliberately has no owner reference so Helm uninstall/reinstall cannot erase user data.
	pvc.OwnerReferences = nil
	_, err = c.Client.CoreV1().PersistentVolumeClaims(c.Config.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	return err
}
func (c *Controller) ensureSecret(ctx context.Context, w model.Workspace) error {
	name := w.ResourceName + "-auth"
	configuration, configErr := c.Store.WorkspaceConfiguration(ctx, w.UserID)
	if configErr != nil && !errors.Is(configErr, database.ErrNotFound) {
		return configErr
	}
	revision := "0"
	if configErr == nil {
		revision = fmt.Sprint(configuration.Revision)
	}
	current, err := c.Client.CoreV1().Secrets(c.Config.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		password := make([]byte, 32)
		if _, err := rand.Read(password); err != nil {
			return err
		}
		secret := &corev1.Secret{ObjectMeta: c.meta(w), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"username": []byte("opencode"), "password": []byte(base64.RawURLEncoding.EncodeToString(password))}}
		secret.Name = name
		secret.Annotations = map[string]string{"opencode.workspaces/config-revision": revision}
		if configErr == nil {
			secret.Data["opencode.json"] = configuration.OpenCodeJSON
			secret.Data["ai-gateway.key"] = []byte(configuration.APIKey)
		}
		_, err = c.Client.CoreV1().Secrets(c.Config.Namespace).Create(ctx, secret, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if current.Annotations["opencode.workspaces/config-revision"] == revision {
		return nil
	}
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations["opencode.workspaces/config-revision"] = revision
	if current.Data == nil {
		current.Data = map[string][]byte{}
	}
	if configErr == nil {
		current.Data["opencode.json"] = configuration.OpenCodeJSON
		current.Data["ai-gateway.key"] = []byte(configuration.APIKey)
	} else {
		delete(current.Data, "opencode.json")
		delete(current.Data, "ai-gateway.key")
	}
	_, err = c.Client.CoreV1().Secrets(c.Config.Namespace).Update(ctx, current, metav1.UpdateOptions{})
	return err
}
func (c *Controller) ensureService(ctx context.Context, w model.Workspace) error {
	_, err := c.Client.CoreV1().Services(c.Config.Namespace).Get(ctx, w.ResourceName, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	svc := &corev1.Service{ObjectMeta: c.meta(w), Spec: corev1.ServiceSpec{Selector: map[string]string{"opencode.workspaces/name": w.ResourceName}, Ports: []corev1.ServicePort{{Name: "http", Port: c.Config.Workspace.Port, TargetPort: intstrValue(c.Config.Workspace.Port)}}}}
	_, err = c.Client.CoreV1().Services(c.Config.Namespace).Create(ctx, svc, metav1.CreateOptions{})
	return err
}
func (c *Controller) ensureDeployment(ctx context.Context, w model.Workspace) error {
	replicas := int32(0)
	if w.DesiredState == "running" {
		replicas = 1
	}
	dep := &appsv1.Deployment{ObjectMeta: c.meta(w), Spec: appsv1.DeploymentSpec{Replicas: &replicas, Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"opencode.workspaces/name": w.ResourceName}}, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": managedBy, "opencode.workspaces/name": w.ResourceName, "opencode.workspaces/user-id": w.UserID}, Annotations: map[string]string{"opencode.workspaces/restart-nonce": fmt.Sprint(w.RestartNonce)}}, Spec: c.podSpec(w)}}}
	current, err := c.Client.AppsV1().Deployments(c.Config.Namespace).Get(ctx, w.ResourceName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = c.Client.AppsV1().Deployments(c.Config.Namespace).Create(ctx, dep, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	dep.ResourceVersion = current.ResourceVersion
	_, err = c.Client.AppsV1().Deployments(c.Config.Namespace).Update(ctx, dep, metav1.UpdateOptions{})
	return err
}
func (c *Controller) podSpec(w model.Workspace) corev1.PodSpec {
	allow := false
	runAs := int64(0)
	secretMode := int32(0400)
	seccomp := corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}
	containerSecurity := &corev1.SecurityContext{RunAsUser: &runAs, RunAsNonRoot: ptr(false), Privileged: ptr(false), AllowPrivilegeEscalation: &allow, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}, SeccompProfile: &seccomp}
	bootstrapCommand := `set -euo pipefail
config_dir=/workspace/home/.config/opencode
if [[ -s /bootstrap/opencode.json ]]; then
  mkdir -p "$config_dir"
  cp /bootstrap/opencode.json "$config_dir/opencode.json"
  chmod 600 "$config_dir/opencode.json"
fi
if [[ -s /bootstrap/ai-gateway.key ]]; then
  mkdir -p "$config_dir"
  cp /bootstrap/ai-gateway.key "$config_dir/ai-gateway.key"
  chmod 600 "$config_dir/ai-gateway.key"
fi`
	spec := corev1.PodSpec{
		AutomountServiceAccountToken: ptr(false), EnableServiceLinks: ptr(false), SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &seccomp},
		InitContainers: []corev1.Container{{Name: "workspace-bootstrap", Image: c.Config.Workspace.Image, ImagePullPolicy: corev1.PullPolicy(c.Config.Workspace.ImagePullPolicy), Command: []string{"/bin/bash", "-c", bootstrapCommand}, SecurityContext: containerSecurity, VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}, {Name: "bootstrap", MountPath: "/bootstrap", ReadOnly: true}}}},
		Containers:     []corev1.Container{{Name: "workspace", Image: c.Config.Workspace.Image, ImagePullPolicy: corev1.PullPolicy(c.Config.Workspace.ImagePullPolicy), Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: c.Config.Workspace.Port}}, SecurityContext: containerSecurity, Env: []corev1.EnvVar{{Name: "OPENCODE_SERVER_USERNAME", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: w.ResourceName + "-auth"}, Key: "username"}}}, {Name: "OPENCODE_SERVER_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: w.ResourceName + "-auth"}, Key: "password"}}}, {Name: "HOME", Value: "/workspace/home"}, {Name: "XDG_DATA_HOME", Value: "/workspace/home/.local/share"}, {Name: "XDG_CONFIG_HOME", Value: "/workspace/home/.config"}, {Name: "WORKSPACE_DIR", Value: "/workspace/projects"}}, VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"}}, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(c.Config.Workspace.CPURequest), corev1.ResourceMemory: resource.MustParse(c.Config.Workspace.MemoryRequest)}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(c.Config.Workspace.CPULimit), corev1.ResourceMemory: resource.MustParse(c.Config.Workspace.MemoryLimit)}}, ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstrValue(c.Config.Workspace.Port)}}, InitialDelaySeconds: 3, PeriodSeconds: 5}, LivenessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstrValue(c.Config.Workspace.Port)}}, InitialDelaySeconds: 20, PeriodSeconds: 20}}},
		Volumes:        []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: w.ResourceName}}}, {Name: "bootstrap", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: w.ResourceName + "-auth", DefaultMode: &secretMode}}}},
		NodeSelector:   c.Config.Workspace.NodeSelector,
	}
	if c.Config.Workspace.RuntimeClassName != "" {
		spec.RuntimeClassName = &c.Config.Workspace.RuntimeClassName
	}
	if c.Config.Workspace.ImagePullSecret != "" {
		spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: c.Config.Workspace.ImagePullSecret}}
	}
	return spec
}
func intstrValue(v int32) intstr.IntOrString { return intstr.FromInt32(v) }
func ptr[T any](v T) *T                      { return &v }
