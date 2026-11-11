// Package store persists conversations and their append-only journals.
package store

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"terminus/internal/model"
)

type Store struct{ root string }

func DefaultRoot() (string, error) {
	if p := os.Getenv("TERMINUS_HOME"); p != "" {
		return p, nil
	}
	if p := os.Getenv("XDG_DATA_HOME"); p != "" {
		return filepath.Join(p, "terminus"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, ".local", "share", "terminus"), nil
}
func New(root string) *Store { return &Store{root: root} }
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func (s *Store) dir(id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid conversation ID %q", id)
	}
	p := filepath.Join(s.root, id)
	st, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("invalid conversation directory: %s", p)
	}
	return p, nil
}
func (s *Store) Create(cwd string) (*model.Conversation, error) {
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(s.root, 0700); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	var id string
	for {
		b := make([]byte, 16)
		if _, err = rand.Read(b); err != nil {
			return nil, err
		}
		id = hex.EncodeToString(b)
		err = os.Mkdir(filepath.Join(s.root, id), 0700)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	now := time.Now().UTC()
	c := &model.Conversation{Version: 1, ID: id, CWD: abs, CreatedAt: now, UpdatedAt: now, Bindings: map[model.Agent]model.Binding{}}
	if err = s.Save(c); err != nil {
		os.RemoveAll(filepath.Join(s.root, id))
		return nil, err
	}
	return c, nil
}
func decodeFile(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	if err = d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("extra JSON value")
		}
		return err
	}
	return nil
}
func (s *Store) Load(id string) (*model.Conversation, error) {
	d, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	var c model.Conversation
	if err = decodeFile(filepath.Join(d, "conversation.json"), &c); err != nil {
		return nil, err
	}
	if c.ID != id || c.Version != 1 {
		return nil, errors.New("invalid conversation manifest")
	}
	return &c, nil
}
func (s *Store) Save(c *model.Conversation) error {
	if c == nil {
		return errors.New("nil conversation")
	}
	d, err := s.dir(c.ID)
	if err != nil {
		return err
	}
	if c.Version != 1 {
		return errors.New("unsupported conversation version")
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.CreateTemp(d, ".manifest-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(d, "conversation.json")); err != nil {
		return err
	}
	return syncDir(d)
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) List(cwd string) ([]model.Conversation, error) {
	ds, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return []model.Conversation{}, nil
	}
	if err != nil {
		return nil, err
	}
	if cwd != "" {
		cwd, err = filepath.Abs(cwd)
		if err != nil {
			return nil, err
		}
	}
	out := []model.Conversation{}
	for _, d := range ds {
		if !validID(d.Name()) {
			continue
		}
		c, e := s.Load(d.Name())
		if e != nil {
			return nil, e
		}
		if cwd == "" || c.CWD == cwd {
			out = append(out, *c)
		}
	}
	return out, nil
}
func key(e model.Event) string {
	b, _ := json.Marshal([]string{string(e.Source), e.SessionID, e.ID})
	return string(b)
}
func journal(d string) ([]model.Entry, error) {
	f, err := os.Open(filepath.Join(d, "events.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return []model.Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	out := []model.Entry{}
	seen := map[string]bool{}
	for {
		line, e := r.ReadBytes('\n')
		if e == io.EOF && len(line) == 0 {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("incomplete journal record: %w", e)
		}
		var entry model.Entry
		if err = json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("invalid journal record: %w", err)
		}
		ev := entry.Event
		if entry.Seq != int64(len(out)+1) || ev.ID == "" || ev.Source == "" || ev.SessionID == "" || ev.Kind == "handoff" || seen[key(ev)] {
			return nil, errors.New("invalid journal sequence or event identity")
		}
		seen[key(ev)] = true
		out = append(out, entry)
	}
	return out, nil
}
func (s *Store) Entries(id string, after int64) ([]model.Entry, error) {
	d, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	entries, err := journal(d)
	if err != nil {
		return nil, err
	}
	out := []model.Entry{}
	for _, e := range entries {
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *Store) Append(id string, events []model.Event) ([]model.Entry, error) {
	d, err := s.dir(id)
	if err != nil {
		return nil, err
	}
	existing, err := journal(d)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range existing {
		seen[key(e.Event)] = true
	}
	added := []model.Entry{}
	var b strings.Builder
	for _, e := range events {
		if e.Kind == "handoff" {
			continue
		}
		if e.ID == "" || e.Source == "" || e.SessionID == "" {
			return nil, errors.New("event requires source, session ID and event ID")
		}
		k := key(e)
		if seen[k] {
			continue
		}
		seen[k] = true
		entry := model.Entry{Seq: int64(len(existing) + len(added) + 1), Event: e}
		raw, er := json.Marshal(entry)
		if er != nil {
			return nil, er
		}
		b.Write(raw)
		b.WriteByte('\n')
		added = append(added, entry)
	}
	if len(added) == 0 {
		return added, nil
	}
	f, err := os.OpenFile(filepath.Join(d, "events.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Chmod(0600); err == nil {
		_, err = io.WriteString(f, b.String())
	}
	if err == nil {
		err = f.Sync()
	}
	if e := f.Close(); err == nil {
		err = e
	}
	if err != nil {
		return nil, err
	}
	if err = syncDir(d); err != nil {
		return nil, err
	}
	return added, nil
}
func (s *Store) WithLock(id string, fn func() error) error {
	d, err := s.dir(id)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(d, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
