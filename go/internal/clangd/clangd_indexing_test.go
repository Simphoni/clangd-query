package clangd

import (
	"testing"
	"time"
)

// Covers the non-blocking indexing status used by the daemon to answer
// "still indexing" immediately while clangd builds its initial index.
func TestIndexingStatus(t *testing.T) {
	t.Run("reports running and elapsed time", func(t *testing.T) {
		client := &ClangdClient{
			indexingDone:  make(chan struct{}),
			isIndexing:    true,
			indexingSince: time.Now().Add(-2 * time.Second),
		}

		done, elapsed := client.IndexingStatus()
		if done {
			t.Fatal("expected indexing to be reported as not done")
		}
		if elapsed < 2*time.Second {
			t.Fatalf("expected at least 2s elapsed, got %v", elapsed)
		}
	})

	t.Run("reports done once indexing finished", func(t *testing.T) {
		client := &ClangdClient{
			indexingDone: make(chan struct{}),
			isIndexing:   false,
		}
		close(client.indexingDone)

		done, elapsed := client.IndexingStatus()
		if !done {
			t.Fatal("expected indexing to be reported as done")
		}
		if elapsed != 0 {
			t.Fatalf("expected zero elapsed without a start time, got %v", elapsed)
		}
	})
}
