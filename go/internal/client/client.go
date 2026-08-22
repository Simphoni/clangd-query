package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"clangd-query/internal/daemon"
)

// Config contains client configuration
type Config struct {
	Command     string
	Arguments   []string
	Limit       int
	Verbose     bool
	Timeout     int
	ProjectRoot string
}

// Client handles communication with the daemon
type Client struct {
	conn    net.Conn
	encoder *json.Encoder
	decoder *json.Decoder
	timeout time.Duration
	reqID   int
}

// RPCOptions contains options for RPC calls
type RPCOptions struct {
	Timeout time.Duration // Custom timeout for this call
}

// Request represents a JSON-RPC request
type Request struct {
	ID     int                    `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params,omitempty"`
}

// Response represents a JSON-RPC response
type Response struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ErrorResponse  `json:"error,omitempty"`
}

// ErrorResponse represents an error in a response
type ErrorResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ErrorCodeStillIndexing mirrors the daemon's special error code for requests
// that arrive before clangd has finished its initial indexing. See RPCError.
const ErrorCodeStillIndexing = 1000

// RPCError preserves the numeric error code returned by the daemon. Plain
// errors discard the code, which would make it impossible to distinguish an
// expected "still indexing" response from a real failure.
type RPCError struct {
	Code    int
	Message string
}

// Error returns the daemon-provided message.
func (e *RPCError) Error() string {
	return e.Message
}

// StatusInfo represents daemon status
type StatusInfo struct {
	PID           int    `json:"pid"`
	ProjectRoot   string `json:"projectRoot"`
	BuildDir      string `json:"buildDir"`
	IndexDir      string `json:"indexDir"`
	Indexing      bool   `json:"indexing"`
	IndexElapsed  string `json:"indexElapsed"`
	Uptime        string `json:"uptime"`
	TotalRequests int    `json:"totalRequests"`
	Connections   int    `json:"connections"`
}

// NewClient creates a new client connected to the daemon
func NewClient(conn net.Conn, timeout time.Duration) *Client {
	return &Client{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		decoder: json.NewDecoder(conn),
		timeout: timeout,
		reqID:   1,
	}
}

// CallRPC makes a generic RPC call to the daemon
func (c *Client) CallRPC(method string, params map[string]interface{}, opts *RPCOptions) (json.RawMessage, error) {
	// Use custom timeout if provided
	timeout := c.timeout
	if opts != nil && opts.Timeout > 0 {
		timeout = opts.Timeout
	}

	// Create request
	req := Request{
		ID:     c.reqID,
		Method: method,
		Params: params,
	}
	c.reqID++

	// Send request
	if err := c.encoder.Encode(req); err != nil {
		return nil, fmt.Errorf("failed to send request: %v", err)
	}

	// Set read timeout
	c.conn.SetReadDeadline(time.Now().Add(timeout))

	// Read response
	var resp Response
	if err := c.decoder.Decode(&resp); err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return nil, fmt.Errorf("request timeout after %v", timeout)
		}
		return nil, fmt.Errorf("failed to read response: %v", err)
	}

	// Check for error
	if resp.Error != nil {
		return nil, &RPCError{Code: resp.Error.Code, Message: resp.Error.Message}
	}

	return resp.Result, nil
}

// CallTyped makes an RPC call and unmarshals the result into the provided interface
func (c *Client) CallTyped(method string, params map[string]interface{}, result interface{}) error {
	raw, err := c.CallRPC(method, params, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, result)
}

// callCommand is a generic helper for commands that return formatted strings.
// It handles the daemon's still-indexing response by printing progress to
// stderr and retrying until the initial background index is ready, within a
// generous overall budget. The command's normal output still goes to stdout.
func (c *Client) callCommand(method string, params map[string]interface{}) (string, error) {
	deadline := time.Now().Add(indexWaitTimeout())

	for {
		var result map[string]string
		err := c.CallTyped(method, params, &result)
		if err == nil {
			return result["output"], nil
		}

		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == ErrorCodeStillIndexing {
			if time.Now().After(deadline) {
				return "", fmt.Errorf("clangd indexing did not finish within %s; results may be incomplete", indexWaitTimeout())
			}
			fmt.Fprintf(os.Stderr, "clangd %s; waiting %s...\n", rpcErr.Message, indexPollInterval())
			time.Sleep(indexPollInterval())
			continue
		}

		return "", err
	}
}

// Returns how long the client retries a command while the daemon reports
// "still indexing". It mirrors the daemon-side CLANGD_INDEX_TIMEOUT fallback.
func indexWaitTimeout() time.Duration {
	if value := os.Getenv("CLANGD_INDEX_TIMEOUT"); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return 5 * time.Minute
}

