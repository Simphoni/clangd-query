package daemon

import (
	"crypto/md5"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Contains metadata about a running daemon process, including its process ID,
// socket path, build time, and project root. This information is stored in a
// lock file to coordinate between multiple client connections and ensure only
// one daemon runs per project.
type LockInfo struct {
	PID         int    `json:"pid"`
	SocketPath  string `json:"socketPath"`
	BuildTime   int64  `json:"buildTime"`
	ProjectRoot string `json:"projectRoot"`

	// Modification time of the project's .clangd-query.json at daemon
	// startup, or 0 when the project has no configuration file. The daemon
	// reads the configuration only once at startup, so clients compare this
	// value against the file's current mtime to detect configuration
	// changes and restart the daemon to pick them up.
	ConfigMTime int64 `json:"configMTime"`
}

// Returns the Unix domain socket path for a given project directory.
// The socket path is generated using an MD5 hash of the project root to ensure
// uniqueness across different projects while maintaining consistency for the
// same project. The socket file is placed in the system's temporary directory.
func GetSocketPath(projectRoot string) string {
	hash := md5.Sum([]byte(projectRoot))
	socketName := fmt.Sprintf("clangd-query-%x.sock", hash)
	return filepath.Join(os.TempDir(), socketName)
}

// Returns the path to the daemon lock file for a given project.
// The lock file is stored in the project root directory and contains metadata
// about the running daemon instance. This file is used to detect if a daemon
// is already running and whether it needs to be restarted.
func GetLockPath(projectRoot string) string {
	return filepath.Join(projectRoot, ".clangd-query.lock")
}

// Returns the path to the daemon log file for a given project.
// The log file is stored in a .cache/clangd-query subdirectory within the
// project root. The cache directory is created if it doesn't exist. This log
// file contains debug information and error messages from the daemon process.
func GetLogPath(projectRoot string) string {
	cacheDir := filepath.Join(projectRoot, ".cache", "clangd-query")
	os.MkdirAll(cacheDir, 0755)
	return filepath.Join(cacheDir, "daemon.log")
}

// Writes daemon metadata to the project's lock file to indicate that a daemon
// is running. This includes the process ID, socket path, build time of the
// executable, and project root. The build time is used to detect when the
// daemon binary has been updated and needs to be restarted.
func WriteLockFile(projectRoot string, pid int, socketPath string) error {
	lockPath := GetLockPath(projectRoot)

	// Get build time of current executable
	execPath, err := os.Executable()
	if err != nil {
		return err
	}

	stat, err := os.Stat(execPath)
	if err != nil {
		return err
	}

	lockInfo := LockInfo{
		PID:         pid,
		SocketPath:  socketPath,
		BuildTime:   stat.ModTime().Unix(),
		ProjectRoot: projectRoot,
		ConfigMTime: ConfigMTime(projectRoot),
	}

	data, err := json.MarshalIndent(lockInfo, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(lockPath, data, 0644)
}

// Reads and parses daemon metadata from the project's lock file.
// Returns nil if the lock file doesn't exist, indicating no daemon is running.
// The returned LockInfo can be used to check if the daemon is still alive
// and whether it needs to be restarted due to binary updates.
func ReadLockFile(projectRoot string) (*LockInfo, error) {
	lockPath := GetLockPath(projectRoot)

	data, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No lock file
		}
		return nil, err
	}

	var lockInfo LockInfo
	if err := json.Unmarshal(data, &lockInfo); err != nil {
		return nil, err
	}

	return &lockInfo, nil
}

