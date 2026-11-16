package adapters

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"baton/internal/model"
)

func fallbackCodexAdapter(t *testing.T) *codexAdapter {
	t.Helper()
	adapter := newCodexAdapter()
	adapter.bin = filepath.Join(t.TempDir(), "missing-codex")
	adapter.home = filepath.Join("testdata", "codex-home")
	return adapter
}

func TestCodexRolloutFallbackDiscovery(t *testing.T) {
	adapter := fallbackCodexAdapter(t)
	sessions, err := adapter.Discover(context.Background(), "/tmp/baton-test/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1: %#v", len(sessions), sessions)
	}
	if sessions[0].ID != "codex-session-1" || sessions[0].Agent != model.Codex || sessions[0].Title != "hello codex" {
		t.Fatalf("unexpected session: %#v", sessions[0])
	}

	other, err := adapter.Discover(context.Background(), "/tmp/baton-other/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("cwd mismatch returned %d sessions", len(other))
	}
}

func TestCodexRolloutReadDeduplicatesAndFilters(t *testing.T) {
	adapter := fallbackCodexAdapter(t)
	sessions, err := adapter.Discover(context.Background(), "/tmp/baton-test/project")
	if err != nil || len(sessions) != 1 {
		t.Fatalf("discover: %v, %#v", err, sessions)
	}
	events, err := adapter.Read(context.Background(), sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("got %d events, want 7: %#v", len(events), events)
	}
	want := []struct {
		kind string
		role string
		text string
	}{
		{"message", "user", "hello codex"},
		{"message", "assistant", "assistant reply"},
		{"tool", "tool", "shell"},
		{"tool", "tool", "hi"},
		{"summary", "assistant", "Earlier summary"},
		{"handoff", "user", "[[BATON_HANDOFF:packet-123:BEGIN]]"},
		{"message", "user", "Please continue with the next task."},
	}
	for index, expectation := range want {
		event := events[index]
		if event.Kind != expectation.kind || event.Role != expectation.role || !strings.Contains(event.Text, expectation.text) {
			t.Fatalf("event %d = {%s %s %q}, want {%s %s containing %q}", index, event.Kind, event.Role, event.Text, expectation.kind, expectation.role, expectation.text)
		}
		if event.SessionID != "codex-session-1" || event.ID == "" {
			t.Fatalf("event %d has invalid identity: %#v", index, event)
		}
	}
	for _, event := range events {
		if strings.Contains(event.Text, "developer instructions") ||
			strings.Contains(event.Text, "private reasoning") ||
			strings.Contains(event.Text, "environment_context") {
			t.Fatalf("synthetic or reasoning content leaked: %#v", event)
		}
	}
	again, err := adapter.Read(context.Background(), sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, again) {
		t.Fatal("rollout event IDs are not deterministic")
	}
}

func TestCodexRolloutIgnoresOnlyUnterminatedTruncatedTail(t *testing.T) {
	source := filepath.Join("testdata", "codex-home", "sessions", "2026", "11", "11", "rollout-2026-11-11T10-00-00-codex-session-1.jsonl")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		suffix  string
		wantErr bool
	}{
		{name: "unterminated tail"},
		{name: "newline-terminated corrupt record", suffix: "\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "truncated.jsonl")
			truncated := append([]byte(nil), data...)
			truncated = append(truncated, []byte(`{"timestamp":"2026-11-11T10:00:11Z","type":"response_item","payload":`+test.suffix)...)
			if err := os.WriteFile(path, truncated, 0o600); err != nil {
				t.Fatal(err)
			}
			events, err := readCodexRollout(model.Session{ID: "codex-session-1", Agent: model.Codex, Path: path})
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "invalid JSON record") {
					t.Fatalf("error = %v, want invalid JSON record", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 7 {
				t.Fatalf("got %d events, want 7", len(events))
			}
		})
	}
}

func TestCodexRolloutReportsMiddleCorruption(t *testing.T) {
	source := filepath.Join("testdata", "codex-home", "sessions", "2026", "11", "11", "rollout-2026-11-11T10-00-00-codex-session-1.jsonl")
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	newline := strings.IndexByte(string(data), '\n')
	if newline < 0 {
		t.Fatal("fixture has no records")
	}
	corrupt := append([]byte(nil), data[:newline+1]...)
	corrupt = append(corrupt, []byte("this is not json\n")...)
	corrupt = append(corrupt, data[newline+1:]...)
	path := filepath.Join(t.TempDir(), "corrupt.jsonl")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = readCodexRollout(model.Session{ID: "codex-session-1", Agent: model.Codex, Path: path})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON record") {
		t.Fatalf("error = %v, want invalid JSON record", err)
	}
}

func TestCodexRolloutCanonicalBlockIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	lines := []string{
		`{"type":"session_meta","payload":{"id":"s"}}`,
		`{"type":"response_item","payload":{"id":"u1","type":"message","role":"user","content":[{"type":"input_text","text":"first"},{"type":"input_text","text":"second"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"first\nsecond"}}`,
		`{"type":"response_item","payload":{"id":"a1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}`,
		`{"type":"event_msg","payload":{"type":"agent_message","message":"answer"}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := readCodexRollout(model.Session{ID: "s", Agent: model.Codex, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id   string
		text string
	}{
		{"u1:content:0", "first"},
		{"u1:content:1", "second"},
		{"a1:text", "answer"},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %#v", len(events), len(want), events)
	}
	for index, expectation := range want {
		if events[index].ID != expectation.id || events[index].Text != expectation.text {
			t.Fatalf("event %d = (%q, %q), want (%q, %q)", index, events[index].ID, events[index].Text, expectation.id, expectation.text)
		}
	}
}
