package adapters

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"terminus/internal/model"
)

// The subprocess speaks line-delimited JSON on stdin/stdout, like app-server.
func TestIndependentServer(t *testing.T) {
	if os.Getenv("INDEPENDENT_SERVER") != "1" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	mode := os.Getenv("INDEPENDENT_MODE")
	for scan.Scan() {
		var req struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		send := func(result any) { _ = out.Encode(map[string]any{"id": *req.ID, "result": result}) }
		unsupported := func() {
			_ = out.Encode(map[string]any{"id": *req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		}
		switch req.Method {
		case "initialize":
			send(map[string]any{})
		case "thread/read":
			if mode == "flood" {
				for i := 0; i < 100000; i++ {
					if out.Encode(map[string]any{"method": "notice", "params": map[string]any{"n": i}}) != nil {
						break
					}
				}
				continue
			}
			send(map[string]any{"thread": map[string]any{"id": "s"}})
		case "thread/items/list":
			if mode == "turns" || mode == "summary" {
				unsupported()
				continue
			}
			if mode == "malformed" {
				send(map[string]any{"unexpected": true})
				continue
			}
			if req.Params["sortDirection"] != "asc" || req.Params["threadId"] != "s" {
				unsupported()
				continue
			}
			if req.Params["cursor"] == nil {
				send(map[string]any{"data": []any{itemEntry("t1", appUserItem("u1", "first"))}, "nextCursor": "next"})
			} else {
				send(map[string]any{"data": []any{itemEntry("t1", appUserItem("u1", "first")), itemEntry("t2", map[string]any{"type": "agentMessage", "id": "a1", "text": "second"})}, "nextCursor": nil})
			}
		case "thread/turns/list":
			if req.Params["sortDirection"] != "asc" || req.Params["itemsView"] != "full" {
				unsupported()
				continue
			}
			if mode == "summary" {
				send(map[string]any{"data": []any{map[string]any{"id": "t1", "itemsView": "summary", "items": []any{}}}, "nextCursor": nil})
				continue
			}
			if req.Params["cursor"] == nil {
				send(map[string]any{"data": []any{map[string]any{"id": "t1", "itemsView": "full", "items": []any{appUserItem("u1", "first")}}}, "nextCursor": "next"})
			} else {
				send(map[string]any{"data": []any{map[string]any{"id": "t2", "itemsView": "full", "items": []any{map[string]any{"type": "agentMessage", "id": "a1", "text": "second"}}}}, "nextCursor": nil})
			}
		}
	}
	os.Exit(0)
}

func independentAdapter(t *testing.T, mode string) *codexAdapter {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestIndependentServer$\n", executable)), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("INDEPENDENT_SERVER", "1")
	t.Setenv("INDEPENDENT_MODE", mode)
	return &codexAdapter{bin: script, timeout: 2 * time.Second}
}

func TestIndependentPaginatedItemsAndTurnsIdentity(t *testing.T) {
	s := model.Session{ID: "s", Agent: model.Codex}
	a := independentAdapter(t, "items")
	items, err := a.Read(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("INDEPENDENT_MODE", "turns")
	turns, err := a.Read(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || len(turns) != 2 {
		t.Fatalf("expected two ordered unique events: items=%#v turns=%#v", items, turns)
	}
	for i, want := range []string{"first", "second"} {
		if items[i].Text != want || turns[i].Text != want {
			t.Fatalf("wrong order: items=%#v turns=%#v", items, turns)
		}
	}
	for i := range items {
		if items[i].ID != turns[i].ID {
			t.Errorf("event %d changed identity across pagination methods: %q / %q", i, items[i].ID, turns[i].ID)
		}
	}
}

func TestIndependentRejectsIncompleteAppServerHistory(t *testing.T) {
	for _, mode := range []string{"malformed", "summary"} {
		t.Run(mode, func(t *testing.T) {
			a := independentAdapter(t, mode)
			events, err := a.Read(context.Background(), model.Session{ID: "s", Agent: model.Codex})
			if err == nil {
				t.Fatalf("incomplete %s response silently accepted as history: %#v", mode, events)
			}
		})
	}
}

func TestIndependentRolloutIdentityAcrossBackends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := []string{`{"type":"session_meta","payload":{"id":"s","cwd":"/tmp"}}`, `{"type":"response_item","payload":{"id":"u1","type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}}`, `{"type":"response_item","payload":{"id":"a1","type":"message","role":"assistant","content":[{"type":"output_text","text":"second"}]}}`}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := model.Session{ID: "s", Agent: model.Codex, Path: path}
	a := independentAdapter(t, "items")
	app, err := a.Read(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	rollout, err := readCodexRollout(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(app) != 2 || len(rollout) != 2 {
		t.Fatalf("unexpected events: %#v %#v", app, rollout)
	}
	for i := range app {
		if app[i].ID != rollout[i].ID {
			t.Errorf("same native item %q changed ID: app-server=%q rollout=%q", app[i].Text, app[i].ID, rollout[i].ID)
		}
	}
}

func TestIndependentRolloutRepeatedMessageIsNotDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := []string{`{"type":"session_meta","payload":{"id":"s"}}`, `{"type":"response_item","payload":{"id":"u1","type":"message","role":"user","content":[{"type":"input_text","text":"yes"}]}}`, `{"type":"response_item","payload":{"id":"a1","type":"message","role":"assistant","content":[{"type":"output_text","text":"continue?"}]}}`, `{"type":"event_msg","payload":{"type":"user_message","message":"yes"}}`}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	events, err := readCodexRollout(model.Session{ID: "s", Path: path})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.Text)
	}
	if !reflect.DeepEqual(got, []string{"yes", "continue?", "yes"}) {
		t.Fatalf("distinct repeated user request lost: %q", got)
	}
}

func TestIndependentCompleteCorruptLastRecordIsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"session_meta\",\"payload\":{\"id\":\"s\"}}\n{broken}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCodexRollout(model.Session{ID: "s", Path: path}); err == nil {
		t.Fatal("newline-terminated corrupt final record silently ignored")
	}
}

func TestIndependentCloseDuringNotificationFlood(t *testing.T) {
	a := independentAdapter(t, "flood")
	client, err := startRPCClient(context.Background(), a.bin, a.timeout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.stdin.Write([]byte(`{"id":10,"method":"thread/read","params":{"threadId":"s"}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(client.messages) < cap(client.messages) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(client.messages) != cap(client.messages) {
		client.Close()
		t.Fatal("fake server did not fill notification queue")
	}
	client.Close()
	// A closed process must also release its stdout reader, even when nobody is
	// consuming notifications after cancellation.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-client.readerDone:
			return
		default:
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatal("stdout reader remained blocked on a full notification queue after close")
}

func TestIndependentClaudeDiscoverReportsCorruption(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "project")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	content := `{"type":"user","uuid":"u1","sessionId":"s","cwd":"/tmp/project","message":{"role":"user","content":"start"}}` + "\n{broken}\n" + `{"type":"assistant","uuid":"a1","parentUuid":"u1","sessionId":"s","cwd":"/tmp/project","message":{"role":"assistant","content":"answer"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	sessions, err := (&claudeAdapter{root: root}).Discover(context.Background(), "/tmp/project")
	if err == nil {
		t.Fatalf("corrupt session silently omitted: %#v", sessions)
	}
}
