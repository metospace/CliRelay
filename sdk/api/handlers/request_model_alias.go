package handlers

import (
	"context"

	"github.com/gin-gonic/gin"
)

const RequestModelResolverContextKey = "cliproxy.request_model_resolver"
const RequestModelResolutionContextKey = "cliproxy.model_resolution"

type RequestModelResolution struct {
	RequestedModel string `json:"requested_model"`
	Model          string `json:"resolved_model"`
	Reason         string `json:"reason"`
}

// RequestModelResolver shares the server's HTTP admission policy for aliases
// and text-model-fallback. failureReason is set only for a classified
// model/route failure before output.
type RequestModelResolver func(model string, payload []byte, failureReason string) RequestModelResolution

func RequestModelResolverFromGin(c *gin.Context) RequestModelResolver {
	if c == nil {
		return nil
	}
	value, _ := c.Get(RequestModelResolverContextKey)
	resolver, _ := value.(RequestModelResolver)
	return resolver
}

// CanResolveRequestModel mirrors execution's provider/group/prefix resolution.
func (h *BaseAPIHandler) CanResolveRequestModel(ctx context.Context, model string) bool {
	_, ok := h.ResolveRequestRoutingModel(ctx, model)
	return ok
}

// ResolveRequestRoutingModel includes auto resolution and preserves the same
// group/model normalization used by execution.
func (h *BaseAPIHandler) ResolveRequestRoutingModel(ctx context.Context, model string) (string, bool) {
	if h == nil {
		return "", false
	}
	_, normalized, err := h.getRequestDetails(ctx, model)
	return normalized, err == nil
}

func RememberRequestModelResolution(c *gin.Context, resolution RequestModelResolution) {
	if c == nil || resolution.Reason == "" || resolution.Model == resolution.RequestedModel {
		return
	}
	if previous, ok := c.Get(RequestModelResolutionContextKey); ok {
		if first, ok := previous.(RequestModelResolution); ok && first.RequestedModel != "" {
			resolution.RequestedModel = first.RequestedModel
		}
	}
	c.Set(RequestModelResolutionContextKey, resolution)
	ApplyRequestModelResolutionHeaders(c)
}

// ApplyRequestModelResolutionHeaders is called after the body has been rewritten
// so the client can see the model that was actually admitted.
func ApplyRequestModelResolutionHeaders(c *gin.Context) {
	if c == nil {
		return
	}
	raw, _ := c.Get(RequestModelResolutionContextKey)
	resolution, ok := raw.(RequestModelResolution)
	if !ok {
		return
	}
	if c.Writer != nil && !c.Writer.Written() {
		c.Header("X-Clirelay-Resolved-Model", resolution.Model)
		c.Header("X-Clirelay-Model-Fallback", resolution.Reason)
	}
}
