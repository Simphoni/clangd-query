package clangd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Request represents a JSON-RPC 2.0 request message that expects a response.
// Requests always have an ID field that correlates the response back to the request.
type Request struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response represents a JSON-RPC 2.0 response message.
// Every response includes the ID from the original request for correlation.
// Either Result or Error will be set, but never both.
type Response struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Notification represents a JSON-RPC 2.0 notification message.
// Notifications are fire-and-forget messages that don't expect a response.
// They lack an ID field, which distinguishes them from requests.
type Notification struct {
	Jsonrpc string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Error represents a JSON-RPC 2.0 error object returned in a response.
// The Code field uses standard JSON-RPC error codes when applicable.
type Error struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Standard JSON-RPC 2.0 error codes as defined in the specification.
// These codes indicate protocol-level errors rather than application errors.
const (
	ParseError     = -32700 // Invalid JSON was received
	InvalidRequest = -32600 // JSON is not a valid Request object
	MethodNotFound = -32601 // Method does not exist or is not available
	InvalidParams  = -32602 // Invalid method parameters
	InternalError  = -32603 // Internal JSON-RPC error
)

// Common transport errors that can occur during JSON-RPC communication.
// These are transport-level errors, distinct from JSON-RPC protocol errors.
var (
	ErrConnectionClosed = errors.New("connection closed")
	ErrTimeout          = errors.New("request timeout")
)

// Transport manages JSON-RPC 2.0 communication over stdin/stdout using a
// dedicated background reader. This is important because clangd sends
// asynchronous notifications (most notably $/progress for background
// indexing) even when no request is outstanding; a transport that only reads
// while waiting for a response would never observe those notifications.
//
// Requests are serialized through a mutex so there is no ambiguity about
// which response belongs to which caller, while the reader goroutine
// concurrently dispatches notifications and routes responses to waiting
// callers.
type Transport struct {
	reader *bufio.Reader
	writer io.Writer
	stderr io.Writer

	nextID atomic.Int64

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan *Response

	handlersMu sync.RWMutex
	handlers   map[string]NotificationHandler

	startOnce sync.Once
	done      chan struct{}

	closedMu sync.Mutex
	closed   bool
}

// NotificationHandler processes incoming notifications from the server.
type NotificationHandler func(params json.RawMessage)

// Creates a new Transport for JSON-RPC communication.
// The stdin parameter is used for reading responses and notifications,
// stdout for writing requests and notifications, and stderr for error logging.
func NewTransport(stdin io.Reader, stdout, stderr io.Writer) *Transport {
	return &Transport{
		reader:   bufio.NewReader(stdin),
		writer:   stdout,
		stderr:   stderr,
		handlers: make(map[string]NotificationHandler),
		pending:  make(map[string]chan *Response),
		done:     make(chan struct{}),
	}
}

// Starts the background reader goroutine. Start is idempotent: only the first
// call has an effect.
func (t *Transport) Start() {
	t.startOnce.Do(func() {
		go t.readLoop()
	})
}

// Registers a handler for a specific notification method.
// Only one handler can be registered per method; subsequent registrations
// will replace the previous handler.
func (t *Transport) RegisterNotificationHandler(method string, handler NotificationHandler) {
	t.handlersMu.Lock()
	defer t.handlersMu.Unlock()
	t.handlers[method] = handler
}

// Sends a JSON-RPC request and blocks until the response arrives or the
// 30-second request timeout expires. This method is safe for concurrent use;
// writes are serialized and responses are correlated by request ID.
func (t *Transport) SendRequest(method string, params interface{}) (json.RawMessage, error) {
	if t.isClosed() {
		return nil, ErrConnectionClosed
	}

	id := strconv.FormatInt(t.nextID.Add(1), 10)
	responseCh := make(chan *Response, 1)

	t.pendingMu.Lock()
	t.pending[id] = responseCh
	t.pendingMu.Unlock()

	defer func() {
		t.pendingMu.Lock()
		delete(t.pending, id)
		t.pendingMu.Unlock()
	}()

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("Error encoding request params: %w", err)
	}

	req := Request{
		Jsonrpc: "2.0",
		ID:      id,
		Method:  method,
		Params:  paramsJSON,
	}

	t.writeMu.Lock()
	err = t.writeMessage(req)
	t.writeMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("Error writing request: %w", err)
	}

	select {
	case resp := <-responseCh:
		if resp == nil {
			return nil, ErrConnectionClosed
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("RPC error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil

	case <-time.After(30 * time.Second):
		return nil, ErrTimeout
	}
}

// Sends a notification to the server without expecting a response.
func (t *Transport) SendNotification(method string, params interface{}) error {
	if t.isClosed() {
		return ErrConnectionClosed
	}

	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("Error encoding request params: %w", err)
	}

	notif := Notification{
		Jsonrpc: "2.0",
		Method:  method,
		Params:  paramsJSON,
	}

	t.writeMu.Lock()
	err = t.writeMessage(notif)
	t.writeMu.Unlock()
	if err != nil {
		return fmt.Errorf("Error writing notification: %w", err)
	}

	return nil
}

