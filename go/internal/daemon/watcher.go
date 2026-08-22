package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"clangd-query/internal/logger"
	"github.com/fsnotify/fsnotify"
)

// Number of files reported by the watcher callbacks. The set callback
// (additions and removals) does not enumerate individual files because the
// reconfigure it eventually triggers regenerates the entire compilation
// database anyway; only the count is interesting for log messages.
type FileChanges struct {
	// Files whose contents were modified and which should be re-indexed by
	// clangd directly.
	Changed []string

	// Files that were newly created. These are not in the compilation
	// database yet (until a reconfigure happens), so clangd is asked to open
	// them directly, which makes their symbols searchable immediately via
	// clangd's compile-flags inference from neighboring files.
	Created []string

	// Number of C++ files that were deleted or renamed away. The paths
	// themselves are not reported: for a deleted file the only meaningful
	// consumer is the set-change reconfigure, which rebuilds the whole
	// compilation database regardless of which files disappeared.
	RemovedCount int
}

// Watches the project's directory tree for C++ source file changes and
// reports them in debounced batches. Two kinds of changes are distinguished
// because they need different reactions:
//
//   - Content changes (writes): clangd re-indexes the affected files.
//   - Set changes (creations, deletions, renames): these alter which files a
//     globbing build system (e.g. CMake's file(GLOB_RECURSE)) would include
//     in the compilation database. Newly created files are additionally
//     opened in clangd right away so their symbols are searchable before any
//     reconfigure runs.
//
// Both kinds are accumulated separately and flushed after a 500ms quiet
// period, so a burst of editor saves or a batch file generation produces one
// callback invocation instead of many.
type FileWatcher struct {
	watcher       *fsnotify.Watcher
	projectRoot   string
	onChange      func(FileChanges)
	debounceTimer *time.Timer
	debounceMu    sync.Mutex
	changedFiles  map[string]bool
	createdFiles  map[string]bool
	removedCount  int
	stop          chan struct{}
	logger        logger.Logger
}

// Creates a file watcher for the project tree. The onChange callback is
// invoked from the watcher's own goroutine after each debounce window closes,
// so it must be safe for concurrent use and should not block.
func NewFileWatcher(projectRoot string, onChange func(FileChanges), log logger.Logger) (*FileWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	fw := &FileWatcher{
		watcher:      watcher,
		projectRoot:  projectRoot,
		onChange:     onChange,
		changedFiles: make(map[string]bool),
		createdFiles: make(map[string]bool),
		stop:         make(chan struct{}),
		logger:       log,
	}

	// Add project root and subdirectories
	if err := fw.addDirectoryRecursive(projectRoot); err != nil {
		watcher.Close()
		return nil, err
	}

	// Start watching
	go fw.watch()

	return fw, nil
}

// Recursively registers a directory and all its subdirectories with the
// watcher. Directories that never contain sources worth tracking (hidden
// directories like .git and .cache, and build output directories) are
// skipped; note this mirrors the default build directory names, so a project
// that stores its sources in a directory called "build" would not be watched.
func (fw *FileWatcher) addDirectoryRecursive(dir string) error {
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Ignore errors walking the tree
		}

		// Skip hidden directories and build directories
		if info.IsDir() {
			base := filepath.Base(path)
			if strings.HasPrefix(base, ".") ||
				base == "build" ||
				base == "cmake-build-debug" ||
				base == "cmake-build-release" ||
				base == "out" ||
				base == "bin" ||
				base == "obj" {
				return filepath.SkipDir
			}

			// Add directory to watcher
			if err := fw.watcher.Add(path); err != nil {
				// Ignore errors adding individual directories
				fw.logger.Info("Warning: failed to watch %s: %v", path, err)
			}
		}

		return nil
	})
}

