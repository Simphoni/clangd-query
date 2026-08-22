package clangd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Writes a single LSP-framed JSON message to w.
func writeLSPMessage(t *testing.T, w io.Writer, msg map[string]interface{}) {
	t.Helper()

	content, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("failed to marshal test message: %v", err)
	}

	header := "Content-Length: " + strconv.Itoa(len(content)) + "\r\n\r\n"
	if _, err := io.WriteString(w, header); err != nil {
		t.Fatalf("failed to write test headers: %v", err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatalf("failed to write test body: %v", err)
	}
}

// Reads a single LSP-framed JSON message from r.
func readLSPMessage(t *testing.T, r *bufio.Reader) map[string]interface{} {
	t.Helper()

	contentLength := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("failed to read test header: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			contentLength, err = strconv.Atoi(strings.TrimPrefix(line, "Content-Length: "))
			if err != nil {
				t.Fatalf("invalid test Content-Length: %v", err)
			}
		}
	}

	content := make([]byte, contentLength)
	if _, err := io.ReadFull(r, content); err != nil {
		t.Fatalf("failed to read test body: %v", err)
	}

	var msg map[string]interface{}
	if err := json.Unmarshal(content, &msg); err != nil {
		t.Fatalf("failed to parse test message: %v", err)
	}
	return msg
}

// Covers the transport's three responsibilities: correlating request responses,
// dispatching asynchronous notifications, and replying to server-initiated
// requests such as window/workDoneProgress/create.
func TestTransportRequestNotificationAndServerRequest(t *testing.T) {
	serverReader, clientWriter := io.Pipe()
	clientReader, serverWriter := io.Pipe()

	transport := NewTransport(clientReader, clientWriter, io.Discard)
	transport.Start()

	notifications := make(chan json.RawMessage, 1)
	transport.RegisterNotificationHandler("test/notify", func(params json.RawMessage) {
		notifications <- params
	})

	serverDone := make(chan struct{})
	serverErr := make(chan error, 1)

	go func() {
		reader := bufio.NewReader(serverReader)

		// 1. Handle the client's request and send a response.
		request := readLSPMessage(t, reader)
		if request["method"] != "test/request" {
			serverErr <- fmt.Errorf("expected test/request, got %v", request["method"])
			return
		}
		writeLSPMessage(t, serverWriter, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      request["id"],
			"result":  json.RawMessage(`"pong"`),
		})

		// 2. Send a server-initiated request and read the client's reply.
		writeLSPMessage(t, serverWriter, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      "server-request-1",
			"method":  "window/workDoneProgress/create",
			"params":  map[string]interface{}{"token": "indexing"},
		})
		reply := readLSPMessage(t, reader)
		if reply["id"] != "server-request-1" {
			serverErr <- fmt.Errorf("expected reply to server request, got %v", reply)
			return
		}

		// 3. Send an asynchronous notification.
		writeLSPMessage(t, serverWriter, map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "test/notify",
			"params":  json.RawMessage(`{"value":42}`),
		})

		close(serverDone)
	}()

	result, err := transport.SendRequest("test/request", map[string]interface{}{})
	if err != nil {
		t.Fatalf("SendRequest failed: %v", err)
	}
	if string(result) != `"pong"` {
		t.Fatalf("unexpected result: %s", result)
	}

	select {
	case params := <-notifications:
		if string(params) != `{"value":42}` {
			t.Fatalf("unexpected notification params: %s", params)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification")
	}

	select {
	case <-serverDone:
	case err := <-serverErr:
		t.Fatalf("server goroutine failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server goroutine")
	}
}
