package test

import (
	"os"
	"strings"
	"testing"
)

func TestBatchQueries(t *testing.T) {
	tc := GetTestContext(t)

	t.Run("Multiple show symbols produce ordered sections", func(t *testing.T) {
		result := tc.RunCommand("show", "GameObject::Update", "Transform")
		tc.AssertExitCode(result, 0)

		firstHeader := "=== show GameObject::Update ==="
		secondHeader := "=== show Transform ==="
		tc.AssertContains(result.Stdout, firstHeader)
		tc.AssertContains(result.Stdout, secondHeader)

		firstIndex := strings.Index(result.Stdout, firstHeader)
		secondIndex := strings.Index(result.Stdout, secondHeader)
		if firstIndex == -1 || secondIndex == -1 || firstIndex > secondIndex {
			t.Errorf("Expected %q to appear before %q\nActual output:\n%s",
				firstHeader, secondHeader, result.Stdout)
		}

		tc.AssertContains(result.Stdout, "void Update(float delta_time) override;")
		tc.AssertContains(result.Stdout, "class Transform")
	})

	t.Run("Search across several symbols returns results for each", func(t *testing.T) {
		result := tc.RunCommand("search", "GameObject", "Vector3")
		tc.AssertExitCode(result, 0)
		tc.AssertContains(result.Stdout, "=== search GameObject ===")
		tc.AssertContains(result.Stdout, "game_engine::GameObject")
		tc.AssertContains(result.Stdout, "=== search Vector3 ===")
	})

	t.Run("Single symbol output has no section header", func(t *testing.T) {
		result := tc.RunCommand("show", "GameObject")
		tc.AssertExitCode(result, 0)
		tc.AssertNotContains(result.Stdout, "===")
		tc.AssertContains(result.Stdout, "Found class 'game_engine::GameObject'")
	})

	t.Run("Unknown symbols do not fail the batch or abort remaining queries", func(t *testing.T) {
		result := tc.RunCommand("show", "GameObject", "NoSuchSymbolAnywhere")
		tc.AssertExitCode(result, 0)
		tc.AssertContains(result.Stdout, "=== show GameObject ===")
		tc.AssertContains(result.Stdout,
			"No symbols found matching \"NoSuchSymbolAnywhere\"")
		// The successful query that follows must still be present.
		tc.AssertContains(result.Stdout, "void Update(float delta_time) override;")
	})

	t.Run("Genuine errors are isolated to their own section", func(t *testing.T) {
		result := tc.RunCommand("usages", "GameObject", "src/does-not-exist.cpp:1:1")
		if result.ExitCode != 1 {
			t.Errorf("Expected exit code 1 when a query fails hard, got %d\nStdout: %s",
				result.ExitCode, result.Stdout)
		}
		tc.AssertContains(result.Stdout, "=== usages GameObject ===")
		tc.AssertContains(result.Stdout, "Found 38 references:")
		tc.AssertContains(result.Stdout, "=== usages src/does-not-exist.cpp:1:1 ===")
		tc.AssertContains(result.Stdout, "Error:")

		if !strings.Contains(result.Stderr, "1 of 2 queries failed") {
			t.Errorf("Expected stderr to summarize batch failures\nStderr: %s",
				result.Stderr)
		}

		// The failed section comes last, so everything before its error line
		// belongs to the successful query and must have survived.
		failedSection := strings.Index(result.Stdout, "src/does-not-exist.cpp:1:1 ===")
		if failedSection != -1 &&
			!strings.Contains(result.Stdout[:failedSection], "Found 38 references:") {
			t.Errorf("Successful query result missing before failing section\nStdout:\n%s",
				result.Stdout)
		}
	})

	t.Run("Mixed input modes work within one usages batch", func(t *testing.T) {
		result := tc.RunCommand("usages", "GameObject", "include/core/component.h:11:7")
		tc.AssertExitCode(result, 0)
		tc.AssertContains(result.Stdout, "=== usages GameObject ===")
		tc.AssertContains(result.Stdout, "=== usages include/core/component.h:11:7 ===")
	})
}

