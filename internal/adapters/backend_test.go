package adapters

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"terminus/internal/model"
)

func codexRolloutFixturePath() string {
	return filepath.Join("testdata", "codex-home", "sessions", "2026", "11", "11", "rollout-2026-11-11T10-00-00-codex-session-1.jsonl")
}

func TestCodexPinnedAppServerDoesNotFallback(t *testing.T) {
	adapter := newCodexAdapter()
	adapter.bin = filepath.Join(t.TempDir(), "missing-codex")
	adapter.home = filepath.Join("testdata", "codex-home")
	session := model.Session{
		ID:      "codex-session-1",
		Agent:   model.Codex,
		Path:    codexRolloutFixturePath(),
		Backend: BackendAppServer,
	}
	events, err := adapter.Read(context.Background(), session)
	if err == nil {
		t.Fatalf("pinned app-server session silently fell back to rollout: %#v", events)
	}
}

func TestCodexReadResolvedPinsRollout(t *testing.T) {
	adapter := newCodexAdapter()
	adapter.bin = filepath.Join(t.TempDir(), "missing-codex")
	adapter.home = filepath.Join("testdata", "codex-home")
	session := model.Session{ID: "codex-session-1", Agent: model.Codex, Path: codexRolloutFixturePath()}
	resolved, events, err := adapter.ReadResolved(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Backend != BackendRollout {
		t.Fatalf("resolved backend = %q, want %q", resolved.Backend, BackendRollout)
	}
	if len(events) != 7 {
		t.Fatalf("events = %#v", events)
	}
}

func TestCodexReadResolvedPinsAppServer(t *testing.T) {
	adapter, _ := fakeCodexAdapter(t, "items")
	session := model.Session{ID: "fake-thread-1", Agent: model.Codex, CWD: "/tmp/terminus-test/project"}
	resolved, events, err := adapter.ReadResolved(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Backend != BackendAppServer {
		t.Fatalf("resolved backend = %q, want %q", resolved.Backend, BackendAppServer)
	}
	if len(events) != 6 {
		t.Fatalf("events = %#v", events)
	}
}

func TestCodexLegacyEmptyBackendStillFallsBack(t *testing.T) {
	adapter := newCodexAdapter()
	adapter.bin = filepath.Join(t.TempDir(), "missing-codex")
	adapter.home = filepath.Join("testdata", "codex-home")
	session := model.Session{ID: "codex-session-1", Agent: model.Codex, Path: codexRolloutFixturePath()}
	events, err := adapter.Read(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 7 {
		t.Fatalf("legacy empty backend events = %#v", events)
	}
}

func TestCodexReadResolvedFollowsNewRolloutPath(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "11", "11")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(dir, "rollout-old.jsonl")
	newPath := filepath.Join(dir, "rollout-new.jsonl")
	oldContent := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"s","cwd":"/tmp/project"}}`,
		`{"type":"response_item","payload":{"id":"u1","type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}}`,
		`{"type":"response_item","payload":{"id":"a1","type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}}`,
	}, "\n") + "\n"
	newContent := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"s","cwd":"/tmp/project"}}`,
		`{"type":"response_item","payload":{"id":"u1","type":"message","role":"user","content":[{"type":"input_text","text":"first"}]}}`,
		`{"type":"response_item","payload":{"id":"a1","type":"message","role":"assistant","content":[{"type":"output_text","text":"old answer"}]}}`,
		`{"type":"response_item","payload":{"id":"u2","type":"message","role":"user","content":[{"type":"input_text","text":"second"}]}}`,
		`{"type":"response_item","payload":{"id":"a2","type":"message","role":"assistant","content":[{"type":"output_text","text":"newer answer"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(oldPath, []byte(oldContent), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte(newContent), 0600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, past, past); err != nil {
		t.Fatal(err)
	}

	adapter := &codexAdapter{bin: filepath.Join(t.TempDir(), "missing"), home: home}
	session := model.Session{ID: "s", Agent: model.Codex, CWD: "/tmp/project", Path: oldPath, Backend: BackendRollout}
	resolved, events, err := adapter.ReadResolved(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Path != newPath {
		t.Fatalf("resolved path = %s, want %s", resolved.Path, newPath)
	}
	found := false
	for _, event := range events {
		if event.Text == "newer answer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events = %#v", events)
	}
}

func TestClaudeDiscoverOnlyParsesTargetCWD(t *testing.T) {
	root := t.TempDir()
	otherDir := filepath.Join(root, "projects", "unrelated")
	targetDir := filepath.Join(root, "projects", "target")
	if err := os.MkdirAll(otherDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		t.Fatal(err)
	}
	unrelated := strings.Join([]string{
		`{"type":"user","uuid":"other-u1","sessionId":"other-session","cwd":"/tmp/unrelated/cwd","message":{"role":"user","content":"other"}}`,
		`{"type":"api-request-blob","uuid":"blob-1","sessionId":"other-session","cwd":"/tmp/unrelated/cwd","message":{"content":"opaque metadata"}}`,
		`{broken}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(otherDir, "other.jsonl"), []byte(unrelated), 0600); err != nil {
		t.Fatal(err)
	}
	target := strings.Join([]string{
		`{"type":"user","uuid":"target-u1","sessionId":"target-session","cwd":"/tmp/target/cwd","message":{"role":"user","content":"hello"}}`,
		`{"type":"assistant","uuid":"target-a1","parentUuid":"target-u1","sessionId":"target-session","cwd":"/tmp/target/cwd","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(targetDir, "target.jsonl"), []byte(target), 0600); err != nil {
		t.Fatal(err)
	}

	adapter := &claudeAdapter{root: root}
	sessions, err := adapter.Discover(context.Background(), "/tmp/target/cwd")
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != "target-session" || sessions[0].Backend != BackendJSONL {
		t.Fatalf("sessions = %#v", sessions)
	}
	if _, err := adapter.Discover(context.Background(), ""); err == nil {
		t.Fatal("unfiltered discovery silently ignored unrelated corruption")
	}
}

func TestClaudeCorruptTargetWithoutHeaderStillErrors(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "project")
	dir := filepath.Join(root, "projects", claudeProjectHint(cwd))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.jsonl"), []byte("{broken}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	adapter := &claudeAdapter{root: root}
	if _, err := adapter.Discover(context.Background(), cwd); err == nil {
		t.Fatal("corrupt target transcript without readable header was silently skipped")
	}
}

func TestClaudeTargetMetadataIsClassified(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "target")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		`{"type":"user","uuid":"u1","sessionId":"s","cwd":"/tmp/target/cwd","message":{"role":"user","content":"hello"}}`,
		`{"type":"api-request-blob","uuid":"blob-1","parentUuid":"u1","sessionId":"s","cwd":"/tmp/target/cwd","message":{"content":"opaque metadata"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"blob-1","sessionId":"s","cwd":"/tmp/target/cwd","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	transcript, err := parseClaudeTranscript(filepath.Join(dir, "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript.Events) != 2 || transcript.Events[0].Text != "hello" || transcript.Events[1].Text != "answer" {
		t.Fatalf("events = %#v", transcript.Events)
	}
}
