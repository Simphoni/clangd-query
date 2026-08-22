package daemon

import (
	"os"
	"strings"
	"testing"
	"time"

	"clangd-query/internal/logger"
)

// writeFile creates or overwrites a file with trivial content.
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("// test\n"), 0644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

// removeFile deletes a file, failing the test if it cannot.
func removeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("failed to remove %s: %v", path, err)
	}
}

// readFile is os.ReadFile wrapped for brevity in polling helpers where the
// file may legitimately not exist yet.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// contains reports whether items contains item.
func contains(items []string, item string) bool {
	for _, s := range items {
		if s == item {
			return true
		}
	}
	return false
}

// Starts a watcher on a fresh temporary project tree with a short source file
// already present, and returns the watcher together with a channel that
// delivers the debounced change batches.
func startTestWatcher(t *testing.T) (*FileWatcher, string, chan FileChanges) {
	t.Helper()

	dir := t.TempDir()
	changes := make(chan FileChanges, 10)

	fw, err := NewFileWatcher(dir, func(c FileChanges) { changes <- c }, &logger.NullLogger{})
	if err != nil {
		t.Fatalf("failed to create watcher: %v", err)
	}
	t.Cleanup(func() { fw.Stop() })

	return fw, dir, changes
}

// Waits for the next change batch or fails the test after a generous timeout.
func nextChanges(t *testing.T, changes chan FileChanges) FileChanges {
	t.Helper()

	select {
	case c := <-changes:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for file change batch")
		return FileChanges{}
	}
}

// Waits until cond reports true, polling briefly, and fails after timeout.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Covers the classification of raw file system events: creating a C++ file is
// a set change, writing to an existing one is a content change, and deleting
// one is counted as a removal. These distinctions drive whether clangd is
// asked to re-index, to open a new file, or the generate command is re-run.
func TestFileWatcherEventClassification(t *testing.T) {
	_, dir, changes := startTestWatcher(t)

	t.Run("creation is reported as created", func(t *testing.T) {
		writeFile(t, dir+"/new_file.cpp")
		c := nextChanges(t, changes)
		if !contains(c.Created, dir+"/new_file.cpp") {
			t.Fatalf("expected new_file.cpp in Created, got %+v", c)
		}
		if len(c.Changed) != 0 || c.RemovedCount != 0 {
			t.Fatalf("unexpected extra changes: %+v", c)
		}
	})

	t.Run("write is reported as changed", func(t *testing.T) {
		path := dir + "/existing.cpp"
		writeFile(t, path)
		nextChanges(t, changes) // creation batch

		writeFile(t, path)
		c := nextChanges(t, changes)
		if !contains(c.Changed, path) {
			t.Fatalf("expected existing.cpp in Changed, got %+v", c)
		}
		if len(c.Created) != 0 {
			t.Fatalf("unexpected creations: %+v", c)
		}
	})

	t.Run("removal is reported as removed", func(t *testing.T) {
		path := dir + "/doomed.cpp"
		writeFile(t, path)
		nextChanges(t, changes) // creation batch

		removeFile(t, path)
		c := nextChanges(t, changes)
		if c.RemovedCount != 1 {
			t.Fatalf("expected RemovedCount 1, got %+v", c)
		}
	})

	t.Run("create then remove within one window cancels out", func(t *testing.T) {
		path := dir + "/temp.cpp"
		writeFile(t, path)
		removeFile(t, path)

		select {
		case c := <-changes:
			t.Fatalf("expected no batch for transient file, got %+v", c)
		case <-time.After(2 * time.Second):
		}
	})
}

// Covers the debounce behavior: a burst of operations must collapse into a
// single callback invocation.
func TestFileWatcherDebouncesBursts(t *testing.T) {
	_, dir, changes := startTestWatcher(t)

	for _, name := range []string{"a.cpp", "b.cpp", "c.cpp"} {
		writeFile(t, dir+"/"+name)
	}

	c := nextChanges(t, changes)
	if len(c.Created) != 3 {
		t.Fatalf("expected 3 creations in one batch, got %+v", c)
	}

	select {
	case c := <-changes:
		t.Fatalf("expected single batch for burst, got extra: %+v", c)
	case <-time.After(2 * time.Second):
	}
}