func TestBatchCommand(t *testing.T) {
	tc := GetTestContext(t)

	t.Run("Mixed commands run one query per input line", func(t *testing.T) {
		stdin := "show GameObject::Update\nusages GameObject\ninterface Engine\n"
		result := tc.RunCommandWithStdin([]string{"batch"}, stdin)
		tc.AssertExitCode(result, 0)
		tc.AssertContains(result.Stdout, "=== show GameObject::Update ===")
		tc.AssertContains(result.Stdout, "void Update(float delta_time) override;")
		tc.AssertContains(result.Stdout, "=== usages GameObject ===")
		tc.AssertContains(result.Stdout, "=== interface Engine ===")
		tc.AssertContains(result.Stdout, "Public Interface:")
	})

	t.Run("Comments and blank lines are ignored", func(t *testing.T) {
		stdin := "# look up two classes\n\nshow Transform\n   \n# show GameObject\n"
		result := tc.RunCommandWithStdin([]string{"batch"}, stdin)
		tc.AssertExitCode(result, 0)
		tc.AssertContains(result.Stdout, "=== show Transform ===")
		tc.AssertNotContains(result.Stdout, "GameObject")
	})

	t.Run("Per-line --limit restricts only its own line", func(t *testing.T) {
		stdin := "search Update --limit 2\nsearch Update --limit 5\n"
		result := tc.RunCommandWithStdin([]string{"batch"}, stdin)
		tc.AssertExitCode(result, 0)

		// Section headers carry the command and symbol but not flags, so the
		// two identical-looking headers are told apart by position.
		header := "=== search Update ==="
		firstIndex := strings.Index(result.Stdout, header)
		secondIndex := strings.Index(result.Stdout[firstIndex+len(header):], header)
		if firstIndex == -1 || secondIndex == -1 {
			t.Fatalf("Expected two %q sections\nStdout:\n%s", header, result.Stdout)
		}
		secondIndex += firstIndex + len(header)

		limited := CountOccurrences(result.Stdout[:secondIndex], "- `")
		if limited != 2 {
			t.Errorf("Expected exactly 2 limited search results, got %d\nStdout:\n%s",
				limited, result.Stdout)
		}
		unlimited := CountOccurrences(result.Stdout[secondIndex:], "- `")
		if unlimited < 3 {
			t.Errorf("Expected the unlimited line to return more results, got %d\nStdout:\n%s",
				unlimited, result.Stdout)
		}
	})

	t.Run("Multiple symbols on one line expand into separate sections", func(t *testing.T) {
		stdin := "show GameObject Transform\n"
		result := tc.RunCommandWithStdin([]string{"batch"}, stdin)
		tc.AssertExitCode(result, 0)
		tc.AssertContains(result.Stdout, "=== show GameObject ===")
		tc.AssertContains(result.Stdout, "=== show Transform ===")
	})

	t.Run("Malformed and unsupported lines fail in isolation", func(t *testing.T) {
		stdin := "show\nstatus\nhierarchy Transform\n"
		result := tc.RunCommandWithStdin([]string{"batch"}, stdin)
		if result.ExitCode != 1 {
			t.Errorf("Expected exit code 1 for failing batch lines, got %d", result.ExitCode)
		}
		tc.AssertContains(result.Stdout, "=== show ===")
		tc.AssertContains(result.Stdout, "requires a symbol argument")
		tc.AssertContains(result.Stdout, `command "status" is not supported in batch mode`)
		// The valid line after the failures must still have run.
		tc.AssertContains(result.Stdout, "=== hierarchy Transform ===")

		if !strings.Contains(result.Stderr, "2 of 3 batch queries failed") {
			t.Errorf("Expected stderr to summarize batch failures\nStderr: %s",
				result.Stderr)
		}
	})

	t.Run("Quoted arguments survive tokenization", func(t *testing.T) {
		stdin := "show \"GameObject::Update\"\n"
		result := tc.RunCommandWithStdin([]string{"batch"}, stdin)
		tc.AssertExitCode(result, 0)
		tc.AssertContains(result.Stdout, "void Update(float delta_time) override;")
	})

	t.Run("Queries can be read from a file instead of stdin", func(t *testing.T) {
		queryFile := tc.T.TempDir() + "/queries.txt"
		if err := os.WriteFile(queryFile,
			[]byte("# generated\nshow Vector3\n"), 0644); err != nil {
			t.Fatalf("Failed to write query file: %v", err)
		}

		result := tc.RunCommand("batch", queryFile)
		tc.AssertExitCode(result, 0)
		tc.AssertContains(result.Stdout, "=== show Vector3 ===")
		tc.AssertContains(result.Stdout, "struct Vector3 {")
	})

	t.Run("Empty input fails before contacting the daemon", func(t *testing.T) {
		result := tc.RunCommandWithStdin([]string{"batch"}, "\n# only a comment\n")
		if result.ExitCode != 1 {
			t.Errorf("Expected exit code 1 for empty batch input, got %d", result.ExitCode)
		}
		tc.AssertContains(result.Stderr, "batch mode requires at least one query line")
	})
}
