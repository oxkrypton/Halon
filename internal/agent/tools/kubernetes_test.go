package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/oxkrypton/Halon/internal/agent"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
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
	mr := miniredis.RunT(t)
	replicas := int32(1)
	client := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "user-service", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}})
	_, tools := NewKubernetesTools(client, KubernetesOptions{
		AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
		Cooldowns: newTestRedisCooldownStore(t, mr, nil),
	})
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
	mr := miniredis.RunT(t)
	client := fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "broken-pod", Namespace: "default"}})
	_, tools := NewKubernetesTools(client, KubernetesOptions{
		AllowedNamespaces: []string{"default"},
		Cooldowns:         newTestRedisCooldownStore(t, mr, nil),
	})
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
	mr := miniredis.RunT(t)
	client := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "user-service", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "user-service", Image: "old:v1"}}}}},
	})
	_, tools := NewKubernetesTools(client, KubernetesOptions{
		AllowedNamespaces: []string{"default"},
		Cooldowns:         newTestRedisCooldownStore(t, mr, nil),
	})
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
	mr := miniredis.RunT(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/rules/blacklist/192.0.2.10" || r.Header.Get("Authorization") != "Bearer test-admin-token" {
			t.Errorf("unexpected request: %s %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	toolset, _ := NewKubernetesTools(fake.NewClientset(), KubernetesOptions{
		Cooldowns: newTestRedisCooldownStore(t, mr, nil),
	})
	tool := NewGatewayActionTool(toolset, server.URL, "test-admin-token", server.Client())
	result, err := tool.Handler(context.Background(), json.RawMessage(`{"ip":"192.0.2.10","reason":"500 requests with 90% errors"}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["status"] != "blocked" {
		t.Fatalf("result = %+v", result)
	}
}

func newResourceLockFixture(t *testing.T) (*miniredis.Miniredis, *fake.Clientset) {
	t.Helper()
	mr := miniredis.RunT(t)
	replicas := int32(1)
	isController := true
	client := fake.NewClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "user-service", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "user-service", Image: "old:v1"}},
					},
				},
			},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "user-service-rs",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "apps/v1", Kind: "Deployment", Name: "user-service", Controller: &isController},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "user-service-rs-abc",
				Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "user-service-rs", Controller: &isController},
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "standalone-pod", Namespace: "default"},
		},
	)
	return mr, client
}

func TestKubernetesDeploymentResourceLevelMutualExclusion(t *testing.T) {
	t.Parallel()

	t.Run("scale blocks image update and owned pod restart across replicas", func(t *testing.T) {
		t.Parallel()
		mr, client := newResourceLockFixture(t)
		_, toolsA := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
			Cooldowns: newTestRedisCooldownStore(t, mr, nil),
		})
		_, toolsB := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
			Cooldowns: newTestRedisCooldownStore(t, mr, nil),
		})

		if _, err := findTool(toolsA, "k8s_scale_deployment").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","replicas":2,"reason":"latency spike"}`)); err != nil {
			t.Fatalf("replica A scale: %v", err)
		}
		if _, err := findTool(toolsB, "k8s_update_deployment_image").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","container":"user-service","image":"new:v2","reason":"rollback"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
			t.Fatalf("replica B image update = %v, want cooling down", err)
		}
		if _, err := findTool(toolsB, "k8s_restart_pod").Handler(context.Background(),
			json.RawMessage(`{"pod":"user-service-rs-abc","reason":"unhealthy pod"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
			t.Fatalf("replica B owned pod restart = %v, want cooling down", err)
		}
	})

	t.Run("owned pod restart blocks scale and image update across replicas", func(t *testing.T) {
		t.Parallel()
		mr, client := newResourceLockFixture(t)
		_, toolsA := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
			Cooldowns: newTestRedisCooldownStore(t, mr, nil),
		})
		_, toolsB := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
			Cooldowns: newTestRedisCooldownStore(t, mr, nil),
		})

		if _, err := findTool(toolsA, "k8s_restart_pod").Handler(context.Background(),
			json.RawMessage(`{"pod":"user-service-rs-abc","reason":"crash loop"}`)); err != nil {
			t.Fatalf("replica A restart: %v", err)
		}
		if _, err := findTool(toolsB, "k8s_scale_deployment").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","replicas":3,"reason":"scale up"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
			t.Fatalf("replica B scale = %v, want cooling down", err)
		}
		if _, err := findTool(toolsB, "k8s_update_deployment_image").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","container":"user-service","image":"new:v2","reason":"rollback"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
			t.Fatalf("replica B image update = %v, want cooling down", err)
		}
	})

	t.Run("standalone pod restart does not block deployment", func(t *testing.T) {
		t.Parallel()
		mr, client := newResourceLockFixture(t)
		_, toolsA := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
			Cooldowns: newTestRedisCooldownStore(t, mr, nil),
		})
		_, toolsB := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
			Cooldowns: newTestRedisCooldownStore(t, mr, nil),
		})

		if _, err := findTool(toolsA, "k8s_restart_pod").Handler(context.Background(),
			json.RawMessage(`{"pod":"standalone-pod","reason":"stuck"}`)); err != nil {
			t.Fatalf("standalone pod restart: %v", err)
		}
		if _, err := findTool(toolsB, "k8s_scale_deployment").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","replicas":2,"reason":"scale up"}`)); err != nil {
			t.Fatalf("deployment scale after standalone pod restart: %v", err)
		}
	})
}

