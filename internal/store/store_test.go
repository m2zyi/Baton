package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"baton/internal/model"
)

func TestJournalAndPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	s := New(root)
	c, err := s.Create(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := model.Event{ID: "1", Source: model.Codex, SessionID: "s", Kind: "message", Text: "hello"}
	e2 := e
	e2.SessionID = "other"
	receipt := e
	receipt.Kind = "handoff"
	got, err := s.Append(c.ID, []model.Event{e, e, e2, receipt})
	if err != nil || len(got) != 2 {
		t.Fatalf("append %v %v", got, err)
	}
	got, err = s.Append(c.ID, []model.Event{e})
	if err != nil || len(got) != 0 {
		t.Fatalf("dedup %v %v", got, err)
	}
	got, err = s.Entries(c.ID, 1)
	if err != nil || len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("entries %v %v", got, err)
	}
	for _, x := range []struct {
		p    string
		mode os.FileMode
	}{{root, 0700}, {filepath.Join(root, c.ID), 0700}, {filepath.Join(root, c.ID, "conversation.json"), 0600}, {filepath.Join(root, c.ID, "events.jsonl"), 0600}} {
		st, err := os.Stat(x.p)
		if err != nil || st.Mode().Perm() != x.mode {
			t.Fatalf("mode %s %v %v", x.p, st, err)
		}
	}
	if _, err = s.Load("../escape"); err == nil {
		t.Fatal("unsafe ID accepted")
	}
	f, err := os.OpenFile(filepath.Join(root, c.ID, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{\"seq\":3}")
	f.Close()
	if _, err = s.Entries(c.ID, 100); err == nil {
		t.Fatal("truncated journal accepted")
	}
	if _, err = s.Append(c.ID, []model.Event{e}); err == nil {
		t.Fatal("append to corrupted journal")
	}
}
func TestLockReleasedOnProcessExit(t *testing.T) {
	if os.Getenv("BATON_LOCK_CHILD") == "1" {
		s := New(os.Getenv("BATON_TEST_ROOT"))
		err := s.WithLock(os.Getenv("BATON_TEST_ID"), func() error { os.Exit(0); return nil })
		if err != nil {
			os.Exit(2)
		}
		return
	}
	root := t.TempDir()
	s := New(root)
	c, err := s.Create(root)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockReleasedOnProcessExit$")
	cmd.Env = append(os.Environ(), "BATON_LOCK_CHILD=1", "BATON_TEST_ROOT="+root, "BATON_TEST_ID="+c.ID)
	if err = cmd.Run(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.WithLock(c.ID, func() error { return nil }) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lock remained held")
	}
}
