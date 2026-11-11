package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"terminus/internal/model"
)

func TestLockWaitCanBeCanceled(t *testing.T) {
	s := New(t.TempDir())
	c, err := s.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- s.WithLock(c.ID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	called := false
	err = s.WithLockContext(ctx, c.ID, func() error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("canceled lock wait: err=%v callback called=%v", err, called)
	}
}

func TestAppendDedupesAcrossSourcesAndSessions(t *testing.T) {
	store := New(t.TempDir())
	conversation, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := model.Event{
		ID:        "shared-id",
		Source:    model.Codex,
		SessionID: "codex-1",
		Kind:      "message",
		Role:      "user",
		Text:      "first text",
	}
	changedText := base
	changedText.Text = "changed text must not replace the first record"
	otherSession := base
	otherSession.SessionID = "codex-2"
	otherSource := base
	otherSource.Source = model.Claude
	otherSource.SessionID = "claude-1"
	receipt := base
	receipt.ID = "receipt"
	receipt.Kind = "handoff"

	added, err := store.Append(conversation.ID, []model.Event{base, changedText, otherSession, otherSource, receipt})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 3 {
		t.Fatalf("added %d events, want 3: %+v", len(added), added)
	}
	for index, entry := range added {
		if entry.Seq != int64(index+1) {
			t.Fatalf("sequence %d = %d", index, entry.Seq)
		}
	}
	if added[0].Event.Text != "first text" {
		t.Fatalf("duplicate identity replaced the first record: %+v", added[0])
	}
	if added[1].Event.SessionID != "codex-2" || added[2].Event.Source != model.Claude {
		t.Fatalf("distinct sources or sessions were deduplicated: %+v", added)
	}

	added, err = store.Append(conversation.ID, []model.Event{base, receipt})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 {
		t.Fatalf("duplicate append added %d events", len(added))
	}
	entries, err := store.Entries(conversation.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("journal has %d entries, want 3", len(entries))
	}
}

func TestListFiltersSameBasenameByAbsoluteCWD(t *testing.T) {
	store := New(filepath.Join(t.TempDir(), "data"))
	cwdA := filepath.Join(t.TempDir(), "alpha", "project")
	cwdB := filepath.Join(t.TempDir(), "beta", "project")
	for _, cwd := range []string{cwdA, cwdB} {
		if err := os.MkdirAll(cwd, 0700); err != nil {
			t.Fatal(err)
		}
	}
	conversationA, err := store.Create(cwdA)
	if err != nil {
		t.Fatal(err)
	}
	conversationB, err := store.Create(cwdB)
	if err != nil {
		t.Fatal(err)
	}

	listedA, err := store.List(cwdA)
	if err != nil {
		t.Fatal(err)
	}
	if len(listedA) != 1 || listedA[0].ID != conversationA.ID {
		t.Fatalf("cwd A listed %+v", listedA)
	}
	listedB, err := store.List(cwdB)
	if err != nil {
		t.Fatal(err)
	}
	if len(listedB) != 1 || listedB[0].ID != conversationB.ID {
		t.Fatalf("cwd B listed %+v", listedB)
	}
	all, err := store.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("all conversations = %d, want 2", len(all))
	}
}

func TestSaveAtomicAndKeepsManifestOnFailure(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	conversation, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := store.dir(conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	staleTemp := filepath.Join(dir, ".manifest-crash")
	if err := os.WriteFile(staleTemp, []byte("{partial"), 0600); err != nil {
		t.Fatal(err)
	}

	stateA := filepath.Join(root, "state-a")
	stateB := filepath.Join(root, "state-b")
	conversation.CWD = stateA
	if err := store.Save(conversation); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				readerDone <- nil
				return
			default:
			}
			loaded, loadErr := store.Load(conversation.ID)
			if loadErr != nil {
				readerDone <- loadErr
				return
			}
			if loaded.ID != conversation.ID || loaded.Version != 1 ||
				(loaded.CWD != stateA && loaded.CWD != stateB) {
				readerDone <- fmt.Errorf("reader saw partial manifest: %+v", loaded)
				return
			}
			runtime.Gosched()
		}
	}()

	var writeErr error
	for iteration := 0; iteration < 50; iteration++ {
		if iteration%2 == 0 {
			conversation.CWD = stateB
		} else {
			conversation.CWD = stateA
		}
		if writeErr = store.Save(conversation); writeErr != nil {
			break
		}
		runtime.Gosched()
	}
	conversation.CWD = stateB
	if writeErr == nil {
		writeErr = store.Save(conversation)
	}
	close(stop)
	if readerErr := <-readerDone; readerErr != nil {
		t.Fatal(readerErr)
	}
	if writeErr != nil {
		t.Fatal(writeErr)
	}

	manifestPath := filepath.Join(dir, "conversation.json")
	beforeFailure, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	originalCreatedAt := conversation.CreatedAt
	conversation.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(conversation); err == nil {
		t.Fatal("Save accepted a manifest that cannot be JSON-encoded")
	}
	afterFailure, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeFailure, afterFailure) {
		t.Fatal("failed Save modified the previous manifest")
	}

	loaded, err := store.Load(conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CWD != stateB || !loaded.CreatedAt.Equal(originalCreatedAt) {
		t.Fatalf("failed Save changed persisted data: %+v", loaded)
	}
}

