package launcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"baton/internal/model"
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

func TestStdinFileIdentityHelper(t *testing.T) {
	if os.Getenv("BATON_STDIN_IDENTITY") != "1" {
		return
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(0, &stat); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	fmt.Printf("%d %d\n", stat.Ino, stat.Mode)
	os.Exit(0)
}

func TestProcessInheritsOriginalStdinFile(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := os.CreateTemp(t.TempDir(), "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := stdin.WriteString("terminal input\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	info, err := stdin.Stat()
	if err != nil {
		t.Fatal(err)
	}
	parent := info.Sys().(*syscall.Stat_t)

	t.Setenv("BATON_STDIN_IDENTITY", "1")
	run := NativeRun{Agent: model.Codex, Binary: executable, Args: []string{"-test.run=^TestStdinFileIdentityHelper$"}, CWD: t.TempDir(), Interactive: true}
	var out, errOut strings.Builder
	if err := (Process{}).Run(context.Background(), run, stdin, &out, &errOut); err != nil {
		t.Fatalf("run: %v, stderr = %q", err, errOut.String())
	}
	var inode, mode uint64
	if _, err := fmt.Sscanf(strings.TrimSpace(out.String()), "%d %d", &inode, &mode); err != nil {
		t.Fatalf("parse child stat %q: %v", out.String(), err)
	}
	if inode != uint64(parent.Ino) {
		t.Fatalf("child stdin inode %d, parent file inode %d; exec used a pipe", inode, parent.Ino)
	}
	if uint32(mode)&uint32(syscall.S_IFMT) != uint32(syscall.S_IFREG) {
		t.Fatalf("child stdin mode %#o is not a regular file", mode)
	}
}

func TestProcessPassesArgumentArrayWithoutShell(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "echo-args")
	content := "#!/bin/sh\nprintf '%s\\n' \"$#\"\nfor arg in \"$@\"; do printf '<%s>\\n' \"$arg\"; done\n"
	if err := os.WriteFile(script, []byte(content), 0700); err != nil {
		t.Fatal(err)
	}
	args := []string{"hello world", "$(touch pwned)", "; echo injected", "`echo quoted`"}
	run := NativeRun{Agent: model.Codex, Binary: script, Args: args, CWD: dir}
	var out, errOut strings.Builder
	if err := (Process{}).Run(context.Background(), run, strings.NewReader(""), &out, &errOut); err != nil {
		t.Fatalf("run: %v, stderr = %q", err, errOut.String())
	}
	want := "4\n<" + args[0] + ">\n<" + args[1] + ">\n<" + args[2] + ">\n<" + args[3] + ">\n"
	if out.String() != want {
		t.Fatalf("output = %q, want %q", out.String(), want)
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Fatal("argument was evaluated by a shell")
	}
}

func TestRunCommandBinariesAndEmptyResumePrompt(t *testing.T) {
	t.Setenv("BATON_CODEX_BIN", "")
	t.Setenv("BATON_CLAUDE_BIN", "")
	codex, err := NewSessionRun(model.Codex, "", "PROMPT", "/tmp", "")
	if err != nil || codex.Binary != "codex" || len(codex.Args) != 1 || codex.Args[0] != "PROMPT" {
		t.Fatalf("codex new = %#v, %v", codex, err)
	}
	claude, err := NewSessionRun(model.Claude, "uuid", "PROMPT", "/tmp", "")
	if err != nil || claude.Binary != "claude" || len(claude.Args) != 3 || claude.Args[0] != "--session-id" {
		t.Fatalf("claude new = %#v, %v", claude, err)
	}
	custom, err := NewSessionRun(model.Claude, "uuid", "PROMPT", "/tmp", "/custom/claude")
	if err != nil || custom.Binary != "/custom/claude" {
		t.Fatalf("custom binary = %#v, %v", custom, err)
	}
	resume, err := ResumeRun(model.Codex, "native-id", "", "/tmp", "")
	if err != nil || resume.Binary != "codex" || len(resume.Args) != 2 || resume.Args[0] != "resume" {
		t.Fatalf("codex empty resume = %#v, %v", resume, err)
	}
	resume, err = ResumeRun(model.Claude, "native-id", "", "/tmp", "")
	if err != nil || resume.Binary != "claude" || len(resume.Args) != 2 || resume.Args[0] != "--resume" {
		t.Fatalf("claude empty resume = %#v, %v", resume, err)
	}
}

func TestProcessCancellationStopsInteractiveChild(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a child process")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "slow-agent")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = (Process{}).Run(ctx, NativeRun{Agent: model.Codex, Binary: script, CWD: dir, Interactive: true}, devNull, devNull, devNull)
	if err == nil {
		t.Fatal("canceled interactive child returned nil error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
}
