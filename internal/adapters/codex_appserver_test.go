package adapters

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"baton/internal/model"
)

const fakeHandoff = "[[BATON_HANDOFF:packet-appserver:BEGIN]]\nImported from Codex\n[[BATON_HANDOFF:packet-appserver:END]]"

func TestFakeCodexServer(t *testing.T) {
	if os.Getenv("BATON_FAKE_CODEX") != "1" {
		return
	}
	if pidFile := os.Getenv("BATON_FAKE_CODEX_PID_FILE"); pidFile != "" {
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	runFakeCodexServer(os.Stdin, os.Stdout, os.Getenv("BATON_FAKE_CODEX_MODE"))
	os.Exit(0)
}

type fakeRPCRequest struct {
	ID     any             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func runFakeCodexServer(in *os.File, out *os.File, mode string) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(out)
	send := func(value any) {
		_ = encoder.Encode(value)
	}
	cwd := os.Getenv("BATON_FAKE_CODEX_CWD")
	if cwd == "" {
		cwd = "/tmp/baton-test/project"
	}
	threadPath := os.Getenv("BATON_FAKE_CODEX_THREAD_PATH")
	if threadPath == "" {
		threadPath = filepath.Join(cwd, "rollout.jsonl")
	}
	thread := map[string]any{
		"id":        "fake-thread-1",
		"sessionId": "fake-thread-1",
		"cwd":       cwd,
		"path":      threadPath,
		"name":      "Fake app-server thread",
		"preview":   "hello appserver",
		"createdAt": int64(1762850000),
		"updatedAt": int64(1762850100),
	}
	page := 0

	for scanner.Scan() {
		var request fakeRPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			continue
		}
		if request.ID == nil {
			continue
		}
		switch request.Method {
		case "initialize":
			send(map[string]any{
				"id": request.ID,
				"result": map[string]any{
					"userAgent":      "fake-codex",
					"platformFamily": "unix",
					"platformOs":     "macos",
				},
			})
		case "thread/list":
			if mode == "hang" {
				for {
					time.Sleep(time.Hour)
				}
			}
			if mode == "pages" {
				page++
				send(map[string]any{
					"id": request.ID,
					"result": map[string]any{
						"data":       []any{},
						"nextCursor": fmt.Sprintf("page-%d", page),
					},
				})
				continue
			}
			send(map[string]any{"method": "warning", "params": map[string]any{"message": "ignored notification"}})
			send(map[string]any{
				"id": request.ID,
				"result": map[string]any{
					"data": []any{
						thread,
						map[string]any{
							"id":        "fake-thread-2",
							"sessionId": "fake-thread-2",
							"cwd":       "/tmp/other/project",
							"path":      "/tmp/other/project/rollout.jsonl",
							"name":      "Other thread",
							"preview":   "other",
							"createdAt": int64(1762850000),
							"updatedAt": int64(1762850000),
						},
					},
					"nextCursor": nil,
				},
			})
		case "thread/read":
			send(map[string]any{"id": request.ID, "result": map[string]any{"thread": thread}})
		case "thread/items/list":
			if mode == "turns" || mode == "legacy" {
				send(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
				continue
			}
			if strings.Contains(string(request.Params), `"cursor":"items-2"`) {
				send(map[string]any{
					"id": request.ID,
					"result": map[string]any{
						"data": []any{
							itemEntry("turn-1", appUserItem("u2", fakeHandoff+"\n\nPlease continue.")),
							itemEntry("turn-1", map[string]any{
								"type":          "functionCallOutput",
								"id":            "output-1",
								"name":          "shell",
								"output":        "tool output",
								"turnId":        "turn-1",
								"startedAtMs":   int64(1762850006500),
								"completedAtMs": int64(1762850007000),
								"server":        nil,
								"arguments":     nil,
							}),
						},
						"nextCursor": nil,
					},
				})
				continue
			}
			send(map[string]any{
				"id": request.ID,
				"result": map[string]any{
					"data": []any{
						itemEntry("turn-1", appUserItem("u1", "hello appserver")),
						itemEntry("turn-1", map[string]any{"type": "reasoning", "id": "reason-1", "summary": []string{"private reasoning"}, "content": []string{}}),
						itemEntry("turn-1", map[string]any{"type": "agentMessage", "id": "a1", "text": "working"}),
						itemEntry("turn-1", map[string]any{
							"type":             "commandExecution",
							"id":               "cmd-1",
							"command":          "echo hi",
							"cwd":              cwd,
							"status":           "completed",
							"commandActions":   []any{},
							"aggregatedOutput": "hi\n",
							"exitCode":         0,
						}),
					},
					"nextCursor": "items-2",
				},
			})
		case "thread/turns/list":
			if mode == "legacy" {
				send(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
				continue
			}
			send(map[string]any{
				"id": request.ID,
				"result": map[string]any{
					"data": []any{
						map[string]any{
							"id":        "turn-1",
							"status":    "completed",
							"startedAt": int64(1762850000),
							"itemsView": "full",
							"items": []any{
								appUserItem("u1", "hello appserver"),
								map[string]any{"type": "reasoning", "id": "reason-1", "summary": []string{"private reasoning"}, "content": []string{}},
								map[string]any{"type": "agentMessage", "id": "a1", "text": "working"},
								map[string]any{
									"type":             "commandExecution",
									"id":               "cmd-1",
									"command":          "echo hi",
									"cwd":              cwd,
									"status":           "completed",
									"commandActions":   []any{},
									"aggregatedOutput": "hi\n",
									"exitCode":         0,
								},
								appUserItem("u2", fakeHandoff+"\n\nPlease continue."),
								map[string]any{"type": "functionCallOutput", "id": "output-1", "name": "shell", "output": "tool output"},
							},
						},
					},
					"nextCursor": nil,
				},
			})
		default:
			send(map[string]any{"id": request.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
	}
}

func appUserItem(id, text string) map[string]any {
	return map[string]any{
		"type":    "userMessage",
		"id":      id,
		"content": []any{map[string]any{"type": "text", "text": text}},
	}
}

func itemEntry(turnID string, item map[string]any) map[string]any {
	return map[string]any{
		"turnId":        turnID,
		"startedAtMs":   int64(1762850000000),
		"completedAtMs": int64(1762850001000),
		"item":          item,
	}
}

func modelSession(id, cwd string) model.Session {
	return model.Session{ID: id, Agent: model.Codex, CWD: cwd}
}

func fakeCodexAdapter(t *testing.T, mode string) (*codexAdapter, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	content := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestFakeCodexServer$ -- \"$@\"\n", executable)
	if err := os.WriteFile(script, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BATON_FAKE_CODEX", "1")
	t.Setenv("BATON_FAKE_CODEX_MODE", mode)
	t.Setenv("BATON_FAKE_CODEX_CWD", "/tmp/baton-test/project")
	adapter := newCodexAdapter()
	adapter.bin = script
	adapter.timeout = 2 * time.Second
	return adapter, dir
}

func TestCodexAppServerDiscoverFiltersExactCWD(t *testing.T) {
	adapter, _ := fakeCodexAdapter(t, "items")
	sessions, err := adapter.Discover(context.Background(), "/tmp/baton-test/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1: %#v", len(sessions), sessions)
	}
	if sessions[0].ID != "fake-thread-1" || sessions[0].Title != "Fake app-server thread" {
		t.Fatalf("unexpected session: %#v", sessions[0])
	}
	other, err := adapter.Discover(context.Background(), "/tmp/other/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].ID != "fake-thread-2" {
		t.Fatalf("same-basename cwd filter returned %#v", other)
	}
}

func TestCodexAppServerPaginatedItemsAndHandoffSplit(t *testing.T) {
	adapter, _ := fakeCodexAdapter(t, "items")
	session := modelSession("fake-thread-1", "/tmp/baton-test/project")
	events, err := adapter.Read(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	assertCodexAppServerEvents(t, events)

	again, err := adapter.Read(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, again) {
		t.Fatalf("event IDs are not deterministic:\nfirst:  %#v\nsecond: %#v", events, again)
	}
}

func TestCodexAppServerDiscoverPaginatesExplicitly(t *testing.T) {
	adapter, _ := fakeCodexAdapter(t, "pages")
	_, err := adapter.Discover(context.Background(), "/tmp/baton-test/project")
	if err == nil || !strings.Contains(err.Error(), "exceeded 1000 pages") {
		t.Fatalf("error = %v, want explicit pagination limit failure", err)
	}
}

func TestCodexAppServerTurnsFallbackUsesFullItems(t *testing.T) {
	adapter, _ := fakeCodexAdapter(t, "turns")
	events, err := adapter.Read(context.Background(), modelSession("fake-thread-1", "/tmp/baton-test/project"))
	if err != nil {
		t.Fatal(err)
	}
	assertCodexAppServerEvents(t, events)
}

func TestCodexAppServerCapabilityFailureFallsBackToRollout(t *testing.T) {
	adapter, _ := fakeCodexAdapter(t, "legacy")
	t.Setenv("BATON_FAKE_CODEX_THREAD_PATH", filepath.Join("testdata", "codex-home", "sessions", "2026", "11", "11", "rollout-2026-11-11T10-00-00-codex-session-1.jsonl"))
	sessions, err := adapter.Discover(context.Background(), "/tmp/baton-test/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1", len(sessions))
	}
	events, err := adapter.Read(context.Background(), sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 || events[4].Kind != "summary" || events[5].Kind != "handoff" {
		t.Fatalf("unexpected rollout fallback events: %#v", events)
	}
}

func assertCodexAppServerEvents(t *testing.T, events []model.Event) {
	t.Helper()
	if len(events) != 6 {
		t.Fatalf("got %d events, want 6: %#v", len(events), events)
	}
	want := []struct {
		kind string
		role string
		text string
	}{
		{"message", "user", "hello appserver"},
		{"message", "assistant", "working"},
		{"tool", "tool", "commandExecution"},
		{"handoff", "user", fakeHandoff},
		{"message", "user", "Please continue."},
		{"tool", "tool", "tool output"},
	}
	for index, expectation := range want {
		event := events[index]
		if event.Kind != expectation.kind || event.Role != expectation.role || !strings.Contains(event.Text, expectation.text) {
			t.Fatalf("event %d = {%s %s %q}, want {%s %s containing %q}", index, event.Kind, event.Role, event.Text, expectation.kind, expectation.role, expectation.text)
		}
		if event.Source != model.Codex || event.SessionID != "fake-thread-1" || event.ID == "" {
			t.Fatalf("event %d has invalid identity: %#v", index, event)
		}
	}
	if strings.Contains(events[0].Text, "private reasoning") {
		t.Fatal("reasoning was imported")
	}
}

func TestCodexAppServerTimeoutCleansUpProcess(t *testing.T) {
	adapter, dir := fakeCodexAdapter(t, "hang")
	adapter.timeout = 200 * time.Millisecond
	pidFile := filepath.Join(dir, "server.pid")
	t.Setenv("BATON_FAKE_CODEX_PID_FILE", pidFile)

	start := time.Now()
	_, err := adapter.Discover(context.Background(), "/tmp/baton-test/project")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout took too long: %s", elapsed)
	}
	var pid int
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		data, readErr := os.ReadFile(pidFile)
		if readErr == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if pid != 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("fake app-server did not record its pid")
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if !processAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake app-server process %d was not cleaned up", pid)
}
