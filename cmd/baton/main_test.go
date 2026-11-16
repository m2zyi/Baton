package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBatonMainHelper(t *testing.T) {
	mode := os.Getenv("BATON_MAIN_HELPER")
	if mode == "" {
		return
	}
	os.Args = []string{"baton"}
	if mode == "native" {
		os.Args = append(os.Args, "start", "--agent", "claude", "--prompt", "synthetic lifecycle test")
	}
	os.Exit(run())
}

func TestSIGTERMStopsNativeChild(t *testing.T) {
	cmd, stdout := mainHelper(t, "native")
	line := waitOutput(t, stdout, "READY:", '\n')
	index := strings.LastIndex(line, "READY:")
	pid, err := strconv.Atoi(strings.TrimSpace(line[index+len("READY:"):]))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid native pid in %q", line)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	assertTerminated(t, cmd)
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("native pid %d survived parent termination: %v", pid, err)
	}
}

func TestSIGTERMExitsAtMenu(t *testing.T) {
	cmd, stdout := mainHelper(t, "menu")
	waitOutput(t, stdout, "Select:", 0)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	assertTerminated(t, cmd)
}

func mainHelper(t *testing.T, mode string) (*exec.Cmd, *bufio.Reader) {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake-claude")
	// exec preserves the reported PID. No real model, credential, or history
	// is involved; cancellation must terminate this synthetic foreground job.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'READY:%s\\n' \"$$\"\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestBatonMainHelper$")
	cmd.Env = append(os.Environ(),
		"BATON_MAIN_HELPER="+mode,
		"BATON_HOME="+filepath.Join(dir, "data"),
		"CLAUDE_CONFIG_DIR="+filepath.Join(dir, "claude"),
		"CODEX_HOME="+filepath.Join(dir, "codex"),
		"BATON_CLAUDE_BIN="+fake,
	)
	cmd.Dir = dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close() })
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, bufio.NewReader(stdout)
}

func waitOutput(t *testing.T, out *bufio.Reader, marker string, terminator byte) string {
	t.Helper()
	ready := make(chan string, 1)
	go func() {
		var b strings.Builder
		for {
			c, err := out.ReadByte()
			if err != nil {
				ready <- fmt.Sprintf("output ended: %v; %s", err, b.String())
				return
			}
			b.WriteByte(c)
			if strings.Contains(b.String(), marker) && (terminator == 0 || c == terminator) {
				ready <- b.String()
				return
			}
		}
	}()
	select {
	case text := <-ready:
		if !strings.Contains(text, marker) {
			t.Fatal(text)
		}
		return text
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %q", marker)
		return ""
	}
}

func assertTerminated(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	err := cmd.Wait()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 143 {
		t.Fatalf("parent termination = %v, want exit 143", err)
	}
}
