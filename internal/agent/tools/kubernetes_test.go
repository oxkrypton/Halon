package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/oxkrypton/Halon/internal/agent"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestKubernetesListPodsAndNamespaceGuard(t *testing.T) {
	client := fake.NewClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "user-1", Namespace: "default", CreationTimestamp: metav1.Time{Time: time.Now()}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "user-service", Ready: true, RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
	})
	_, tools := NewKubernetesTools(client, KubernetesOptions{AllowedNamespaces: []string{"default"}})
	result, err := findTool(tools, "k8s_list_pods").Handler(context.Background(), json.RawMessage(`{"namespace":"default"}`))
	if err != nil {
		t.Fatal(err)
	}
	pods := result.([]PodSummary)
	if len(pods) != 1 || !pods[0].Ready || pods[0].Restarts != 2 || pods[0].Containers[0].State != "running" {
		t.Fatalf("unexpected pods: %+v", pods)
	}
	_, err = findTool(tools, "k8s_list_pods").Handler(context.Background(), json.RawMessage(`{"namespace":"kube-system"}`))
	if err == nil {
		t.Fatal("expected namespace allowlist error")
	}
}

func TestKubernetesScaleDeploymentAndCooldown(t *testing.T) {
	replicas := int32(1)
	client := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "user-service", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}})
	_, tools := NewKubernetesTools(client, KubernetesOptions{AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute})
	handler := findTool(tools, "k8s_scale_deployment").Handler
	arguments := json.RawMessage(`{"namespace":"default","name":"user-service","replicas":3,"reason":"high p95 and CPU saturation"}`)
	if _, err := handler(context.Background(), arguments); err != nil {
		t.Fatal(err)
	}
	deployment, err := client.AppsV1().Deployments("default").Get(context.Background(), "user-service", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 3 || deployment.Annotations["halon.dev/last-agent-action"] == "" {
		t.Fatalf("unexpected deployment: %+v", deployment)
	}
	if _, err := handler(context.Background(), arguments); err == nil {
		t.Fatal("expected cooldown error")
	}
}

func TestKubernetesRestartPod(t *testing.T) {
	client := fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "broken-pod", Namespace: "default"}})
	_, tools := NewKubernetesTools(client, KubernetesOptions{AllowedNamespaces: []string{"default"}})
	_, err := findTool(tools, "k8s_restart_pod").Handler(context.Background(), json.RawMessage(`{"pod":"broken-pod","reason":"CrashLoopBackOff with repeated panics"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CoreV1().Pods("default").Get(context.Background(), "broken-pod", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("pod still exists or unexpected error: %v", err)
	}
}

func TestKubernetesFullAccessImageUpdate(t *testing.T) {
	client := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "user-service", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "user-service", Image: "old:v1"}}}}},
	})
	_, tools := NewKubernetesTools(client, KubernetesOptions{AllowedNamespaces: []string{"default"}})
	_, err := findTool(tools, "k8s_update_deployment_image").Handler(context.Background(), json.RawMessage(`{"name":"user-service","container":"user-service","image":"registry.local/user:v2","reason":"known bad image"}`))
	if err != nil {
		t.Fatal(err)
	}
	deployment, _ := client.AppsV1().Deployments("default").Get(context.Background(), "user-service", metav1.GetOptions{})
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != "registry.local/user:v2" {
		t.Fatalf("image = %q", got)
	}
}

func TestGatewayBlockIP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/rules/blacklist/192.0.2.10" || r.Header.Get("Authorization") != "Bearer test-admin-token" {
			t.Errorf("unexpected request: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	toolset, _ := NewKubernetesTools(fake.NewClientset(), KubernetesOptions{})
	tool := NewGatewayActionTool(toolset, server.URL, "test-admin-token", server.Client())
	result, err := tool.Handler(context.Background(), json.RawMessage(`{"ip":"192.0.2.10","reason":"500 requests with 90% errors"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["status"] != "blocked" {
		t.Fatalf("result = %+v", result)
	}
}

func TestOperationLevelsExposeExpectedKubernetesTools(t *testing.T) {
	_, toolList := NewKubernetesTools(fake.NewClientset(), KubernetesOptions{})
	readonly, err := agent.NewRegistry(agent.LevelReadOnly, toolList...)
	if err != nil {
		t.Fatal(err)
	}
	for _, definition := range readonly.Definitions() {
		if definition.Function.Name == "k8s_restart_pod" || definition.Function.Name == "k8s_scale_deployment" || definition.Function.Name == "k8s_update_deployment_image" {
			t.Fatalf("readonly exposed mutation: %s", definition.Function.Name)
		}
	}
	full, _ := agent.NewRegistry(agent.LevelFull, toolList...)
	if len(full.Definitions()) != len(toolList) {
		t.Fatalf("full exposed %d of %d tools", len(full.Definitions()), len(toolList))
	}
}

func findTool(tools []agent.Tool, name string) agent.Tool {
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	panic("tool not found: " + name)
}