// Event handling loop: classifies raw fsnotify events into content changes
// and set changes, and keeps the watched directory set current as new
// directories appear. Runs until Stop is called.
func (fw *FileWatcher) watch() {
	for {
		select {
		case event, ok := <-fw.watcher.Events:
			if !ok {
				return
			}

			if fw.isCppFile(event.Name) {
				// A rename event arrives without a Remove flag for the old
				// name on some platforms, so both are treated as removals.
				switch {
				case event.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
					fw.handleSetChange(event.Name, false)
				case event.Op&fsnotify.Create != 0:
					fw.handleSetChange(event.Name, true)
				case event.Op&fsnotify.Write != 0:
					fw.handleFileChange(event.Name)
				}
			}

			// If a new directory was created, add it to the watcher
			if event.Op&fsnotify.Create != 0 {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					fw.addDirectoryRecursive(event.Name)
				}
			}

		case err, ok := <-fw.watcher.Errors:
			if !ok {
				return
			}
			fw.logger.Error("File watcher error: %v", err)

		case <-fw.stop:
			return
		}
	}
}

// Records a content change and (re)starts the debounce timer. A write to a
// file that was just created is folded into the creation: the file is new to
// the compilation database either way, and opening it in clangd also picks
// up its current contents, so reporting it as created alone is sufficient.
func (fw *FileWatcher) handleFileChange(path string) {
	fw.debounceMu.Lock()
	defer fw.debounceMu.Unlock()

	if !fw.createdFiles[path] {
		fw.changedFiles[path] = true
	}
	fw.scheduleFlushLocked()
}

// Records a file appearing or disappearing and (re)starts the debounce
// timer. Creation and removal of the same path within one debounce window
// cancel each other out, which keeps temporary editor files from triggering
// any downstream work.
func (fw *FileWatcher) handleSetChange(path string, created bool) {
	fw.debounceMu.Lock()
	defer fw.debounceMu.Unlock()

	if created {
		// A creation cancels a pending removal of the same path (e.g. a
		// delete-then-recreate editor save): the file exists again, so only
		// its contents changed.
		if fw.removedCount > 0 {
			fw.removedCount--
			fw.changedFiles[path] = true
		} else {
			fw.createdFiles[path] = true
			delete(fw.changedFiles, path)
		}
	} else {
		if fw.createdFiles[path] {
			// Created and removed within the same window: as if nothing
			// happened.
			delete(fw.createdFiles, path)
		} else {
			fw.removedCount++
			delete(fw.changedFiles, path)
		}
	}
	fw.scheduleFlushLocked()
}

// Restarts the debounce timer. Callers must hold debounceMu.
func (fw *FileWatcher) scheduleFlushLocked() {
	if fw.debounceTimer != nil {
		fw.debounceTimer.Stop()
	}

	fw.debounceTimer = time.AfterFunc(500*time.Millisecond, func() {
		fw.debounceMu.Lock()

		changes := FileChanges{
			Changed:      make([]string, 0, len(fw.changedFiles)),
			Created:      make([]string, 0, len(fw.createdFiles)),
			RemovedCount: fw.removedCount,
		}
		for file := range fw.changedFiles {
			changes.Changed = append(changes.Changed, file)
		}
		for file := range fw.createdFiles {
			changes.Created = append(changes.Created, file)
		}

		fw.changedFiles = make(map[string]bool)
		fw.createdFiles = make(map[string]bool)
		fw.removedCount = 0

		fw.debounceMu.Unlock()

		if len(changes.Changed) > 0 || len(changes.Created) > 0 || changes.RemovedCount > 0 {
			fw.onChange(changes)
		}
	})
}

// Reports whether the path looks like a C++ source or header file.
func (fw *FileWatcher) isCppFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".cpp", ".cc", ".cxx", ".c++",
		".h", ".hpp", ".hxx", ".h++",
		".c", ".hh":
		return true
	default:
		return false
	}
}

// Stops the watcher and releases its resources.
func (fw *FileWatcher) Stop() error {
	close(fw.stop)

	fw.debounceMu.Lock()
	if fw.debounceTimer != nil {
		fw.debounceTimer.Stop()
	}
	fw.debounceMu.Unlock()

	return fw.watcher.Close()
}
