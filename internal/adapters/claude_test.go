package adapters

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"terminus/internal/model"
)

func claudeFixtureAdapter(t *testing.T) *claudeAdapter {
	t.Helper()
	return &claudeAdapter{root: filepath.Join("testdata", "claude-home")}
}

func TestClaudeActiveBranchBlocksAndSummary(t *testing.T) {
	adapter := claudeFixtureAdapter(t)
	sessions, err := adapter.Discover(context.Background(), "/tmp/terminus-test/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("got %d sessions, want 1: %#v", len(sessions), sessions)
	}
	if sessions[0].ID != "claude-session-1" || sessions[0].Title != "Branch test" {
		t.Fatalf("unexpected session: %#v", sessions[0])
	}

	events, err := adapter.Read(context.Background(), sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		kind string
		role string
		text string
	}{
		{"summary", "assistant", "Compacted project summary"},
		{"message", "user", "hello claude"},
		{"message", "assistant", "part one"},
		{"tool", "tool", "Read"},
		{"message", "assistant", "part two"},
		{"tool", "tool", "package main"},
		{"message", "user", "after tool"},
		{"message", "assistant", "active answer"},
		{"handoff", "user", "[[TERMINUS_HANDOFF:packet-claude:BEGIN]]"},
		{"message", "user", "Continue in Claude."},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %#v", len(events), len(want), events)
	}
	for index, expectation := range want {
		event := events[index]
		if event.Kind != expectation.kind || event.Role != expectation.role || !strings.Contains(event.Text, expectation.text) {
			t.Fatalf("event %d = {%s %s %q}, want {%s %s containing %q}", index, event.Kind, event.Role, event.Text, expectation.kind, expectation.role, expectation.text)
		}
		if event.Source != model.Claude || event.SessionID != "claude-session-1" || event.ID == "" {
			t.Fatalf("event %d has invalid identity: %#v", index, event)
		}
	}
	for _, event := range events {
		if strings.Contains(event.Text, "dead branch") ||
			strings.Contains(event.Text, "private chain of thought") ||
			strings.Contains(event.Text, "hidden metadata") {
			t.Fatalf("excluded content leaked: %#v", event)
		}
	}
	again, err := adapter.Read(context.Background(), sessions[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(events, again) {
		t.Fatal("claude event IDs are not deterministic")
	}
}

func TestClaudeDiscoverUsesExactFullCWD(t *testing.T) {
	adapter := claudeFixtureAdapter(t)
	tests := []struct {
		cwd string
		id  string
	}{
		{"/tmp/a/project", "claude-a"},
		{"/tmp/b/project", "claude-b"},
	}
	for _, test := range tests {
		sessions, err := adapter.Discover(context.Background(), test.cwd)
		if err != nil {
			t.Fatal(err)
		}
		if len(sessions) != 1 || sessions[0].ID != test.id {
			t.Fatalf("Discover(%q) = %#v, want only %s", test.cwd, sessions, test.id)
		}
	}
	all, err := adapter.Discover(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("Discover all returned %d sessions, want 3", len(all))
	}
}

func TestClaudeCompactSummaryAndTruncatedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compact.jsonl")
	content := strings.Join([]string{
		`{"parentUuid":null,"isSidechain":false,"cwd":"/tmp/compact/project","sessionId":"compact-session","type":"user","message":{"role":"user","content":"start"},"uuid":"u1","timestamp":"2026-11-11T13:00:00Z"}`,
		`{"parentUuid":"u1","isSidechain":false,"isCompactSummary":true,"cwd":"/tmp/compact/project","sessionId":"compact-session","type":"user","message":{"role":"user","content":"compact summary text"},"uuid":"c1","timestamp":"2026-11-11T13:00:01Z"}`,
		`{"parentUuid":"c1","isSidechain":false,"cwd":"/tmp/compact/project","sessionId":"compact-session","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"after compact"}]},"uuid":"a1","timestamp":"2026-11-11T13:00:02Z"}`,
	}, "\n") + "\n"
	content += `{"parentUuid":"a1","type":"assistant","message":`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	transcript, err := parseClaudeTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != 3 {
		t.Fatalf("got %d events, want 3: %#v", len(transcript.Events), transcript.Events)
	}
	if transcript.Events[1].Kind != "summary" || transcript.Events[1].Text != "compact summary text" {
		t.Fatalf("compact summary not classified: %#v", transcript.Events[1])
	}
}

func TestClaudeReportsMiddleCorruptionAndUnknownDialogue(t *testing.T) {
	dir := t.TempDir()
	corrupt := filepath.Join(dir, "corrupt.jsonl")
	content := `{"parentUuid":null,"cwd":"/tmp/x","sessionId":"s","type":"user","message":{"role":"user","content":"ok"},"uuid":"u1","timestamp":"2026-11-11T13:00:00Z"}` + "\n" +
		"not json\n" +
		`{"parentUuid":"u1","cwd":"/tmp/x","sessionId":"s","type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"after"}]},"uuid":"a1","timestamp":"2026-11-11T13:00:01Z"}` + "\n"
	if err := os.WriteFile(corrupt, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseClaudeTranscript(corrupt); err == nil || !strings.Contains(err.Error(), "invalid JSON record") {
		t.Fatalf("corrupt error = %v", err)
	}

	unknown := filepath.Join(dir, "unknown.jsonl")
	line := `{"parentUuid":null,"cwd":"/tmp/x","sessionId":"s","type":"mystery","message":{"role":"user","content":"future dialogue"},"uuid":"m1","timestamp":"2026-11-11T13:00:00Z"}` + "\n"
	if err := os.WriteFile(unknown, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseClaudeTranscript(unknown); err == nil || !strings.Contains(err.Error(), "unsupported claude record type") {
		t.Fatalf("unknown error = %v", err)
	}
}
