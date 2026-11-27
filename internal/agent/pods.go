package agent

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const podQueryTimeout = 10 * time.Second

var (
	// ErrNamespaceForbidden indicates the requested namespace is outside the allowlist.
	ErrNamespaceForbidden = errors.New("namespace is not in the agent allowlist")
	// ErrPodNotFound indicates the target pod does not exist in the namespace.
	ErrPodNotFound = errors.New("pod not found")
	// ErrInvalidPodRequest indicates malformed pod or log query parameters.
	ErrInvalidPodRequest = errors.New("invalid pod log request")
)

// PodSummary describes a Kubernetes pod and its container states.
type PodSummary struct {
	Name       string           `json:"name"`
	Phase      string           `json:"phase"`
	Ready      bool             `json:"ready"`
	Restarts   int32            `json:"restarts"`
	Node       string           `json:"node"`
	CreatedAt  time.Time        `json:"created_at"`
	Containers []ContainerState `json:"containers"`
}

// ContainerState describes the status of a single container within a pod.
type ContainerState struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int32  `json:"restart_count"`
	State        string `json:"state"`
	Reason       string `json:"reason"`
}

// PodLogOptions configures a bounded pod log read.
type PodLogOptions struct {
	Namespace    string
	Pod          string
	Container    string
	TailLines    int
	SinceSeconds int
	Previous     bool
}

// PodLogResult holds the bounded log payload returned for a pod container.
type PodLogResult struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Previous  bool   `json:"previous"`
	Logs      string `json:"logs"`
}

// PodLogReader is the read-only Kubernetes contract used by the Agent HTTP API.
type PodLogReader interface {
	AllowedNamespaces() []string
	ListPods(ctx context.Context, namespace, labelSelector string, limit int) (string, []PodSummary, error)
	GetPodLogs(ctx context.Context, options PodLogOptions) (PodLogResult, error)
}

func listPods(pods PodLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if pods == nil {
			abortWithAPIError(c, http.StatusServiceUnavailable, "K8S_UNAVAILABLE", "kubernetes client is not configured")
			return
		}
		namespace := strings.TrimSpace(c.Query("namespace"))
		labelSelector := strings.TrimSpace(c.Query("label_selector"))
		limit := 100
		if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 100 {
				abortWithAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "limit must be an integer between 1 and 100")
				return
			}
			limit = parsed
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), podQueryTimeout)
		defer cancel()
		resolvedNS, items, err := pods.ListPods(ctx, namespace, labelSelector, limit)
		if err != nil {
			handlePodQueryError(c, err, "failed to list pods")
			return
		}
		if items == nil {
			items = []PodSummary{}
		}
		namespaces := pods.AllowedNamespaces()
		if namespaces == nil {
			namespaces = []string{}
		}
		c.JSON(http.StatusOK, gin.H{
			"namespace":  resolvedNS,
			"namespaces": namespaces,
			"pods":       items,
		})
	}
}

func getPodLogs(pods PodLogReader) gin.HandlerFunc {
	return func(c *gin.Context) {
		if pods == nil {
			abortWithAPIError(c, http.StatusServiceUnavailable, "K8S_UNAVAILABLE", "kubernetes client is not configured")
			return
		}
		pod := strings.TrimSpace(c.Param("pod"))
		if pod == "" || len(pod) > 253 || strings.ContainsAny(pod, "/ \t\r\n") {
			abortWithAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid pod name")
			return
		}
		namespace := strings.TrimSpace(c.Query("namespace"))
		container := strings.TrimSpace(c.Query("container"))
		if len(container) > 253 || strings.ContainsAny(container, " \t\r\n") {
			abortWithAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "invalid container name")
			return
		}

		tailLines := 200
		if raw := strings.TrimSpace(c.Query("tail_lines")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 1000 {
				abortWithAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "tail_lines must be an integer between 1 and 1000")
				return
			}
			tailLines = parsed
		}

		sinceSeconds := 900
		if raw := strings.TrimSpace(c.Query("since_seconds")); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 1 || parsed > 86400 {
				abortWithAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "since_seconds must be an integer between 1 and 86400")
				return
			}
			sinceSeconds = parsed
		}

		previous := false
		if raw := strings.TrimSpace(c.Query("previous")); raw != "" {
			parsed, err := strconv.ParseBool(raw)
			if err != nil {
				abortWithAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", "previous must be a boolean")
				return
			}
			previous = parsed
		}

		ctx, cancel := context.WithTimeout(c.Request.Context(), podQueryTimeout)
		defer cancel()
		result, err := pods.GetPodLogs(ctx, PodLogOptions{
			Namespace:    namespace,
			Pod:          pod,
			Container:    container,
			TailLines:    tailLines,
			SinceSeconds: sinceSeconds,
			Previous:     previous,
		})
		if err != nil {
			handlePodQueryError(c, err, "failed to fetch pod logs")
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

func handlePodQueryError(c *gin.Context, err error, fallbackMessage string) {
	switch {
	case errors.Is(err, ErrNamespaceForbidden):
		abortWithAPIError(c, http.StatusForbidden, "NAMESPACE_FORBIDDEN", err.Error())
	case errors.Is(err, ErrPodNotFound):
		abortWithAPIError(c, http.StatusNotFound, "POD_NOT_FOUND", err.Error())
	case errors.Is(err, ErrInvalidPodRequest):
		abortWithAPIError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error())
	default:
		abortWithAPIError(c, http.StatusInternalServerError, "QUERY_FAILED", fallbackMessage)
	}
}
