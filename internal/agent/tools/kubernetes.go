package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oxkrypton/Halon/internal/agent"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

type KubernetesOptions struct {
	AllowedNamespaces []string
	MaxReplicas       int
	ActionCooldown    time.Duration
	// Cooldowns arbitrates mutating actions. Nil selects the single-replica
	// memory store; shared mode injects a RedisCooldownStore.
	Cooldowns CooldownStore
}

type KubernetesToolset struct {
	client      kubernetes.Interface
	namespaces  map[string]struct{}
	maxReplicas int
	cooldown    time.Duration
	cooldowns   CooldownStore
}

func NewKubernetesTools(client kubernetes.Interface, options KubernetesOptions) (*KubernetesToolset, []agent.Tool) {
	if len(options.AllowedNamespaces) == 0 {
		options.AllowedNamespaces = []string{"default"}
	}
	if options.MaxReplicas <= 0 {
		options.MaxReplicas = 5
	}
	if options.ActionCooldown <= 0 {
		options.ActionCooldown = 2 * time.Minute
	}
	if options.Cooldowns == nil {
		options.Cooldowns = newMemoryCooldownStore()
	}
	toolset := &KubernetesToolset{
		client: client, namespaces: make(map[string]struct{}), maxReplicas: options.MaxReplicas,
		cooldown: options.ActionCooldown, cooldowns: options.Cooldowns,
	}
	for _, namespace := range options.AllowedNamespaces {
		namespace = strings.TrimSpace(namespace)
		if namespace != "" {
			toolset.namespaces[namespace] = struct{}{}
		}
	}
	return toolset, toolset.tools()
}

