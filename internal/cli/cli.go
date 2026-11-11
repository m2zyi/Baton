// Package cli implements the Terminus command-line interface.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
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

	firstRequestPrefix = "terminus:first-request:"
)

var errNoReceipt = errors.New("native transcript does not yet contain the pending handoff receipt")

type Options struct {
	StoreRoot string
	Runner    launcher.Runner
}

type resolvedReader interface {
	ReadResolved(ctx context.Context, session model.Session) (model.Session, []model.Event, error)
}

type App struct {
	options Options
	store   *store.Store
	runner  launcher.Runner
	adapter func(ctx context.Context, agent model.Agent) (adapters.Adapter, error)
	in      io.Reader
	out     io.Writer
	errOut  io.Writer
}

func newAdapter(_ context.Context, agent model.Agent) (adapters.Adapter, error) {
	return adapters.New(agent)
}

func Run(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) int {
	return RunWithOptions(ctx, args, in, out, errOut, Options{Runner: launcher.Process{}})
}

func RunWithOptions(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer, options Options) int {
	if in == nil || out == nil || errOut == nil {
		if errOut != nil {
			fmt.Fprintln(errOut, "terminus: input and output streams are required")
		}
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
	options.StoreRoot = root
	runner := options.Runner
	if runner == nil {
		runner = launcher.Process{}
	}
	app := &App{
		options: options,
		store:   store.New(root),
		runner:  runner,
		adapter: newAdapter,
		in:      in,
		out:     out,
		errOut:  errOut,
	}
	code, err := app.execute(ctx, args)
	if err != nil {
		fmt.Fprintf(errOut, "terminus: %v\n", err)
		if code == 0 {
			code = 1
		}
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

The menu also creates and attaches conversations. A conversation without any
binding can still be opened, inspected, and handed off later.

ID distinction:
  --session and --from are native CLI session IDs.
  TERMINUS_ID, --conversation, resume, and context use Terminus conversation IDs.
  Terminus conversation IDs are local 32-character lowercase hex values.

Options may follow the conversation ID for resume and context, for example:
  terminus context 0123abcd... --after 20 --limit 10 --json
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
	first := strings.TrimSpace(*prompt)
	if first == "" {
		var err error
		first, err = a.promptLine("First user request")
		if err != nil {
			return 1, err
		}
		first = strings.TrimSpace(first)
	}
	if first == "" {
		return 2, errors.New("first user request must not be empty")
	}
	id, code, err := a.startAction(ctx, agent.value, first)
	if err != nil {
		return code, err
	}
	return a.menuFor(ctx, id)
}

func (a *App) startAction(ctx context.Context, agent model.Agent, first string) (string, int, error) {
	c, err := a.store.Create(a.cwd())
	if err != nil {
		return "", 1, err
	}
	now := time.Now().UTC()
	provisional := model.Event{
		ID:        firstRequestPrefix + c.ID,
		Source:    agent,
		SessionID: "terminus:" + c.ID,
		Kind:      "message",
		Role:      "user",
		Text:      first,
		Timestamp: now,
	}
	if _, err := a.store.Append(c.ID, []model.Event{provisional}); err != nil {
		return "", 1, err
	}
	entries, err := a.store.Entries(c.ID, 0)
	if err != nil {
		return "", 1, err
	}
	packet, err := handoff.Build(c, entries, agent, budget)
	if err != nil {
		return "", 1, err
	}
	pending := &model.Pending{Packet: packet, StartedAt: now}
	if agent == model.Claude {
		uuid, err := randomUUID()
		if err != nil {
			return "", 1, err
		}
		pending.SessionID = uuid
	}
	c.Pending = pending
	c.ActiveAgent = agent
	c.UpdatedAt = now
	if err := a.store.Save(c); err != nil {
		return "", 1, err
	}
	runPrompt := handoff.Prompt(packet, a.contextCommand(c.ID)) + "\n\n" + first
	code, err := a.runAndSettle(ctx, c, runPrompt)
	return c.ID, code, err
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
	if agent.value == "" || strings.TrimSpace(*sessionID) == "" || fs.NArg() != 0 {
		return 2, errors.New("attach requires --agent and native --session ID")
	}
	id, code, err := a.attachAction(ctx, agent.value, strings.TrimSpace(*sessionID))
	if err != nil {
		return code, err
	}
	return a.menuFor(ctx, id)
}

func (a *App) attachAction(ctx context.Context, agent model.Agent, sessionID string) (string, int, error) {
	cwd := a.cwd()
	session, err := a.discoverExact(ctx, agent, cwd, sessionID)
	if err != nil {
		return "", 1, err
	}
	c, err := a.store.Create(cwd)
	if err != nil {
		return "", 1, err
	}
	c.Bindings[agent] = model.Binding{Session: session}
	c.UpdatedAt = time.Now().UTC()
	if err := a.store.Save(c); err != nil {
		return "", 1, err
	}
	resolved, err := a.importSessionDirect(ctx, c, session, nil)
	if err != nil {
		return "", 1, err
	}
	c.Bindings[agent] = model.Binding{Session: resolved}
	c.ActiveAgent = agent
	c.UpdatedAt = time.Now().UTC()
	if err := a.store.Save(c); err != nil {
		return "", 1, err
	}
	return c.ID, 0, nil
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
	id := strings.TrimSpace(*conversationID)
	if id == "" {
		conversations, err := a.store.List(a.cwd())
		if err != nil {
			return 1, err
		}
		switch len(conversations) {
		case 0:
			return 1, errors.New("no managed conversation in this directory; use attach or start")
		case 1:
			id = conversations[0].ID
		default:
			choice, err := a.selectConversation(conversations)
			if err != nil {
				return 1, err
			}
			id = choice.ID
		}
	}
	if _, err := a.store.Load(id); err != nil {
		return 1, err
	}
	code, err := a.handoffAction(ctx, id, target.value, strings.TrimSpace(*fromSession))
	if err != nil {
		return code, err
	}
	return a.menuFor(ctx, id)
}

func (a *App) handoffAction(ctx context.Context, id string, target model.Agent, fromSession string) (int, error) {
	c, err := a.store.Load(id)
	if err != nil {
		return 1, err
	}
	if c.Pending != nil {
		if c.Pending.Packet.Target != target {
			return 1, fmt.Errorf("pending packet targets %s; settle it before switching to %s", c.Pending.Packet.Target, target)
		}
		recovered, err := a.recoverPending(ctx, id)
		if err != nil {
			return 1, err
		}
		if recovered {
			return 0, nil
		}
		c, err = a.store.Load(id)
		if err != nil {
			return 1, err
		}
		prompt, err := a.pendingPrompt(ctx, c)
		if err != nil {
			return 1, err
		}
		return a.runAndSettle(ctx, c, prompt)
	}

	if c.ActiveAgent != "" && c.ActiveAgent != target {
		if err := a.importBinding(ctx, c, c.ActiveAgent); err != nil {
			return 1, err
		}
	}
	if fromSession != "" {
		if err := a.importFromID(ctx, c, fromSession); err != nil {
			return 1, err
		}
	} else if c.ActiveAgent == "" {
		for _, agent := range []model.Agent{model.Codex, model.Claude} {
			if err := a.importBinding(ctx, c, agent); err != nil {
				return 1, err
			}
		}
	}
	c, err = a.store.Load(id)
	if err != nil {
		return 1, err
	}
	entries, err := a.store.Entries(id, 0)
	if err != nil {
		return 1, err
	}
	packet, err := handoff.Build(c, entries, target, budget)
	if err != nil {
		return 1, err
	}
	if binding, ok := c.Bindings[target]; ok && packet.Body == "" {
		run, err := launcher.ResumeRun(target, binding.Session.ID, "", c.CWD, a.options.customBinary(target))
		if err != nil {
			return 2, err
		}
		runErr := a.runner.Run(ctx, run, a.in, a.out, a.errOut)
		importErr := a.importUpdates(ctx, id)
		if runErr != nil && importErr != nil {
			return 1, fmt.Errorf("run native %s: %w; import updates: %v", target, runErr, importErr)
		}
		if runErr != nil {
			return 1, fmt.Errorf("run native %s: %w", target, runErr)
		}
		if importErr != nil {
			return 1, importErr
		}
		return 0, nil
	}
	pending := &model.Pending{Packet: packet, StartedAt: time.Now().UTC()}
	if _, bound := c.Bindings[target]; !bound && target == model.Claude {
		uuid, err := randomUUID()
		if err != nil {
			return 1, err
		}
		pending.SessionID = uuid
	}
	c.Pending = pending
	c.UpdatedAt = time.Now().UTC()
	if err := a.store.Save(c); err != nil {
		return 1, err
	}
	return a.runAndSettle(ctx, c, handoff.Prompt(packet, a.contextCommand(id)))
}

func (a *App) resume(ctx context.Context, args []string) (int, error) {
	id, rest, err := splitPositional(args, map[string]bool{"--agent": true})
	if err != nil {
		return 2, err
	}
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	var agent agentFlag
	fs.Var(&agent, "agent", "native agent (codex or claude)")
	if err := fs.Parse(rest); err != nil {
		return 2, err
	}
	if id == "" {
		return 2, errors.New("resume requires one Terminus conversation ID")
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	code, err := a.resumeAction(ctx, id, agent.value)
	if err != nil {
		return code, err
	}
	return a.menuFor(ctx, id)
}

func (a *App) resumeAction(ctx context.Context, id string, selected model.Agent) (int, error) {
	c, err := a.store.Load(id)
	if err != nil {
		return 1, err
	}
	if selected == "" {
		selected = c.ActiveAgent
	}
	if selected == "" {
		return 1, errors.New("conversation has no active agent; pass --agent codex|claude")
	}
	if c.Pending != nil && c.Pending.Packet.Target != selected {
		return 1, fmt.Errorf("pending packet targets %s; resume or switch to that agent first", c.Pending.Packet.Target)
	}
	if c.Pending != nil {
		recovered, err := a.recoverPending(ctx, id)
		if err != nil {
			return 1, err
		}
		if recovered {
			return 0, nil
		}
		c, err = a.store.Load(id)
		if err != nil {
			return 1, err
		}
		prompt, err := a.pendingPrompt(ctx, c)
		if err != nil {
			return 1, err
		}
		return a.runAndSettle(ctx, c, prompt)
	}
	binding, ok := c.Bindings[selected]
	if !ok {
		return 1, fmt.Errorf("conversation has no bound %s session", selected)
	}
	if err := a.importBinding(ctx, c, selected); err != nil {
		return 1, err
	}
	run, err := launcher.ResumeRun(selected, binding.Session.ID, "", c.CWD, a.options.customBinary(selected))
	if err != nil {
		return 2, err
	}
	runErr := a.runner.Run(ctx, run, a.in, a.out, a.errOut)
	importErr := a.importUpdates(ctx, id)
	if runErr != nil && importErr != nil {
		return 1, fmt.Errorf("run native %s: %w; import updates: %v", selected, runErr, importErr)
	}
	if runErr != nil {
		return 1, fmt.Errorf("run native %s: %w", selected, runErr)
	}
	if importErr != nil {
		return 1, importErr
	}
	return 0, nil
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
	if err := ctx.Err(); err != nil {
		return 1, err
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
	if len(conversations) == 0 {
		fmt.Fprintln(a.out, "no conversations in this directory")
		return 0, nil
	}
	for _, c := range conversations {
		active := string(c.ActiveAgent)
		if active == "" {
			active = "-"
		}
		pending := ""
		if c.Pending != nil {
			pending = " pending=" + string(c.Pending.Packet.Target)
		}
		fmt.Fprintf(a.out, "%s  agent=%s%s  cwd=%s  updated=%s\n", c.ID, active, pending, c.CWD, c.UpdatedAt.Format(time.RFC3339))
	}
	return 0, nil
}

func (a *App) context(ctx context.Context, args []string) (int, error) {
	id, rest, err := splitPositional(args, map[string]bool{"--after": true, "--limit": true})
	if err != nil {
		return 2, err
	}
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	fs.SetOutput(a.errOut)
	after := fs.Int64("after", 0, "return events with sequence greater than N")
	limit := fs.Int64("limit", 0, "return at most N events (0 means no limit)")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(rest); err != nil {
		return 2, err
	}
	if id == "" {
		return 2, errors.New("context requires one Terminus conversation ID")
	}
	if fs.NArg() != 0 {
		return 2, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *after < 0 || *limit < 0 {
		return 2, errors.New("--after and --limit must not be negative")
	}
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	entries, err := a.store.Entries(id, *after)
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
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if fs.NArg() != 0 {
		return 2, errors.New("doctor takes no arguments")
	}
	if err := ctx.Err(); err != nil {
		return 1, err
	}
	root := a.storeRoot()
	fmt.Fprintf(a.out, "data root: %s\n", root)
	healthy := true
	storeStatus, storeErr := checkStore(root)
	if storeErr != nil {
		healthy = false
		fmt.Fprintf(a.out, "store: failed: %v\n", storeErr)
	} else {
		fmt.Fprintf(a.out, "store: ok (%s)\n", storeStatus)
	}
	for _, agent := range []model.Agent{model.Codex, model.Claude} {
		candidate := a.options.customBinary(agent)
		if candidate == "" {
			candidate = string(agent)
		}
		path, err := exec.LookPath(candidate)
		if err != nil {
			healthy = false
			fmt.Fprintf(a.out, "%s executable: %s (not found: %v)\n", agent, candidate, err)
			continue
		}
		fmt.Fprintf(a.out, "%s executable: %s (ok)\n", agent, path)
	}
	if !healthy {
		return 1, nil
	}
	return 0, nil
}

func checkStore(root string) (string, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", root)
	}
	dirMode := info.Mode().Perm()
	if dirMode&0077 != 0 {
		// This is Terminus's own data root, and store.Create applies the same
		// tightening. Repair it instead of accepting a world-readable journal.
		if err := os.Chmod(root, 0700); err != nil {
			return "", fmt.Errorf("directory mode %04o is more open than 0700: %w", dirMode, err)
		}
		info, err = os.Stat(root)
		if err != nil {
			return "", err
		}
		dirMode = info.Mode().Perm()
		if dirMode&0077 != 0 {
			return "", fmt.Errorf("directory mode %04o is more open than 0700", dirMode)
		}
	}
	file, err := os.CreateTemp(root, ".doctor-*")
	if err != nil {
		return "", err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return "", err
	}
	if _, err := file.WriteString("terminus doctor\n"); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return "", err
	}
	fileInfo, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return "", statErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if fileInfo.Mode().Perm() != 0600 {
		return "", fmt.Errorf("probe file mode %04o, want 0600", fileInfo.Mode().Perm())
	}
	return fmt.Sprintf("dir %04o, write probe 0600", dirMode), nil
}

func (a *App) menu(ctx context.Context) (int, error) {
	lastCode := 0
	for {
		conversations, err := a.store.List(a.cwd())
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(a.out, "\nTerminus in %s\n", a.cwd())
		fmt.Fprintln(a.out, "1) new conversation")
		fmt.Fprintln(a.out, "2) attach existing native session")
		for index, c := range conversations {
			state := string(c.ActiveAgent)
			if state == "" {
				state = "unbound"
			}
			if c.Pending != nil {
				state += ", pending " + string(c.Pending.Packet.Target)
			}
			fmt.Fprintf(a.out, "%d) open %s (%s, %s)\n", index+3, c.ID, state, c.UpdatedAt.Format(time.RFC3339))
		}
		fmt.Fprintln(a.out, "q) quit")
		choice, err := a.promptLine("Select")
		if err != nil {
			if errors.Is(err, io.EOF) {
				return lastCode, nil
			}
			return 1, err
		}
		choice = strings.TrimSpace(choice)
		if choice == "" || choice == "q" || choice == "Q" {
			return lastCode, nil
		}
		switch choice {
		case "1":
			agent, err := a.chooseAgent()
			if err != nil {
				return 1, err
			}
			first, err := a.promptLine("First user request")
			if err != nil {
				return 1, err
			}
			first = strings.TrimSpace(first)
			if first == "" {
				fmt.Fprintln(a.errOut, "first user request must not be empty")
				continue
			}
			id, code, actionErr := a.startAction(ctx, agent, first)
			if actionErr != nil {
				fmt.Fprintf(a.errOut, "terminus: %v\n", actionErr)
				lastCode = nonzero(code)
				continue
			}
			lastCode = 0
			code, actionErr = a.menuFor(ctx, id)
			if actionErr != nil {
				fmt.Fprintf(a.errOut, "terminus: %v\n", actionErr)
			}
			lastCode = code
		case "2":
			agent, err := a.chooseAgent()
			if err != nil {
				return 1, err
			}
			sessions, err := a.discoverSessions(ctx, agent)
			if err != nil {
				return 1, err
			}
			if len(sessions) == 0 {
				fmt.Fprintf(a.out, "no %s sessions found in %s\n", agent, a.cwd())
				continue
			}
			session, err := a.selectSession(sessions)
			if err != nil {
				return 1, err
			}
			id, code, actionErr := a.attachAction(ctx, agent, session.ID)
			if actionErr != nil {
				fmt.Fprintf(a.errOut, "terminus: %v\n", actionErr)
				lastCode = nonzero(code)
				continue
			}
			lastCode = 0
			code, actionErr = a.menuFor(ctx, id)
			if actionErr != nil {
				fmt.Fprintf(a.errOut, "terminus: %v\n", actionErr)
			}
			lastCode = code
		default:
			index, parseErr := strconv.Atoi(choice)
			if parseErr != nil || index < 3 || index >= len(conversations)+3 {
				fmt.Fprintln(a.errOut, "invalid choice")
				continue
			}
			code, actionErr := a.menuFor(ctx, conversations[index-3].ID)
			if actionErr != nil {
				fmt.Fprintf(a.errOut, "terminus: %v\n", actionErr)
			}
			lastCode = code
		}
	}
}

func (a *App) menuFor(ctx context.Context, id string) (int, error) {
	lastCode := 0
	for {
		c, err := a.store.Load(id)
		if err != nil {
			return 1, err
		}
		active := string(c.ActiveAgent)
		if active == "" {
			active = "unbound"
		}
		pending := ""
		if c.Pending != nil {
			pending = fmt.Sprintf(", pending %s", c.Pending.Packet.Target)
		}
		fmt.Fprintf(a.out, "\nTerminus conversation %s in %s (active %s%s)\n", c.ID, c.CWD, active, pending)
		if c.Pending != nil {
			fmt.Fprintf(a.out, "1) retry pending %s handoff\n", c.Pending.Packet.Target)
		} else {
			fmt.Fprintln(a.out, "1) resume active agent")
		}
		fmt.Fprintln(a.out, "2) switch to codex")
		fmt.Fprintln(a.out, "3) switch to claude")
		fmt.Fprintln(a.out, "4) show context")
		fmt.Fprintln(a.out, "5) back to conversations")
		fmt.Fprintln(a.out, "q) quit")
		choice, err := a.promptLine("Select")
		if err != nil {
			if errors.Is(err, io.EOF) {
				return lastCode, nil
			}
			return 1, err
		}
		switch strings.TrimSpace(choice) {
		case "", "q", "Q":
			return lastCode, nil
		case "5", "b", "B":
			return lastCode, nil
		case "1":
			selected := c.ActiveAgent
			if c.Pending != nil {
				selected = c.Pending.Packet.Target
			}
			code, actionErr := a.resumeAction(ctx, id, selected)
			lastCode = code
			if actionErr != nil {
				fmt.Fprintf(a.errOut, "terminus: %v\n", actionErr)
			}
		case "2", "3":
			target := model.Codex
			if strings.TrimSpace(choice) == "3" {
				target = model.Claude
			}
			code, actionErr := a.handoffAction(ctx, id, target, "")
			lastCode = code
			if actionErr != nil {
				fmt.Fprintf(a.errOut, "terminus: %v\n", actionErr)
			}
		case "4":
			code, actionErr := a.context(ctx, []string{id})
			lastCode = code
			if actionErr != nil {
				fmt.Fprintf(a.errOut, "terminus: %v\n", actionErr)
			}
		default:
			fmt.Fprintln(a.errOut, "invalid choice")
		}
	}
}

func nonzero(code int) int {
	if code == 0 {
		return 1
	}
	return code
}

func (a *App) chooseAgent() (model.Agent, error) {
	line, err := a.promptLine("Agent (1=codex, 2=claude)")
	if err != nil {
		return "", err
	}
	switch strings.TrimSpace(line) {
	case "1", "codex":
		return model.Codex, nil
	case "2", "claude":
		return model.Claude, nil
	default:
		return "", errors.New("agent must be 1=codex or 2=claude")
	}
}

func (a *App) selectConversation(conversations []model.Conversation) (model.Conversation, error) {
	fmt.Fprintln(a.out, "Terminus conversations in this directory:")
	for i, c := range conversations {
		fmt.Fprintf(a.out, "%d) %s  agent=%s  updated=%s\n", i+1, c.ID, c.ActiveAgent, c.UpdatedAt.Format(time.RFC3339))
	}
	line, err := a.promptLine("Select conversation")
	if err != nil {
		return model.Conversation{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(conversations) {
		return model.Conversation{}, errors.New("invalid conversation choice")
	}
	return conversations[n-1], nil
}

func (a *App) discoverSessions(ctx context.Context, agent model.Agent) ([]model.Session, error) {
	adapter, err := a.adapter(ctx, agent)
	if err != nil {
		return nil, err
	}
	return adapter.Discover(ctx, a.cwd())
}

func (a *App) selectSession(sessions []model.Session) (model.Session, error) {
	fmt.Fprintf(a.out, "Native %s sessions in %s:\n", sessions[0].Agent, a.cwd())
	for i, session := range sessions {
		title := strings.TrimSpace(session.Title)
		if title == "" {
			title = "-"
		}
		fmt.Fprintf(a.out, "%d) %s  id=%s  updated=%s  %s\n", i+1, title, session.ID, session.UpdatedAt.Format(time.RFC3339), session.Path)
	}
	line, err := a.promptLine("Select session")
	if err != nil {
		return model.Session{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > len(sessions) {
		return model.Session{}, errors.New("invalid session choice")
	}
	return sessions[n-1], nil
}

func (a *App) runAndSettle(ctx context.Context, c *model.Conversation, runPrompt string) (int, error) {
	pending := c.Pending
	if pending == nil {
		return 1, errors.New("no pending handoff to run")
	}
	target := pending.Packet.Target
	var run launcher.NativeRun
	var err error
	if binding, ok := c.Bindings[target]; ok {
		run, err = launcher.ResumeRun(target, binding.Session.ID, runPrompt, c.CWD, a.options.customBinary(target))
	} else {
		sessionID := pending.SessionID
		if target == model.Claude && sessionID == "" {
			sessionID, err = randomUUID()
			if err != nil {
				return 1, err
			}
			c.Pending.SessionID = sessionID
			if err := a.store.Save(c); err != nil {
				return 1, err
			}
		}
		run, err = launcher.NewSessionRun(target, sessionID, runPrompt, c.CWD, a.options.customBinary(target))
	}
	if err != nil {
		return 2, err
	}
	runErr := a.runner.Run(ctx, run, a.in, a.out, a.errOut)
	settleCode, settleErr := a.settle(ctx, c.ID)
	if settleErr != nil {
		if runErr != nil {
			return 1, fmt.Errorf("run native %s: %w; settle handoff: %v", target, runErr, settleErr)
		}
		return nonzero(settleCode), settleErr
	}
	importErr := a.importUpdates(ctx, c.ID)
	if runErr != nil && importErr != nil {
		return 1, fmt.Errorf("run native %s: %w; import updates: %v", target, runErr, importErr)
	}
	if runErr != nil {
		return 1, fmt.Errorf("run native %s: %w", target, runErr)
	}
	if importErr != nil {
		return 1, importErr
	}
	return 0, nil
}

func (a *App) settle(ctx context.Context, id string) (int, error) {
	var code int
	err := a.store.WithLock(id, func() error {
		c, err := a.store.Load(id)
		if err != nil {
			return err
		}
		if c.Pending == nil {
			return nil
		}
		return a.settleConversation(ctx, c)
	})
	if err != nil {
		code = 1
		return code, err
	}
	return 0, nil
}

func (a *App) recoverPending(ctx context.Context, id string) (bool, error) {
	recovered := false
	err := a.store.WithLock(id, func() error {
		c, err := a.store.Load(id)
		if err != nil {
			return err
		}
		if c.Pending == nil {
			return nil
		}
		err = a.settleConversation(ctx, c)
		if errors.Is(err, errNoReceipt) {
			return nil
		}
		if err != nil {
			return err
		}
		recovered = true
		return nil
	})
	return recovered, err
}

func (a *App) settleConversation(ctx context.Context, c *model.Conversation) error {
	pending := c.Pending
	if pending == nil {
		return nil
	}
	receipts, err := a.receiptEntries(ctx, c, *pending)
	if err != nil {
		return err
	}
	if len(receipts) == 0 {
		return errNoReceipt
	}
	if len(receipts) != 1 {
		return fmt.Errorf("multiple candidate %s sessions contain handoff marker %s; refusing to guess", pending.Packet.Target, pending.Packet.ID)
	}
	target := pending.Packet.Target
	session := receipts[0]
	var delivered int64
	if old, ok := c.Bindings[target]; ok {
		if old.Session.ID != session.ID {
			return fmt.Errorf("handoff receipt is in %s session %s, but the conversation is bound to %s", target, session.ID, old.Session.ID)
		}
		merged, err := mergeSession(old.Session, session)
		if err != nil {
			return err
		}
		session = merged
		delivered = old.DeliveredThrough
	}
	if delivered < pending.Packet.Through {
		delivered = pending.Packet.Through
	}
	provisional, err := a.provisionalEvent(c, target)
	if err != nil {
		return err
	}
	resolved, err := a.importSessionDirect(ctx, c, session, provisional)
	if err != nil {
		return err
	}
	c.Bindings[target] = model.Binding{Session: resolved, DeliveredThrough: delivered}
	c.Pending = nil
	c.ActiveAgent = target
	c.UpdatedAt = time.Now().UTC()
	return a.store.Save(c)
}

func (a *App) receiptEntries(ctx context.Context, c *model.Conversation, pending model.Pending) ([]model.Session, error) {
	target := pending.Packet.Target
	if binding, ok := c.Bindings[target]; ok {
		// The conversation is already bound. Check exactly that native session
		// through its pinned backend instead of probing every session in the
		// directory or letting discovery change the read transport.
		resolved, events, err := a.readSession(ctx, binding.Session)
		if err != nil {
			return nil, fmt.Errorf("read bound native %s session %s while checking receipt: %w", target, binding.Session.ID, err)
		}
		if hasReceipt(events, pending.Packet.ID) {
			return []model.Session{resolved}, nil
		}
		return []model.Session{}, nil
	}

	adapter, err := a.adapter(ctx, target)
	if err != nil {
		return nil, err
	}
	sessions, err := adapter.Discover(ctx, c.CWD)
	if err != nil {
		return nil, err
	}
	matches := []model.Session{}
	for _, session := range sessions {
		resolved, events, err := a.readSession(ctx, session)
		if err != nil {
			return nil, fmt.Errorf("read native %s session %s while checking receipt: %w", session.Agent, session.ID, err)
		}
		if hasReceipt(events, pending.Packet.ID) {
			matches = append(matches, resolved)
			continue
		}
		// A first-launch Codex thread may be visible to app-server before the
		// newly written rollout is indexed. This is backend selection for a new
		// binding, not a later switch, so checking the rollout path is safe and
		// pins rollout if that is where the receipt actually is.
		if resolved.Agent == model.Codex && resolved.Backend == adapters.BackendAppServer && resolved.Path != "" {
			rollout := resolved
			rollout.Backend = adapters.BackendRollout
			rolloutResolved, rolloutEvents, err := a.readSession(ctx, rollout)
			if err != nil {
				return nil, fmt.Errorf("read codex rollout %s while checking receipt: %w", resolved.Path, err)
			}
			if hasReceipt(rolloutEvents, pending.Packet.ID) {
				matches = append(matches, rolloutResolved)
			}
		}
	}
	return matches, nil
}

func hasReceipt(events []model.Event, packetID string) bool {
	marker := handoff.Marker(packetID)
	for _, event := range events {
		if event.Kind == "handoff" && handoff.IsInjected(event.Text) && strings.Contains(event.Text, marker) {
			return true
		}
	}
	return false
}

func (a *App) importUpdates(ctx context.Context, id string) error {
	c, err := a.store.Load(id)
	if err != nil {
		return err
	}
	for _, agent := range []model.Agent{model.Codex, model.Claude} {
		if _, ok := c.Bindings[agent]; !ok {
			continue
		}
		if err := a.importBinding(ctx, c, agent); err != nil {
			return err
		}
		updated, err := a.store.Load(id)
		if err != nil {
			return err
		}
		c = updated
	}
	return nil
}

func (a *App) importBinding(ctx context.Context, c *model.Conversation, agent model.Agent) error {
	binding, ok := c.Bindings[agent]
	if !ok {
		return nil
	}
	session := binding.Session
	if session.ID == "" {
		return fmt.Errorf("conversation binding for %s has no native session ID", agent)
	}
	if session.Agent == "" {
		session.Agent = agent
	}
	if session.Agent != agent {
		return fmt.Errorf("binding for %s points at %s session %s", agent, session.Agent, session.ID)
	}
	if agent == model.Claude && session.Backend == "" {
		session.Backend = adapters.BackendJSONL
		binding.Session = session
		c.Bindings[agent] = binding
		if err := a.store.Save(c); err != nil {
			return err
		}
	}
	if session.Path == "" && agent == model.Claude {
		resolved, err := a.discoverExact(ctx, agent, c.CWD, session.ID)
		if err != nil {
			return err
		}
		merged, err := mergeSession(session, resolved)
		if err != nil {
			return err
		}
		session = merged
		binding.Session = session
		c.Bindings[agent] = binding
		if err := a.store.Save(c); err != nil {
			return err
		}
	}
	provisional, err := a.provisionalEvent(c, agent)
	if err != nil {
		return err
	}
	resolved, err := a.importSessionDirect(ctx, c, session, provisional)
	if err != nil {
		return err
	}
	if resolved.Backend != session.Backend || resolved.Path != session.Path {
		binding.Session = resolved
		c.Bindings[agent] = binding
		c.UpdatedAt = time.Now().UTC()
		return a.store.Save(c)
	}
	return nil
}

func (a *App) importFromID(ctx context.Context, c *model.Conversation, sessionID string) error {
	matches := []model.Session{}
	for _, agent := range []model.Agent{model.Codex, model.Claude} {
		adapter, err := a.adapter(ctx, agent)
		if err != nil {
			return err
		}
		sessions, err := adapter.Discover(ctx, c.CWD)
		if err != nil {
			return err
		}
		for _, session := range sessions {
			if session.ID == sessionID {
				matches = append(matches, session)
			}
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("native session %s not found in %s", sessionID, c.CWD)
	}
	if len(matches) > 1 {
		return fmt.Errorf("native session ID %s is ambiguous in %s", sessionID, c.CWD)
	}
	session := matches[0]
	if old, ok := c.Bindings[session.Agent]; ok {
		merged, err := mergeSession(old.Session, session)
		if err != nil {
			return err
		}
		session = merged
	}
	c.Bindings[session.Agent] = model.Binding{Session: session, DeliveredThrough: c.Bindings[session.Agent].DeliveredThrough}
	c.UpdatedAt = time.Now().UTC()
	if err := a.store.Save(c); err != nil {
		return err
	}
	provisional, err := a.provisionalEvent(c, session.Agent)
	if err != nil {
		return err
	}
	resolved, err := a.importSessionDirect(ctx, c, session, provisional)
	if err != nil {
		return err
	}
	binding := c.Bindings[session.Agent]
	if resolved.Backend != binding.Session.Backend || resolved.Path != binding.Session.Path {
		binding.Session = resolved
		c.Bindings[session.Agent] = binding
		c.UpdatedAt = time.Now().UTC()
		return a.store.Save(c)
	}
	return nil
}

func (a *App) discoverExact(ctx context.Context, agent model.Agent, cwd, sessionID string) (model.Session, error) {
	adapter, err := a.adapter(ctx, agent)
	if err != nil {
		return model.Session{}, err
	}
	sessions, err := adapter.Discover(ctx, cwd)
	if err != nil {
		return model.Session{}, err
	}
	matches := []model.Session{}
	for _, session := range sessions {
		if session.ID == sessionID {
			matches = append(matches, session)
		}
	}
	if len(matches) == 0 {
		return model.Session{}, fmt.Errorf("native %s session %s not found in %s", agent, sessionID, cwd)
	}
	if len(matches) > 1 {
		return model.Session{}, fmt.Errorf("native %s session %s is ambiguous in %s", agent, sessionID, cwd)
	}
	return matches[0], nil
}

func (a *App) importSessionDirect(ctx context.Context, c *model.Conversation, session model.Session, provisional *model.Event) (model.Session, error) {
	session, events, err := a.readSession(ctx, session)
	if err != nil {
		return session, err
	}
	if provisional != nil {
		events = skipProvisionalDuplicate(events, *provisional)
	}
	added, err := a.store.Append(c.ID, events)
	if err == nil && len(added) != 0 {
		c.UpdatedAt = time.Now().UTC()
		err = a.store.Save(c)
	}
	return session, err
}

func (a *App) readSession(ctx context.Context, session model.Session) (model.Session, []model.Event, error) {
	if session.Agent == model.Claude && session.Backend == "" {
		session.Backend = adapters.BackendJSONL
	}
	adapter, err := a.adapter(ctx, session.Agent)
	if err != nil {
		return session, nil, err
	}
	if reader, ok := adapter.(resolvedReader); ok {
		resolved, events, err := reader.ReadResolved(ctx, session)
		if err != nil {
			return resolved, nil, fmt.Errorf("read native %s session %s: %w", session.Agent, session.ID, err)
		}
		return resolved, events, nil
	}
	events, err := adapter.Read(ctx, session)
	if err != nil {
		return session, nil, fmt.Errorf("read native %s session %s: %w", session.Agent, session.ID, err)
	}
	return session, events, nil
}

func skipProvisionalDuplicate(events []model.Event, provisional model.Event) []model.Event {
	out := make([]model.Event, 0, len(events))
	skipped := false
	for _, event := range events {
		if !skipped && event.Kind == "message" && event.Role == "user" && event.Text == provisional.Text {
			skipped = true
			continue
		}
		out = append(out, event)
	}
	return out
}

func (a *App) provisionalEvent(c *model.Conversation, agent model.Agent) (*model.Event, error) {
	entries, err := a.store.Entries(c.ID, 0)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		event := entries[i].Event
		if event.ID == firstRequestPrefix+c.ID && event.Source == agent {
			return &event, nil
		}
	}
	return nil, nil
}

func mergeSession(current, discovered model.Session) (model.Session, error) {
	if current.ID != "" && discovered.ID != "" && current.ID != discovered.ID {
		return model.Session{}, fmt.Errorf("session ID changed from %s to %s", current.ID, discovered.ID)
	}
	if current.Backend != "" && discovered.Backend != "" && current.Backend != discovered.Backend {
		return model.Session{}, fmt.Errorf("session %s read backend changed from %s to %s", current.ID, current.Backend, discovered.Backend)
	}
	merged := current
	if merged.ID == "" {
		merged.ID = discovered.ID
	}
	if merged.Agent == "" {
		merged.Agent = discovered.Agent
	}
	if merged.CWD == "" {
		merged.CWD = discovered.CWD
	}
	if discovered.Path != "" {
		merged.Path = discovered.Path
	}
	if merged.Title == "" {
		merged.Title = discovered.Title
	}
	if merged.Backend == "" {
		merged.Backend = discovered.Backend
	}
	if merged.UpdatedAt.IsZero() {
		merged.UpdatedAt = discovered.UpdatedAt
	}
	return merged, nil
}

func (a *App) pendingPrompt(ctx context.Context, c *model.Conversation) (string, error) {
	pending := c.Pending
	if pending == nil {
		return "", errors.New("no pending handoff")
	}
	prompt := handoff.Prompt(pending.Packet, a.contextCommand(c.ID))
	if _, bound := c.Bindings[pending.Packet.Target]; bound {
		// A retry of an ordinary switch resumes an existing native session. The
		// first-request provisional is only a start-mode prompt.
		return prompt, nil
	}
	provisional, err := a.provisionalEvent(c, pending.Packet.Target)
	if err != nil {
		return "", err
	}
	if provisional != nil {
		entries, err := a.store.Entries(c.ID, 0)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			if entry.Event.ID == provisional.ID && entry.Seq <= pending.Packet.Through {
				prompt += "\n\n" + provisional.Text
				break
			}
		}
	}
	return prompt, nil
}

func (a *App) promptLine(label string) (string, error) {
	fmt.Fprintf(a.out, "%s: ", label)
	return readLine(a.in)
}

// readLine reads exactly one line without buffering past its newline. This is
// important because the same stream is handed to the native TUI afterwards.
func readLine(reader io.Reader) (string, error) {
	line := make([]byte, 0, 64)
	var one [1]byte
	for {
		n, err := reader.Read(one[:])
		if n > 0 {
			if one[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) != 0 {
					return strings.TrimSuffix(string(line), "\r"), nil
				}
				return "", io.EOF
			}
			return "", err
		}
	}
}

func splitPositional(args []string, valueFlags map[string]bool) (string, []string, error) {
	id := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") && arg != "-" {
			rest = append(rest, arg)
			if strings.Contains(arg, "=") || !valueFlags[arg] {
				continue
			}
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("flag %s requires a value", arg)
			}
			i++
			rest = append(rest, args[i])
			continue
		}
		if id != "" {
			return "", nil, fmt.Errorf("unexpected argument %q", arg)
		}
		id = arg
	}
	return id, rest, nil
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
