package test

import (
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
