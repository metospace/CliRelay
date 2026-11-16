package config

import (
	"strings"
	"time"
)

// DefaultAccountConcurrencyWaitSeconds is how long a request waits for a busy
// account when the config omits the field. Queuing is the default because a
// per-account limit is meant to pace traffic, not to reject it: without a wait
// the request after the limit fails even though the account frees up moments later.
const DefaultAccountConcurrencyWaitSeconds = 30

// AccountConcurrencyConfig controls what happens once an AI account reaches the
// `concurrency_limit` recorded on its auth file.
type AccountConcurrencyConfig struct {
	// WaitTimeoutSeconds is how long a request may queue for a saturated account
	// before giving up with a 429. Omit it for the default; set 0 to disable
	// queuing and fail as soon as every candidate account is busy.
	WaitTimeoutSeconds *int `yaml:"wait-timeout-seconds,omitempty" json:"wait-timeout-seconds,omitempty"`

	// MaxQueueDepth caps how many requests may queue per account. 0 means
	// unbounded; set it when a stalled upstream should shed load instead of
	// letting callers pile up behind it.
	MaxQueueDepth int `yaml:"max-queue-depth,omitempty" json:"max-queue-depth,omitempty"`

	// ConcurrencyOverflowModel, when set, rewrites a request to this model
	// when every candidate account for the original model is already at
	// capacity. Empty disables overflow. The original model's queue is used
	// only when the overflow account is also full or the request is ineligible.
	ConcurrencyOverflowModel string `yaml:"concurrency-overflow-model,omitempty" json:"concurrency-overflow-model,omitempty"`
}

// WaitTimeout resolves the configured queue timeout. A zero duration disables queuing.
func (c AccountConcurrencyConfig) WaitTimeout() time.Duration {
	seconds := DefaultAccountConcurrencyWaitSeconds
	if c.WaitTimeoutSeconds != nil {
		seconds = *c.WaitTimeoutSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// QueueDepth resolves the per-account queue cap, treating negatives as unbounded.
func (c AccountConcurrencyConfig) QueueDepth() int {
	if c.MaxQueueDepth < 0 {
		return 0
	}
	return c.MaxQueueDepth
}

// OverflowModel returns the configured concurrency overflow target, or empty
// when overflow is disabled.
func (c AccountConcurrencyConfig) OverflowModel() string {
	return strings.TrimSpace(c.ConcurrencyOverflowModel)
}