// Returns how long the client sleeps between still-indexing retries.
func indexPollInterval() time.Duration {
	if value := os.Getenv("CLANGD_INDEX_POLL_INTERVAL"); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return 3 * time.Second
}

// Search searches for symbols
func (c *Client) Search(symbol string, limit int) (string, error) {
	return c.callCommand("search", map[string]interface{}{
		"symbol": symbol,
		"limit":  limit,
	})
}

// Show shows declaration and definition
func (c *Client) Show(symbol string) (string, error) {
	return c.callCommand("show", map[string]interface{}{
		"symbol": symbol,
	})
}

// View views complete source code
func (c *Client) View(symbol string) (string, error) {
	return c.callCommand("view", map[string]interface{}{
		"symbol": symbol,
	})
}

// Usages finds all usages of a symbol
func (c *Client) Usages(symbol string, limit int) (string, error) {
	return c.callCommand("usages", map[string]interface{}{
		"symbol": symbol,
		"limit":  limit,
	})
}

// Hierarchy shows type hierarchy
func (c *Client) Hierarchy(symbol string, limit int) (string, error) {
	return c.callCommand("hierarchy", map[string]interface{}{
		"symbol": symbol,
		"limit":  limit,
	})
}

// Signature shows function signature
func (c *Client) Signature(symbol string) (string, error) {
	return c.callCommand("signature", map[string]interface{}{
		"symbol": symbol,
	})
}

// Interface shows public interface
func (c *Client) Interface(symbol string) (string, error) {
	return c.callCommand("interface", map[string]interface{}{
		"symbol": symbol,
	})
}

// GetLogs retrieves daemon logs
func (c *Client) GetLogs(level string) (string, error) {
	params := map[string]interface{}{
		"level": level,
	}

	var logsResponse map[string]string
	err := c.CallTyped("logs", params, &logsResponse)
	if err != nil {
		return "", err
	}
	return logsResponse["logs"], nil
}

// GetStatus retrieves daemon status
func (c *Client) GetStatus() (*StatusInfo, error) {
	var status StatusInfo
	err := c.CallTyped("status", map[string]interface{}{}, &status)
	return &status, err
}

// Shutdown initiates daemon shutdown
func (c *Client) Shutdown() error {
	_, err := c.CallRPC("shutdown", map[string]interface{}{}, nil)
	return err
}

// handleCommand processes a command and returns the output as a string
func (c *Client) handleCommand(config *Config) (string, error) {
	// Extract symbol if needed
	symbolCommands := map[string]bool{
		"search":    true,
		"show":      true,
		"view":      true,
		"usages":    true,
		"hierarchy": true,
		"signature": true,
		"interface": true,
	}

	symbol := ""
	if symbolCommands[config.Command] {
		if len(config.Arguments) == 0 {
			return "", fmt.Errorf("%s requires a symbol argument", config.Command)
		}
		symbol = config.Arguments[0]
	}

	// Handle each command
	switch config.Command {
	case "search":
		return c.Search(symbol, config.Limit)
	case "show":
		return c.Show(symbol)
	case "view":
		return c.View(symbol)
	case "usages":
		return c.Usages(symbol, config.Limit)
	case "hierarchy":
		return c.Hierarchy(symbol, config.Limit)
	case "signature":
		return c.Signature(symbol)
	case "interface":
		return c.Interface(symbol)

	case "logs":
		// Parse log level from arguments
		logLevel := "info" // default
		for _, arg := range config.Arguments {
			if arg == "--verbose" || arg == "-v" {
				logLevel = "verbose"
			} else if arg == "--error" || arg == "-e" {
				logLevel = "error"
			}
		}
		// Global verbose flag overrides
		if config.Verbose {
			logLevel = "verbose"
		}
		return c.GetLogs(logLevel)

	case "status":
		status, err := c.GetStatus()
		if err != nil {
			return "", err
		}
		indexing := "no"
		if status.Indexing {
			indexing = "yes"
		}
		return fmt.Sprintf("Daemon Status:\n  PID: %d\n  Project: %s\n  Build Dir: %s\n  Index Dir: %s\n  Indexing: %s\n  Index Elapsed: %s\n  Uptime: %s\n  Requests: %d\n  Connections: %d\n",
			status.PID, status.ProjectRoot, status.BuildDir, status.IndexDir,
			indexing, status.IndexElapsed, status.Uptime, status.TotalRequests, status.Connections), nil

	case "shutdown":
		if err := c.Shutdown(); err != nil {
			return "", err
		}
		return "Daemon shutdown initiated\n", nil

	default:
		return "", fmt.Errorf("unknown command: %s", config.Command)
	}
}

