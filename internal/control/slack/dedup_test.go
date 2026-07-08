package slack

import (
	"testing"
	"time"
)

func TestDedupCacheSeenRecently(t *testing.T) {
	t.Parallel()
	c := newDedupCache(5 * time.Minute)
	if c.seenRecently("key-1") {
		t.Fatal("first observation should not be reported as seen")
	}
	if !c.seenRecently("key-1") {
		t.Fatal("second observation within ttl should be reported as seen")
	}
	if c.seenRecently("key-2") {
		t.Fatal("a distinct key should not be reported as seen")
	}
}

func TestDedupCacheExpiresEntries(t *testing.T) {
	t.Parallel()
	now := time.Unix(1782194400, 0)
	c := newDedupCache(time.Minute)
	c.now = func() time.Time { return now }

	if c.seenRecently("key-1") {
		t.Fatal("first observation should not be reported as seen")
	}

	// Advance beyond ttl: the earlier key must both expire (be swept) and no
	// longer be reported as seen.
	now = now.Add(2 * time.Minute)
	if c.seenRecently("key-1") {
		t.Fatal("expired key should not be reported as seen")
	}
	if len(c.seen) != 1 {
		t.Fatalf("expired entries should be swept, len(seen) = %d", len(c.seen))
	}
}

func TestDedupCacheNilReceiverAndEmptyKey(t *testing.T) {
	t.Parallel()
	var c *dedupCache
	if c.seenRecently("anything") {
		t.Fatal("nil cache must never report seen")
	}

	c = newDedupCache(time.Minute)
	if c.seenRecently("") {
		t.Fatal("empty key must never report seen")
	}
	if _, ok := c.seen[""]; ok {
		t.Fatal("empty key must not be recorded")
	}
}

func TestDedupCacheDefaultsNowWhenUnset(t *testing.T) {
	t.Parallel()
	c := &dedupCache{ttl: time.Minute, seen: make(map[string]time.Time)}
	if c.seenRecently("key-1") {
		t.Fatal("first observation should not be reported as seen")
	}
	if !c.seenRecently("key-1") {
		t.Fatal("second observation should be reported as seen")
	}
}
