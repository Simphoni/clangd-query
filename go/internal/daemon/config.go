package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Name of the optional per-project configuration file that clangd-query looks
// for at the project root. The file also acts as a project root marker: when
// searching ancestor directories for the project root, a directory containing
// this file qualifies just like one containing CMakeLists.txt.
const ConfigFileName = ".clangd-query.json"

// Default wait between the last detected file addition or removal and the
// re-run of the generate command when auto-reconfigure is enabled. The delay
// is deliberately generous: the watcher already makes new files searchable
// immediately by opening them in clangd directly, so the re-run exists only
// to restore the authoritative compile_commands.json, and batching many file
// operations into one configure run matters more than speed.
const DefaultReconfigureDelay = 30 * time.Second

// Per-project configuration for clangd-query, loaded from a .clangd-query.json
// file at the project root. The file declares how the project's compilation
// database is produced and where it lives, so that projects with custom build
// setups (CMake presets, cross-compilation toolchains, non-CMake generators)
// can point the tool at their own compile_commands.json instead of relying on
// the built-in cmake invocation.
//
// A typical file for a CMake preset workflow looks like:
//
//	{
//	  "compileCommands": "build/compile_commands.json",
//	  "generate": "cmake --preset local"
//	}
type ProjectConfig struct {
	// Path to the project's compile_commands.json file, either absolute or
	// relative to the project root. After loading, this field always holds an
	// absolute path. The directory containing the file is passed to clangd via
	// --compile-commands-dir, which also determines where clangd stores its
	// background index (<dir>/.cache/clangd).
	CompileCommands string `json:"compileCommands"`

	// Shell command that produces the compilation database, executed via
	// "sh -c" with the project root as the working directory. It runs only
	// when the file named by CompileCommands is missing; regenerating an
	// existing database is left to the user, since clangd reloads a changed
	// compile_commands.json on its own. When empty and the database is
	// missing, the daemon reports an error instead of guessing a build
	// system invocation.
	Generate string `json:"generate"`

	// Additional command-line arguments appended to the clangd invocation,
	// after the built-in flags. Useful for options like --query-driver when
	// the compilation database references a non-host compiler.
	ClangdArgs []string `json:"clangdArgs"`

	// When true, the daemon watches for C++ source files being added or
	// removed and re-runs the Generate command after a quiet period, so that
	// build systems using file globs (e.g. CMake's file(GLOB_RECURSE)) pick
	// up the changed file set without a manual reconfigure. Requires
	// Generate to be set. clangd reloads the regenerated compilation
	// database on its own, so no further notification is needed.
	AutoReconfigure bool `json:"autoReconfigure"`

	// How long after the last file addition or removal the Generate command
	// is re-run, using Go duration syntax (e.g. "30s", "1m"). Defaults to
	// DefaultReconfigureDelay. Only meaningful together with
	// AutoReconfigure.
	ReconfigureDelay string `json:"reconfigureDelay"`

	// Parsed form of ReconfigureDelay, populated and validated by
	// LoadProjectConfig. Zero means "use DefaultReconfigureDelay".
	ReconfigureDelayParsed time.Duration `json:"-"`
}

// Returns the path of the configuration file for the given project root. The
// file may not exist; callers that need to distinguish "no configuration"
// should stat the path or use LoadProjectConfig, which returns a nil config
// for missing files.
func ConfigPath(projectRoot string) string {
	return filepath.Join(projectRoot, ConfigFileName)
}

// Reads and validates the project configuration from <projectRoot>/.clangd-query.json.
// A missing file is not an error: it yields a nil config and nil error, and the
// daemon falls back to its built-in defaults. A malformed or inconsistent file
// is a hard error, because silently ignoring a configuration the user wrote
// would make the daemon's behavior diverge from the user's intent without any
// visible signal.
//
// Relative paths in the configuration are resolved against the project root at
// load time, so the returned config is safe to use from a daemon process whose
// working directory is unrelated to the project.
func LoadProjectConfig(projectRoot string) (*ProjectConfig, error) {
	path := ConfigPath(projectRoot)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read %s: %v", path, err)
	}

	var cfg ProjectConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse %s: %v", path, err)
	}

	if cfg.CompileCommands != "" && !filepath.IsAbs(cfg.CompileCommands) {
		cfg.CompileCommands = filepath.Join(projectRoot, cfg.CompileCommands)
	}

	// A generate command without a known output location cannot be verified:
	// the daemon would have no way to tell whether the command produced the
	// database it was supposed to produce.
	if cfg.Generate != "" && cfg.CompileCommands == "" {
		return nil, fmt.Errorf("%s sets \"generate\" but not \"compileCommands\": "+
			"the daemon needs to know where the generated compile_commands.json lands", path)
	}

	// Auto-reconfigure is defined as a re-run of the generate command, so it
	// is meaningless without one.
	if cfg.AutoReconfigure && cfg.Generate == "" {
		return nil, fmt.Errorf("%s sets \"autoReconfigure\" but not \"generate\": "+
			"there is no command to re-run when files are added or removed", path)
	}

	if cfg.ReconfigureDelay != "" {
		delay, err := time.ParseDuration(cfg.ReconfigureDelay)
		if err != nil {
			return nil, fmt.Errorf("%s has invalid \"reconfigureDelay\" %q: %v "+
				"(use Go duration syntax, e.g. \"30s\", \"1m\")", path, cfg.ReconfigureDelay, err)
		}
		cfg.ReconfigureDelayParsed = delay
	}

	return &cfg, nil
}

// Returns the modification time of the project's configuration file in Unix
// seconds, or 0 when the file does not exist or cannot be stat'ed. The daemon
// records this value in its lock file so that clients can detect configuration
// changes and restart the daemon to pick them up.
func ConfigMTime(projectRoot string) int64 {
	stat, err := os.Stat(ConfigPath(projectRoot))
	if err != nil {
		return 0
	}
	return stat.ModTime().UnixNano()
}
