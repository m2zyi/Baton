// Package cli implements the Terminus command-line interface.
package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"terminus/internal/adapters"
	"terminus/internal/handoff"
	"terminus/internal/launcher"
	"terminus/internal/model"
	"terminus/internal/store"
)

const (
	Version = "0.1.0"
	budget  = 12 * 1024
)

var errQuitMenu = errors.New("quit terminus menu")

type Options struct {
	StoreRoot string
	Runner    launcher.Runner
}

type App struct {
	options Options
	store   *store.Store
	runner  launcher.Runner
	adapter func(ctx context.Context, agent model.Agent) (adapters.Adapter, error)
	in      *bufio.Reader
	out     io.Writer
	errOut  io.Writer
}

func newAdapter(ctx context.Context, agent model.Agent) (adapters.Adapter, error) {
	return adapters.New(agent)
}

func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	return RunWithOptions(ctx, args, in, out, errOut, Options{Runner: launcher.Process{}})
}

func RunWithOptions(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, options Options) int {
	if in == nil || out == nil || errOut == nil {
		fmt.Fprintln(errOut, "terminus: input and output streams are required")
		return 2
	}
	root := options.StoreRoot
	if root == "" {
		var err error
		root, err = store.DefaultRoot()
		if err != nil {
			fmt.Fprintf(errOut, "terminus: resolve data directory: %v\n", err)
			return 1
		}
	}
	runner := options.Runner
	if runner == nil {
		runner = launcher.Process{}
	}
	app := &App{options: options, store: store.New(root), runner: runner, adapter: newAdapter, in: bufio.NewReader(in), out: out, errOut: errOut}
	code, err := app.execute(ctx, args)
	if err != nil {
		if errors.Is(err, errQuitMenu) {
			return code
		}
		fmt.Fprintf(errOut, "terminus: %v\n", err)
	}
	return code
}

func (a *App) execute(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 {
		return a.menu(ctx)
	}
	switch args[0] {
	case "start":
		return a.start(ctx, args[1:])
	case "attach":
		return a.attach(ctx, args[1:])
	case "switch":
		return a.switchCommand(ctx, args[1:])
	case "resume":
		return a.resume(ctx, args[1:])
	case "list":
		return a.list(ctx, args[1:])
	case "context":
		return a.context(ctx, args[1:])
	case "doctor":
		return a.doctor(ctx, args[1:])
	case "help", "--help", "-h":
		writeHelp(a.out)
		return 0, nil
	case "version", "--version":
		fmt.Fprintf(a.out, "terminus %s\n", Version)
		return 0, nil
	default:
		return 2, fmt.Errorf("unknown command %q; run terminus help", args[0])
	}
}

func writeHelp(w io.Writer) {
	fmt.Fprint(w, `Terminus: continue one conversation between native agent CLIs.

Usage:
  terminus                             Open the session menu in the current directory
  terminus start --agent codex|claude [--prompt TEXT]
  terminus attach --agent codex|claude --session NATIVE_SESSION_ID
  terminus switch --to codex|claude [--conversation TERMINUS_ID] [--from NATIVE_SESSION_ID]
  terminus resume TERMINUS_ID [--agent codex|claude]
  terminus list [--json]
  terminus context TERMINUS_ID [--after N] [--limit N] [--json]
  terminus doctor
  terminus help | version

ID distinction:
  --session and --from are native CLI session IDs.
  TERMINUS_ID, --conversation, resume, and context use Terminus conversation IDs.
`)
}

type agentFlag struct{ value model.Agent }

func (f *agentFlag) Set(s string) error {
	if s != string(model.Codex) && s != string(model.Claude) {
		return fmt.Errorf("agent must be codex or claude")
	}
	f.value = model.Agent(s)
	return nil
}
func (f *agentFlag) String() string { return string(f.value) }

