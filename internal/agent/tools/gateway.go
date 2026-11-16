package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oxkrypton/Halon/internal/agent"
)

func NewGatewayActionTool(kubernetes *KubernetesToolset, adminBaseURL, adminToken string, client *http.Client) agent.Tool {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return agent.Tool{
		Name: "gateway_block_ip", Level: agent.LevelOperator,
		Description: "Add one IP to the live gateway blacklist. Use only when query_top_ips or error logs show abuse; this does not fix ordinary upstream timeouts.",
		Parameters:  objectSchema(map[string]any{"ip": stringProperty("IPv4 or IPv6 address supported by request evidence."), "reason": stringProperty("Evidence-based blocking reason.")}, "ip", "reason"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			input, err := decodeArguments[struct {
				IP     string `json:"ip"`
				Reason string `json:"reason"`
			}](raw)
			if err != nil {
				return nil, err
			}
			parsed := net.ParseIP(input.IP)
			if parsed == nil || strings.TrimSpace(input.Reason) == "" {
				return nil, fmt.Errorf("valid ip and reason are required")
			}
			if strings.TrimSpace(adminBaseURL) == "" {
				return nil, fmt.Errorf("GATEWAY_ADMIN_URL is not configured")
			}
			ip := parsed.String()
			if err := kubernetes.allowAction("block-ip/" + ip); err != nil {
				return nil, err
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(adminBaseURL, "/")+"/admin/rules/blacklist/"+url.PathEscape(ip), nil)
			if err != nil {
				return nil, err
			}
			request.Header.Set("Authorization", "Bearer "+adminToken)
			response, err := client.Do(request)
			if err != nil {
				return nil, err
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				return nil, fmt.Errorf("gateway returned %s: %s", response.Status, strings.TrimSpace(string(body)))
			}
			return map[string]any{"status": "blocked", "ip": ip, "reason": input.Reason}, nil
		},
	}
}
