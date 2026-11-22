package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

type namedBlockingExecutor struct {
	blockingExecutor
	id string
}

func (e *namedBlockingExecutor) Identifier() string { return e.id }

type overflowRecordingExecutor struct {
	id          string
	calls       atomic.Int32
	lastAuthID  atomic.Value
	lastModel   atomic.Value
	lastPayload atomic.Value
}

func (e *overflowRecordingExecutor) Identifier() string { return e.id }

func (e *overflowRecordingExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	if auth != nil {
		e.lastAuthID.Store(auth.ID)
	}
	e.lastModel.Store(req.Model)
	e.lastPayload.Store(append([]byte(nil), req.Payload...))
	return cliproxyexecutor.Response{Payload: []byte("overflow-ok")}, nil
}

func (e *overflowRecordingExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	resp, err := e.Execute(ctx, auth, req, opts)
	if err != nil {
		return nil, err
	}
	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	chunks <- cliproxyexecutor.StreamChunk{Payload: resp.Payload}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Headers: http.Header{}, Chunks: chunks}, nil
}

func (e *overflowRecordingExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e *overflowRecordingExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return e.Execute(ctx, auth, req, opts)
}

func (e *overflowRecordingExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("not implemented")
}

func (e *overflowRecordingExecutor) model() string {
	value, _ := e.lastModel.Load().(string)
	return value
}

func (e *overflowRecordingExecutor) payload() []byte {
	value, _ := e.lastPayload.Load().([]byte)
	return value
}

func overflowTestAuth(id, provider, label string, limit int) *Auth {
	return &Auth{
		ID:         id,
		Provider:   provider,
		Label:      label,
		Status:     StatusActive,
		Attributes: map[string]string{"concurrency_limit": fmt.Sprintf("%d", limit)},
	}
}

func newOverflowTestManager(t *testing.T, overflowModel string, waitTimeout time.Duration, auths ...*Auth) (*Manager, *namedBlockingExecutor, *overflowRecordingExecutor) {
	t.Helper()
	mgr := NewManager(nil, &FillFirstSelector{}, nil)
	mgr.SetAccountConcurrencyConfig(waitTimeout, 0)
	mgr.SetConcurrencyOverflowModel(overflowModel)
	for _, auth := range auths {
		if _, err := mgr.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	codexExec := &namedBlockingExecutor{
		blockingExecutor: blockingExecutor{
			started: make(chan struct{}, 16),
			release: make(chan struct{}),
		},
		id: "codex",
	}
	xaiExec := &overflowRecordingExecutor{id: "xai"}
	mgr.RegisterExecutor(codexExec)
	mgr.RegisterExecutor(xaiExec)
	return mgr, codexExec, xaiExec
}

func TestManagerExecute_OverflowsToConfiguredModelWhenCodexIsSaturated(t *testing.T) {
	gpt := overflowTestAuth("gpt-001", "codex", "GPT-001", 1)
	grok := overflowTestAuth("grok-1", "xai", "Grok", 4)
	mgr, gptExec, xaiExec := newOverflowTestManager(t, "grok-4.7", time.Minute, gpt, grok)

	firstDone := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
			Model:   "gpt-6-luna",
			Payload: []byte(`{"model":"gpt-6-luna","input":"hi"}`),
		}, cliproxyexecutor.Options{})
		firstDone <- err
	}()
	<-gptExec.started

	start := time.Now()
	resp, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
		Model:   "gpt-6-luna",
		Payload: []byte(`{"model":"gpt-6-luna","input":"hi"}`),
	}, cliproxyexecutor.Options{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("overflow request failed: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("overflow waited %s instead of running immediately on grok-4.7", elapsed)
	}
	if string(resp.Payload) != "overflow-ok" {
		t.Fatalf("unexpected overflow payload %q", resp.Payload)
	}
	if got := xaiExec.calls.Load(); got != 1 {
		t.Fatalf("expected overflow to reach xAI once, got %d calls", got)
	}
	if xaiExec.model() != "grok-4.7" {
		t.Fatalf("expected rewritten model grok-4.7, got %q", xaiExec.model())
	}
	if gjsonModel := payloadModel(t, xaiExec.payload()); gjsonModel != "grok-4.7" {
		t.Fatalf("expected payload model grok-4.7, got %q", gjsonModel)
	}
	if authID, _ := xaiExec.lastAuthID.Load().(string); authID != grok.ID {
		t.Fatalf("expected overflow onto the Grok account, got %q", authID)
	}
	if depth := mgr.ConcurrencyLimiter().QueueDepth(gpt.ID); depth != 0 {
		t.Fatalf("expected no GPT queue when overflow is available, got depth %d", depth)
	}

	close(gptExec.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("held GPT request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held GPT request never completed")
	}
}