// Covers the reconfigurer's run semantics: a trigger after the delay runs the
// command once, notifications during a run schedule exactly one follow-up,
// and Stop waits for an in-flight run.
func TestReconfigurer(t *testing.T) {
	// Generates a shell command that appends a line to a counter file and,
	// optionally, sleeps first to simulate a slow configure. The sleep uses
	// the portable fractional-second form accepted by GNU coreutils, dash
	// and bash alike.
	counterCmd := func(counterFile string, sleepSeconds string) string {
		return "sleep " + sleepSeconds + "; echo run >> " + counterFile
	}
	countRuns := func(counterFile string) int {
		data, err := readFile(counterFile)
		if err != nil {
			return 0
		}
		content := strings.TrimSpace(string(data))
		if content == "" {
			return 0
		}
		return len(strings.Split(content, "\n"))
	}

	t.Run("runs command after delay", func(t *testing.T) {
		dir := t.TempDir()
		counter := dir + "/count"
		r := NewReconfigurer(dir, counterCmd(counter, "0"), 50*time.Millisecond, &logger.NullLogger{})
		defer r.Stop()

		r.NotifySetChanged()
		waitFor(t, 5*time.Second, "generate run", func() bool { return countRuns(counter) == 1 })
	})

	t.Run("notifications during a run schedule one follow-up", func(t *testing.T) {
		dir := t.TempDir()
		counter := dir + "/count"
		r := NewReconfigurer(dir, counterCmd(counter, "0.3"), 20*time.Millisecond, &logger.NullLogger{})
		defer r.Stop()

		r.NotifySetChanged()
		// While the first run is in flight (its shell command sleeps for
		// 300ms), additional notifications arrive. Changes the in-flight
		// run could not have observed must schedule exactly one follow-up
		// run, not one per notification.
		time.Sleep(50 * time.Millisecond)
		r.NotifySetChanged()
		time.Sleep(50 * time.Millisecond)
		r.NotifySetChanged()

		waitFor(t, 5*time.Second, "follow-up run", func() bool { return countRuns(counter) == 2 })

		// Give a potential third run a chance to appear; it must not. The
		// grace period must comfortably exceed the time an erroneous extra
		// run would need (debounce delay + the command's sleep).
		time.Sleep(2 * time.Second)
		if got := countRuns(counter); got != 2 {
			t.Fatalf("expected exactly 2 runs, got %d", got)
		}
	})

	t.Run("stop waits for in-flight run", func(t *testing.T) {
		dir := t.TempDir()
		counter := dir + "/count"
		r := NewReconfigurer(dir, counterCmd(counter, "0.4"), 20*time.Millisecond, &logger.NullLogger{})

		r.NotifySetChanged()

		// Stop while the run is (most likely) still sleeping: Stop must
		// block until the command has finished. The counter is only
		// appended at the very end of the command, so a premature return
		// would leave the file absent or empty here.
		time.Sleep(120 * time.Millisecond)
		r.Stop()

		if got := countRuns(counter); got != 1 {
			t.Fatalf("Stop returned before the in-flight run finished (runs: %d)", got)
		}

		// Notifications after Stop must be ignored.
		r.NotifySetChanged()
		time.Sleep(200 * time.Millisecond)
		if got := countRuns(counter); got != 1 {
			t.Fatalf("run happened after Stop (runs: %d)", got)
		}
	})
}

// Covers configuration parsing for the new auto-reconfigure fields.
func TestAutoReconfigureConfig(t *testing.T) {
	t.Run("autoReconfigure without generate is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigFile(t, dir, `{"compileCommands": "build/compile_commands.json", "autoReconfigure": true}`)
		_, err := LoadProjectConfig(dir)
		if err == nil {
			t.Fatal("expected error when autoReconfigure is set without generate")
		}
		if !strings.Contains(err.Error(), "generate") {
			t.Fatalf("error should mention generate, got: %v", err)
		}
	})

	t.Run("invalid reconfigureDelay is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigFile(t, dir, `{
			"compileCommands": "build/compile_commands.json",
			"generate": "true",
			"autoReconfigure": true,
			"reconfigureDelay": "soon"
		}`)
		_, err := LoadProjectConfig(dir)
		if err == nil {
			t.Fatal("expected error for invalid reconfigureDelay")
		}
	})

	t.Run("valid autoReconfigure configuration parses", func(t *testing.T) {
		dir := t.TempDir()
		writeConfigFile(t, dir, `{
			"compileCommands": "build/compile_commands.json",
			"generate": "cmake --preset local",
			"autoReconfigure": true,
			"reconfigureDelay": "1m30s"
		}`)
		cfg, err := LoadProjectConfig(dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !cfg.AutoReconfigure {
			t.Fatal("expected AutoReconfigure to be true")
		}
		if cfg.ReconfigureDelayParsed != 90*time.Second {
			t.Fatalf("expected delay 90s, got %v", cfg.ReconfigureDelayParsed)
		}
	})
}
