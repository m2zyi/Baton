package adapters

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcClient struct {
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	messages   chan rpcMessage
	readerDone chan error
	closed     chan struct{}
	stderr     *boundedBuffer
	nextID     int
	timeout    time.Duration
	closeOnce  sync.Once
}

func startRPCClient(ctx context.Context, executable string, timeout time.Duration) (*rpcClient, error) {
	cmd := exec.Command(executable, "app-server")
	cmd.Env = os.Environ()
	configureProcess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, failure(true, "codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, failure(true, "codex app-server stdout: %w", err)
	}
	stderr := &boundedBuffer{limit: 64 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, failure(true, "start codex app-server: %w", err)
	}

	client := &rpcClient{
		cmd:        cmd,
		stdin:      stdin,
		messages:   make(chan rpcMessage, 64),
		readerDone: make(chan error, 1),
		closed:     make(chan struct{}),
		stderr:     stderr,
		timeout:    timeout,
	}
	go client.readLoop(stdout)
	if err := client.initialize(ctx); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func (c *rpcClient) readLoop(stdout io.Reader) {
	defer close(c.messages)
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
			var message rpcMessage
			if decodeErr := json.Unmarshal(trimmed, &message); decodeErr != nil {
				c.readerDone <- fmt.Errorf("decode codex app-server output: %w", decodeErr)
				return
			}
			select {
			case c.messages <- message:
			case <-c.closed:
				c.readerDone <- errors.New("codex app-server client closed")
				return
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				c.readerDone <- io.EOF
			} else {
				c.readerDone <- err
			}
			return
		}
	}
}

func (c *rpcClient) initialize(ctx context.Context) error {
	params := map[string]any{
		"clientInfo": map[string]any{
			"name":    "baton",
			"title":   "Baton",
			"version": "0.1.0",
		},
	}
	if _, err := c.call(ctx, "initialize", params); err != nil {
		if isUnsupportedRPC(err) {
			return failure(true, "codex app-server initialize: %w", err)
		}
		return err
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		return failure(true, "codex app-server initialized notification: %w", err)
	}
	return nil
}

func (c *rpcClient) notify(method string, params any) error {
	message := map[string]any{"method": method, "params": params}
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = c.stdin.Write(encoded)
	return err
}

func (c *rpcClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID
	c.nextID++
	message := map[string]any{"method": method, "id": id, "params": params}
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if _, err := c.stdin.Write(encoded); err != nil {
		return nil, failure(true, "write %s to codex app-server: %w", method, err)
	}

	timeout := c.timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	wanted := strconv.Itoa(id)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("codex app-server %s timed out after %s", method, timeout)
		case message, ok := <-c.messages:
			if !ok {
				select {
				case readerErr := <-c.readerDone:
					if errors.Is(readerErr, io.EOF) {
						return nil, failure(true, "codex app-server closed during %s: %s", method, c.stderr.String())
					}
					return nil, failure(true, "read codex app-server response during %s: %v: %s", method, readerErr, c.stderr.String())
				default:
					return nil, failure(true, "codex app-server closed during %s: %s", method, c.stderr.String())
				}
			}
			if message.Method != "" {
				if len(message.ID) > 0 {
					// Read-only history does not handle server approval or
					// elicitation requests. Refuse rather than leaving the server
					// waiting for a response.
					_ = c.replyError(message.ID, -32601, "baton read-only adapter does not support "+message.Method)
				}
				continue
			}
			if string(message.ID) != wanted {
				continue
			}
			if message.Error != nil {
				return nil, message.Error
			}
			return message.Result, nil
		}
	}
}

func (c *rpcClient) replyError(id json.RawMessage, code int, message string) error {
	response := map[string]any{
		"id":    json.RawMessage(id),
		"error": map[string]any{"code": code, "message": message},
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = c.stdin.Write(encoded)
	return err
}

func (c *rpcClient) Close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.stdin.Close()
		terminateProcess(c.cmd)
		finished := make(chan struct{})
		go func() {
			_ = c.cmd.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-time.After(time.Second):
			_ = c.cmd.Process.Kill()
			<-finished
		}
	})
}

type boundedBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(p)
	if b.limit > 0 && len(b.data)+len(p) > b.limit {
		remaining := b.limit - len(b.data)
		if remaining > 0 {
			b.data = append(b.data, p[:remaining]...)
		}
		return original, nil
	}
	b.data = append(b.data, p...)
	return original, nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
