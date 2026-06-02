package queue

import (
	"sync"
	"testing"
)

func TestNewID_ConcurrentUniqueness(t *testing.T) {
	const n = 10_000
	ids := make([]string, n)

	var wg sync.WaitGroup
	wg.Add(n)

	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			ids[idx] = NewID()
		}(i)
	}

	wg.Wait()

	seen := make(map[string]struct{}, n)
	for _, id := range ids {
		if id == "" {
			t.Fatal("generated empty ID")
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("duplicate ID detected: %s", id)
		}
		seen[id] = struct{}{}
	}

	t.Logf("successfully generated %d unique ULIDs with zero duplicates", n)
}

func TestNewID_Format(t *testing.T) {
	id := NewID()
	// ULIDs are 26 characters, Crockford's Base32 encoded
	if len(id) != 26 {
		t.Fatalf("expected ULID length 26, got %d: %q", len(id), id)
	}
}
