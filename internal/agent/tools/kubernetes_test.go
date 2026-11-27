package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	now := metav1.Time{Time: time.Now()}
	replicas := int32(2)
	client := fake.NewClientset(
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "user-1", Namespace: "default", CreationTimestamp: now},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "user-service", Ready: true, RestartCount: 2, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "user-1.evt", Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "user-1"},
			Type:           "Warning",
			Reason:         "Unhealthy",
			Message:        "Readiness probe failed",
			LastTimestamp:  now,
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "user-service", Namespace: "default"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "user-service", Image: "user:v1"}}}},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 2, AvailableReplicas: 2},
		},
	)
	_, tools := NewKubernetesTools(client, KubernetesOptions{AllowedNamespaces: []string{"default"}})
	readK8s := findTool(tools, "read_k8s").Handler

	result, err := readK8s(context.Background(), json.RawMessage(`{
		"queries": [
			{"source": "pods", "namespace": "default"},
			{"source": "events", "namespace": "default", "type": "Warning"},
			{"source": "deployment", "namespace": "default", "name": "user-service"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	results := result.(map[string]any)["results"].([]batchItemResult)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	pods := results[0].Data.([]PodSummary)
	if len(pods) != 1 || !pods[0].Ready || pods[0].Restarts != 2 || pods[0].Containers[0].State != "running" {
		t.Fatalf("unexpected pods: %+v", pods)
	}
	events := results[1].Data.([]EventSummary)
	if len(events) != 1 || events[0].Reason != "Unhealthy" {
		t.Fatalf("unexpected events: %+v", events)
	}
	dep := results[2].Data.(map[string]any)
	if dep["name"] != "user-service" || dep["desired_replicas"] != int32(2) {
		t.Fatalf("unexpected deployment: %+v", dep)
	}

	if _, err := readK8s(context.Background(), json.RawMessage(`{"queries":[{"source":"pods","namespace":"kube-system"}]}`)); err == nil {
		t.Fatal("expected namespace allowlist error")
	}
	if _, err := readK8s(context.Background(), json.RawMessage(`{"queries":[{"source":"pods","pod":"user-1"}]}`)); err == nil || !strings.Contains(err.Error(), "allowed fields:") {
		t.Fatalf("expected allowed fields error, got %v", err)
	}
}

func TestKubernetesScaleDeploymentAndCooldown(t *testing.T) {
	replicas := int32(1)
	client := fake.NewClientset(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "user-service", Namespace: "default"}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}})
	_, tools := NewKubernetesTools(client, KubernetesOptions{
		AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
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
	client := fake.NewClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "broken-pod", Namespace: "default"}})
	_, tools := NewKubernetesTools(client, KubernetesOptions{
		AllowedNamespaces: []string{"default"},
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
	client := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "user-service", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "user-service", Image: "old:v1"}}}}},
	})
	_, tools := NewKubernetesTools(client, KubernetesOptions{
		AllowedNamespaces: []string{"default"},
		ActionCooldown:    time.Minute,
	})
	handler := findTool(tools, "k8s_update_deployment_image").Handler
	// Missing container validation must fail without consuming the cooldown slot.
	if _, err := handler(context.Background(), json.RawMessage(`{"name":"user-service","container":"wrong-container","image":"registry.local/user:v2","reason":"typo"}`)); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing container err = %v, want not found", err)
	}
	if _, err := handler(context.Background(), json.RawMessage(`{"name":"user-service","container":"user-service","image":"registry.local/user:v2","reason":"known bad image"}`)); err != nil {
		t.Fatal(err)
	}
	deployment, _ := client.AppsV1().Deployments("default").Get(context.Background(), "user-service", metav1.GetOptions{})
	if got := deployment.Spec.Template.Spec.Containers[0].Image; got != "registry.local/user:v2" {
		t.Fatalf("image = %q", got)
	}
}

func TestGatewayBlockIPAndReadTools(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/admin/rules/blacklist/192.0.2.10":
			if r.Method != http.MethodPut {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.WriteHeader(http.StatusCreated)
		case "/admin/state/gateway":
			_, _ = w.Write([]byte(`{"redis_up":true,"blacklist":{"count":1,"ips":["192.0.2.10"]},"rate_limit":{"requests_per_second":20,"burst":40},"breakers":[]}`))
		case "/admin/state/rate-limit/2001:db8::1":
			_, _ = w.Write([]byte(`{"ip":"2001:db8::1","in_blacklist":false,"active":true,"tokens":12.5,"burst":40}`))
		case "/admin/state/incident-windows":
			if got := r.URL.Query().Get("scope"); got != "/api/v1/orders/" {
				t.Errorf("scope query = %q, want /api/v1/orders/", got)
			}
			if got := r.URL.Query().Get("type"); got != "upstream_timeout" {
				t.Errorf("type query = %q, want upstream_timeout", got)
			}
			_, _ = w.Write([]byte(`{"redis_up":true,"windows":[{"event_type":"upstream_timeout","scope_key":"/api/v1/orders/","event_id":"evt-1","occurrences":9}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	toolset, _ := NewKubernetesTools(fake.NewClientset(), KubernetesOptions{ActionCooldown: time.Minute})
	if got := NewGatewayTools(toolset, "", "test-admin-token", server.Client()); len(got) != 0 {
		t.Fatalf("expected no gateway tools when adminBaseURL is empty, got %d", len(got))
	}
	if got := NewGatewayTools(toolset, server.URL, "", server.Client()); len(got) != 0 {
		t.Fatalf("expected no gateway tools when adminToken is empty, got %d", len(got))
	}

	gwTools := NewGatewayTools(toolset, server.URL, "test-admin-token", server.Client())

	readonly, err := agent.NewRegistry(agent.LevelReadOnly, gwTools...)
	if err != nil {
		t.Fatal(err)
	}
	if defs := readonly.Definitions(); len(defs) != 1 || defs[0].Function.Name != "read_gateway" {
		t.Fatalf("readonly exposed %+v, want [read_gateway]", defs)
	}

	readGateway := findTool(gwTools, "read_gateway").Handler
	stateRes, err := readGateway(context.Background(), json.RawMessage(`{"queries":[{"source":"state"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	stateResults := stateRes.(map[string]any)["results"].([]batchItemResult)
	if len(stateResults) != 1 || stateResults[0].Data.(map[string]any)["redis_up"] != true {
		t.Fatalf("unexpected read_gateway state: %+v", stateRes)
	}

	batchRes, err := readGateway(context.Background(), json.RawMessage(`{
		"queries": [
			{"source": "state"},
			{"source": "ip_status", "ip": "2001:0db8::1"},
			{"source": "incident_windows", "type": "upstream_timeout", "scope": "/api/v1/orders/"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	batchResults := batchRes.(map[string]any)["results"].([]batchItemResult)
	if len(batchResults) != 3 {
		t.Fatalf("len(batchResults) = %d, want 3", len(batchResults))
	}
	if batchResults[1].Data.(map[string]any)["ip"] != "2001:db8::1" {
		t.Fatalf("unexpected ip_status: %+v", batchResults[1])
	}
	if len(batchResults[2].Data.(map[string]any)["windows"].([]any)) != 1 {
		t.Fatalf("unexpected incident_windows: %+v", batchResults[2])
	}

	if _, err := readGateway(context.Background(), json.RawMessage(`{"queries":[{"source":"ip_status","ip":"bad-ip"}]}`)); err == nil {
		t.Fatal("expected invalid ip error")
	}
	if _, err := readGateway(context.Background(), json.RawMessage(`{"queries":[{"source":"state","ip":"192.0.2.10"}]}`)); err == nil || !strings.Contains(err.Error(), "allowed fields:") {
		t.Fatalf("expected allowed fields error, got %v", err)
	}

	blockRes, err := findTool(gwTools, "gateway_block_ip").Handler(context.Background(), json.RawMessage(`{"ip":"192.0.2.10","reason":"500 requests with 90% errors"}`))
	if err != nil {
		t.Fatal(err)
	}
	if blockRes.(map[string]any)["status"] != "blocked" {
		t.Fatalf("result = %+v", blockRes)
	}
}

func newResourceLockFixture() *fake.Clientset {
	replicas := int32(1)
	isController := true
	return fake.NewClientset(
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
}

func TestKubernetesDeploymentResourceLevelMutualExclusion(t *testing.T) {
	t.Parallel()

	t.Run("scale blocks image update and owned pod restart across sessions", func(t *testing.T) {
		t.Parallel()
		client := newResourceLockFixture()
		_, tools := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
		})

		if _, err := findTool(tools, "k8s_scale_deployment").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","replicas":2,"reason":"latency spike"}`)); err != nil {
			t.Fatalf("scale: %v", err)
		}
		if _, err := findTool(tools, "k8s_update_deployment_image").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","container":"user-service","image":"new:v2","reason":"rollback"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
			t.Fatalf("image update = %v, want cooling down", err)
		}
		if _, err := findTool(tools, "k8s_restart_pod").Handler(context.Background(),
			json.RawMessage(`{"pod":"user-service-rs-abc","reason":"unhealthy pod"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
			t.Fatalf("owned pod restart = %v, want cooling down", err)
		}
	})

	t.Run("owned pod restart blocks scale and image update across sessions", func(t *testing.T) {
		t.Parallel()
		client := newResourceLockFixture()
		_, tools := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
		})

		if _, err := findTool(tools, "k8s_restart_pod").Handler(context.Background(),
			json.RawMessage(`{"pod":"user-service-rs-abc","reason":"crash loop"}`)); err != nil {
			t.Fatalf("restart: %v", err)
		}
		if _, err := findTool(tools, "k8s_scale_deployment").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","replicas":3,"reason":"scale up"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
			t.Fatalf("scale = %v, want cooling down", err)
		}
		if _, err := findTool(tools, "k8s_update_deployment_image").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","container":"user-service","image":"new:v2","reason":"rollback"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
			t.Fatalf("image update = %v, want cooling down", err)
		}
	})

	t.Run("standalone pod restart does not block deployment", func(t *testing.T) {
		t.Parallel()
		client := newResourceLockFixture()
		_, tools := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
		})

		if _, err := findTool(tools, "k8s_restart_pod").Handler(context.Background(),
			json.RawMessage(`{"pod":"standalone-pod","reason":"stuck"}`)); err != nil {
			t.Fatalf("standalone pod restart: %v", err)
		}
		if _, err := findTool(tools, "k8s_scale_deployment").Handler(context.Background(),
			json.RawMessage(`{"name":"user-service","replicas":2,"reason":"scale up"}`)); err != nil {
			t.Fatalf("deployment scale after standalone pod restart: %v", err)
		}
	})

	t.Run("concurrent mutations on same resource allow only one", func(t *testing.T) {
		t.Parallel()
		client := newResourceLockFixture()
		_, tools := NewKubernetesTools(client, KubernetesOptions{
			AllowedNamespaces: []string{"default"}, MaxReplicas: 5, ActionCooldown: time.Minute,
		})
		scale := findTool(tools, "k8s_scale_deployment").Handler
		var successes atomic.Int32
		var wg sync.WaitGroup
		wg.Add(8)
		for i := 0; i < 8; i++ {
			go func() {
				defer wg.Done()
				if _, err := scale(context.Background(), json.RawMessage(`{"name":"user-service","replicas":2,"reason":"concurrent race"}`)); err == nil {
					successes.Add(1)
				}
			}()
		}
		wg.Wait()
		if successes.Load() != 1 {
			t.Fatalf("concurrent scale successes = %d, want 1", successes.Load())
		}
	})
}

func TestGatewayBlockIPCanonicalCooldown(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	toolset, _ := NewKubernetesTools(fake.NewClientset(), KubernetesOptions{ActionCooldown: time.Minute})
	tool := NewGatewayActionTool(toolset, server.URL, "test-admin-token", server.Client())

	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"ip":"2001:0db8::1","reason":"flood"}`)); err != nil {
		t.Fatalf("first block IPv6: %v", err)
	}
	if _, err := tool.Handler(context.Background(), json.RawMessage(`{"ip":"2001:db8::1","reason":"flood"}`)); err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("canonical IPv6 block = %v, want cooling down", err)
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