func TestCorruptManifestIsRejected(t *testing.T) {
	store := New(t.TempDir())
	conversation, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := store.dir(conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conversation.json"), []byte(`{"version":1`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(conversation.ID); err == nil {
		t.Fatal("Load accepted a truncated manifest")
	}
	if _, err := store.List(""); err == nil {
		t.Fatal("List silently accepted a truncated manifest")
	}
}

func TestCorruptJournalRejectsAppendWithoutModification(t *testing.T) {
	store := New(t.TempDir())
	conversation, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	event := model.Event{ID: "one", Source: model.Codex, SessionID: "codex-1", Kind: "message", Text: "one"}
	if _, err := store.Append(conversation.ID, []model.Event{event}); err != nil {
		t.Fatal(err)
	}
	dir, err := store.dir(conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(dir, "events.jsonl")
	file, err := os.OpenFile(journalPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"seq":2`); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	corrupted, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Entries(conversation.ID, 0); err == nil {
		t.Fatal("Entries accepted a truncated journal record")
	}
	event.ID = "two"
	if _, err := store.Append(conversation.ID, []model.Event{event}); err == nil {
		t.Fatal("Append accepted a truncated journal record")
	}
	after, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(corrupted, after) {
		t.Fatal("failed Append modified the corrupted journal")
	}
}

func TestLockSerializesCompetingProcesses(t *testing.T) {
	const childMode = "TERMINUS_LOCK_CONTENTION_CHILD"
	if os.Getenv(childMode) == "1" {
		store := New(os.Getenv("TERMINUS_LOCK_CONTENTION_ROOT"))
		if err := os.WriteFile(os.Getenv("TERMINUS_LOCK_CONTENTION_ATTEMPT"), []byte("attempting"), 0600); err != nil {
			t.Fatal(err)
		}
		err := store.WithLock(os.Getenv("TERMINUS_LOCK_CONTENTION_ID"), func() error {
			return os.WriteFile(os.Getenv("TERMINUS_LOCK_CONTENTION_ACQUIRED"), []byte("acquired"), 0600)
		})
		if err != nil {
			t.Fatal(err)
		}
		return
	}

	root := t.TempDir()
	store := New(root)
	conversation, err := store.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := store.dir(conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	attemptPath := filepath.Join(dir, "child-attempt")
	acquiredPath := filepath.Join(dir, "child-acquired")

	held := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLock := func() {
		releaseOnce.Do(func() { close(release) })
	}
	defer releaseLock()
	parentDone := make(chan error, 1)
	go func() {
		parentDone <- store.WithLock(conversation.ID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	command := exec.Command(os.Args[0], "-test.run=^TestLockSerializesCompetingProcesses$")
	var childOutput bytes.Buffer
	command.Stdout = &childOutput
	command.Stderr = &childOutput
	command.Env = append(os.Environ(),
		childMode+"=1",
		"TERMINUS_LOCK_CONTENTION_ROOT="+root,
		"TERMINUS_LOCK_CONTENTION_ID="+conversation.ID,
		"TERMINUS_LOCK_CONTENTION_ATTEMPT="+attemptPath,
		"TERMINUS_LOCK_CONTENTION_ACQUIRED="+acquiredPath,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = command.Process.Kill()
	}()
	waitForPath(t, attemptPath, 3*time.Second)
	for deadline := time.Now().Add(500 * time.Millisecond); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(acquiredPath); err == nil {
			t.Fatal("child acquired the lock while the parent held it")
		}
	}

	releaseLock()
	if err := <-parentDone; err != nil {
		t.Fatal(err)
	}
	childDone := make(chan error, 1)
	go func() { childDone <- command.Wait() }()
	select {
	case err := <-childDone:
		if err != nil {
			t.Fatalf("child failed after the lock was released: %v; output=%q", err, childOutput.String())
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("child did not acquire the lock after it was released")
	}
	if _, err := os.Stat(acquiredPath); err != nil {
		t.Fatalf("child never recorded lock acquisition: %v", err)
	}
}

func waitForPath(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
