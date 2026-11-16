// Package launcher runs native agent CLIs as direct child processes.
package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"baton/internal/model"
)

const CommandTimeout = 5 * time.Second

// NativeRun describes one direct native CLI invocation.
type NativeRun struct {
	Agent       model.Agent
	Binary      string
	Args        []string
	CWD         string
	Interactive bool
}

// Runner isolates process creation and stdio for tests and callers.
type Runner interface {
	Run(ctx context.Context, run NativeRun, stdin io.Reader, stdout, stderr io.Writer) error
}

type Process struct{}

func (Process) Run(ctx context.Context, run NativeRun, stdin io.Reader, stdout, stderr io.Writer) error {
	if run.Binary == "" {
		return errors.New("native agent binary path is empty")
	}
	if stdin == nil || stdout == nil || stderr == nil {
		return errors.New("native agent process requires stdin, stdout and stderr")
	}
	if !run.Interactive {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, CommandTimeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, run.Binary, run.Args...)
	cmd.Dir = run.CWD
	// Passing the original *os.File values straight through is essential. An
	// exec.Cmd uses an *os.File directly (dup2), while any other io.Reader or
	// io.Writer is serviced by a pipe. A buffered wrapper here would turn the
	// native TUI's terminal into a pipe and break raw mode, so callers must
	// hand Process the original files.
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = 2 * time.Second
	return cmd.Run()
}

func binaryFor(agent model.Agent, customBinary string) string {
	if customBinary != "" {
		return customBinary
	}
	switch agent {
	case model.Codex:
		if value := os.Getenv("BATON_CODEX_BIN"); value != "" {
			return value
		}
		return "codex"
	case model.Claude:
		if value := os.Getenv("BATON_CLAUDE_BIN"); value != "" {
			return value
		}
		return "claude"
	default:
		return ""
	}
}

// NewSessionRun builds a first-launch command. Claude requires an explicit UUID.
func NewSessionRun(agent model.Agent, sessionID, prompt, cwd, customBinary string) (NativeRun, error) {
	switch agent {
	case model.Codex:
		if sessionID != "" {
			return NativeRun{}, errors.New("codex first launch does not accept a native session ID")
		}
		return NativeRun{Agent: agent, Binary: binaryFor(agent, customBinary), Args: []string{prompt}, CWD: cwd, Interactive: true}, nil
	case model.Claude:
		if sessionID == "" {
			return NativeRun{}, errors.New("claude first launch requires an explicit UUID")
		}
		return NativeRun{Agent: agent, Binary: binaryFor(agent, customBinary), Args: []string{"--session-id", sessionID, prompt}, CWD: cwd, Interactive: true}, nil
	default:
		return NativeRun{}, fmt.Errorf("unsupported native agent %q", agent)
	}
}

// ResumeRun builds a native resume command for an already bound session.
func ResumeRun(agent model.Agent, sessionID, prompt, cwd, customBinary string) (NativeRun, error) {
	if sessionID == "" {
		return NativeRun{}, errors.New("native resume requires a session ID")
	}
	switch agent {
	case model.Codex:
		args := []string{"resume", sessionID}
		if prompt != "" {
			args = append(args, prompt)
		}
		return NativeRun{Agent: agent, Binary: binaryFor(agent, customBinary), Args: args, CWD: cwd, Interactive: true}, nil
	case model.Claude:
		args := []string{"--resume", sessionID}
		if prompt != "" {
			args = append(args, prompt)
		}
		return NativeRun{Agent: agent, Binary: binaryFor(agent, customBinary), Args: args, CWD: cwd, Interactive: true}, nil
	default:
		return NativeRun{}, fmt.Errorf("unsupported native agent %q", agent)
	}
}