func (k *KubernetesToolset) tools() []agent.Tool {
	namespaceProperty := stringProperty("Kubernetes namespace; must be in the configured allowlist.")
	return []agent.Tool{
		{
			Name: "k8s_list_pods", Level: agent.LevelReadOnly,
			Description: "List pods with phase, readiness, restart counts and container states in an allowed namespace.",
			Parameters:  objectSchema(map[string]any{"namespace": namespaceProperty, "label_selector": stringProperty("Optional Kubernetes label selector."), "limit": integerProperty("Maximum pods, default 100.", 1, 100)}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Namespace     string `json:"namespace"`
					LabelSelector string `json:"label_selector"`
					Limit         int    `json:"limit"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Limit == 0 {
					input.Limit = 100
				}
				if input.Limit < 1 || input.Limit > 100 {
					return nil, fmt.Errorf("limit must be 1..100")
				}
				if err := k.checkNamespace(&input.Namespace); err != nil {
					return nil, err
				}
				pods, err := k.client.CoreV1().Pods(input.Namespace).List(ctx, metav1.ListOptions{LabelSelector: input.LabelSelector, Limit: int64(input.Limit)})
				if err != nil {
					return nil, err
				}
				results := make([]PodSummary, 0, len(pods.Items))
				for _, pod := range pods.Items {
					results = append(results, summarizePod(pod))
				}
				return results, nil
			},
		},
		{
			Name: "k8s_get_pod_logs", Level: agent.LevelReadOnly,
			Description: "Read bounded recent logs from a pod container. Set previous=true to inspect a crashed container's previous instance.",
			Parameters: objectSchema(map[string]any{
				"namespace": namespaceProperty, "pod": stringProperty("Exact pod name."), "container": stringProperty("Optional container name."),
				"tail_lines": integerProperty("Maximum last lines, default 200.", 1, 1000), "since_seconds": integerProperty("Lookback seconds, default 900.", 1, 86400),
				"previous": map[string]any{"type": "boolean", "description": "Read previous terminated container logs."},
			}, "pod"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Namespace    string `json:"namespace"`
					Pod          string `json:"pod"`
					Container    string `json:"container"`
					TailLines    int    `json:"tail_lines"`
					SinceSeconds int    `json:"since_seconds"`
					Previous     bool   `json:"previous"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Pod == "" {
					return nil, fmt.Errorf("pod is required")
				}
				if input.TailLines == 0 {
					input.TailLines = 200
				}
				if input.SinceSeconds == 0 {
					input.SinceSeconds = 900
				}
				if input.TailLines < 1 || input.TailLines > 1000 || input.SinceSeconds < 1 || input.SinceSeconds > 86400 {
					return nil, fmt.Errorf("tail_lines must be 1..1000 and since_seconds must be 1..86400")
				}
				if err := k.checkNamespace(&input.Namespace); err != nil {
					return nil, err
				}
				tailLines := int64(input.TailLines)
				since := time.Now().Add(-time.Duration(input.SinceSeconds) * time.Second)
				limitBytes := int64(1 << 20)
				logs, err := k.client.CoreV1().Pods(input.Namespace).GetLogs(input.Pod, &corev1.PodLogOptions{Container: input.Container, TailLines: &tailLines, SinceTime: &metav1.Time{Time: since}, Previous: input.Previous, LimitBytes: &limitBytes, Timestamps: true}).DoRaw(ctx)
				if err != nil {
					return nil, err
				}
				return map[string]any{"namespace": input.Namespace, "pod": input.Pod, "container": input.Container, "logs": string(logs)}, nil
			},
		},
		{
			Name: "k8s_list_events", Level: agent.LevelReadOnly,
			Description: "List recent Kubernetes warning and lifecycle events, optionally filtered to an exact involved object name.",
			Parameters:  objectSchema(map[string]any{"namespace": namespaceProperty, "involved_object": stringProperty("Optional exact Pod, Deployment or other object name."), "type": stringProperty("Optional event type such as Warning."), "limit": integerProperty("Maximum events, default 100.", 1, 100)}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Namespace      string `json:"namespace"`
					InvolvedObject string `json:"involved_object"`
					Type           string `json:"type"`
					Limit          int    `json:"limit"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Limit == 0 {
					input.Limit = 100
				}
				if input.Limit < 1 || input.Limit > 100 {
					return nil, fmt.Errorf("limit must be 1..100")
				}
				if err := k.checkNamespace(&input.Namespace); err != nil {
					return nil, err
				}
				selectors := []fields.Selector{}
				if input.InvolvedObject != "" {
					selectors = append(selectors, fields.OneTermEqualSelector("involvedObject.name", input.InvolvedObject))
				}
				if input.Type != "" {
					selectors = append(selectors, fields.OneTermEqualSelector("type", input.Type))
				}
				events, err := k.client.CoreV1().Events(input.Namespace).List(ctx, metav1.ListOptions{FieldSelector: fields.AndSelectors(selectors...).String(), Limit: int64(input.Limit)})
				if err != nil {
					return nil, err
				}
				results := make([]EventSummary, 0, len(events.Items))
				for _, event := range events.Items {
					results = append(results, EventSummary{Object: event.InvolvedObject.Name, Kind: event.InvolvedObject.Kind, Type: event.Type, Reason: event.Reason, Message: event.Message, Count: event.Count, LastTimestamp: event.LastTimestamp.Time})
				}
				sort.Slice(results, func(i, j int) bool { return results[i].LastTimestamp.After(results[j].LastTimestamp) })
				return results, nil
			},
		},
		{
			Name: "k8s_get_deployment", Level: agent.LevelReadOnly,
			Description: "Get Deployment replica status, conditions, container images and update strategy.",
			Parameters:  objectSchema(map[string]any{"namespace": namespaceProperty, "name": stringProperty("Exact Deployment name.")}, "name"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Name == "" {
					return nil, fmt.Errorf("name is required")
				}
				if err := k.checkNamespace(&input.Namespace); err != nil {
					return nil, err
				}
				deployment, err := k.client.AppsV1().Deployments(input.Namespace).Get(ctx, input.Name, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				containers := make([]map[string]string, 0, len(deployment.Spec.Template.Spec.Containers))
				for _, container := range deployment.Spec.Template.Spec.Containers {
					containers = append(containers, map[string]string{"name": container.Name, "image": container.Image})
				}
				return map[string]any{"name": deployment.Name, "namespace": deployment.Namespace, "desired_replicas": replicasValue(deployment.Spec.Replicas), "ready_replicas": deployment.Status.ReadyReplicas, "available_replicas": deployment.Status.AvailableReplicas, "updated_replicas": deployment.Status.UpdatedReplicas, "unavailable_replicas": deployment.Status.UnavailableReplicas, "conditions": deployment.Status.Conditions, "containers": containers, "strategy": deployment.Spec.Strategy.Type}, nil
			},
		},
		{
			Name: "k8s_restart_pod", Level: agent.LevelOperator,
			Description: "Gracefully delete one evidenced faulty pod so its controller recreates it. Do not use without pod state, event or log evidence.",
			Parameters:  objectSchema(map[string]any{"namespace": namespaceProperty, "pod": stringProperty("Exact pod name."), "reason": stringProperty("Evidence-based reason recorded in the audit log.")}, "pod", "reason"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Namespace string `json:"namespace"`
					Pod       string `json:"pod"`
					Reason    string `json:"reason"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Pod == "" || strings.TrimSpace(input.Reason) == "" {
					return nil, fmt.Errorf("pod and reason are required")
				}
				if err := k.checkNamespace(&input.Namespace); err != nil {
					return nil, err
				}
				if err := k.allowAction(ctx, "restart/"+input.Namespace+"/"+input.Pod); err != nil {
					return nil, err
				}
				if _, err := k.client.CoreV1().Pods(input.Namespace).Get(ctx, input.Pod, metav1.GetOptions{}); err != nil {
					return nil, err
				}
				if err := k.client.CoreV1().Pods(input.Namespace).Delete(ctx, input.Pod, metav1.DeleteOptions{}); err != nil {
					return nil, err
				}
				return map[string]any{"status": "restart_requested", "namespace": input.Namespace, "pod": input.Pod, "reason": input.Reason}, nil
			},
		},
		{
			Name: "k8s_scale_deployment", Level: agent.LevelOperator,
			Description: "Scale a Deployment within the configured replica cap when metrics support a capacity-related fix.",
			Parameters:  objectSchema(map[string]any{"namespace": namespaceProperty, "name": stringProperty("Exact Deployment name."), "replicas": integerProperty("Desired replica count.", 0, 100), "reason": stringProperty("Evidence-based scaling reason.")}, "name", "replicas", "reason"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
					Replicas  int    `json:"replicas"`
					Reason    string `json:"reason"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Name == "" || strings.TrimSpace(input.Reason) == "" {
					return nil, fmt.Errorf("name and reason are required")
				}
				if input.Replicas < 0 || input.Replicas > k.maxReplicas {
					return nil, fmt.Errorf("replicas must be 0..%d", k.maxReplicas)
				}
				if err := k.checkNamespace(&input.Namespace); err != nil {
					return nil, err
				}
				if err := k.allowAction(ctx, "scale/"+input.Namespace+"/"+input.Name); err != nil {
					return nil, err
				}
				// #nosec G115 -- input.Replicas is validated to 0..k.maxReplicas above.
				desiredReplicas := int32(input.Replicas)
				err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					deployment, err := k.client.AppsV1().Deployments(input.Namespace).Get(ctx, input.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					deployment.Spec.Replicas = &desiredReplicas
					if deployment.Annotations == nil {
						deployment.Annotations = map[string]string{}
					}
					deployment.Annotations["halon.dev/last-agent-action"] = time.Now().UTC().Format(time.RFC3339) + ": " + input.Reason
					_, err = k.client.AppsV1().Deployments(input.Namespace).Update(ctx, deployment, metav1.UpdateOptions{})
					return err
				})
				if err != nil {
					return nil, err
				}
				return map[string]any{"status": "scale_requested", "namespace": input.Namespace, "deployment": input.Name, "replicas": input.Replicas}, nil
			},
		},
		{
			Name: "k8s_update_deployment_image", Level: agent.LevelFull,
			Description: "Full-access emergency remediation: update one existing container image after verifying the exact Deployment and container name.",
			Parameters:  objectSchema(map[string]any{"namespace": namespaceProperty, "name": stringProperty("Exact Deployment name."), "container": stringProperty("Existing container name."), "image": stringProperty("Complete container image reference."), "reason": stringProperty("Evidence-based image update reason.")}, "name", "container", "image", "reason"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Namespace string `json:"namespace"`
					Name      string `json:"name"`
					Container string `json:"container"`
					Image     string `json:"image"`
					Reason    string `json:"reason"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Name == "" || input.Container == "" || input.Image == "" || strings.TrimSpace(input.Reason) == "" {
					return nil, fmt.Errorf("name, container, image, and reason are required")
				}
				if len(input.Image) > 256 || strings.ContainsAny(input.Image, " \t\r\n") {
					return nil, fmt.Errorf("invalid container image")
				}
				if err := k.checkNamespace(&input.Namespace); err != nil {
					return nil, err
				}
				if err := k.allowAction(ctx, "image/"+input.Namespace+"/"+input.Name+"/"+input.Container); err != nil {
					return nil, err
				}
				err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
					deployment, err := k.client.AppsV1().Deployments(input.Namespace).Get(ctx, input.Name, metav1.GetOptions{})
					if err != nil {
						return err
					}
					found := false
					for i := range deployment.Spec.Template.Spec.Containers {
						if deployment.Spec.Template.Spec.Containers[i].Name == input.Container {
							deployment.Spec.Template.Spec.Containers[i].Image = input.Image
							found = true
							break
						}
					}
					if !found {
						return fmt.Errorf("container %q not found", input.Container)
					}
					if deployment.Annotations == nil {
						deployment.Annotations = map[string]string{}
					}
					deployment.Annotations["halon.dev/last-agent-action"] = time.Now().UTC().Format(time.RFC3339) + ": " + input.Reason
					_, err = k.client.AppsV1().Deployments(input.Namespace).Update(ctx, deployment, metav1.UpdateOptions{})
					return err
				})
				if err != nil {
					return nil, err
				}
				return map[string]any{"status": "image_update_requested", "namespace": input.Namespace, "deployment": input.Name, "container": input.Container, "image": input.Image}, nil
			},
		},
	}
}