// Run executes the client with the given configuration
func Run(config *Config) error {
	// Get project root from config
	projectRoot := config.ProjectRoot
	if projectRoot == "" {
		return fmt.Errorf("project root not set")
	}

	// Check if daemon is running
	lockInfo, err := daemon.ReadLockFile(projectRoot)
	if err != nil {
		return fmt.Errorf("failed to read lock file: %v", err)
	}

	needStart := false
	if lockInfo == nil {
		needStart = true
	} else if !daemon.IsProcessAlive(lockInfo.PID) {
		needStart = true
		daemon.RemoveLockFile(projectRoot)
		daemon.CleanupSocket(lockInfo.SocketPath)
	} else if daemon.IsDaemonStale(projectRoot, lockInfo) {
		// Stop old daemon
		if config.Verbose {
			fmt.Fprintf(os.Stderr, "Stopping stale daemon (PID %d)...\n", lockInfo.PID)
		}
		syscall.Kill(lockInfo.PID, syscall.SIGTERM)
		time.Sleep(500 * time.Millisecond)
		needStart = true
	}

	if needStart {
		if err := startDaemon(projectRoot, config.Verbose); err != nil {
			return fmt.Errorf("failed to start daemon: %v", err)
		}

		// Re-read lock file
		lockInfo, err = daemon.ReadLockFile(projectRoot)
		if err != nil || lockInfo == nil {
			return fmt.Errorf("daemon started but lock file not found")
		}
	}

	// Connect to daemon
	conn, err := net.Dial("unix", lockInfo.SocketPath)
	if err != nil {
		return fmt.Errorf("failed to connect to daemon: %v", err)
	}
	defer conn.Close()

	// Create client
	client := NewClient(conn, time.Duration(config.Timeout)*time.Second)

	// Execute command and print output
	output, err := client.handleCommand(config)
	if err != nil {
		return err
	}
	fmt.Println(output)

	return nil
}

func startDaemon(projectRoot string, verbose bool) error {
	if verbose {
		fmt.Fprintf(os.Stderr, "Starting daemon...\n")
	}

	// Get current executable path
	execPath, err := os.Executable()
	if err != nil {
		return err
	}

	// Start daemon as background process
	args := []string{"daemon", projectRoot}
	if verbose {
		args = append(args, "--verbose")
	}
	cmd := exec.Command(execPath, args...)

	// Detach from current process group
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Redirect output to daemon log
	logPath := daemon.GetLogPath(projectRoot)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return err
	}

	// Don't wait for it - let it run in background
	go cmd.Wait()

	// Wait for the daemon's socket to appear. Startup can legitimately take
	// a long time: on a cold start the daemon may run a user-configured
	// generate command (e.g. a CMake preset configure) and clangd's initial
	// indexing before it begins listening, so the wait budget is generous
	// and configurable. Liveness is tracked through the spawned process
	// itself rather than the lock file, so a leftover lock from a previous
	// daemon cannot trick the client into giving up on a healthy one.
	socketPath := daemon.GetSocketPath(projectRoot)
	timeout := startupTimeout()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socketPath); err == nil {
			// Socket exists, try to connect
			if conn, err := net.Dial("unix", socketPath); err == nil {
				conn.Close()
				return nil
			}
		}

		if !daemon.IsProcessAlive(cmd.Process.Pid) {
			return fmt.Errorf("daemon exited during startup%s", daemonLogTail(projectRoot))
		}

		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("daemon failed to start within %s%s", timeout, daemonLogTail(projectRoot))
}

// Returns how long the client waits for a freshly started daemon to become
// ready. Defaults to two minutes and can be overridden with the
// CLANGD_QUERY_STARTUP_TIMEOUT environment variable using Go duration syntax
// (e.g. "30s", "5m"), mirroring the daemon's CLANGD_DAEMON_TIMEOUT setting.
// Cold starts of large projects can involve a full CMake configure and clangd
// indexing before the daemon accepts connections, which is why the budget is
// far larger than a request timeout.
func startupTimeout() time.Duration {
	if value := os.Getenv("CLANGD_QUERY_STARTUP_TIMEOUT"); value != "" {
		if duration, err := time.ParseDuration(value); err == nil {
			return duration
		}
	}
	return 2 * time.Minute
}

// Reads the last chunk of the daemon's log file and formats it for inclusion
// in a startup error message. Returns an empty string when the log cannot be
// read. The tail usually contains the reason a daemon died during startup,
// most commonly a failing generate command, so surfacing it saves a separate
// trip to the log file.
func daemonLogTail(projectRoot string) string {
	const tailSize = 2000

	logPath := daemon.GetLogPath(projectRoot)
	file, err := os.Open(logPath)
	if err != nil {
		return ""
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return ""
	}

	offset := int64(0)
	if stat.Size() > tailSize {
		offset = stat.Size() - tailSize
	}

	buf := make([]byte, stat.Size()-offset)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return ""
	}

	return "\n--- daemon log tail ---\n" + string(buf)
}
