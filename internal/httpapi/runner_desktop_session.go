package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Probe sessions have their own token prefix. Apply the route boundary even
// in local installations where interactive management authentication is off.
func limitDesktopProbeSession(c *gin.Context) {
	raw, bearer := bearerToken(c.Request)
	if !bearer || !strings.HasPrefix(raw, desktopProbeSessionTokenPrefix) {
		c.Next()
		return
	}
	route := strings.TrimPrefix(c.FullPath(), "/api")
	switch c.Request.Method + " " + route {
	case "POST /runners/deregister", "POST /runners/disconnect", "POST /runners/heartbeat", "POST /runners/tasks/claim",
		"GET /runners/tasks/:id", "GET /runners/tasks/:id/control", "GET /runners/tasks/:id/events",
		"POST /runners/tasks/:id/start", "POST /runners/tasks/:id/lease/renew",
		"POST /runners/tasks/:id/events", "POST /runners/tasks/:id/complete":
		c.Next()
	default:
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "此操作超出桌面探测会话的权限"})
	}
}

// The self-revocation endpoint takes no target. Reject supplied selectors
// rather than accidentally unbinding the caller when another ID was intended.
func validDesktopDeregisterRequest(c *gin.Context) bool {
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 512))
	if err == nil && c.Request.URL.RawQuery == "" {
		if len(bytes.TrimSpace(body)) == 0 {
			return true
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) == nil && fields != nil && len(fields) == 0 {
			return true
		}
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": "桌面解绑仅接受本机会话，无需指定设备"})
	return false
}
