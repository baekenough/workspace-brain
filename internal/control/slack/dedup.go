package slack

import (
	"sync"
	"time"
)

// dedupTTL bounds how long a request key is remembered. It matches the
// signature timestamp validity window enforced in verify(), so a genuinely
// replayed request cannot outlive both defenses.
const dedupTTL = 5 * time.Minute

// dedupCache remembers recently processed request keys so that Slack's
// at-least-once retry delivery (signaled by X-Slack-Retry-Num /
// X-Slack-Retry-Reason, but not required to be present) does not trigger
// duplicate side effects such as double tenant creation or duplicate ingest
// jobs. It is intentionally process-local and in-memory: a single control
// plane instance is enough to absorb Slack's short retry window.
type dedupCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	now  func() time.Time
	seen map[string]time.Time
}

// newDedupCache creates a dedupCache that forgets keys after ttl.
func newDedupCache(ttl time.Duration) *dedupCache {
	return &dedupCache{ttl: ttl, now: time.Now, seen: make(map[string]time.Time)}
}

// seenRecently reports whether key was already recorded within ttl. It
// records key as seen when this is the first observation, so the first
// caller for a given key always gets false (proceed) and every subsequent
// caller within ttl gets true (skip). Expired entries are swept
// opportunistically on each call to bound memory growth.
func (c *dedupCache) seenRecently(key string) bool {
	if c == nil || key == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now
	if now == nil {
		now = time.Now
	}
	nowTime := now()
	for k, seenAt := range c.seen {
		if nowTime.Sub(seenAt) > c.ttl {
			delete(c.seen, k)
		}
	}
	if seenAt, ok := c.seen[key]; ok && nowTime.Sub(seenAt) <= c.ttl {
		return true
	}
	c.seen[key] = nowTime
	return false
}