func TestManagerExecute_DoesNotOverflowWhenACodexSlotIsFree(t *testing.T) {
	busy := overflowTestAuth("gpt-001", "codex", "GPT-001", 1)
	idle := overflowTestAuth("gpt-002", "codex", "GPT-002", 1)
	grok := overflowTestAuth("grok-1", "xai", "Grok", 4)
	mgr, gptExec, xaiExec := newOverflowTestManager(t, "grok-4.7", time.Minute, busy, idle, grok)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
	}()
	<-gptExec.started

	done := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
		done <- err
	}()
	<-gptExec.started

	if got := xaiExec.calls.Load(); got != 0 {
		t.Fatalf("expected no overflow while a GPT slot was free, got %d xAI calls", got)
	}
	if inFlight := mgr.ConcurrencyLimiter().GetInFlight(idle.ID) + mgr.ConcurrencyLimiter().GetInFlight(busy.ID); inFlight != 2 {
		t.Fatalf("expected both GPT accounts to be in use, in-flight=%d", inFlight)
	}

	close(gptExec.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("idle GPT request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle GPT request never completed")
	}
}

func TestManagerExecute_QueuesOnCodexWhenOverflowAccountIsSaturated(t *testing.T) {
	gpt := overflowTestAuth("gpt-001", "codex", "GPT-001", 1)
	grok := overflowTestAuth("grok-1", "xai", "Grok", 1)
	mgr, gptExec, xaiExec := newOverflowTestManager(t, "grok-4.7", time.Minute, gpt, grok)

	// Occupy the only Grok slot with a direct xAI request.
	releaseGrok, err := mgr.acquireAccountSlot(grok)
	if err != nil {
		t.Fatalf("occupy grok: %v", err)
	}
	defer releaseGrok()

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
	}()
	<-gptExec.started

	queued := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
		queued <- err
	}()
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), gpt.ID, 1)
	if got := xaiExec.calls.Load(); got != 0 {
		t.Fatalf("expected overflow to be skipped when Grok is full, got %d xAI calls", got)
	}

	close(gptExec.release)
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("queued GPT request failed after a slot freed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued GPT request never resumed")
	}
}

func TestManagerExecute_DoesNotOverflowNonCodexPools(t *testing.T) {
	deepseek := overflowTestAuth("ds-1", "deepseek", "DeepSeek", 1)
	grok := overflowTestAuth("grok-1", "xai", "Grok", 4)
	mgr := NewManager(nil, &FillFirstSelector{}, nil)
	mgr.SetAccountConcurrencyConfig(time.Minute, 0)
	mgr.SetConcurrencyOverflowModel("grok-4.7")
	for _, auth := range []*Auth{deepseek, grok} {
		if _, err := mgr.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
	}
	dsExec := &namedBlockingExecutor{
		blockingExecutor: blockingExecutor{started: make(chan struct{}, 8), release: make(chan struct{})},
		id:               "deepseek",
	}
	xaiExec := &overflowRecordingExecutor{id: "xai"}
	mgr.RegisterExecutor(dsExec)
	mgr.RegisterExecutor(xaiExec)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"deepseek"}, cliproxyexecutor.Request{Model: "deepseek-chat"}, cliproxyexecutor.Options{})
	}()
	<-dsExec.started

	queued := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"deepseek"}, cliproxyexecutor.Request{Model: "deepseek-chat"}, cliproxyexecutor.Options{})
		queued <- err
	}()
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), deepseek.ID, 1)
	if got := xaiExec.calls.Load(); got != 0 {
		t.Fatalf("expected DeepSeek requests not to overflow, got %d xAI calls", got)
	}
	close(dsExec.release)
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("queued DeepSeek request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued DeepSeek request never resumed")
	}
}

func TestManagerExecute_SkipsOverflowForToolRequests(t *testing.T) {
	gpt := overflowTestAuth("gpt-001", "codex", "GPT-001", 1)
	grok := overflowTestAuth("grok-1", "xai", "Grok", 4)
	mgr, gptExec, xaiExec := newOverflowTestManager(t, "grok-4.7", time.Minute, gpt, grok)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
	}()
	<-gptExec.started

	queued := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
			Model:   "gpt-6-luna",
			Payload: []byte(`{"model":"gpt-6-luna","tools":[{"type":"function","name":"lookup"}]}`),
		}, cliproxyexecutor.Options{})
		queued <- err
	}()
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), gpt.ID, 1)
	if got := xaiExec.calls.Load(); got != 0 {
		t.Fatalf("expected tool requests to stay on the GPT queue, got %d xAI calls", got)
	}
	close(gptExec.release)
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("queued tool request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued tool request never resumed")
	}
}

