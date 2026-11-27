package gateway

import (
	"github.com/gin-gonic/gin"
)

type errorResponse struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func abortWithError(c *gin.Context, httpStatus int, code, message string) {
	c.AbortWithStatusJSON(httpStatus, errorResponse{Error: errorDetail{
		Code:      code,
		Message:   message,
		RequestID: requestIDFromContext(c),
	}})
}

const (
	// gatewayIncidentTypeKey records the classified incident type for a failed
	// request; the access log middleware turns it into an incident event.
	gatewayIncidentTypeKey = "gateway_incident_type"
	// gatewayIncidentDetailKey optionally carries short diagnostic context for
	// incident details.
	gatewayIncidentDetailKey = "gateway_incident_detail"
)