// Removes the daemon lock file for a project, typically called when the
// daemon shuts down or when cleaning up after a stale daemon. This function
// treats a missing lock file as a successful operation, avoiding errors
// during cleanup of already-cleaned state.
func RemoveLockFile(projectRoot string) error {
	lockPath := GetLockPath(projectRoot)
	err := os.Remove(lockPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Removes the project's lock file only when it still belongs to the daemon
// with the given PID. A daemon uses this during its own shutdown: between the
// moment a client decides the daemon is stale and the moment the old daemon
// finishes exiting, a successor daemon may already have written its own lock
// file, and unconditionally deleting it would orphan the successor.
func RemoveOwnLockFile(projectRoot string, pid int) error {
	lockInfo, err := ReadLockFile(projectRoot)
	if err != nil || lockInfo == nil {
		return err
	}
	if lockInfo.PID != pid {
		return nil
	}
	return RemoveLockFile(projectRoot)
}

// Checks whether a process with the given PID is still running on the system.
// This is done by sending signal 0 to the process, which performs a permission
// check without actually sending a signal. Returns false for invalid PIDs or
// if the process no longer exists.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	// Send signal 0 to check if process exists
	err := syscall.Kill(pid, 0)
	return err == nil
}

// Determines whether a running daemon needs to be restarted. Three conditions
// make a daemon stale: its process is gone, the clangd-query binary has been
// updated since the daemon started, or the project's .clangd-query.json has
// been modified, created, or deleted after the daemon loaded it. A stale
// daemon should be stopped and a new one started to ensure clients talk to a
// daemon running the latest code and configuration.
func IsDaemonStale(projectRoot string, lockInfo *LockInfo) bool {
	// Check if process is alive
	if !IsProcessAlive(lockInfo.PID) {
		return true
	}

	// Check if binary has been updated
	execPath, err := os.Executable()
	if err != nil {
		return false // Can't determine, assume not stale
	}

	stat, err := os.Stat(execPath)
	if err != nil {
		return false // Can't determine, assume not stale
	}

	// If binary is newer than lock file, daemon is stale
	if stat.ModTime().Unix() > lockInfo.BuildTime {
		return true
	}

	// A configuration change is only visible to a freshly started daemon,
	// since the running one loaded the old configuration at startup.
	return ConfigMTime(projectRoot) != lockInfo.ConfigMTime
}

// Removes the Unix domain socket file from the filesystem, typically called
// during daemon shutdown or when cleaning up after a crashed daemon. This
// prevents "address already in use" errors when starting a new daemon.
// Missing socket files are treated as successful cleanup.
func CleanupSocket(socketPath string) error {
	err := os.Remove(socketPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Returns the modification timestamp of the currently running executable.
// This timestamp is used to detect when the clangd-query binary has been
// updated, allowing the daemon to know when it should restart itself to
// use the new version.
func GetBuildTime() (int64, error) {
	execPath, err := os.Executable()
	if err != nil {
		return 0, err
	}

	stat, err := os.Stat(execPath)
	if err != nil {
		return 0, err
	}

	return stat.ModTime().Unix(), nil
}

// Manages the size of the daemon log file by truncating it when it exceeds
// the specified maximum size. When truncation occurs, the function keeps only
// the last 10% of the file content, preserving the most recent log entries.
// A header is added to indicate when truncation occurred. This prevents
// unbounded log growth while maintaining recent debugging information.
func TruncateLogFile(projectRoot string, maxSize int64) error {
	logPath := GetLogPath(projectRoot)

	stat, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No log file yet
		}
		return err
	}

	if stat.Size() > maxSize {
		// Keep last 10% of the file
		keepSize := maxSize / 10

		file, err := os.OpenFile(logPath, os.O_RDWR, 0644)
		if err != nil {
			return err
		}
		defer file.Close()

		// Seek to position where we want to start keeping
		_, err = file.Seek(stat.Size()-keepSize, 0)
		if err != nil {
			return err
		}

		// Read the remaining content
		remaining := make([]byte, keepSize)
		n, err := file.Read(remaining)
		if err != nil && err != io.EOF {
			return err
		}

		// Write header and remaining content to new file
		tempFile := logPath + ".tmp"
		header := fmt.Sprintf("=== Log truncated at %s ===\n", time.Now().Format(time.RFC3339))
		content := append([]byte(header), remaining[:n]...)

		if err := os.WriteFile(tempFile, content, 0644); err != nil {
			return err
		}

		// Replace old file with new
		return os.Rename(tempFile, logPath)
	}

	return nil
}
