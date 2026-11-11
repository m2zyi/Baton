package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terminus/internal/model"
)

func TestNewRunCommands(t *testing.T) {
	tests := []struct {
		name      string
		agent     string
		sessionID string
		wantArgs  []string
	}{
		{"codex new", "codex", "", []string{"PROMPT"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewSessionRun(model.Agent(tt.agent), tt.sessionID, "PROMPT", "/tmp", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Args) != len(tt.wantArgs) {
				t.Fatalf("args = %v, want %v", got.Args, tt.wantArgs)
			}
			for i := range tt.wantArgs {
				if got.Args[i] != tt.wantArgs[i] {
					t.Fatalf("args = %v, want %v", got.Args, tt.wantArgs)
				}
			}
		})
	}
	if _, err := NewSessionRun(model.Claude, "", "PROMPT", "/tmp", ""); err == nil {
		t.Fatal("claude first launch without UUID: error = nil")
	}
	got, err := ResumeRun(model.Claude, "abc", "PROMPT", "/tmp", "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--resume", "abc", "PROMPT"}
	if len(got.Args) != len(want) {
		t.Fatalf("resume args = %v, want %v", got.Args, want)
	}
}

func TestProcessRunsDirectlyWithCWDAndStdio(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-agent")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'cwd='; pwd\nprintf ' arg='; printf '%s' \"$1\"\nprintf ' stdin='; cat\n"), 0755); err != nil {
		t.Fatal(err)
	}
	run := NativeRun{Agent: "codex", Binary: script, Args: []string{"hello"}, CWD: dir}
	var out, errOut strings.Builder
	if err := (Process{}).Run(context.Background(), run, strings.NewReader("typed"), &out, &errOut); err != nil {
		t.Fatal(err)
	}
	reported := out.String()
	actual, err := filepath.EvalSymlinks(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(reported, "cwd="), "\n arg=hello stdin=typed")))
	if err != nil {
		t.Fatal(err)
	}
	if actual != dir || !strings.HasSuffix(reported, "\n arg=hello stdin=typed") {
		t.Fatalf("output = %q", reported)
	}
	if errOut.String() != "" {
		t.Fatalf("stderr = %q", errOut.String())
	}
}
