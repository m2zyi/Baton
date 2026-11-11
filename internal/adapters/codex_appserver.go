package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"terminus/internal/model"
)

type appServerThread struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Preview   string `json:"preview"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	RecencyAt *int64 `json:"recencyAt"`
}

type appServerThreadPage struct {
	Data       json.RawMessage `json:"data"`
	NextCursor *string         `json:"nextCursor"`
}

type appServerUserInput struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Path     string `json:"path"`
	URL      string `json:"url"`
	Name     string `json:"name"`
	Detail   string `json:"detail"`
	FileID   string `json:"fileId"`
	AudioURL string `json:"audioUrl"`
}

type appServerItem struct {
	Type             string               `json:"type"`
	ID               string               `json:"id"`
	Text             string               `json:"text"`
	Content          []appServerUserInput `json:"content"`
	Output           json.RawMessage      `json:"output"`
	Name             string               `json:"name"`
	Command          string               `json:"command"`
	CWD              string               `json:"cwd"`
	Status           string               `json:"status"`
	AggregatedOutput *string              `json:"aggregatedOutput"`
	ExitCode         *int                 `json:"exitCode"`
	Changes          json.RawMessage      `json:"changes"`
	Server           string               `json:"server"`
	Tool             string               `json:"tool"`
	Arguments        json.RawMessage      `json:"arguments"`
	Result           json.RawMessage      `json:"result"`
	Error            json.RawMessage      `json:"error"`
	ContentItems     json.RawMessage      `json:"contentItems"`
	Query            string               `json:"query"`
	Path             string               `json:"path"`
	Prompt           string               `json:"prompt"`
	RevisedPrompt    string               `json:"revisedPrompt"`
	SavedPath        string               `json:"savedPath"`
	Failure          json.RawMessage      `json:"failure"`
}

type appServerItemEntry struct {
	TurnID        string         `json:"turnId"`
	StartedAtMs   *int64         `json:"startedAtMs"`
	CompletedAtMs *int64         `json:"completedAtMs"`
	Item          *appServerItem `json:"item"`
}

type appServerItemsPage struct {
	Data       json.RawMessage `json:"data"`
	NextCursor *string         `json:"nextCursor"`
}

type appServerTurn struct {
	ID          string           `json:"id"`
	Items       *[]appServerItem `json:"items"`
	ItemsView   string           `json:"itemsView"`
	StartedAt   *int64           `json:"startedAt"`
	CompletedAt *int64           `json:"completedAt"`
}

type appServerTurnsPage struct {
	Data       json.RawMessage `json:"data"`
	NextCursor *string         `json:"nextCursor"`
}

const (
	maxDiscoverPages = 1000
	maxHistoryPages  = 10000
)

func decodePageData(method string, raw json.RawMessage, target any) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return failure(false, "%s response is missing required data field", method)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return failure(false, "decode %s data: %w", method, err)
	}
	return nil
}

func classifyRPC(method string, err error) error {
	var known *adapterFailure
	if errors.As(err, &known) {
		return err
	}
	if isUnsupportedRPC(err) {
		return failure(true, "%s: %w", method, err)
	}
	return failure(false, "%s: %w", method, err)
}

func (a *codexAdapter) discoverAppServer(ctx context.Context, cwd string) ([]model.Session, error) {
	client, err := startRPCClient(ctx, a.executable(), a.timeout)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	params := map[string]any{
		"limit": 100,
	}
	if strings.TrimSpace(cwd) != "" {
		params["cwd"] = cwd
	}

	sessions := make([]model.Session, 0)
	seenCursors := make(map[string]struct{})
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= maxDiscoverPages {
			return nil, failure(false, "thread/list exceeded %d pages without a terminal cursor", maxDiscoverPages)
		}
		result, callErr := client.call(ctx, "thread/list", params)
		if callErr != nil {
			return nil, classifyRPC("thread/list", callErr)
		}
		var page appServerThreadPage
		if err := json.Unmarshal(result, &page); err != nil {
			return nil, failure(false, "decode thread/list: %w", err)
		}
		var data []appServerThread
		if err := decodePageData("thread/list", page.Data, &data); err != nil {
			return nil, err
		}
		for _, thread := range data {
			if thread.ID == "" {
				return nil, failure(false, "thread/list returned a thread without an id")
			}
			if cwd != "" && !sameDir(thread.CWD, cwd) {
				continue
			}
			updated := thread.UpdatedAt
			if thread.RecencyAt != nil && *thread.RecencyAt > updated {
				updated = *thread.RecencyAt
			}
			sessions = append(sessions, model.Session{
				ID:        thread.ID,
				Agent:     model.Codex,
				CWD:       thread.CWD,
				Path:      thread.Path,
				Title:     appSessionTitle(thread.Name, thread.Preview),
				UpdatedAt: timeFromUnix(updated),
			})
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		if _, duplicate := seenCursors[*page.NextCursor]; duplicate {
			return nil, failure(false, "thread/list repeated cursor %q", *page.NextCursor)
		}
		seenCursors[*page.NextCursor] = struct{}{}
		params["cursor"] = *page.NextCursor
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].UpdatedAt.Equal(sessions[j].UpdatedAt) {
			if sessions[i].ID == sessions[j].ID {
				return sessions[i].Path < sessions[j].Path
			}
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	return sessions, nil
}

func (a *codexAdapter) readAppServer(ctx context.Context, s model.Session) ([]model.Event, error) {
	client, err := startRPCClient(ctx, a.executable(), a.timeout)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	if _, err := client.call(ctx, "thread/read", map[string]any{"threadId": s.ID}); err != nil {
		return nil, classifyRPC("thread/read", err)
	}

	events, err := a.readAppServerItems(ctx, client, s)
	if err == nil {
		return events, nil
	}
	if !canFallback(err) {
		return nil, err
	}
	turnEvents, turnErr := a.readAppServerTurns(ctx, client, s)
	if turnErr == nil {
		return turnEvents, nil
	}
	return nil, turnErr
}

func (a *codexAdapter) readAppServerItems(ctx context.Context, client *rpcClient, s model.Session) ([]model.Event, error) {
	params := map[string]any{
		"threadId":      s.ID,
		"limit":         100,
		"sortDirection": "asc",
	}
	var entries []appServerItemEntry
	seenCursors := make(map[string]struct{})
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= maxHistoryPages {
			return nil, failure(false, "thread/items/list exceeded %d pages without a terminal cursor", maxHistoryPages)
		}
		result, err := client.call(ctx, "thread/items/list", params)
		if err != nil {
			return nil, classifyRPC("thread/items/list", err)
		}
		var page appServerItemsPage
		if err := json.Unmarshal(result, &page); err != nil {
			return nil, failure(false, "decode thread/items/list: %w", err)
		}
		var data []appServerItemEntry
		if err := decodePageData("thread/items/list", page.Data, &data); err != nil {
			return nil, err
		}
		for _, entry := range data {
			if entry.Item == nil || entry.Item.Type == "" {
				return nil, failure(false, "thread/items/list returned an incomplete item entry")
			}
		}
		entries = append(entries, data...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		if _, duplicate := seenCursors[*page.NextCursor]; duplicate {
			return nil, failure(false, "thread/items/list repeated cursor %q", *page.NextCursor)
		}
		seenCursors[*page.NextCursor] = struct{}{}
		params["cursor"] = *page.NextCursor
	}
	return parseAppServerItemEntries(s, entries), nil
}

func (a *codexAdapter) readAppServerTurns(ctx context.Context, client *rpcClient, s model.Session) ([]model.Event, error) {
	params := map[string]any{
		"threadId":      s.ID,
		"limit":         100,
		"sortDirection": "asc",
		"itemsView":     "full",
	}
	var entries []appServerItemEntry
	seenCursors := make(map[string]struct{})
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= maxHistoryPages {
			return nil, failure(false, "thread/turns/list exceeded %d pages without a terminal cursor", maxHistoryPages)
		}
		result, err := client.call(ctx, "thread/turns/list", params)
		if err != nil {
			return nil, classifyRPC("thread/turns/list", err)
		}
		var page appServerTurnsPage
		if err := json.Unmarshal(result, &page); err != nil {
			return nil, failure(false, "decode thread/turns/list: %w", err)
		}
		var data []appServerTurn
		if err := decodePageData("thread/turns/list", page.Data, &data); err != nil {
			return nil, err
		}
		for _, turn := range data {
			// The request explicitly asks for full items. A summary or
			// notLoaded response is not a complete history and must not be
			// mistaken for an empty turn. Mark it fallback-capable so callers
			// can use a rollout only when one is explicitly available.
			if turn.ItemsView != "" && turn.ItemsView != "full" {
				return nil, failure(true, "thread/turns/list returned itemsView %q after requesting full", turn.ItemsView)
			}
			if turn.Items == nil {
				return nil, failure(false, "thread/turns/list returned turn %q without an items field", turn.ID)
			}
			for _, item := range *turn.Items {
				if item.Type == "" {
					return nil, failure(false, "thread/turns/list returned an incomplete item in turn %q", turn.ID)
				}
				item := item
				entry := appServerItemEntry{TurnID: turn.ID, Item: &item}
				if turn.StartedAt != nil {
					value := *turn.StartedAt * 1000
					entry.StartedAtMs = &value
				}
				if turn.CompletedAt != nil {
					value := *turn.CompletedAt * 1000
					entry.CompletedAtMs = &value
				}
				entries = append(entries, entry)
			}
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		if _, duplicate := seenCursors[*page.NextCursor]; duplicate {
			return nil, failure(false, "thread/turns/list repeated cursor %q", *page.NextCursor)
		}
		seenCursors[*page.NextCursor] = struct{}{}
		params["cursor"] = *page.NextCursor
	}
	return parseAppServerItemEntries(s, entries), nil
}

func parseAppServerItemEntries(s model.Session, entries []appServerItemEntry) []model.Event {
	events := make([]model.Event, 0, len(entries))
	dedup := newDeduper()
	for index, entry := range entries {
		item := *entry.Item
		baseID := item.ID
		if baseID == "" {
			baseID = eventIDFromContent("item", s.ID, entry.TurnID, strconv.Itoa(index), item.Type)
		}
		ts := time.Time{}
		if entry.CompletedAtMs != nil {
			ts = time.UnixMilli(*entry.CompletedAtMs).UTC()
		} else if entry.StartedAtMs != nil {
			ts = time.UnixMilli(*entry.StartedAtMs).UTC()
		}
		appendAppServerItem(&events, dedup, s, baseID, ts, item)
	}
	return events
}

func appendAppServerItem(out *[]model.Event, dedup *deduper, s model.Session, baseID string, ts time.Time, item appServerItem) {
	addMessage := func(kind, role, text, id string) {
		dedup.add(out, model.Event{
			ID:        id,
			Source:    model.Codex,
			SessionID: s.ID,
			Kind:      kind,
			Role:      role,
			Text:      text,
			Timestamp: ts,
		})
	}

	switch item.Type {
	case "userMessage":
		var userEvents []model.Event
		for index, input := range item.Content {
			text := appServerUserInputText(input)
			text = stripCodexSyntheticUserContext(text)
			if strings.TrimSpace(text) == "" {
				continue
			}
			blockID := stableID(baseID, "content", strconv.Itoa(index))
			appendUserText(&userEvents, model.Codex, s.ID, blockID, ts, text)
		}
		for _, event := range userEvents {
			dedup.add(out, event)
		}
	case "agentMessage":
		if strings.TrimSpace(item.Text) != "" {
			addMessage("message", "assistant", item.Text, stableID(baseID, "text"))
		}
	case "plan":
		if strings.TrimSpace(item.Text) != "" {
			addMessage("message", "assistant", item.Text, stableID(baseID, "plan"))
		}
	case "functionCallOutput":
		text := toolText(item.Name, appServerOutput(item.Output))
		if strings.TrimSpace(text) != "" {
			addMessage("tool", "tool", text, stableID(baseID, "output"))
		}
	case "commandExecution":
		parts := []string{"commandExecution\n" + item.Command}
		if item.AggregatedOutput != nil && *item.AggregatedOutput != "" {
			parts = append(parts, *item.AggregatedOutput)
		}
		if item.ExitCode != nil {
			parts = append(parts, "exit_code="+strconv.Itoa(*item.ExitCode))
		}
		addMessage("tool", "tool", strings.Join(parts, "\n"), stableID(baseID, "command"))
	case "fileChange":
		text := toolText("fileChange", rawAny(item.Changes))
		if strings.TrimSpace(text) != "" {
			addMessage("tool", "tool", text, stableID(baseID, "changes"))
		}
	case "mcpToolCall":
		parts := []string{item.Server + "." + item.Tool}
		if arguments := rawAny(item.Arguments); arguments != nil {
			parts = append(parts, textFromJSON(arguments))
		}
		if result := rawAny(item.Result); result != nil {
			parts = append(parts, textFromJSON(result))
		}
		if rpcErr := rawAny(item.Error); rpcErr != nil {
			parts = append(parts, "error: "+textFromJSON(rpcErr))
		}
		addMessage("tool", "tool", strings.Join(parts, "\n"), stableID(baseID, "mcp"))
	case "dynamicToolCall":
		parts := []string{item.Tool}
		if arguments := rawAny(item.Arguments); arguments != nil {
			parts = append(parts, textFromJSON(arguments))
		}
		if content := rawAny(item.ContentItems); content != nil {
			parts = append(parts, textFromJSON(content))
		}
		addMessage("tool", "tool", strings.Join(parts, "\n"), stableID(baseID, "dynamic"))
	case "webSearch":
		addMessage("tool", "tool", "webSearch\n"+item.Query, stableID(baseID, "search"))
	case "imageView":
		addMessage("tool", "tool", "imageView\n"+item.Path, stableID(baseID, "image"))
	case "imageGeneration":
		text := "imageGeneration\n" + item.RevisedPrompt
		if item.SavedPath != "" {
			text += "\n" + item.SavedPath
		}
		if failureValue := rawAny(item.Failure); failureValue != nil {
			text += "\nerror: " + textFromJSON(failureValue)
		}
		addMessage("tool", "tool", text, stableID(baseID, "generation"))
	}
}

func appServerUserInputText(input appServerUserInput) string {
	switch input.Type {
	case "text":
		return input.Text
	case "image":
		if input.URL != "" {
			return "[image] " + input.URL
		}
		return "[image]"
	case "localImage":
		return "[image] " + input.Path
	case "audio":
		return "[audio] " + input.URL
	case "localAudio":
		return "[audio] " + input.Path
	case "skill":
		return "[skill] " + input.Name
	case "mention":
		return "[mention] " + input.Name
	default:
		return ""
	}
}

func appServerOutput(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	return value
}

func rawAny(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return string(raw)
	}
	return value
}

func appSessionTitle(name, preview string) string {
	if strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	line := strings.TrimSpace(visibleUserText(preview))
	if newline := strings.IndexByte(line, '\n'); newline >= 0 {
		line = strings.TrimSpace(line[:newline])
	}
	if len([]rune(line)) > 100 {
		return string([]rune(line)[:100])
	}
	return line
}

func stripCodexSyntheticUserContext(text string) string {
	trimmed := strings.TrimSpace(text)
	tags := []string{"environment_context", "user_instructions", "permissions instructions"}
	for {
		removed := false
		for _, tag := range tags {
			open := "<" + tag + ">"
			closeTag := "</" + tag + ">"
			if strings.HasPrefix(trimmed, open) {
				if end := strings.Index(trimmed, closeTag); end >= 0 {
					trimmed = strings.TrimSpace(trimmed[end+len(closeTag):])
					removed = true
					break
				}
			}
		}
		if !removed {
			break
		}
	}
	return trimmed
}
