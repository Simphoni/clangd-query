package daemon

import (
	"fmt"
	"os"
	"path/filepath"
)

// Determines the project root by walking up from startDir towards the
// filesystem root and returning the first directory that contains a project
// marker. Two kinds of markers are recognized, and whichever is encountered
// first (i.e. deepest in the tree) wins:
//
//   - .clangd-query.json, the per-project configuration file. This is the
//     primary marker and works for any build system, since the configuration
//     file can declare where the compilation database lives even when the
//     project is not driven by CMake at all.
//   - CMakeLists.txt, which preserves the original zero-configuration
//     behavior for plain CMake projects.
//
// The marker search is shared by the client and the daemon so that both agree
// on the project identity; the Unix socket used for their communication is
// derived from the returned path, so any inconsistency would strand clients
// on a socket no daemon listens to.
func FindProjectRoot(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}

	for {
		if _, err := os.Stat(ConfigPath(dir)); err == nil {
			return dir, nil
		}

		cmakePath := filepath.Join(dir, "CMakeLists.txt")
		if _, err := os.Stat(cmakePath); err == nil {
			return dir, nil
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			// Reached root of filesystem
			break
		}
		dir = parent
	}

	return "", fmt.Errorf("no %s or CMakeLists.txt found in any parent directory of %s",
		ConfigFileName, startDir)
}
