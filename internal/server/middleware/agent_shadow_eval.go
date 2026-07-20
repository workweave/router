package middleware

import (
	"context"
	"net/http"
	"strings"

	"workweave/router/internal/proxy"

	"github.com/gin-gonic/gin"
)

// WithAgentShadowEvaluation validates the evaluation header triplet; partial headers fail closed, absent headers pass through.
func WithAgentShadowEvaluation() gin.HandlerFunc {
	return func(c *gin.Context) {
		model := strings.TrimSpace(c.GetHeader(proxy.AgentShadowModelHeader))
		rollout := strings.TrimSpace(c.GetHeader(proxy.AgentShadowRolloutHeader))
		stateID := strings.TrimSpace(c.GetHeader(proxy.AgentShadowStateHeader))
		present := model != "" || rollout != "" || stateID != ""
		if !present {
			c.Next()
			return
		}
		if model == "" || rollout == "" || stateID == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "agent_shadow_eval_headers_incomplete"})
			return
		}
		installation := InstallationFrom(c)
		if installation == nil || !installation.PolicyHeaderOverridesEnabled {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "agent_shadow_eval_not_authorized"})
			return
		}
		ctx := context.WithValue(c.Request.Context(), proxy.AgentShadowEvalContextKey{}, proxy.AgentShadowEvaluation{
			Model: model, RolloutID: rollout, StateID: stateID,
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}