func (a *App) start(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	var agent agentFlag
	fs.Var(&agent, "agent", "native agent (codex or claude)")
	prompt := fs.String("prompt", "", "first user request")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if agent.value == "" {
		return 2, errors.New("start requires --agent codex|claude")
	}
	if fs.NArg() != 0 {
		return 2, errors.New("start takes no positional arguments")
	}
	c, err := a.store.Create(a.cwd())
	if err != nil {
		return 1, err
	}
	promptText := *prompt
	if promptText == "" {
		promptText, err = a.promptLine("First user request")
		if err != nil {
			return 1, err
		}
	}
	if strings.TrimSpace(promptText) == "" {
		return 2, errors.New("first user request must not be empty")
	}
	firstEvent := model.Event{
		ID:        "terminus:first-request",
		Source:    agent.value,
		SessionID: "terminus:" + c.ID,
		Kind:      "message",
		Role:      "user",
		Text:      promptText,
		Timestamp: time.Now().UTC(),
	}
	if _, err := a.store.Append(c.ID, []model.Event{firstEvent}); err != nil {
		return 1, err
	}
	entries, err := a.store.Entries(c.ID, 0)
	if err != nil {
		return 1, err
	}
	pending, err := a.preparePending(c, entries, agent.value)
	if err != nil {
		return 1, err
	}
	c.Pending = pending
	c.ActiveAgent = agent.value
	c.UpdatedAt = time.Now().UTC()
	if err := a.store.Save(c); err != nil {
		return 1, err
	}
	var run launcher.NativeRun
	if agent.value == model.Claude {
		uuid, err := randomUUID()
		if err != nil {
			return 1, err
		}
		run, err = launcher.NewSessionRun(agent.value, uuid, promptText, c.CWD, a.options.customBinary(agent.value))
	} else {
		run, err = launcher.NewSessionRun(agent.value, "", promptText, c.CWD, a.options.customBinary(agent.value))
	}
	if err != nil {
		return 2, err
	}
	if err := a.runner.Run(ctx, run, a.in, a.out, a.errOut); err != nil {
		return 1, fmt.Errorf("run native %s: %w", agent.value, err)
	}
	code, err := a.settle(ctx, c.ID)
	if err != nil {
		return code, err
	}
	if err := a.importSessionUpdates(ctx, c); err != nil {
		return 1, err
	}
	return 0, nil
}

func (a *App) attach(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	var agent agentFlag
	fs.Var(&agent, "agent", "native agent (codex or claude)")
	sessionID := fs.String("session", "", "native session ID")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if agent.value == "" || *sessionID == "" || fs.NArg() != 0 {
		return 2, errors.New("attach requires --agent and native --session ID")
	}
	c, err := a.store.Create(a.cwd())
	if err != nil {
		return 1, err
	}
	c.Bindings[agent.value] = model.Binding{Session: model.Session{ID: *sessionID, Agent: agent.value, CWD: c.CWD}}
	if err := a.store.Save(c); err != nil {
		return 1, err
	}
	if err := a.importSession(ctx, c, agent.value, *sessionID); err != nil {
		return 1, err
	}
	loaded, err := a.store.Load(c.ID)
	if err != nil {
		return 1, err
	}
	if binding, ok := loaded.Bindings[agent.value]; !ok || binding.Session.ID != *sessionID {
		return 1, errors.New("attach failed to persist native session binding")
	}
	return a.menuFor(ctx, c.ID)
}

func (a *App) switchCommand(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("switch", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	var target agentFlag
	fs.Var(&target, "to", "target native agent")
	conversationID := fs.String("conversation", "", "Terminus conversation ID")
	fromSession := fs.String("from", "", "native source session ID")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if target.value == "" {
		return 2, errors.New("switch requires --to codex|claude")
	}
	if fs.NArg() != 0 {
		return 2, errors.New("switch takes no positional arguments")
	}
	id := *conversationID
	if id == "" {
		conversations, err := a.store.List(a.cwd())
		if err != nil {
			return 1, err
		}
		if len(conversations) == 0 {
			return 1, errors.New("no managed conversation in this directory; use attach or start")
		}
		choice, err := a.selectConversation(conversations)
		if err != nil {
			return 1, err
		}
		id = choice.ID
	}
	if *fromSession != "" {
		c, err := a.store.Load(id)
		if err != nil {
			return 1, err
		}
		for agent := range c.Bindings {
			if agent != target.value {
				if err := a.importSession(ctx, c, agent, *fromSession); err != nil {
					return 1, err
				}
				break
			}
		}
	}
	return a.handoff(ctx, id, target.value)
}

func (a *App) resume(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	var agent agentFlag
	fs.Var(&agent, "agent", "native agent (codex or claude)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, errors.New("resume requires one Terminus conversation ID")
	}
	c, err := a.store.Load(fs.Arg(0))
	if err != nil {
		return 1, err
	}
	selected := agent.value
	if selected == "" {
		selected = c.ActiveAgent
	}
	binding, ok := c.Bindings[selected]
	if selected == "" || !ok {
		return 1, fmt.Errorf("conversation has no bound %s session", selected)
	}
	for boundAgent := range c.Bindings {
		if boundAgent != selected {
			if err := a.discoverImport(ctx, c, boundAgent); err != nil {
				return 1, err
			}
		}
	}
	run, err := launcher.ResumeRun(selected, binding.Session.ID, "", c.CWD, a.options.customBinary(selected))
	if err != nil {
		return 2, err
	}
	if err := a.runner.Run(ctx, run, a.in, a.out, a.errOut); err != nil {
		return 1, fmt.Errorf("run native %s: %w", selected, err)
	}
	return a.settle(ctx, c.ID)
}

func (a *App) list(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, errors.New("list takes no positional arguments")
	}
	conversations, err := a.store.List(a.cwd())
	if err != nil {
		return 1, err
	}
	if *jsonOut {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(conversations)
	}
	for _, c := range conversations {
		active := string(c.ActiveAgent)
		if active == "" {
			active = "-"
		}
		fmt.Fprintf(a.out, "%s  agent=%s  cwd=%s  updated=%s\n", c.ID, active, c.CWD, c.UpdatedAt.Format(time.RFC3339))
	}
	return 0, nil
}