// Continuous read loop for the server output stream. It separates incoming
// messages into notifications, server-initiated requests, and responses, and
// routes each appropriately. The loop exits when the stream closes.
func (t *Transport) readLoop() {
	for {
		msg, err := t.readMessage()
		if err != nil {
			if !t.isClosed() {
				t.setClosed()
				t.failAllPending()
			}
			return
		}

		_, hasMethod := msg["method"].(string)
		if hasMethod {
			if msg["id"] != nil {
				// Server-initiated request. The only one clangd currently
				// sends is window/workDoneProgress/create, which expects a
				// null result. Answering it is required before clangd sends
				// $/progress notifications.
				t.handleServerRequest(msg)
			} else {
				var notif Notification
				data, _ := json.Marshal(msg)
				if json.Unmarshal(data, &notif) == nil {
					t.handleNotification(&notif)
				}
			}
			continue
		}

		// Response to one of our outstanding requests.
		id := responseID(msg)
		if id == "" {
			continue
		}

		var resp Response
		data, _ := json.Marshal(msg)
		if json.Unmarshal(data, &resp) != nil {
			continue
		}
		t.deliverResponse(id, &resp)
	}
}

// Replies to a server-initiated request. clangd's window/workDoneProgress/create
// accepts a null result; no other server requests are currently implemented.
func (t *Transport) handleServerRequest(msg map[string]interface{}) {
	response := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      msg["id"],
		"result":  nil,
	}

	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if err := t.writeMessage(response); err != nil {
		t.setClosed()
		t.failAllPending()
	}
}

// Reads a single JSON-RPC message from the input stream using the standard
// LSP Content-Length framing.
func (t *Transport) readMessage() (map[string]interface{}, error) {
	var contentLength int
	for {
		line, err := t.reader.ReadString('\n')
		if err != nil {
			return nil, err
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break
		}

		if strings.HasPrefix(line, "Content-Length: ") {
			lengthStr := strings.TrimPrefix(line, "Content-Length: ")
			length, err := strconv.Atoi(strings.TrimSpace(lengthStr))
			if err != nil {
				return nil, fmt.Errorf("invalid Content-Length: %w", err)
			}
			if length < 0 || length > 10*1024*1024 {
				return nil, fmt.Errorf("invalid Content-Length %d: must be between 0 and 10MB", length)
			}
			contentLength = length
		}
	}

	if contentLength == 0 {
		return nil, errors.New("missing Content-Length header")
	}

	content := make([]byte, contentLength)
	if _, err := io.ReadFull(t.reader, content); err != nil {
		return nil, err
	}

	var msg map[string]interface{}
	if err := json.Unmarshal(content, &msg); err != nil {
		return nil, fmt.Errorf("parse message: %w", err)
	}

	return msg, nil
}

// Extracts a request ID into the string form used by pending lookups.
func responseID(msg map[string]interface{}) string {
	if id, ok := msg["id"].(string); ok {
		return id
	}
	if id, ok := msg["id"].(float64); ok {
		return strconv.FormatFloat(id, 'f', -1, 64)
	}
	return ""
}

// Dispatches a notification to its registered handler, if any.
func (t *Transport) handleNotification(notif *Notification) {
	t.handlersMu.RLock()
	handler, ok := t.handlers[notif.Method]
	t.handlersMu.RUnlock()

	if ok {
		handler(notif.Params)
	}
}

// Routes a response to the goroutine waiting for its request ID.
func (t *Transport) deliverResponse(id string, resp *Response) {
	t.pendingMu.Lock()
	ch, ok := t.pending[id]
	t.pendingMu.Unlock()

	if ok {
		select {
		case ch <- resp:
		default:
			// The caller already timed out; discard the late response.
		}
	}
}

// Fails every outstanding request because the connection is gone.
func (t *Transport) failAllPending() {
	t.pendingMu.Lock()
	defer t.pendingMu.Unlock()

	for id, ch := range t.pending {
		select {
		case ch <- nil:
		default:
		}
		delete(t.pending, id)
	}
}

// Writes a JSON-RPC message with LSP Content-Length framing. The caller must
// hold writeMu.
func (t *Transport) writeMessage(msg interface{}) error {
	content, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(content))

	if _, err := t.writer.Write([]byte(header)); err != nil {
		return err
	}

	if _, err := t.writer.Write(content); err != nil {
		return err
	}

	return nil
}

// Reports whether the transport has been marked closed.
func (t *Transport) isClosed() bool {
	t.closedMu.Lock()
	defer t.closedMu.Unlock()
	return t.closed
}

// Marks the transport closed.
func (t *Transport) setClosed() {
	t.closedMu.Lock()
	t.closed = true
	t.closedMu.Unlock()
}
