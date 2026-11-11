package adapters

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"terminus/internal/model"
)

type adapterFailure struct {
	fallback bool
	err      error
}

func (e *adapterFailure) Error() string { return e.err.Error() }
func (e *adapterFailure) Unwrap() error { return e.err }

func failure(fallback bool, format string, args ...any) error {
	return &adapterFailure{fallback: fallback, err: fmt.Errorf(format, args...)}
}

func canFallback(err error) bool {
	var failure *adapterFailure
	return errors.As(err, &failure) && failure.fallback
}

type codexAdapter struct {
	bin     string
	home    string
	timeout time.Duration
}

func newCodexAdapter() *codexAdapter {
	return &codexAdapter{
		bin:     envPath("TERMINUS_CODEX_BIN", ""),
		home:    envPath("CODEX_HOME", ".codex"),
		timeout: 8 * time.Second,
	}
}

func (a *codexAdapter) executable() string {
	if a.bin != "" {
		return a.bin
	}
	return "codex"
}

func (a *codexAdapter) Discover(ctx context.Context, cwd string) ([]model.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sessions, err := a.discoverAppServer(ctx, cwd)
	if err == nil {
		return sessions, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !canFallback(err) {
		return nil, err
	}
	return a.discoverRollouts(ctx, cwd)
}

func (a *codexAdapter) Read(ctx context.Context, s model.Session) ([]model.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	events, err := a.readAppServer(ctx, s)
	if err == nil {
		return events, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s.Path == "" || !canFallback(err) {
		return nil, err
	}
	if _, statErr := os.Stat(s.Path); statErr != nil {
		return nil, err
	}
	return readCodexRollout(s)
}

func isUnsupportedRPC(err error) bool {
	var rpc *rpcError
	if errors.As(err, &rpc) {
		if rpc.Code == -32601 {
			return true
		}
		message := strings.ToLower(rpc.Message)
		if rpc.Code == -32602 {
			return strings.Contains(message, "unknown field") ||
				strings.Contains(message, "unexpected field") ||
				strings.Contains(message, "invalid type") ||
				strings.Contains(message, "missing field")
		}
		return strings.Contains(message, "not supported") ||
			strings.Contains(message, "not support") ||
			strings.Contains(message, "unsupported") ||
			strings.Contains(message, "not implemented") ||
			strings.Contains(message, "method not found") ||
			(strings.Contains(message, "pagination") &&
				(strings.Contains(message, "unavailable") || strings.Contains(message, "does not exist"))) ||
			strings.Contains(message, "requires experimentalapi")
	}
	return false
}
