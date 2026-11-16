package auth

import (
	"context"
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	concurrencyOverflowReason          = "gpt_concurrency_full"
	concurrencyOverflowSourceProvider  = "codex"
	concurrencyOverflowDefaultProvider = "xai"
)

// tryConcurrencyOverflow rewrites a saturated Codex/GPT request onto the
// configured overflow model when that model's account can take a slot
// immediately. Callers must fall back to the original queue-then-429 path
// when this returns ok=false.
func (s executionService) tryConcurrencyOverflow(ctx context.Context, scope *mixedExecutionScope, req cliproxyexecutor.Request) (*mixedExecutionCandidate, func(), bool) {
	if s.manager == nil || scope == nil || scope.skipConcurrencyOverflow || scope.overflowAttempted || scope.singlePickRoute {
		return nil, nil, false
	}
	overflowModel := s.manager.concurrencyOverflowModel()
	if overflowModel == "" || strings.EqualFold(overflowModel, strings.TrimSpace(scope.routeModel)) {
		return nil, nil, false
	}
	if !saturatedCandidatesAreOverflowSource(scope.saturated) {
		return nil, nil, false
	}
	if requestBlocksConcurrencyOverflow(req, scope.opts) {
		return nil, nil, false
	}

	scope.overflowAttempted = true
	originalModel := scope.routeModel
	originalProviders := scope.providers
	scope.originalRouteModel = originalModel
	scope.originalProviders = originalProviders
	scope.routeModel = overflowModel
	scope.providers = []string{concurrencyOverflowDefaultProvider}

	candidate, errPick := s.nextMixedCandidate(ctx, scope, req)
	if errPick != nil || candidate == nil {
		restoreConcurrencyOverflowScope(scope)
		return nil, nil, false
	}
	candidate.execReq = rewriteRequestModelPayload(candidate.execReq, overflowModel)

	if errModeration := s.manager.moderateRequest(candidate.execCtx, candidate.auth, scope.opts); errModeration != nil {
		restoreConcurrencyOverflowScope(scope)
		return nil, nil, false
	}
	releaseSlot, errSlot := s.manager.acquireAccountSlot(candidate.auth)
	if errSlot != nil {
		restoreConcurrencyOverflowScope(scope)
		return nil, nil, false
	}

	scope.overflowApplied = true
	logEntryWithRequestID(ctx).WithFields(log.Fields{
		"requested_model": originalModel,
		"overflow_model":  overflowModel,
		"reason":          concurrencyOverflowReason,
		"provider":        candidate.provider,
		"account":         overflowAccountName(candidate.auth),
	}).Info("concurrency overflow")
	return candidate, releaseSlot, true
}

func restoreConcurrencyOverflowScope(scope *mixedExecutionScope) {
	if scope == nil || (!scope.overflowApplied && scope.originalRouteModel == "" && scope.originalProviders == nil) {
		return
	}
	if scope.originalRouteModel != "" || scope.overflowApplied {
		if scope.originalRouteModel != "" {
			scope.routeModel = scope.originalRouteModel
		}
		if scope.originalProviders != nil {
			scope.providers = scope.originalProviders
		}
	}
	scope.overflowApplied = false
}

func saturatedCandidatesAreOverflowSource(candidates []*mixedExecutionCandidate) bool {
	if len(candidates) == 0 {
		return false
	}
	for _, candidate := range candidates {
		if candidate == nil || candidate.auth == nil {
			return false
		}
		if !strings.EqualFold(strings.TrimSpace(candidate.auth.Provider), concurrencyOverflowSourceProvider) {
			return false
		}
	}
	return true
}

func requestBlocksConcurrencyOverflow(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	if strings.TrimSpace(opts.Alt) != "" {
		return true
	}
	return payloadBlocksConcurrencyOverflow(req.Payload) || payloadBlocksConcurrencyOverflow(opts.OriginalRequest)
}

func payloadBlocksConcurrencyOverflow(payload []byte) bool {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) != "" {
		return true
	}
	if tools := gjson.GetBytes(payload, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		return true
	}
	if functions := gjson.GetBytes(payload, "functions"); functions.IsArray() && len(functions.Array()) > 0 {
		return true
	}
	blocked := false
	var walk func(gjson.Result)
	walk = func(value gjson.Result) {
		if blocked {
			return
		}
		if value.IsArray() {
			for _, item := range value.Array() {
				walk(item)
				if blocked {
					return
				}
			}
			return
		}
		if !value.IsObject() {
			return
		}
		if strings.TrimSpace(value.Get("encrypted_content").String()) != "" {
			blocked = true
			return
		}
		switch strings.TrimSpace(value.Get("type").String()) {
		case "function_call", "function_call_output", "tool_use", "tool_result", "computer_call", "computer_call_output", "image_generation_call", "custom_tool_call", "custom_tool_call_output":
			blocked = true
			return
		}
		value.ForEach(func(_, item gjson.Result) bool {
			walk(item)
			return !blocked
		})
	}
	walk(gjson.ParseBytes(payload))
	return blocked
}

func rewriteRequestModelPayload(req cliproxyexecutor.Request, model string) cliproxyexecutor.Request {
	model = strings.TrimSpace(model)
	req.Model = model
	if model == "" || len(req.Payload) == 0 || !gjson.GetBytes(req.Payload, "model").Exists() {
		return req
	}
	updated, err := sjson.SetBytes(req.Payload, "model", model)
	if err != nil {
		return req
	}
	req.Payload = updated
	return req
}

func overflowAccountName(auth *Auth) string {
	if auth == nil {
		return ""
	}
	if label := strings.TrimSpace(auth.Label); label != "" {
		return label
	}
	if index := strings.TrimSpace(auth.Index); index != "" {
		return index
	}
	return strings.TrimSpace(auth.Provider)
}
