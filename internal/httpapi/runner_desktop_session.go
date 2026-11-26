package httpapi

import (
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
	case "POST /runners/disconnect", "POST /runners/heartbeat", "POST /runners/tasks/claim",
		"GET /runners/tasks/:id", "GET /runners/tasks/:id/control", "GET /runners/tasks/:id/events",
		"POST /runners/tasks/:id/start", "POST /runners/tasks/:id/lease/renew",
		"POST /runners/tasks/:id/events", "POST /runners/tasks/:id/complete":
		c.Next()
	default:
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "此操作超出桌面探测会话的权限"})
	}
}
