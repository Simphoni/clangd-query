package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"clangd-query/internal/logger"
)

// Maximum number of bytes of a failed generate command's output that is
// embedded in the returned error. The full output is always written to the
// daemon log; the tail is what the invoking user sees on their terminal.
const generateOutputTailSize = 4000

// Locates the directory containing the project's compile_commands.json,
// creating the database first when necessary. The returned directory is what
// the daemon passes to clangd via --compile-commands-dir, which also fixes
// where clangd keeps its background index (<dir>/.cache/clangd).
//
// When the project carries a .clangd-query.json with a "compileCommands"
// setting, that location is authoritative: an existing database is reused
// as-is, a missing one is produced by running the configured "generate"
// command, and the built-in cmake fallback is bypassed entirely. Without a
// configuration file the historical behavior applies: a database under
// .cache/clangd-query/build is reused or generated with a plain cmake
// configure.
//
// Generation only ever happens when the database is absent. Refreshing an
// existing database is deliberately left to the user: clangd watches the
// file and reloads changed compilation flags on its own, so regenerating in
// place requires no involvement from the daemon.
func EnsureCompilationDatabase(projectRoot string, cfg *ProjectConfig, log logger.Logger) (string, error) {
	if cfg != nil && cfg.CompileCommands != "" {
		return ensureConfiguredDatabase(projectRoot, cfg, log)
	}
	return ensureDefaultDatabase(projectRoot, log)
}

// Handles the configuration-driven case: the user has declared where their
// compile_commands.json lives, so the tool reuses it or produces it with the
// configured generate command, and never invents a build setup of its own.
func ensureConfiguredDatabase(projectRoot string, cfg *ProjectConfig, log logger.Logger) (string, error) {
	buildDir := filepath.Dir(cfg.CompileCommands)

	if _, err := os.Stat(cfg.CompileCommands); err == nil {
		log.Info("Using configured compilation database: %s", cfg.CompileCommands)
		return buildDir, nil
	}

	if cfg.Generate == "" {
		return "", fmt.Errorf("compile_commands.json not found at %s and %s sets no "+
			"\"generate\" command to create it", cfg.CompileCommands, ConfigFileName)
	}

	log.Info("Compilation database missing, running generate command: %s", cfg.Generate)
	if err := runGenerateCommand(projectRoot, cfg.Generate, log); err != nil {
		return "", err
	}

	if _, err := os.Stat(cfg.CompileCommands); err != nil {
		return "", fmt.Errorf("generate command succeeded but %s was not created",
			cfg.CompileCommands)
	}

	return buildDir, nil
}

// Handles the zero-configuration case: the compilation database lives in the
// tool's private build directory, generated with a plain cmake configure of
// the project root.
func ensureDefaultDatabase(projectRoot string, log logger.Logger) (string, error) {
	buildDir := filepath.Join(projectRoot, ".cache", "clangd-query", "build")
	compileCommandsPath := filepath.Join(buildDir, "compile_commands.json")

	if _, err := os.Stat(compileCommandsPath); err == nil {
		return buildDir, nil
	}

	if err := os.MkdirAll(buildDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create build directory: %v", err)
	}

	log.Info("Generating compile_commands.json in %s...", buildDir)

	cmd := exec.Command("cmake",
		"-S", projectRoot,
		"-B", buildDir,
		"-DCMAKE_EXPORT_COMPILE_COMMANDS=ON")

	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Error("cmake configure failed: %v\nOutput: %s", err, output)
		// Check if cmake is not found
		if strings.Contains(err.Error(), "executable file not found") {
			return "", fmt.Errorf("cmake not found in PATH. Please install CMake to use clangd-query")
		}
		return "", fmt.Errorf("cmake failed: %v\nOutput: %s", err, output)
	}

	if _, err := os.Stat(compileCommandsPath); err != nil {
		return "", fmt.Errorf("cmake succeeded but compile_commands.json was not created")
	}

	return buildDir, nil
}

// Runs the user-provided generate command through the system shell with the
// project root as the working directory. Generate commands routinely involve
// arguments, pipes or preset names, so a shell is the honest way to interpret
// them; anchoring the working directory at the project root keeps relative
// paths inside the command deterministic regardless of where the invoking
// client was standing.
func runGenerateCommand(projectRoot, command string, log logger.Logger) error {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = projectRoot

	output, err := cmd.CombinedOutput()
	if err != nil {
		// The full output goes to the daemon log for post-mortem analysis;
		// the error returned to the user carries only the tail, since cmake
		// output for a failed configure can run to hundreds of lines.
		log.Error("generate command failed: %v\nCommand: %s\nOutput:\n%s",
			err, command, output)
		tail := output
		if len(tail) > generateOutputTailSize {
			tail = tail[len(tail)-generateOutputTailSize:]
		}
		return fmt.Errorf("generate command failed: %v\nOutput (tail):\n%s", err, tail)
	}

	log.Info("generate command completed successfully")
	return nil
}
