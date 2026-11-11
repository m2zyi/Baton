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

	"terminus/internal/model"
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
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// NewSessionRun builds a first-launch command. Claude requires an explicit UUID.
func NewSessionRun(agent model.Agent, sessionID, prompt, cwd, customBinary string) (NativeRun, error) {
	if customBinary == "" {
		switch agent {
		case model.Codex:
			customBinary = os.Getenv("TERMINUS_CODEX_BIN")
		case model.Claude:
			customBinary = os.Getenv("TERMINUS_CLAUDE_BIN")
		}
	}
	switch agent {
	case model.Codex:
		if sessionID != "" {
			return NativeRun{}, errors.New("codex first launch does not accept a native session ID")
		}
		if customBinary != "" {
			return NativeRun{Agent: agent, Binary: customBinary, Args: []string{prompt}, CWD: cwd, Interactive: true}, nil
		}
		return NativeRun{Agent: agent, Binary: "codex", Args: []string{prompt}, CWD: cwd, Interactive: true}, nil
	case model.Claude:
		if sessionID == "" {
			return NativeRun{}, errors.New("claude first launch requires an explicit UUID")
		}
		return NativeRun{Agent: agent, Binary: "claude", Args: []string{"--session-id", sessionID, prompt}, CWD: cwd, Interactive: true}, nil
	default:
		return NativeRun{}, fmt.Errorf("unsupported native agent %q", agent)
	}
}

// ResumeRun builds a native resume command for an already bound session.
func ResumeRun(agent model.Agent, sessionID, prompt, cwd, customBinary string) (NativeRun, error) {
	if sessionID == "" {
		return NativeRun{}, errors.New("native resume requires a session ID")
	}
	if customBinary == "" {
		switch agent {
		case model.Codex:
			customBinary = os.Getenv("TERMINUS_CODEX_BIN")
		case model.Claude:
			customBinary = os.Getenv("TERMINUS_CLAUDE_BIN")
		}
	}
	switch agent {
	case model.Codex:
		return NativeRun{Agent: agent, Binary: customBinary, Args: []string{"resume", sessionID, prompt}, CWD: cwd, Interactive: true}, nil
	case model.Claude:
		return NativeRun{Agent: agent, Binary: customBinary, Args: []string{"--resume", sessionID, prompt}, CWD: cwd, Interactive: true}, nil
	default:
		return NativeRun{}, fmt.Errorf("unsupported native agent %q", agent)
	}
}
