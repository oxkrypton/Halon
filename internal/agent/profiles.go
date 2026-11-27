package agent

import (
	"fmt"

	"github.com/oxkrypton/Halon/internal/incident"
)

// InvestigationProfile describes how the agent approaches a specific incident
// type, providing a human-readable title and initial triage guidance.
type InvestigationProfile struct {
	Type        incident.EventType
	Title       string
	FocusPrompt string
}

var builtinProfiles = map[incident.EventType]InvestigationProfile{
	incident.TypeUpstreamTimeout: {
		Type:        incident.TypeUpstreamTimeout,
		Title:       "上游请求超时 (504 Gateway Timeout)",
		FocusPrompt: "Prioritize inspecting error logs/slow requests via read_observability (by request_id/path), upstream Pod readiness/resource/logs via read_k8s, and active incident windows via read_gateway.",
	},
	incident.TypeUpstreamError: {
		Type:        incident.TypeUpstreamError,
		Title:       "上游连接/传输故障 (502 Bad Gateway)",
		FocusPrompt: "Prioritize checking whether upstream Pods are down, restarting, OOMKilled, or failing probes via read_k8s (pods, events, deployment, logs), then correlate recent errors via read_observability.",
	},
	incident.TypeBreakerOpen: {
		Type:        incident.TypeBreakerOpen,
		Title:       "路由熔断器开启 (Circuit Breaker Open)",
		FocusPrompt: "Inspect breaker status and incident windows via read_gateway first, then trace the underlying upstream failures via read_observability (traffic_stats, error_logs) and upstream Pod health via read_k8s.",
	},
	incident.TypeUpstream5xx: {
		Type:        incident.TypeUpstream5xx,
		Title:       "上游 5xx 错误突增",
		FocusPrompt: "Prioritize inspecting captured response bodies and error_logs via read_observability along with upstream container logs via read_k8s to locate application exceptions or downstream dependency failures.",
	},
	incident.TypeConcurrencyLimit: {
		Type:        incident.TypeConcurrencyLimit,
		Title:       "路由并发舱壁打满 (429 Concurrency Limit)",
		FocusPrompt: "Determine whether the upstream is slowed down or saturated (latency/Prometheus/Pods/deployments via read_observability and read_k8s) or whether sudden client traffic spikes caused in-flight exhaustion.",
	},
	incident.TypeRateLimitAbuse: {
		Type:        incident.TypeRateLimitAbuse,
		Title:       "单 IP 高频触发限流异常 (429 Rate Limit Abuse)",
		FocusPrompt: "Inspect the client IP's live token-bucket/blacklist status via read_gateway (ip_status) and request volume/paths via read_observability (top_ips, error_logs). Only block an IP via gateway_block_ip when clear abuse evidence supports it.",
	},
}

// ProfileFor returns the registered investigation profile for eventType, or a
// generic fallback profile for custom or newly introduced trigger types.
func ProfileFor(eventType incident.EventType) InvestigationProfile {
	if profile, ok := builtinProfiles[eventType]; ok {
		return profile
	}
	return InvestigationProfile{
		Type:        eventType,
		Title:       fmt.Sprintf("网关异常事件 (%s)", eventType),
		FocusPrompt: "Gather evidence across read_observability, read_gateway, and read_k8s according to the event summary, scope_key, and details.",
	}
}