func (a *App) context(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	after := fs.Int64("after", 0, "return events with sequence greater than N")
	limit := fs.Int64("limit", 0, "return at most N events (0 means no limit)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 1 {
		return 2, errors.New("context requires one Terminus conversation ID")
	}
	if *after < 0 || *limit < 0 {
		return 2, errors.New("--after and --limit must not be negative")
	}
	entries, err := a.store.Entries(fs.Arg(0), *after)
	if err != nil {
		return 1, err
	}
	if *limit > 0 && int64(len(entries)) > *limit {
		entries = entries[:*limit]
	}
	if *jsonOut {
		enc := json.NewEncoder(a.out)
		enc.SetIndent("", "  ")
		return 0, enc.Encode(entries)
	}
	for _, entry := range entries {
		fmt.Fprintf(a.out, "[%d] %s/%s %s %s\n%s\n\n", entry.Seq, entry.Event.Source, entry.Event.SessionID, entry.Event.Kind, entry.Event.Role, entry.Event.Text)
	}
	return 0, nil
}

func (a *App) doctor(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2, errors.New("doctor takes no arguments")
	}
	root := a.storeRoot()
	fmt.Fprintf(a.out, "data root: %s\n", root)
	fmt.Fprintf(a.out, "codex binary: %s\n", binaryName(model.Codex))
	fmt.Fprintf(a.out, "claude binary: %s\n", binaryName(model.Claude))
	fmt.Fprintf(a.out, "store: writable\n")
	return 0, nil
}

func (a *App) menu(ctx context.Context) (int, error) {
	conversations, err := a.store.List(a.cwd())
	if err != nil {
		return 1, err
	}
	choice, err := a.selectConversation(conversations)
	if err != nil {
		return 1, err
	}
	return a.menuFor(ctx, choice.ID)
}

func (a *App) menuFor(ctx context.Context, id string) (int, error) {
	for {
		c, err := a.store.Load(id)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(a.out, "\nTerminus conversation %s (%s)\n", c.ID, a.cwd())
		fmt.Fprintln(a.out, "1) resume active agent")
		fmt.Fprintln(a.out, "2) switch to codex")
		fmt.Fprintln(a.out, "3) switch to claude")
		fmt.Fprintln(a.out, "4) show context")
		fmt.Fprintln(a.out, "q) quit")
		choice, err := a.promptLine("Select")
		if err != nil {
			return 1, err
		}
		switch choice {
		case "1":
			code, err := a.resume(ctx, []string{id})
			if err != nil {
				return code, err
			}
		case "2", "3":
			target := model.Codex
			if choice == "3" {
				target = model.Claude
			}
			code, err := a.handoff(ctx, id, target)
			if err != nil {
				return code, err
			}
		case "4":
			code, err := a.context(ctx, []string{id})
			if err != nil {
				return code, err
			}
		case "q", "Q", "":
			return 0, errQuitMenu
		default:
			fmt.Fprintln(a.errOut, "invalid choice")
		}
	}
}

