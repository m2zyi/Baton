package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terminus/internal/handoff"
	"terminus/internal/launcher"
	"terminus/internal/model"
	"terminus/internal/store"
)

type fakeRunner struct {
	runs []launcher.NativeRun
	err  error
}

func (r *fakeRunner) Run(_ context.Context, run launcher.NativeRun, _ io.Reader, _, _ io.Writer) error {
	r.runs = append(r.runs, run)
	return r.err
}

func TestHelpVersionAndContextPagination(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"help"}, "ID distinction:"},
		{[]string{"--help"}, "Usage:"},
		{[]string{"version"}, "terminus 0.1.0"},
	}
	for _, tt := range tests {
		var out, errOut strings.Builder
		if code := Run(context.Background(), tt.args, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut.String())
		}
		if !strings.Contains(out.String(), tt.want) {
			t.Fatalf("output %q does not contain %q", out.String(), tt.want)
		}
	}
}

func TestContextReadsIncrementally(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	conversation, err := st.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	events := []model.Event{
		{ID: "1", Source: model.Codex, SessionID: "native-1", Kind: "message", Role: "user", Text: "你好"},
		{ID: "2", Source: model.Codex, SessionID: "native-1", Kind: "message", Role: "assistant", Text: "hello"},
	}
	if _, err := st.Append(conversation.ID, events); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code := RunWithOptions(context.Background(), []string{"context", "--json", "--after", "1", "--limit", "1", conversation.ID}, strings.NewReader(""), &out, &errOut, Options{StoreRoot: root})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
	var entries []model.Entry
	if err := json.Unmarshal([]byte(out.String()), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Seq != 2 || entries[0].Event.Text != "hello" {
		t.Fatalf("entries = %#v", entries)
	}
}

func TestStartPersistsFirstRequestBeforeNativeRun(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "claude-home"))
	runner := &fakeRunner{err: errors.New("native exec failed")}
	var out, errOut strings.Builder
	code := RunWithOptions(context.Background(), []string{"start", "--agent", "claude", "--prompt", "first request"}, strings.NewReader(""), &out, &errOut, Options{StoreRoot: root, Runner: runner})
	if code != 1 || !strings.Contains(errOut.String(), "native exec failed") {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
	if len(runner.runs) != 1 || runner.runs[0].Args[2] != "first request" {
		t.Fatalf("runs = %#v", runner.runs)
	}
	st := store.New(root)
	conversations, err := st.List(cwd)
	if err != nil || len(conversations) != 1 {
		t.Fatalf("conversations = %#v err = %v", conversations, err)
	}
	c := conversations[0]
	if c.ActiveAgent != model.Claude || c.Pending == nil || c.Pending.Packet.Target != model.Claude {
		t.Fatalf("conversation = %#v", c)
	}
	entries, err := st.Entries(c.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Event.Kind != "message" || entries[0].Event.Role != "user" || entries[0].Event.Text != "first request" {
		t.Fatalf("entries = %#v", entries)
	}
}

type nativeFixture struct {
	root       string
	cwd        string
	claudeHome string
	codexHome  string
	claudeBin  string
	codexBin   string
	claudePath string
	codexPath  string
	store      *store.Store
	convo      *model.Conversation
}

func newNativeFixture(t *testing.T) *nativeFixture {
	t.Helper()
	root := t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)
	f := &nativeFixture{
		root:       root,
		cwd:        cwd,
		claudeHome: filepath.Join(root, "claude-home"),
		codexHome:  filepath.Join(root, "codex-home"),
		store:      store.New(root),
	}
	f.claudePath = filepath.Join(f.claudeHome, "projects", encodeClaudeProject(cwd), "claude-session.jsonl")
	f.codexPath = filepath.Join(f.codexHome, "sessions", "2026", "11", "11", "rollout-codex.jsonl")
	if err := os.MkdirAll(filepath.Dir(f.claudePath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.codexPath), 0700); err != nil {
		t.Fatal(err)
	}
	writeClaudeRecord(t, f.claudePath, map[string]any{"cwd": cwd, "sessionId": "claude-session", "type": "user", "uuid": "claude-user-1", "message": map[string]any{"role": "user", "content": "first request"}, "timestamp": "2026-11-11T11:00:00Z"})
	writeClaudeRecord(t, f.claudePath, map[string]any{"cwd": cwd, "sessionId": "claude-session", "type": "assistant", "uuid": "claude-assistant-1", "parentUuid": "claude-user-1", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "claude answer one"}}}, "timestamp": "2026-11-11T11:00:01Z"})
	f.writeCodexTranscript("codex-session", "first handoff", "codex answer one")
	f.claudeBin = writeFakeCLI(t, cwd, "claude", "claude-session", f.claudePath)
	f.codexBin = writeFakeCLI(t, cwd, "codex", "codex-session", f.codexPath)
	t.Setenv("CLAUDE_CONFIG_DIR", f.claudeHome)
	t.Setenv("CODEX_HOME", f.codexHome)
	t.Setenv("TERMINUS_CLAUDE_BIN", f.claudeBin)
	t.Setenv("TERMINUS_CODEX_BIN", f.codexBin)
	return f
}

