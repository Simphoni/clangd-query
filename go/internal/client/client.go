package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

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

// Reports whether the given command operates on one or more symbol arguments.
// These are exactly the commands that support batch execution: every positional
// argument after the command name is treated as an independent query target.
func isSymbolCommand(command string) bool {
	switch command {
	case "search", "show", "view", "usages", "hierarchy", "signature", "interface":
		return true
	}
	return false
}

// Executes a single symbol-based query and returns its formatted output. The
// mapping between command names and RPC calls lives here so that both the
// single-query path in handleCommand and the batch path in runBatchQueries
// share identical behavior per query.
func (c *Client) executeSymbolQuery(command string, symbol string, limit int) (string, error) {
	switch command {
	case "search":
		return c.Search(symbol, limit)
	case "show":
		return c.Show(symbol)
	case "view":
		return c.View(symbol)
	case "usages":
		return c.Usages(symbol, limit)
	case "hierarchy":
		return c.Hierarchy(symbol, limit)
	case "signature":
		return c.Signature(symbol)
	case "interface":
		return c.Interface(symbol)
	default:
		return "", fmt.Errorf("unknown command: %s", command)
	}
}

// Runs each positional argument as an independent query of the same command
// and returns the combined sectioned output. Every section starts with a
// "=== <command> <symbol> ===" header so that consumers can reliably locate
// the result of an individual query. A failing query does not abort the
// remaining ones: its error is written into its own section instead, and the
// returned error only summarizes how many queries failed so that callers can
// signal a non-zero exit code without discarding successful results.
func (c *Client) runBatchQueries(config *Config) (string, error) {
	var output strings.Builder
	failed := 0

	for i, symbol := range config.Arguments {
		if i > 0 {
			output.WriteString("\n")
		}
		fmt.Fprintf(&output, "=== %s %s ===\n", config.Command, symbol)

		result, err := c.executeSymbolQuery(config.Command, symbol, config.Limit)
		if err != nil {
			failed++
			fmt.Fprintf(&output, "Error: %v\n", err)
			continue
		}

		output.WriteString(result)
		if !strings.HasSuffix(result, "\n") {
			output.WriteString("\n")
		}
	}

	if failed > 0 {
		return output.String(), fmt.Errorf("%d of %d queries failed", failed, len(config.Arguments))
	}
	return output.String(), nil
}

// Parses one batch input line into its command name, positional arguments and
// optional per-line limit. Lines are whitespace-separated with support for
// double-quoted arguments, so a line like `search "My Class" --limit 5` is
// split the same way the top-level argument parser would split it. Only
// --limit is recognized per line because the other global flags either apply
// to the whole process (--verbose) or make no sense per query (--timeout).
func parseQueryLine(line string) (command string, args []string, limit int, err error) {
	tokens, err := tokenizeQueryLine(line)
	if err != nil {
		return "", nil, 0, err
	}

	limit = -1
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		switch {
		case token == "--limit":
			if i+1 >= len(tokens) {
				return "", nil, 0, fmt.Errorf("flag --limit requires a value")
			}
			parsed, convErr := strconv.Atoi(tokens[i+1])
			if convErr != nil {
				return "", nil, 0, fmt.Errorf("invalid limit value: %s", tokens[i+1])
			}
			limit = parsed
			i++
		case strings.HasPrefix(token, "-"):
			return "", nil, 0, fmt.Errorf("unsupported flag %q in batch query line", token)
		case command == "":
			command = token
		default:
			args = append(args, token)
		}
	}
	return command, args, limit, nil
}

// Splits a batch input line into whitespace-separated tokens, treating any
// text between double quotes as a single token. Quotes themselves are stripped
// from the token so that quoted symbols arrive at the daemon without them.
func tokenizeQueryLine(line string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	inQuotes := false

	for _, r := range line {
		switch {
		case r == '"':
			inQuotes = !inQuotes
		case unicode.IsSpace(r) && !inQuotes:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}

	if inQuotes {
		return nil, fmt.Errorf("unterminated quote in batch query line")
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens, nil
}

// Runs a full batch session where every input line is a complete query command
// such as `show GameObject` or `hierarchy Transform --limit 5`. This differs
// from runBatchQueries, which repeats one fixed command across all arguments:
// here each line picks its own command and flags. Sections follow the same
// rules as everywhere else in batch mode: every query gets a stable header,
// failures are confined to their own section, and the returned error only
// summarizes how many queries failed so that successful results survive on
// stdout even when the process exits non-zero.
func (c *Client) runCommandLineBatch(lines []string) (string, error) {
	var output strings.Builder
	failed := 0
	total := 0
	firstSection := true

	writeSection := func(header string, result string, err error) {
		if !firstSection {
			output.WriteString("\n")
		}
		firstSection = false
		total++

		fmt.Fprintf(&output, "=== %s ===\n", header)
		if err != nil {
			failed++
			fmt.Fprintf(&output, "Error: %v\n", err)
			return
		}
		output.WriteString(result)
		if !strings.HasSuffix(result, "\n") {
			output.WriteString("\n")
		}
	}

	for _, line := range lines {
		command, args, limit, err := parseQueryLine(line)
		if err == nil && !isSymbolCommand(command) {
			err = fmt.Errorf("command %q is not supported in batch mode", command)
		}
		if err == nil && len(args) == 0 {
			err = fmt.Errorf("%s requires a symbol argument", command)
		}
		if err != nil {
			writeSection(line, "", err)
			continue
		}

		for _, symbol := range args {
			result, queryErr := c.executeSymbolQuery(command, symbol, limit)
			writeSection(command+" "+symbol, result, queryErr)
		}
	}

	if failed > 0 {
		return output.String(), fmt.Errorf("%d of %d batch queries failed", failed, total)
	}
	return output.String(), nil
}

// handleCommand processes a command and returns the output as a string
func (c *Client) handleCommand(config *Config) (string, error) {
	symbol := ""
	if isSymbolCommand(config.Command) {
		if len(config.Arguments) == 0 {
			return "", fmt.Errorf("%s requires a symbol argument", config.Command)
		}
		symbol = config.Arguments[0]
	}

	if isSymbolCommand(config.Command) {
		return c.executeSymbolQuery(config.Command, symbol, config.Limit)
	}

	switch config.Command {
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

	// Batch-file mode: the `batch` command reads complete query lines and
	// runs each as an independent query of whatever command the line names.
	// Output rules match the multi-symbol path above.
	if config.Command == "batch" {
		output, batchErr := client.runCommandLineBatch(config.Arguments)
		fmt.Println(output)
		return batchErr
	}

	// Batch mode: more than one positional argument on a symbol command runs
	// every argument as an independent query. The combined sections always go
	// to stdout, even when some queries fail; the error only drives the exit
	// code so that partial results are never discarded.
	if isSymbolCommand(config.Command) && len(config.Arguments) > 1 {
		output, batchErr := client.runBatchQueries(config)
		fmt.Println(output)
		return batchErr
	}

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