func (a *App) selectConversation(conversations []model.Conversation) (model.Conversation, error) {
	if len(conversations) == 1 {
		return conversations[0], nil
	}
	fmt.Fprintln(a.out, "Terminus conversations in this directory:")
	for i, c := range conversations {
		fmt.Fprintf(a.out, "%d) %s  agent=%s  updated=%s\n", i+1, c.ID, c.ActiveAgent, c.UpdatedAt.Format(time.RFC3339))
	}
	line, err := a.promptLine("Select conversation")
	if err != nil {
		return model.Conversation{}, err
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(conversations) {
		return model.Conversation{}, errors.New("invalid conversation choice")
	}
	return conversations[n-1], nil
}

func (a *App) handoff(ctx context.Context, id string, target model.Agent) (int, error) {
	var code int
	err := a.store.WithLock(id, func() error {
		c, err := a.store.Load(id)
		if err != nil {
			return err
		}
		if source := c.ActiveAgent; source != "" && source != target {
			if err := a.discoverImport(ctx, c, source); err != nil {
				return err
			}
		}
		entries, err := a.store.Entries(id, 0)
		if err != nil {
			return err
		}
		pending, err := a.preparePending(c, entries, target)
		if err != nil {
			return err
		}
		c.Pending = pending
		c.ActiveAgent = target
		c.UpdatedAt = time.Now().UTC()
		if err := a.store.Save(c); err != nil {
			return err
		}
		var run launcher.NativeRun
		if binding, ok := c.Bindings[target]; ok {
			run, err = launcher.ResumeRun(target, binding.Session.ID, handoff.Prompt(pending.Packet, a.contextCommand(id)), c.CWD, a.options.customBinary(target))
		} else if target == model.Claude {
			var uuid string
			uuid, err = randomUUID()
			if err != nil {
				return err
			}
			run, err = launcher.NewSessionRun(target, uuid, handoff.Prompt(pending.Packet, a.contextCommand(id)), c.CWD, a.options.customBinary(target))
		} else {
			run, err = launcher.NewSessionRun(target, "", handoff.Prompt(pending.Packet, a.contextCommand(id)), c.CWD, a.options.customBinary(target))
		}
		if err != nil {
			return err
		}
		if err = a.runner.Run(ctx, run, a.in, a.out, a.errOut); err != nil {
			code = 1
			return fmt.Errorf("run native %s: %w", target, err)
		}
		return nil
	})
	if err != nil {
		if code == 0 {
			code = 1
		}
		return code, err
	}
	code, err = a.settle(ctx, id)
	if err != nil {
		return code, err
	}
	c, err := a.store.Load(id)
	if err != nil {
		return 1, err
	}
	if err := a.importSessionUpdates(ctx, c); err != nil {
		return 1, err
	}
	return 0, nil
}

func (a *App) preparePending(c *model.Conversation, entries []model.Entry, target model.Agent) (*model.Pending, error) {
	if c.Pending != nil {
		if c.Pending.Packet.Target != target {
			return nil, fmt.Errorf("pending packet targets %s; settle it before switching to %s", c.Pending.Packet.Target, target)
		}
		return c.Pending, nil
	}
	packet, err := handoff.Build(c, entries, target, budget)
	if err != nil {
		return nil, err
	}
	return &model.Pending{Packet: packet, StartedAt: time.Now().UTC()}, nil
}

var errAlreadyDelivered = errors.New("pending packet already has a native receipt")

func (a *App) settle(ctx context.Context, id string) (int, error) {
	var code int
	err := a.store.WithLock(id, func() error {
		c, err := a.store.Load(id)
		if err != nil {
			return err
		}
		if c.Pending == nil {
			for agent := range c.Bindings {
				if agent != c.ActiveAgent {
					if err := a.discoverImport(ctx, c, agent); err != nil {
						return err
					}
				}
			}
			return nil
		}
		receipts, err := a.receiptEntries(ctx, id, *c.Pending)
		if err != nil {
			return err
		}
		if len(receipts) == 0 {
			code = 1
			return errors.New("native transcript does not yet contain the pending handoff receipt")
		}
		if c.ActiveAgent != "" {
			if err := a.discoverImport(ctx, c, c.ActiveAgent); err != nil {
				code = 1
				return err
			}
			updated, err := a.store.Load(id)
			if err != nil {
				code = 1
				return err
			}
			c = updated
		}
		binding, ok := c.Bindings[c.Pending.Packet.Target]
		if !ok {
			if len(receipts) != 1 {
				return fmt.Errorf("multiple candidate first-launch sessions contain marker %s", c.Pending.Packet.ID)
			}
			binding = model.Binding{Session: receipts[0], DeliveredThrough: c.Pending.Packet.Through}
		} else {
			found := false
			for _, receipt := range receipts {
				if receipt.ID == binding.Session.ID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("handoff receipt is not in bound %s session %s", binding.Session.Agent, binding.Session.ID)
			}
			binding.DeliveredThrough = c.Pending.Packet.Through
		}
		c.Bindings[binding.Session.Agent] = binding
		c.Pending = nil
		c.UpdatedAt = time.Now().UTC()
		return a.store.Save(c)
	})
	if err != nil {
		if code == 0 {
			code = 1
		}
		return code, err
	}
	return 0, nil
}

func (a *App) importSessionUpdates(ctx context.Context, c *model.Conversation) error {
	for agent := range c.Bindings {
		if err := a.discoverImport(ctx, c, agent); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) receiptEntries(ctx context.Context, id string, pending model.Pending) ([]model.Session, error) {
	adapter, err := a.adapter(ctx, pending.Packet.Target)
	if err != nil {
		return nil, err
	}
	c, err := a.store.Load(id)
	if err != nil {
		return nil, err
	}
	sessions, err := adapter.Discover(ctx, c.CWD)
	if err != nil {
		return nil, err
	}
	matches := []model.Session{}
	for _, session := range sessions {
		events, err := adapter.Read(ctx, session)
		if err != nil {
			continue
		}
		for _, event := range events {
			if event.Kind == "handoff" && handoff.IsInjected(event.Text) && strings.Contains(event.Text, handoff.Marker(pending.Packet.ID)) {
				matches = append(matches, session)
				break
			}
		}
	}
	return matches, nil
}

func (a *App) importSession(ctx context.Context, c *model.Conversation, agent model.Agent, sessionID string) error {
	adapter, err := a.adapter(ctx, agent)
	if err != nil {
		return err
	}
	sessions, err := adapter.Discover(ctx, c.CWD)
	if err != nil {
		return err
	}
	matches := []model.Session{}
	for _, session := range sessions {
		if session.ID == sessionID {
			matches = append(matches, session)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("expected exactly one native %s session %s in %s, found %d", agent, sessionID, c.CWD, len(matches))
	}
	if agent == model.Codex {
		if binding, ok := c.Bindings[agent]; ok {
			if binding.Session.Path != "" && binding.Session.Path != matches[0].Path {
				return fmt.Errorf("codex session %s binding uses %s, but discovery returned %s", sessionID, binding.Session.Path, matches[0].Path)
			}
		}
	}
	events, err := adapter.Read(ctx, matches[0])
	if err != nil {
		return err
	}
	if _, err = a.store.Append(c.ID, events); err != nil {
		return err
	}
	c.UpdatedAt = time.Now().UTC()
	if err = a.store.Save(c); err != nil {
		return err
	}
	updated, err := a.store.Load(c.ID)
	if err != nil {
		return err
	}
	*c = *updated
	if binding, ok := updated.Bindings[agent]; ok && agent == model.Codex && binding.Session.Path == "" {
		binding.Session.Path = matches[0].Path
		updated.Bindings[agent] = binding
		*c = *updated
		return a.store.Save(updated)
	}
	return nil
}

func (a *App) discoverImport(ctx context.Context, c *model.Conversation, agent model.Agent) error {
	binding, ok := c.Bindings[agent]
	if !ok {
		return nil
	}
	return a.importSession(ctx, c, agent, binding.Session.ID)
}

func (a *App) promptLine(label string) (string, error) {
	fmt.Fprintf(a.out, "%s: ", label)
	line, err := a.in.ReadString('\n')
	if err != nil && !(err == io.EOF && line != "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (a *App) cwd() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func (a *App) storeRoot() string { return a.options.StoreRoot }

func (a *App) contextCommand(id string) string { return "terminus context " + id }

func (o Options) customBinary(agent model.Agent) string {
	if agent == model.Codex {
		return os.Getenv("TERMINUS_CODEX_BIN")
	}
	return os.Getenv("TERMINUS_CLAUDE_BIN")
}

func binaryName(agent model.Agent) string {
	if p := os.Getenv("TERMINUS_CODEX_BIN"); agent == model.Codex && p != "" {
		return p
	}
	if p := os.Getenv("TERMINUS_CLAUDE_BIN"); agent == model.Claude && p != "" {
		return p
	}
	return string(agent)
}

func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hex := fmt.Sprintf("%x", b[:])
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32], nil
}