func TestManagerExecute_SkipsOverflowForEncryptedReasoning(t *testing.T) {
	gpt := overflowTestAuth("gpt-001", "codex", "GPT-001", 1)
	grok := overflowTestAuth("grok-1", "xai", "Grok", 4)
	mgr, gptExec, xaiExec := newOverflowTestManager(t, "grok-4.7", time.Minute, gpt, grok)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
	}()
	<-gptExec.started

	queued := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{
			Model:   "gpt-6-luna",
			Payload: []byte(`{"model":"gpt-6-luna","input":[{"type":"reasoning","encrypted_content":"abc"}]}`),
		}, cliproxyexecutor.Options{})
		queued <- err
	}()
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), gpt.ID, 1)
	if got := xaiExec.calls.Load(); got != 0 {
		t.Fatalf("expected encrypted reasoning to stay on the GPT queue, got %d xAI calls", got)
	}
	close(gptExec.release)
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("queued encrypted request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued encrypted request never resumed")
	}
}

func TestManagerExecuteCount_DoesNotOverflow(t *testing.T) {
	gpt := overflowTestAuth("gpt-001", "codex", "GPT-001", 1)
	grok := overflowTestAuth("grok-1", "xai", "Grok", 4)
	mgr, gptExec, xaiExec := newOverflowTestManager(t, "grok-4.7", time.Minute, gpt, grok)

	go func() {
		_, _ = mgr.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
	}()
	<-gptExec.started

	queued := make(chan error, 1)
	go func() {
		_, err := mgr.ExecuteCount(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
		queued <- err
	}()
	waitForQueueDepth(t, mgr.ConcurrencyLimiter(), gpt.ID, 1)
	if got := xaiExec.calls.Load(); got != 0 {
		t.Fatalf("expected token-count requests not to overflow, got %d xAI calls", got)
	}
	close(gptExec.release)
	select {
	case err := <-queued:
		if err != nil {
			t.Fatalf("queued count request failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued count request never resumed")
	}
}

func TestPayloadBlocksConcurrencyOverflow(t *testing.T) {
	if payloadBlocksConcurrencyOverflow([]byte(`{"model":"gpt-6-luna","input":"hi"}`)) {
		t.Fatal("plain text payload should be eligible for overflow")
	}
	if !payloadBlocksConcurrencyOverflow([]byte(`{"tools":[{"type":"function"}]}`)) {
		t.Fatal("tools should block overflow")
	}
	if !payloadBlocksConcurrencyOverflow([]byte(`{"previous_response_id":"resp_123"}`)) {
		t.Fatal("previous_response_id should block overflow")
	}
	if !payloadBlocksConcurrencyOverflow([]byte(`{"input":[{"type":"function_call","name":"lookup"}]}`)) {
		t.Fatal("function_call history should block overflow")
	}
	if !payloadBlocksConcurrencyOverflow([]byte(`{"input":[{"type":"reasoning","encrypted_content":"abc"}]}`)) {
		t.Fatal("encrypted_content should block overflow")
	}
}

func TestManagerExecute_OverflowDisabledLeavesQueueBehavior(t *testing.T) {
	gpt := overflowTestAuth("gpt-001", "codex", "GPT-001", 1)
	grok := overflowTestAuth("grok-1", "xai", "Grok", 4)
	mgr, gptExec, xaiExec := newOverflowTestManager(t, "", 80*time.Millisecond, gpt, grok)
	defer close(gptExec.release)

	go func() {
		_, _ = mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
	}()
	<-gptExec.started

	_, err := mgr.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-6-luna"}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected the queued request to time out when overflow is disabled")
	}
	if !errors.Is(err, ErrAccountConcurrencyExceeded) {
		t.Fatalf("expected ErrAccountConcurrencyExceeded, got %v", err)
	}
	if got := xaiExec.calls.Load(); got != 0 {
		t.Fatalf("disabled overflow still reached xAI %d times", got)
	}
}

func payloadModel(t *testing.T, payload []byte) string {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err != nil {
		t.Fatalf("payload %q is not JSON: %v", payload, err)
	}
	model, _ := parsed["model"].(string)
	return model
}