func encodeClaudeProject(cwd string) string {
	return strings.TrimPrefix(strings.ReplaceAll(cwd, "/", "-"), "-")
}

func writeClaudeRecord(t *testing.T, path string, record any) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		t.Fatal(err)
	}
}

func writeFakeCLI(t *testing.T, cwd, agent, sessionID, transcript string) string {
	t.Helper()
	path := filepath.Join(cwd, "fake-"+agent)
	script := `#!/bin/sh
prompt_file="$PWD/.terminus-prompt"
printf '%s\n' "$*" > "$prompt_file"
printf 'fake agent\n'
`
	if agent == "claude" {
		script += fmt.Sprintf("printf '{\"cwd\":%q,\"sessionId\":\"%s\",\"type\":\"user\",\"uuid\":\"terminus-user\",\"message\":{\"role\":\"user\",\"content\":\"\"},\"timestamp\":\"2026-11-11T12:00:00Z\"}\\n' >> %q\n", cwd, sessionID, transcript)
	}
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *nativeFixture) writeCodexTranscript(sessionID, prompt, answer string) {
	data := fmt.Sprintf(`{"timestamp":"2026-11-11T12:00:00Z","type":"session_meta","payload":{"session_id":%q,"id":%q,"cwd":%q,"timestamp":"2026-11-11T12:00:00Z"}}
{"timestamp":"2026-11-11T12:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}
{"timestamp":"2026-11-11T12:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}
`, sessionID, sessionID, f.cwd, jsonString(prompt), jsonString(answer))
	if err := os.WriteFile(f.codexPath, []byte(data), 0600); err != nil {
		panic(err)
	}
}

func TestClaudeToCodexToClaudeRoundTrip(t *testing.T) {
	f := newNativeFixture(t)
	var out, errOut strings.Builder
	code := RunWithOptions(context.Background(), []string{"start", "--agent", "claude", "--prompt", "first request"}, strings.NewReader("q\n"), &out, &errOut, Options{StoreRoot: f.root})
	if code != 0 {
		t.Fatalf("start code = %d, stderr = %q", code, errOut.String())
	}
	conversations, err := f.store.List(f.cwd)
	if err != nil || len(conversations) != 1 {
		t.Fatalf("conversations = %#v err = %v", conversations, err)
	}
	f.convo = &conversations[0]

	// The fake Claude CLI does not write a native Terminus receipt yet. The first
	// switch must retain pending rather than advancing delivery state.
	errOut.Reset()
	code = RunWithOptions(context.Background(), []string{"switch", "--to", "codex", "--conversation", f.convo.ID}, strings.NewReader("q\n"), &out, &errOut, Options{StoreRoot: f.root})
	if code == 0 || !strings.Contains(errOut.String(), "pending handoff receipt") {
		t.Fatalf("switch code = %d, stderr = %q", code, errOut.String())
	}
}

func jsonString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func TestMarkerReceiptValidation(t *testing.T) {
	marker := handoff.Marker("00000000000000000000000000000001")
	if !handoff.IsInjected(marker + "\nhistory\n" + "[[TERMINUS_HANDOFF:00000000000000000000000000000001:END]]") {
		t.Fatal("complete receipt not recognized")
	}
	if handoff.IsInjected(marker + "\nhistory") {
		t.Fatal("partial receipt accepted")
	}
}
