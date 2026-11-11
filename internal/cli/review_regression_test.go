package cli

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"terminus/internal/launcher"
	"terminus/internal/model"
)

type reviewRunner struct{ entered, release chan struct{} }

func (r reviewRunner) Run(_ context.Context, _ launcher.NativeRun, _ io.Reader, _, _ io.Writer) error {
	close(r.entered)
	<-r.release
	return nil
}

// These scenarios use only the synthetic native CLIs and isolated homes.
func TestReviewResumeSelectedAgentBecomesActive(t *testing.T) {
	f := newNativeFixture(t)
	if code, _, errout := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "initial"); code != 0 {
		t.Fatal(errout)
	}
	c := f.onlyConversation(t)
	if code, _, errout := f.run(t, "q\n", "switch", "--conversation", c.ID, "--to", "claude"); code != 0 {
		t.Fatal(errout)
	}
	if code, _, errout := f.run(t, "q\n", "resume", c.ID, "--agent", "codex"); code != 0 {
		t.Fatal(errout)
	}
	if got := f.onlyConversation(t).ActiveAgent; got != model.Codex {
		t.Fatalf("after explicitly resuming codex active agent = %s, want codex", got)
	}
}

func TestReviewResumeHonorsConversationLock(t *testing.T) {
	f := newNativeFixture(t)
	if code, _, errout := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", "initial"); code != 0 {
		t.Fatal(errout)
	}
	c := f.onlyConversation(t)
	locked, release, lockDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() { lockDone <- f.store.WithLock(c.ID, func() error { close(locked); <-release; return nil }) }()
	<-locked
	runner := reviewRunner{make(chan struct{}), make(chan struct{})}
	done := make(chan int, 1)
	go func() {
		done <- RunWithOptions(context.Background(), []string{"resume", c.ID}, strings.NewReader("q\n"), io.Discard, io.Discard, Options{StoreRoot: f.root, Runner: runner})
	}()
	select {
	case <-runner.entered:
		close(runner.release)
		close(release)
		<-lockDone
		<-done
		t.Fatal("native resume started while another operation held the conversation lock")
	case <-time.After(2 * time.Second):
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	close(runner.release)
	if code := <-done; code != 0 {
		t.Fatalf("resume returned %d after lock released", code)
	}
}

func TestRepeatedInitialRequestSurvivesRoundTrip(t *testing.T) {
	f := newNativeFixture(t)
	const request = "repeat this real user request"
	if code, _, errout := f.run(t, "q\n", "start", "--agent", "codex", "--prompt", request); code != 0 {
		t.Fatal(errout)
	}
	c := f.onlyConversation(t)
	path := c.Bindings[model.Codex].Session.Path
	if err := appendJSONLine(path, map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"id": "later-real-user", "type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": request}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"claude", "codex"} {
		if code, _, errout := f.run(t, "q\n", "resume", c.ID, "--agent", target); code != 0 {
			t.Fatal(errout)
		}
		count := 0
		for _, entry := range f.entries(t, c.ID) {
			if entry.Event.Role == "user" && entry.Event.Text == request {
				count++
			}
		}
		if count != 2 {
			t.Fatalf("after resuming %s: kept %d requests, want initial and later request", target, count)
		}
	}
}