func TestGatewayBlockIPMultiReplicaResourceCooldown(t *testing.T) {
	t.Parallel()
	mr := miniredis.RunT(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	toolsetA, _ := NewKubernetesTools(fake.NewClientset(), KubernetesOptions{
		ActionCooldown: time.Minute,
		Cooldowns:      newTestRedisCooldownStore(t, mr, nil),
	})
	toolsetB, _ := NewKubernetesTools(fake.NewClientset(), KubernetesOptions{
		ActionCooldown: time.Minute,
		Cooldowns:      newTestRedisCooldownStore(t, mr, nil),
	})
	toolA := NewGatewayActionTool(toolsetA, server.URL, "test-admin-token", server.Client())
	toolB := NewGatewayActionTool(toolsetB, server.URL, "test-admin-token", server.Client())

	if _, err := toolA.Handler(context.Background(), json.RawMessage(`{"ip":"2001:0db8::1","reason":"flood"}`)); err != nil {
		t.Fatalf("replica A block IPv6: %v", err)
	}
	if _, err := toolB.Handler(context.Background(), json.RawMessage(`{"ip":"2001:db8::1","reason":"flood"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("replica B canonical IPv6 block = %v, want cooling down", err)
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

func TestKubernetesToolsetPodLogReader(t *testing.T) {
	t.Parallel()

	client := fake.NewClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "gateway-b",
				Namespace:         "halon",
				Labels:            map[string]string{"app.kubernetes.io/part-of": "halon"},
				CreationTimestamp: metav1.Time{Time: time.Now()},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "gateway",
					Ready: true,
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "agent-a",
				Namespace: "halon",
				Labels:    map[string]string{"app.kubernetes.io/part-of": "halon"},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "other-pod",
				Namespace: "halon",
			},
		},
	)

	toolset, _ := NewKubernetesTools(client, KubernetesOptions{AllowedNamespaces: []string{"prod", "halon"}})
	if got := toolset.AllowedNamespaces(); !reflect.DeepEqual(got, []string{"halon", "prod"}) {
		t.Fatalf("AllowedNamespaces = %v, want [halon prod]", got)
	}

	resolvedNS, pods, err := toolset.ListPods(context.Background(), "", "app.kubernetes.io/part-of=halon", 0)
	if err != nil {
		t.Fatalf("ListPods default fallback: %v", err)
	}
	if resolvedNS != "halon" || len(pods) != 2 || pods[0].Name != "agent-a" || pods[1].Name != "gateway-b" {
		t.Fatalf("ListPods = (%q, %+v)", resolvedNS, pods)
	}

	if _, _, err := toolset.ListPods(context.Background(), "kube-system", "", 10); !errors.Is(err, agent.ErrNamespaceForbidden) {
		t.Fatalf("forbidden namespace err = %v, want ErrNamespaceForbidden", err)
	}
	if _, _, err := toolset.ListPods(context.Background(), "halon", "", 101); !errors.Is(err, agent.ErrInvalidPodRequest) {
		t.Fatalf("invalid limit err = %v, want ErrInvalidPodRequest", err)
	}

	wildcardToolset, _ := NewKubernetesTools(client, KubernetesOptions{AllowedNamespaces: []string{"*"}})
	if resolved, _, err := wildcardToolset.ListPods(context.Background(), "", "", 10); err != nil || resolved != "default" {
		t.Fatalf("wildcard ListPods = (%q, %v)", resolved, err)
	}

	logRes, err := toolset.GetPodLogs(context.Background(), agent.PodLogOptions{
		Namespace: "halon",
		Pod:       "gateway-b",
		Container: "gateway",
		Previous:  true,
	})
	if err != nil {
		t.Fatalf("GetPodLogs: %v", err)
	}
	if logRes.Namespace != "halon" || logRes.Pod != "gateway-b" || logRes.Container != "gateway" || !logRes.Previous || logRes.Logs == "" {
		t.Fatalf("unexpected GetPodLogs result: %+v", logRes)
	}

	for _, bad := range []agent.PodLogOptions{
		{Namespace: "halon", Pod: ""},
		{Namespace: "halon", Pod: "bad/pod"},
		{Namespace: "halon", Pod: "gateway-b", Container: "bad container"},
		{Namespace: "halon", Pod: "gateway-b", TailLines: 1001},
		{Namespace: "halon", Pod: "gateway-b", SinceSeconds: 86401},
	} {
		if _, err := toolset.GetPodLogs(context.Background(), bad); !errors.Is(err, agent.ErrInvalidPodRequest) {
			t.Fatalf("bad options %+v: err = %v, want ErrInvalidPodRequest", bad, err)
		}
	}
	if _, err := toolset.GetPodLogs(context.Background(), agent.PodLogOptions{Namespace: "kube-system", Pod: "gateway-b"}); !errors.Is(err, agent.ErrNamespaceForbidden) {
		t.Fatalf("forbidden GetPodLogs err = %v, want ErrNamespaceForbidden", err)
	}

	// Verify real client-go StatusError decoding for 404/400/403 from the API server.
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/pods/missing/log"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(metav1.Status{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status:   metav1.StatusFailure,
				Reason:   metav1.StatusReasonNotFound,
				Message:  `pods "missing" not found`,
				Code:     http.StatusNotFound,
			})
		case strings.Contains(r.URL.Path, "/pods/noprev/log"):
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(metav1.Status{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status:   metav1.StatusFailure,
				Reason:   metav1.StatusReasonBadRequest,
				Message:  "previous terminated container not found",
				Code:     http.StatusBadRequest,
			})
		default:
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(metav1.Status{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
				Status:   metav1.StatusFailure,
				Reason:   metav1.StatusReasonForbidden,
				Message:  "forbidden",
				Code:     http.StatusForbidden,
			})
		}
	}))
	defer apiServer.Close()

	realClient, err := kubernetes.NewForConfig(&rest.Config{Host: apiServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	realToolset, _ := NewKubernetesTools(realClient, KubernetesOptions{AllowedNamespaces: []string{"default"}})
	if _, err := realToolset.GetPodLogs(context.Background(), agent.PodLogOptions{Pod: "missing"}); !errors.Is(err, agent.ErrPodNotFound) {
		t.Fatalf("missing pod err = %v, want ErrPodNotFound", err)
	}
	if _, err := realToolset.GetPodLogs(context.Background(), agent.PodLogOptions{Pod: "noprev", Previous: true}); !errors.Is(err, agent.ErrInvalidPodRequest) {
		t.Fatalf("no previous err = %v, want ErrInvalidPodRequest", err)
	}
	if _, err := realToolset.GetPodLogs(context.Background(), agent.PodLogOptions{Pod: "denied"}); !errors.Is(err, agent.ErrNamespaceForbidden) {
		t.Fatalf("forbidden err = %v, want ErrNamespaceForbidden", err)
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