type PodSummary struct {
	Name       string           `json:"name"`
	Phase      string           `json:"phase"`
	Ready      bool             `json:"ready"`
	Restarts   int32            `json:"restarts"`
	Node       string           `json:"node"`
	CreatedAt  time.Time        `json:"created_at"`
	Containers []ContainerState `json:"containers"`
}
type ContainerState struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restart_count"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
}
type EventSummary struct {
	Object        string    `json:"object"`
	Kind          string    `json:"kind"`
	Type          string    `json:"type"`
	Reason        string    `json:"reason"`
	Message       string    `json:"message"`
	Count         int32     `json:"count"`
	LastTimestamp time.Time `json:"last_timestamp"`
}

func summarizePod(pod corev1.Pod) PodSummary {
	summary := PodSummary{Name: pod.Name, Phase: string(pod.Status.Phase), Node: pod.Spec.NodeName, CreatedAt: pod.CreationTimestamp.Time}
	ready := len(pod.Status.ContainerStatuses) > 0
	for _, status := range pod.Status.ContainerStatuses {
		ready = ready && status.Ready
		state, reason := "unknown", ""
		switch {
		case status.State.Waiting != nil:
			state, reason = "waiting", status.State.Waiting.Reason
		case status.State.Running != nil:
			state = "running"
		case status.State.Terminated != nil:
			state, reason = "terminated", status.State.Terminated.Reason
		}
		summary.Restarts += status.RestartCount
		summary.Containers = append(summary.Containers, ContainerState{Name: status.Name, Ready: status.Ready, RestartCount: status.RestartCount, State: state, Reason: reason})
	}
	summary.Ready = ready
	return summary
}

func (k *KubernetesToolset) checkNamespace(namespace *string) error {
	if *namespace == "" {
		*namespace = "default"
	}
	if _, all := k.namespaces["*"]; all {
		return nil
	}
	if _, ok := k.namespaces[*namespace]; !ok {
		return fmt.Errorf("namespace %q is not in the agent allowlist", *namespace)
	}
	return nil
}

// allowAction claims the cooldown slot for key. In shared mode the claim is
// an atomic Redis command, so only one replica executes the action per
// window; a store outage rejects the mutation instead of risking a repeat.
func (k *KubernetesToolset) allowAction(ctx context.Context, key string) error {
	remaining, claimed, err := k.cooldowns.Claim(ctx, key, k.cooldown)
	if err != nil {
		return fmt.Errorf("action %s is unavailable: %w", key, err)
	}
	if !claimed {
		return fmt.Errorf("action %s is cooling down; retry after %s", key, remaining.Round(time.Second))
	}
	return nil
}

func replicasValue(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}
