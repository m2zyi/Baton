package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"terminus/internal/adapters"
	"terminus/internal/handoff"
	"terminus/internal/launcher"
	"terminus/internal/model"
	"terminus/internal/store"
)

// TestFakeNativeCLI is a helper process that behaves like a tiny native agent.
// It writes only synthetic transcripts under the test-provided homes.
func TestFakeNativeCLI(t *testing.T) {
	agent := os.Getenv("TERMINUS_FAKE_AGENT")
	if agent == "" {
		return
	}
	code := runFakeNative(agent, fakeCLIArgs())
	if raw := os.Getenv("TERMINUS_FAKE_EXIT"); raw != "" {
		if exit, err := strconv.Atoi(raw); err == nil {
			code = exit
		}
	}
	os.Exit(code)
}

type fakeInvocation struct {
	Agent string   `json:"agent"`
	Args  []string `json:"args"`
	CWD   string   `json:"cwd"`
}

func fakeCLIArgs() []string {
	for i, arg := range os.Args {
		if arg == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

func runFakeNative(agent string, args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake %s: cwd: %v\n", agent, err)
		return 2
	}
	appendFakeLog(fakeInvocation{Agent: agent, Args: args, CWD: cwd})
	switch agent {
	case "codex":
		return runFakeCodex(args, cwd)
	case "claude":
		return runFakeClaude(args, cwd)
	default:
		fmt.Fprintf(os.Stderr, "fake native: unknown agent %q\n", agent)
		return 2
	}
}

func appendFakeLog(invocation fakeInvocation) {
	path := os.Getenv("TERMINUS_FAKE_LOG")
	if path == "" {
		return
	}
	data, err := json.Marshal(invocation)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	defer file.Close()
	file.Write(append(data, '\n'))
}

func runFakeCodex(args []string, cwd string) int {
	if len(args) > 0 && args[0] == "app-server" {
		fmt.Fprintln(os.Stderr, "fake codex: app-server is not implemented")
		return 1
	}
	sessionID := ""
	prompt := ""
	if len(args) > 0 && args[0] == "resume" {
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "fake codex: resume requires a session ID")
			return 2
		}
		sessionID = args[1]
		if len(args) > 2 {
			prompt = args[2]
		}
	} else {
		var err error
		sessionID, err = newFakeSessionID("codex")
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake codex: session ID: %v\n", err)
			return 2
		}
		if len(args) > 0 {
			prompt = args[0]
		}
	}
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		fmt.Fprintln(os.Stderr, "fake codex: CODEX_HOME is required")
		return 2
	}
	path := filepath.Join(home, "sessions", "2026", "11", "11", "rollout-"+sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "fake codex: mkdir: %v\n", err)
		return 2
	}
	turn := 1
	if data, err := os.ReadFile(path); err == nil {
		turn += bytes.Count(data, []byte(`"role":"user"`))
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := appendJSONLine(path, map[string]any{
			"timestamp": fakeTimestamp(),
			"type":      "session_meta",
			"payload": map[string]any{
				"id":         sessionID,
				"session_id": sessionID,
				"cwd":        cwd,
				"timestamp":  fakeTimestamp(),
			},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "fake codex: session meta: %v\n", err)
			return 2
		}
	}
	if prompt != "" && os.Getenv("TERMINUS_FAKE_NO_RECEIPT") != "1" {
		if err := appendJSONLine(path, map[string]any{
			"timestamp": fakeTimestamp(),
			"type":      "response_item",
			"payload": map[string]any{
				"id":      fmt.Sprintf("%s-u-%d", sessionID, turn),
				"type":    "message",
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": prompt}},
			},
		}); err != nil {
			fmt.Fprintf(os.Stderr, "fake codex: user: %v\n", err)
			return 2
		}
	}
	if err := appendJSONLine(path, map[string]any{
		"timestamp": fakeTimestamp(),
		"type":      "response_item",
		"payload": map[string]any{
			"id":      fmt.Sprintf("%s-a-%d", sessionID, turn),
			"type":    "message",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": fmt.Sprintf("codex answer %d", turn)}},
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fake codex: assistant: %v\n", err)
		return 2
	}
	if err := appendJSONLine(path, map[string]any{
		"timestamp": fakeTimestamp(),
		"type":      "response_item",
		"payload": map[string]any{
			"id":      fmt.Sprintf("%s-t-%d", sessionID, turn),
			"type":    "function_call_output",
			"name":    "shell",
			"call_id": fmt.Sprintf("call-%d", turn),
			"output":  fmt.Sprintf("codex tool %d", turn),
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fake codex: tool: %v\n", err)
		return 2
	}
	return 0
}

func runFakeClaude(args []string, cwd string) int {
	sessionID := ""
	prompt := ""
	switch {
	case len(args) >= 2 && args[0] == "--session-id":
		sessionID = args[1]
		if len(args) > 2 {
			prompt = args[2]
		}
	case len(args) >= 2 && args[0] == "--resume":
		sessionID = args[1]
		if len(args) > 2 {
			prompt = args[2]
		}
	default:
		fmt.Fprintf(os.Stderr, "fake claude: unsupported args %q\n", args)
		return 2
	}
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	if home == "" {
		fmt.Fprintln(os.Stderr, "fake claude: CLAUDE_CONFIG_DIR is required")
		return 2
	}
	path := filepath.Join(home, "projects", encodeClaudeProject(cwd), sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "fake claude: mkdir: %v\n", err)
		return 2
	}
	turn := 1
	lastUUID := ""
	if data, err := os.ReadFile(path); err == nil {
		turn += bytes.Count(data, []byte(`"type":"assistant"`))
		for _, line := range strings.Split(string(data), "\n") {
			var record struct {
				UUID string `json:"uuid"`
			}
			if json.Unmarshal([]byte(line), &record) == nil && record.UUID != "" {
				lastUUID = record.UUID
			}
		}
	}
	userUUID := ""
	if prompt != "" && os.Getenv("TERMINUS_FAKE_NO_RECEIPT") != "1" {
		userUUID = fmt.Sprintf("%s-u-%d", sessionID, turn)
		var parent any
		if lastUUID != "" {
			parent = lastUUID
		}
		if err := appendJSONLine(path, map[string]any{
			"parentUuid":  parent,
			"isSidechain": false,
			"cwd":         cwd,
			"sessionId":   sessionID,
			"type":        "user",
			"message":     map[string]any{"role": "user", "content": prompt},
			"uuid":        userUUID,
			"timestamp":   fakeTimestamp(),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "fake claude: user: %v\n", err)
			return 2
		}
		lastUUID = userUUID
	}
	assistantUUID := fmt.Sprintf("%s-a-%d", sessionID, turn)
	if err := appendJSONLine(path, map[string]any{
		"parentUuid":  nullableString(lastUUID),
		"isSidechain": false,
		"cwd":         cwd,
		"sessionId":   sessionID,
		"type":        "assistant",
		"message": map[string]any{
			"role": "assistant",
			"content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("claude answer %d", turn)},
				map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", turn), "name": "Read", "input": map[string]any{"file_path": "fixture.txt"}},
			},
		},
		"uuid":      assistantUUID,
		"timestamp": fakeTimestamp(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fake claude: assistant: %v\n", err)
		return 2
	}
	if err := appendJSONLine(path, map[string]any{
		"parentUuid":  assistantUUID,
		"isSidechain": false,
		"cwd":         cwd,
		"sessionId":   sessionID,
		"type":        "user",
		"message": map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("toolu_%d", turn), "content": fmt.Sprintf("claude tool %d", turn), "is_error": false}},
		},
		"uuid":      fmt.Sprintf("%s-t-%d", sessionID, turn),
		"timestamp": fakeTimestamp(),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "fake claude: tool: %v\n", err)
		return 2
	}
	return 0
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func newFakeSessionID(prefix string) (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(value[:]), nil
}

func fakeTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func appendJSONLine(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

type nativeFixture struct {
	root       string
	cwd        string
	codexHome  string
	claudeHome string
	codexBin   string
	claudeBin  string
	logPath    string
	store      *store.Store
}

func newNativeFixture(t *testing.T) *nativeFixture {
	t.Helper()
	root := t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)
	f := &nativeFixture{
		root:       root,
		cwd:        cwd,
		codexHome:  filepath.Join(root, "codex-home"),
		claudeHome: filepath.Join(root, "claude-home"),
		logPath:    filepath.Join(root, "fake-cli.log"),
		store:      store.New(root),
	}
	binDir := t.TempDir()
	f.codexBin = writeFakeCLI(t, binDir, "codex")
	f.claudeBin = writeFakeCLI(t, binDir, "claude")
	t.Setenv("TERMINUS_HOME", root)
	t.Setenv("CODEX_HOME", f.codexHome)
	t.Setenv("CLAUDE_CONFIG_DIR", f.claudeHome)
	t.Setenv("TERMINUS_CODEX_BIN", f.codexBin)
	t.Setenv("TERMINUS_CLAUDE_BIN", f.claudeBin)
	t.Setenv("TERMINUS_FAKE_LOG", f.logPath)
	t.Setenv("TERMINUS_FAKE_EXIT", "")
	t.Setenv("TERMINUS_FAKE_NO_RECEIPT", "")
	return f
}

func writeFakeCLI(t *testing.T, dir, agent string) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fake-"+agent)
	script := fmt.Sprintf("#!/bin/sh\nTERMINUS_FAKE_AGENT=%s\nexport TERMINUS_FAKE_AGENT\nexec %q -test.run=^TestFakeNativeCLI$ -- \"$@\"\n", agent, executable)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func encodeClaudeProject(cwd string) string {
	return strings.TrimPrefix(strings.ReplaceAll(cwd, "/", "-"), "-")
}

func (f *nativeFixture) run(t *testing.T, input string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := RunWithOptions(context.Background(), args, strings.NewReader(input), &out, &errOut, Options{StoreRoot: f.root})
	return code, out.String(), errOut.String()
}

func (f *nativeFixture) onlyConversation(t *testing.T) model.Conversation {
	t.Helper()
	conversations, err := f.store.List(f.cwd)
	if err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 1 {
		t.Fatalf("conversations = %#v", conversations)
	}
	return conversations[0]
}

func (f *nativeFixture) entries(t *testing.T, id string) []model.Entry {
	t.Helper()
	entries, err := f.store.Entries(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func (f *nativeFixture) invocations(t *testing.T) []fakeInvocation {
	t.Helper()
	data, err := os.ReadFile(f.logPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	out := []fakeInvocation{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var invocation fakeInvocation
		if err := json.Unmarshal([]byte(line), &invocation); err != nil {
			t.Fatal(err)
		}
		out = append(out, invocation)
	}
	return out
}

func lastNativeInvocation(t *testing.T, invocations []fakeInvocation) fakeInvocation {
	t.Helper()
	for i := len(invocations) - 1; i >= 0; i-- {
		if len(invocations[i].Args) > 0 && invocations[i].Args[0] == "app-server" {
			continue
		}
		return invocations[i]
	}
	t.Fatal("no native invocation recorded")
	return fakeInvocation{}
}

func countText(entries []model.Entry, text string) int {
	count := 0
	for _, entry := range entries {
		if entry.Event.Text == text {
			count++
		}
	}
	return count
}

func containsText(entries []model.Entry, text string) bool {
	for _, entry := range entries {
		if strings.Contains(entry.Event.Text, text) {
			return true
		}
	}
	return false
}

func assertUnique(t *testing.T, entries []model.Entry, text string) {
	t.Helper()
	if got := countText(entries, text); got != 1 {
		t.Fatalf("text %q occurs %d times, want exactly once: %#v", text, got, entries)
	}
}

func assertContainsOnce(t *testing.T, entries []model.Entry, text string) {
	t.Helper()
	count := 0
	for _, entry := range entries {
		if strings.Contains(entry.Event.Text, text) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("text %q occurs %d times, want exactly once: %#v", text, count, entries)
	}
}

func assertJournalClean(t *testing.T, entries []model.Entry) {
	t.Helper()
	for _, entry := range entries {
		if entry.Event.Kind == "handoff" || strings.Contains(entry.Event.Text, "TERMINUS_HANDOFF:") {
			t.Fatalf("handoff/injection leaked into journal: %#v", entry)
		}
	}
}

func TestIntegrationAlternatingRoundTrips(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "user request 0")
	if code != 0 {
		t.Fatalf("start code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	if c.Pending != nil {
		t.Fatalf("start left pending: %#v", c.Pending)
	}
	codexBinding, ok := c.Bindings[model.Codex]
	if !ok || codexBinding.Session.ID == "" || codexBinding.Session.Path == "" {
		t.Fatalf("codex binding = %#v", codexBinding)
	}
	if codexBinding.Session.Backend != adapters.BackendRollout {
		t.Fatalf("codex backend = %q, want %q", codexBinding.Session.Backend, adapters.BackendRollout)
	}
	codexID := codexBinding.Session.ID
	codexPath := codexBinding.Session.Path
	entries := f.entries(t, c.ID)
	assertUnique(t, entries, "user request 0")
	assertUnique(t, entries, "codex answer 1")
	if !containsText(entries, "codex tool 1") {
		t.Fatalf("codex tool output missing: %#v", entries)
	}
	assertJournalClean(t, entries)
	if inv := lastNativeInvocation(t, f.invocations(t)); inv.Agent != "codex" || len(inv.Args) != 1 || inv.CWD != f.cwd {
		t.Fatalf("start invocation = %#v", inv)
	}

	codexBefore, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut = f.run(t, "q\n", "switch", "--to", "claude", "--conversation", c.ID)
	if code != 0 {
		t.Fatalf("switch claude code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c = f.onlyConversation(t)
	claudeBinding, ok := c.Bindings[model.Claude]
	if !ok || claudeBinding.Session.ID == "" || claudeBinding.Session.Backend != adapters.BackendJSONL {
		t.Fatalf("claude binding = %#v", claudeBinding)
	}
	claudeID := claudeBinding.Session.ID
	inv := lastNativeInvocation(t, f.invocations(t))
	if inv.Agent != "claude" || len(inv.Args) < 3 || inv.Args[0] != "--session-id" || inv.Args[1] != claudeID || inv.CWD != f.cwd {
		t.Fatalf("claude first launch = %#v", inv)
	}
	if prompt := inv.Args[2]; !strings.Contains(prompt, "codex answer 1") || !strings.Contains(prompt, "user request 0") || strings.Contains(prompt, "claude answer 1") {
		t.Fatalf("claude handoff prompt = %q", prompt)
	}
	codexAfter, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(codexBefore, codexAfter) {
		t.Fatal("source codex transcript changed during claude handoff")
	}
	entries = f.entries(t, c.ID)
	assertUnique(t, entries, "user request 0")
	assertUnique(t, entries, "codex answer 1")
	assertUnique(t, entries, "claude answer 1")
	assertUnique(t, entries, "claude tool 1")
	assertJournalClean(t, entries)

	code, out, errOut = f.run(t, "q\n", "switch", "--to", "codex", "--conversation", c.ID)
	if code != 0 {
		t.Fatalf("switch codex code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	inv = lastNativeInvocation(t, f.invocations(t))
	if inv.Agent != "codex" || len(inv.Args) < 3 || inv.Args[0] != "resume" || inv.Args[1] != codexID || inv.CWD != f.cwd {
		t.Fatalf("codex resume = %#v", inv)
	}
	if prompt := inv.Args[2]; !strings.Contains(prompt, "claude answer 1") || !strings.Contains(prompt, "claude tool 1") || strings.Contains(prompt, "codex answer 1") {
		t.Fatalf("codex handoff prompt = %q", prompt)
	}
	entries = f.entries(t, c.ID)
	assertUnique(t, entries, "codex answer 2")
	assertUnique(t, entries, "claude answer 1")
	assertJournalClean(t, entries)

	code, out, errOut = f.run(t, "q\n", "switch", "--to", "claude", "--conversation", c.ID)
	if code != 0 {
		t.Fatalf("switch claude again code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	inv = lastNativeInvocation(t, f.invocations(t))
	if inv.Agent != "claude" || len(inv.Args) < 3 || inv.Args[0] != "--resume" || inv.Args[1] != claudeID || inv.CWD != f.cwd {
		t.Fatalf("claude resume = %#v", inv)
	}
	if prompt := inv.Args[2]; !strings.Contains(prompt, "codex answer 2") || strings.Contains(prompt, "claude answer 1") {
		t.Fatalf("claude incremental handoff prompt = %q", prompt)
	}
	entries = f.entries(t, c.ID)
	assertUnique(t, entries, "claude answer 2")
	assertJournalClean(t, entries)

	code, out, errOut = f.run(t, "q\n", "switch", "--to", "codex", "--conversation", c.ID)
	if code != 0 {
		t.Fatalf("final switch codex code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	inv = lastNativeInvocation(t, f.invocations(t))
	if inv.Agent != "codex" || len(inv.Args) < 3 || inv.Args[0] != "resume" || inv.Args[1] != codexID {
		t.Fatalf("final codex resume = %#v", inv)
	}
	if prompt := inv.Args[2]; !strings.Contains(prompt, "claude answer 2") || strings.Contains(prompt, "codex answer 2") {
		t.Fatalf("final codex incremental prompt = %q", prompt)
	}
	c = f.onlyConversation(t)
	if c.Pending != nil || c.ActiveAgent != model.Codex {
		t.Fatalf("final conversation = %#v", c)
	}
	entries = f.entries(t, c.ID)
	assertUnique(t, entries, "user request 0")
	assertContainsOnce(t, entries, "codex tool 2")
	assertUnique(t, entries, "codex answer 3")
	assertContainsOnce(t, entries, "codex tool 3")
	assertUnique(t, entries, "claude answer 2")
	assertUnique(t, entries, "claude tool 2")
	assertJournalClean(t, entries)
}

func TestIntegrationStartClaudeAndResumeIDs(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n", "start", "--agent", "claude", "--prompt", "claude first")
	if code != 0 {
		t.Fatalf("start code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	claudeBinding, ok := c.Bindings[model.Claude]
	if !ok || claudeBinding.Session.ID == "" || claudeBinding.Session.Backend != adapters.BackendJSONL {
		t.Fatalf("claude binding = %#v", claudeBinding)
	}
	claudeID := claudeBinding.Session.ID
	inv := lastNativeInvocation(t, f.invocations(t))
	if len(inv.Args) < 3 || inv.Args[0] != "--session-id" || inv.Args[1] != claudeID {
		t.Fatalf("claude start = %#v", inv)
	}
	entries := f.entries(t, c.ID)
	assertUnique(t, entries, "claude first")
	assertUnique(t, entries, "claude answer 1")
	assertUnique(t, entries, "claude tool 1")
	assertJournalClean(t, entries)

	codexID := ""
	for _, target := range []model.Agent{model.Codex, model.Claude, model.Codex} {
		var args []string
		if target == model.Codex {
			args = []string{"switch", "--to", "codex", "--conversation", c.ID}
		} else {
			args = []string{"switch", "--to", "claude", "--conversation", c.ID}
		}
		code, out, errOut = f.run(t, "q\n", args...)
		if code != 0 {
			t.Fatalf("switch %s code = %d, stdout = %q, stderr = %q", target, code, out, errOut)
		}
		inv = lastNativeInvocation(t, f.invocations(t))
		if target == model.Claude {
			if len(inv.Args) < 2 || inv.Args[0] != "--resume" || inv.Args[1] != claudeID {
				t.Fatalf("claude resume = %#v", inv)
			}
		} else {
			if codexID == "" {
				if len(inv.Args) != 1 || !strings.Contains(inv.Args[0], "claude answer 1") {
					t.Fatalf("codex first launch = %#v", inv)
				}
			} else if len(inv.Args) < 3 || inv.Args[0] != "resume" || inv.Args[1] != codexID {
				t.Fatalf("codex resume = %#v", inv)
			}
		}
		c = f.onlyConversation(t)
		if c.Pending != nil {
			t.Fatalf("pending after %s switch: %#v", target, c.Pending)
		}
		if target == model.Codex {
			codexID = c.Bindings[model.Codex].Session.ID
		}
		entries = f.entries(t, c.ID)
		assertJournalClean(t, entries)
	}
	assertUnique(t, f.entries(t, c.ID), "claude first")
}

func TestAttachExistingKeepsSourceAndMenuAttach(t *testing.T) {
	f := newNativeFixture(t)
	sessionID := "claude-existing"
	path := filepath.Join(f.claudeHome, "projects", encodeClaudeProject(f.cwd), sessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeClaudeFixtureRecord(t, path, f.cwd, sessionID, map[string]any{
		"parentUuid":  nil,
		"isSidechain": false,
		"cwd":         f.cwd,
		"sessionId":   sessionID,
		"type":        "user",
		"message":     map[string]any{"role": "user", "content": "existing request"},
		"uuid":        "existing-u1",
		"timestamp":   "2026-11-11T11:00:00Z",
	})
	writeClaudeFixtureRecord(t, path, f.cwd, sessionID, map[string]any{
		"parentUuid":  "existing-u1",
		"isSidechain": false,
		"cwd":         f.cwd,
		"sessionId":   sessionID,
		"type":        "assistant",
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "existing answer"},
		}},
		"uuid":      "existing-a1",
		"timestamp": "2026-11-11T11:00:01Z",
	})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := f.run(t, "2\n2\n1\nq\n")
	if code != 0 {
		t.Fatalf("menu attach code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	binding, ok := c.Bindings[model.Claude]
	if !ok || binding.Session.ID != sessionID {
		t.Fatalf("binding = %#v", c.Bindings)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("attach rewrote the source transcript")
	}
	entries := f.entries(t, c.ID)
	assertUnique(t, entries, "existing request")
	assertUnique(t, entries, "existing answer")
	assertJournalClean(t, entries)

	code, out, errOut = f.run(t, "q\n", "switch", "--to", "codex", "--conversation", c.ID)
	if code != 0 {
		t.Fatalf("switch after attach code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	inv := lastNativeInvocation(t, f.invocations(t))
	if inv.Agent != "codex" || len(inv.Args) != 1 {
		t.Fatalf("codex new launch = %#v", inv)
	}
	if prompt := inv.Args[0]; !strings.Contains(prompt, "existing request") || !strings.Contains(prompt, "existing answer") {
		t.Fatalf("codex prompt = %q", prompt)
	}
	afterSwitch, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, afterSwitch) {
		t.Fatal("switching to codex changed the claude source transcript")
	}
}

func TestAttachExistingCodexDirect(t *testing.T) {
	f := newNativeFixture(t)
	sessionID := "codex-existing"
	path := filepath.Join(f.codexHome, "sessions", "2026", "11", "11", "rollout-existing.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONLine(path, map[string]any{
		"timestamp": "2026-11-11T11:00:00Z",
		"type":      "session_meta",
		"payload": map[string]any{
			"id":         sessionID,
			"session_id": sessionID,
			"cwd":        f.cwd,
			"timestamp":  "2026-11-11T11:00:00Z",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONLine(path, map[string]any{
		"timestamp": "2026-11-11T11:00:01Z",
		"type":      "response_item",
		"payload": map[string]any{
			"id":      "existing-codex-u1",
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "existing codex request"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := appendJSONLine(path, map[string]any{
		"timestamp": "2026-11-11T11:00:02Z",
		"type":      "response_item",
		"payload": map[string]any{
			"id":      "existing-codex-a1",
			"type":    "message",
			"role":    "assistant",
			"content": []any{map[string]any{"type": "output_text", "text": "existing codex answer"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := f.run(t, "q\n", "attach", "--agent", "codex", "--session", sessionID)
	if code != 0 {
		t.Fatalf("attach code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	binding, ok := c.Bindings[model.Codex]
	if !ok || binding.Session.ID != sessionID || binding.Session.Backend != adapters.BackendRollout {
		t.Fatalf("binding = %#v", c.Bindings)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("attach rewrote the codex rollout")
	}
	entries := f.entries(t, c.ID)
	assertUnique(t, entries, "existing codex request")
	assertUnique(t, entries, "existing codex answer")
	assertJournalClean(t, entries)
}

func TestSwitchFromImportsNativeSource(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "start request")
	if code != 0 {
		t.Fatalf("start code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	sourceID := "source-claude"
	path := filepath.Join(f.claudeHome, "projects", encodeClaudeProject(f.cwd), sourceID+".jsonl")
	writeClaudeFixtureRecord(t, path, f.cwd, sourceID, map[string]any{
		"parentUuid": nil,
		"type":       "user",
		"message":    map[string]any{"role": "user", "content": "source request"},
		"uuid":       "source-u1",
		"timestamp":  "2026-11-11T12:00:00Z",
	})
	writeClaudeFixtureRecord(t, path, f.cwd, sourceID, map[string]any{
		"parentUuid": "source-u1",
		"type":       "assistant",
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "source answer"},
		}},
		"uuid":      "source-a1",
		"timestamp": "2026-11-11T12:00:01Z",
	})
	code, out, errOut = f.run(t, "q\n", "switch", "--to", "claude", "--conversation", c.ID, "--from", sourceID)
	if code != 0 {
		t.Fatalf("switch --from code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c = f.onlyConversation(t)
	binding, ok := c.Bindings[model.Claude]
	if !ok || binding.Session.ID != sourceID {
		t.Fatalf("claude binding = %#v", c.Bindings)
	}
	inv := lastNativeInvocation(t, f.invocations(t))
	if inv.Agent != "claude" || len(inv.Args) < 3 || inv.Args[0] != "--resume" || inv.Args[1] != sourceID {
		t.Fatalf("switch --from invocation = %#v", inv)
	}
	if prompt := inv.Args[2]; !strings.Contains(prompt, "codex answer 1") || strings.Contains(prompt, "source answer") {
		t.Fatalf("switch --from prompt = %q", prompt)
	}
	entries := f.entries(t, c.ID)
	assertUnique(t, entries, "start request")
	assertUnique(t, entries, "source request")
	assertUnique(t, entries, "source answer")
	assertUnique(t, entries, "claude answer 2")
	assertJournalClean(t, entries)
}

func TestResumeOptionsAfterID(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "start request")
	if code != 0 {
		t.Fatalf("start code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	binding := c.Bindings[model.Codex]
	code, out, errOut = f.run(t, "q\n", "resume", c.ID, "--agent", "codex")
	if code != 0 {
		t.Fatalf("resume code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	inv := lastNativeInvocation(t, f.invocations(t))
	if inv.Agent != "codex" || len(inv.Args) != 2 || inv.Args[0] != "resume" || inv.Args[1] != binding.Session.ID {
		t.Fatalf("resume invocation = %#v", inv)
	}
	assertUnique(t, f.entries(t, c.ID), "codex answer 2")
}

func writeClaudeFixtureRecord(t *testing.T, path, cwd, sessionID string, record map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if record["cwd"] == nil {
		record["cwd"] = cwd
	}
	if record["sessionId"] == nil {
		record["sessionId"] = sessionID
	}
	if err := appendJSONLine(path, record); err != nil {
		t.Fatal(err)
	}
}

func TestMenuEmptyListStillOffersNewAndAttach(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n")
	if code != 0 {
		t.Fatalf("menu code = %d, stderr = %q", code, errOut)
	}
	if !strings.Contains(out, "new conversation") || !strings.Contains(out, "attach existing native session") {
		t.Fatalf("empty menu did not offer new/attach: %q", out)
	}
}

func TestMenuCreatesConversation(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "1\n1\nmenu request\nq\n")
	if code != 0 {
		t.Fatalf("menu new code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	if c.Pending != nil || c.Bindings[model.Codex].Session.ID == "" {
		t.Fatalf("conversation = %#v", c)
	}
	assertUnique(t, f.entries(t, c.ID), "menu request")
}

type recordingRunner struct {
	runs []launcher.NativeRun
	err  error
}

func (r *recordingRunner) Run(_ context.Context, run launcher.NativeRun, _ io.Reader, _, _ io.Writer) error {
	r.runs = append(r.runs, run)
	return r.err
}

func TestPendingRecoveryChecksReceiptWithoutResending(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "first")
	if code != 0 {
		t.Fatalf("start code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	runner := &recordingRunner{err: errors.New("simulated crash")}
	var sink strings.Builder
	code = RunWithOptions(context.Background(), []string{"switch", "--to", "claude", "--conversation", c.ID}, strings.NewReader("q\n"), &sink, &sink, Options{StoreRoot: f.root, Runner: runner})
	if code == 0 || len(runner.runs) != 1 {
		t.Fatalf("crash switch code = %d, runs = %d", code, len(runner.runs))
	}
	c = f.onlyConversation(t)
	if c.Pending == nil || c.Pending.SessionID == "" || c.Pending.Packet.Target != model.Claude {
		t.Fatalf("pending = %#v", c.Pending)
	}
	path := filepath.Join(f.claudeHome, "projects", encodeClaudeProject(f.cwd), c.Pending.SessionID+".jsonl")
	writeClaudeFixtureRecord(t, path, f.cwd, c.Pending.SessionID, map[string]any{
		"parentUuid": nil,
		"type":       "user",
		"message":    map[string]any{"role": "user", "content": handoff.Prompt(c.Pending.Packet, "terminus context "+c.ID)},
		"uuid":       "late-receipt",
		"timestamp":  "2026-11-11T12:00:00Z",
	})
	recovery := &recordingRunner{}
	var recoveryOut strings.Builder
	code = RunWithOptions(context.Background(), []string{"switch", "--to", "claude", "--conversation", c.ID}, strings.NewReader("q\n"), &recoveryOut, &recoveryOut, Options{StoreRoot: f.root, Runner: recovery})
	if code != 0 {
		t.Fatalf("recovery code = %d, output = %q", code, recoveryOut.String())
	}
	if len(recovery.runs) != 0 {
		t.Fatalf("recovery resent the handoff: %#v", recovery.runs)
	}
	c = f.onlyConversation(t)
	if c.Pending != nil {
		t.Fatalf("pending remained after recovery: %#v", c.Pending)
	}
	if c.Bindings[model.Claude].Session.ID == "" {
		t.Fatalf("claude not bound: %#v", c.Bindings)
	}
	assertJournalClean(t, f.entries(t, c.ID))
}

func TestBoundReceiptRecoveryUsesPinnedSession(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "first")
	if code != 0 {
		t.Fatalf("start code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	codexBinding := c.Bindings[model.Codex]
	code, out, errOut = f.run(t, "q\n", "switch", "--to", "claude", "--conversation", c.ID)
	if code != 0 {
		t.Fatalf("switch claude code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	runner := &recordingRunner{err: errors.New("simulated crash")}
	var sink strings.Builder
	code = RunWithOptions(context.Background(), []string{"switch", "--to", "codex", "--conversation", c.ID}, strings.NewReader("q\n"), &sink, &sink, Options{StoreRoot: f.root, Runner: runner})
	if code == 0 {
		t.Fatalf("crash switch unexpectedly succeeded: %q", sink.String())
	}
	c = f.onlyConversation(t)
	if c.Pending == nil || c.Pending.Packet.Target != model.Codex {
		t.Fatalf("pending = %#v", c.Pending)
	}
	receipt := handoff.Prompt(c.Pending.Packet, "terminus context "+c.ID)
	if err := appendJSONLine(codexBinding.Session.Path, map[string]any{
		"timestamp": fakeTimestamp(),
		"type":      "response_item",
		"payload": map[string]any{
			"id":      "late-codex-receipt",
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": receipt}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	before := countAppServerInvocations(f.invocations(t))
	recovery := &recordingRunner{}
	var recoveryOut strings.Builder
	code = RunWithOptions(context.Background(), []string{"switch", "--to", "codex", "--conversation", c.ID}, strings.NewReader("q\n"), &recoveryOut, &recoveryOut, Options{StoreRoot: f.root, Runner: recovery})
	if code != 0 {
		t.Fatalf("recovery code = %d, output = %q", code, recoveryOut.String())
	}
	if len(recovery.runs) != 0 {
		t.Fatalf("bound recovery resent a process: %#v", recovery.runs)
	}
	if after := countAppServerInvocations(f.invocations(t)); after != before {
		t.Fatalf("bound receipt recovery probed app-server (before %d, after %d)", before, after)
	}
	c = f.onlyConversation(t)
	if c.Pending != nil {
		t.Fatalf("pending remained: %#v", c.Pending)
	}
}

func countAppServerInvocations(invocations []fakeInvocation) int {
	count := 0
	for _, invocation := range invocations {
		if len(invocation.Args) > 0 && invocation.Args[0] == "app-server" {
			count++
		}
	}
	return count
}

func TestPendingPromptDoesNotReplayFirstRequestForBoundSwitch(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "first request")
	if code != 0 {
		t.Fatalf("start code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	code, out, errOut = f.run(t, "q\n", "switch", "--to", "claude", "--conversation", c.ID)
	if code != 0 {
		t.Fatalf("switch claude code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	runner := &recordingRunner{err: errors.New("simulated crash")}
	var output strings.Builder
	code = RunWithOptions(context.Background(), []string{"switch", "--to", "codex", "--conversation", c.ID}, strings.NewReader("q\n"), &output, &output, Options{StoreRoot: f.root, Runner: runner})
	if code == 0 {
		t.Fatalf("crash switch unexpectedly succeeded: %q", output.String())
	}
	c = f.onlyConversation(t)
	if c.Pending == nil || c.Pending.Packet.Target != model.Codex {
		t.Fatalf("pending = %#v", c.Pending)
	}
	app := &App{options: Options{StoreRoot: f.root}, store: f.store, adapter: newAdapter}
	prompt, err := app.pendingPrompt(context.Background(), &c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "first request") {
		t.Fatalf("bound-switch retry replayed the start request: %q", prompt)
	}
}

func TestNonzeroExitWithSavedReceiptIsRecoverable(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "first")
	if code != 0 {
		t.Fatalf("start code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	t.Setenv("TERMINUS_FAKE_EXIT", "1")
	code, out, errOut = f.run(t, "q\n", "switch", "--to", "claude", "--conversation", c.ID)
	if code == 0 || !strings.Contains(errOut, "run native claude") {
		t.Fatalf("nonzero switch code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c = f.onlyConversation(t)
	if c.Pending != nil {
		t.Fatalf("saved receipt should have settled pending: %#v", c.Pending)
	}
	binding, ok := c.Bindings[model.Claude]
	if !ok || binding.Session.ID == "" {
		t.Fatalf("claude binding = %#v", c.Bindings)
	}
	assertUnique(t, f.entries(t, c.ID), "claude answer 1")

	t.Setenv("TERMINUS_FAKE_EXIT", "")
	before := len(f.invocations(t))
	code, out, errOut = f.run(t, "q\n", "switch", "--to", "claude", "--conversation", c.ID)
	if code != 0 {
		t.Fatalf("retry code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	invasions := f.invocations(t)[before:]
	inv := lastNativeInvocation(t, invasions)
	if inv.Agent != "claude" || len(inv.Args) != 2 || inv.Args[0] != "--resume" || inv.Args[1] != binding.Session.ID {
		t.Fatalf("retry resent a handoff instead of plain resume: %#v", inv)
	}
}

func TestNoReceiptKeepsPendingAndRetryResends(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "first")
	if code != 0 {
		t.Fatalf("start code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c := f.onlyConversation(t)
	t.Setenv("TERMINUS_FAKE_NO_RECEIPT", "1")
	code, out, errOut = f.run(t, "q\n", "switch", "--to", "claude", "--conversation", c.ID)
	if code == 0 || !strings.Contains(errOut, "pending handoff receipt") {
		t.Fatalf("no receipt code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	c = f.onlyConversation(t)
	if c.Pending == nil || c.Pending.Packet.Target != model.Claude {
		t.Fatalf("pending = %#v", c.Pending)
	}
	marker := handoff.Marker(c.Pending.Packet.ID)
	t.Setenv("TERMINUS_FAKE_NO_RECEIPT", "")
	code, out, errOut = f.run(t, "q\n", "switch", "--to", "claude", "--conversation", c.ID)
	if code != 0 {
		t.Fatalf("retry code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	inv := lastNativeInvocation(t, f.invocations(t))
	if len(inv.Args) < 3 || !strings.Contains(inv.Args[2], marker) {
		t.Fatalf("retry did not resend the same packet: %#v", inv)
	}
	c = f.onlyConversation(t)
	if c.Pending != nil {
		t.Fatalf("pending after retry: %#v", c.Pending)
	}
}

func TestMultipleReceiptCandidatesAreRejected(t *testing.T) {
	f := newNativeFixture(t)
	c, err := f.store.Create(f.cwd)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := handoff.Build(c, nil, model.Claude, 1024)
	if err != nil {
		t.Fatal(err)
	}
	c.Pending = &model.Pending{Packet: packet, StartedAt: time.Now().UTC()}
	c.ActiveAgent = model.Codex
	if err := f.store.Save(c); err != nil {
		t.Fatal(err)
	}
	content := handoff.Prompt(packet, "terminus context "+c.ID)
	for _, sessionID := range []string{"candidate-a", "candidate-b"} {
		path := filepath.Join(f.claudeHome, "projects", encodeClaudeProject(f.cwd), sessionID+".jsonl")
		writeClaudeFixtureRecord(t, path, f.cwd, sessionID, map[string]any{
			"parentUuid": nil,
			"type":       "user",
			"message":    map[string]any{"role": "user", "content": content},
			"uuid":       sessionID + "-u1",
			"timestamp":  "2026-11-11T12:00:00Z",
		})
	}
	runner := &recordingRunner{}
	var out strings.Builder
	code := RunWithOptions(context.Background(), []string{"switch", "--to", "claude", "--conversation", c.ID}, strings.NewReader("q\n"), &out, &out, Options{StoreRoot: f.root, Runner: runner})
	if code == 0 || !strings.Contains(out.String(), "multiple candidate") {
		t.Fatalf("code = %d, output = %q", code, out.String())
	}
	if len(runner.runs) != 0 {
		t.Fatalf("ambiguous receipt launched a new process: %#v", runner.runs)
	}
	loaded, err := f.store.Load(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Pending == nil {
		t.Fatal("ambiguous receipt cleared pending")
	}
}

func TestContextOptionsAfterIDAndFullText(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	conversation, err := st.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	events := []model.Event{
		{ID: "1", Source: model.Codex, SessionID: "native-1", Kind: "message", Role: "user", Text: "你好，世界"},
		{ID: "2", Source: model.Codex, SessionID: "native-1", Kind: "message", Role: "assistant", Text: "完整原文\nsecond line"},
	}
	if _, err := st.Append(conversation.ID, events); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code := RunWithOptions(context.Background(), []string{"context", conversation.ID, "--json", "--after", "1", "--limit", "1"}, strings.NewReader(""), &out, &errOut, Options{StoreRoot: root})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
	var entries []model.Entry
	if err := json.Unmarshal([]byte(out.String()), &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Seq != 2 || entries[0].Event.Text != "完整原文\nsecond line" {
		t.Fatalf("entries = %#v", entries)
	}
	out.Reset()
	code = RunWithOptions(context.Background(), []string{"context", conversation.ID, "--after", "1"}, strings.NewReader(""), &out, &errOut, Options{StoreRoot: root})
	if code != 0 || !strings.Contains(out.String(), "完整原文\nsecond line") {
		t.Fatalf("code = %d, output = %q, stderr = %q", code, out.String(), errOut.String())
	}
}

func TestListJSON(t *testing.T) {
	root := t.TempDir()
	st := store.New(root)
	cwd := t.TempDir()
	t.Chdir(cwd)
	for i := 0; i < 2; i++ {
		if _, err := st.Create(cwd); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut strings.Builder
	code := RunWithOptions(context.Background(), []string{"list", "--json"}, strings.NewReader(""), &out, &errOut, Options{StoreRoot: root})
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errOut.String())
	}
	var conversations []model.Conversation
	if err := json.Unmarshal([]byte(out.String()), &conversations); err != nil {
		t.Fatal(err)
	}
	if len(conversations) != 2 {
		t.Fatalf("conversations = %#v", conversations)
	}
}

func TestDoctorChecksStoreAndExecutables(t *testing.T) {
	f := newNativeFixture(t)
	code, out, errOut := f.run(t, "", "doctor")
	if code != 0 {
		t.Fatalf("doctor code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	for _, want := range []string{"store: ok", f.codexBin, f.claudeBin, "codex executable", "claude executable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output %q missing %q", out, want)
		}
	}
	t.Setenv("TERMINUS_CODEX_BIN", filepath.Join(f.root, "missing-codex"))
	t.Setenv("TERMINUS_CLAUDE_BIN", filepath.Join(f.root, "missing-claude"))
	code, out, errOut = f.run(t, "", "doctor")
	if code == 0 || !strings.Contains(out, "not found") {
		t.Fatalf("missing executables: code = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
}

func TestDoctorRejectsUnusableStore(t *testing.T) {
	newNativeFixture(t)
	badRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(badRoot, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut strings.Builder
	code := RunWithOptions(context.Background(), []string{"doctor"}, strings.NewReader(""), &out, &errOut, Options{StoreRoot: badRoot})
	if code == 0 || !strings.Contains(out.String(), "store: failed") {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, out.String(), errOut.String())
	}
}

func TestDoctorUsesResolvedDefaultRootAndChecksPermissions(t *testing.T) {
	f := newNativeFixture(t)
	var out, errOut strings.Builder
	code := RunWithOptions(context.Background(), []string{"doctor"}, strings.NewReader(""), &out, &errOut, Options{})
	if code != 0 {
		t.Fatalf("code = %d, stdout = %q, stderr = %q", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "data root: "+f.root) {
		t.Fatalf("doctor did not resolve TERMINUS_HOME: %q", out.String())
	}

	openRoot := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(openRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(openRoot, 0755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	code = RunWithOptions(context.Background(), []string{"doctor"}, strings.NewReader(""), &out, &errOut, Options{StoreRoot: openRoot})
	if code != 0 || !strings.Contains(out.String(), "store: ok") {
		t.Fatalf("open store: code = %d, stdout = %q, stderr = %q", code, out.String(), errOut.String())
	}
	info, err := os.Stat(openRoot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("doctor left data root mode at %04o", info.Mode().Perm())
	}
}

type trackingReader struct {
	data string
	pos  int
}

func (r *trackingReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[r.pos]
	r.pos++
	return 1, nil
}

func TestReadLineDoesNotBufferAhead(t *testing.T) {
	reader := &trackingReader{data: "one\ntwo"}
	line, err := readLine(reader)
	if err != nil || line != "one" {
		t.Fatalf("line = %q, err = %v", line, err)
	}
	if reader.pos != 4 {
		t.Fatalf("readLine consumed %d bytes, want exactly 4", reader.pos)
	}
	line, err = readLine(reader)
	if err != nil || line != "two" {
		t.Fatalf("second line = %q, err = %v", line, err)
	}
}

func TestHelpVersionAndMarker(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"help"}, "ID distinction:"},
		{[]string{"--help"}, "Usage:"},
		{[]string{"version"}, "terminus 0.1.0"},
	} {
		var out, errOut strings.Builder
		if code := Run(context.Background(), test.args, strings.NewReader(""), &out, &errOut); code != 0 {
			t.Fatalf("code = %d, stderr = %q", code, errOut.String())
		}
		if !strings.Contains(out.String(), test.want) {
			t.Fatalf("output %q does not contain %q", out.String(), test.want)
		}
	}
	marker := handoff.Marker("00000000000000000000000000000001")
	if !handoff.IsInjected(marker + "\nhistory\n" + "[[TERMINUS_HANDOFF:00000000000000000000000000000001:END]]") {
		t.Fatal("complete receipt not recognized")
	}
	if handoff.IsInjected(marker + "\nhistory") {
		t.Fatal("partial receipt accepted")
	}
}

func TestLineReaderWorksWithBufferedReader(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("q\n"))
	line, err := readLine(reader)
	if err != nil || line != "q" {
		t.Fatalf("line = %q, err = %v", line, err)
	}
}
