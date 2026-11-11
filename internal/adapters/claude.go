package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"terminus/internal/model"
)

type claudeAdapter struct {
	root string
}

func newClaudeAdapter() *claudeAdapter {
	return &claudeAdapter{root: envPath("CLAUDE_CONFIG_DIR", ".claude")}
}

type claudeLine struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	UUID             string          `json:"uuid"`
	ParentUUID       *string         `json:"parentUuid"`
	IsSidechain      bool            `json:"isSidechain"`
	CWD              string          `json:"cwd"`
	SessionID        string          `json:"sessionId"`
	Timestamp        json.RawMessage `json:"timestamp"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Summary          string          `json:"summary"`
	LeafUUID         string          `json:"leafUuid"`
	CustomTitle      string          `json:"customTitle"`
	Message          json.RawMessage `json:"message"`
}

type claudeMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type claudeNode struct {
	line      int
	uuid      string
	parent    string
	typ       string
	sidechain bool
}

type claudeTranscript struct {
	CWD       string
	SessionID string
	Title     string
	UpdatedAt time.Time
	Events    []model.Event
}

func (a *claudeAdapter) Discover(ctx context.Context, cwd string) ([]model.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projects := filepath.Join(a.root, "projects")
	if _, err := os.Stat(projects); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	sessions := make([]model.Session, 0)
	visited := 0
	err := filepath.WalkDir(projects, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		visited++
		if visited%32 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if entry.IsDir() {
			if entry.Name() == "subagents" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		transcript, err := parseClaudeTranscript(path)
		if err != nil {
			return fmt.Errorf("discover claude transcript %s: %w", path, err)
		}
		if transcript.SessionID == "" {
			transcript.SessionID = strings.TrimSuffix(entry.Name(), ".jsonl")
		}
		if cwd != "" && !sameDir(transcript.CWD, cwd) {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		updated := transcript.UpdatedAt
		if updated.IsZero() {
			updated = info.ModTime()
		}
		sessions = append(sessions, model.Session{
			ID:        transcript.SessionID,
			Agent:     model.Claude,
			CWD:       transcript.CWD,
			Path:      path,
			Title:     transcript.Title,
			UpdatedAt: updated,
		})
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	sort.Slice(sessions, func(i, j int) bool {
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

func (a *claudeAdapter) Read(ctx context.Context, s model.Session) ([]model.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Path == "" {
		return nil, fmt.Errorf("claude session %s has no transcript path", s.ID)
	}
	transcript, err := parseClaudeTranscript(s.Path)
	if err != nil {
		return nil, err
	}
	return transcript.Events, nil
}

func parseClaudeTranscript(path string) (claudeTranscript, error) {
	lines, err := readJSONLines(path)
	if err != nil {
		return claudeTranscript{}, fmt.Errorf("read claude transcript %s: %w", path, err)
	}
	records := make([]claudeLine, 0, len(lines))
	for _, line := range lines {
		var record claudeLine
		if err := json.Unmarshal(line.data, &record); err != nil {
			return claudeTranscript{}, fmt.Errorf("%s:%d: invalid claude record: %w", path, line.number, err)
		}
		records = append(records, record)
	}
	return buildClaudeTranscript(path, records)
}

func buildClaudeTranscript(path string, records []claudeLine) (claudeTranscript, error) {
	result := claudeTranscript{}
	var titleFromSummary string
	for _, record := range records {
		if record.CWD != "" {
			result.CWD = record.CWD
		}
		if record.SessionID != "" {
			result.SessionID = record.SessionID
		}
		if len(record.Timestamp) > 0 {
			if ts := claudeTimestamp(record.Timestamp); !ts.IsZero() {
				result.UpdatedAt = ts
			}
		}
		switch record.Type {
		case "custom-title":
			if strings.TrimSpace(record.CustomTitle) != "" {
				result.Title = strings.TrimSpace(record.CustomTitle)
			}
		case "summary":
			if titleFromSummary == "" {
				titleFromSummary = firstLine(record.Summary, 100)
			}
		}
	}

	nodes := make([]claudeNode, 0)
	hasParentLink := false
	for index, record := range records {
		if record.UUID == "" {
			continue
		}
		parent := ""
		if record.ParentUUID != nil {
			parent = *record.ParentUUID
			if parent != "" {
				hasParentLink = true
			}
		}
		nodes = append(nodes, claudeNode{line: index, uuid: record.UUID, parent: parent, typ: record.Type, sidechain: record.IsSidechain})
	}

	active := make(map[string]struct{})
	if hasParentLink {
		if leaf, ok := activeClaudeLeaf(nodes); ok {
			byUUID := make(map[string]claudeNode, len(nodes))
			for _, node := range nodes {
				byUUID[node.uuid] = node
			}
			seen := make(map[string]struct{})
			for leaf != "" {
				if _, duplicate := seen[leaf]; duplicate {
					break
				}
				seen[leaf] = struct{}{}
				active[leaf] = struct{}{}
				node, exists := byUUID[leaf]
				if !exists {
					break
				}
				leaf = node.parent
			}
		}
	}

	ordered := make([]orderedEvent, 0)
	order := 0
	var firstUser string
	seenSummaries := make(map[string]struct{})
	seenToolResults := make(map[string]struct{})
	dedup := newDeduper()
	addEvent := func(line int, event model.Event) {
		order++
		ordered = append(ordered, orderedEvent{line: line, order: order, event: event})
	}

	for index, record := range records {
		if record.UUID != "" && len(active) > 0 {
			if _, onChain := active[record.UUID]; !onChain {
				continue
			}
		}
		if record.IsSidechain && len(active) == 0 {
			continue
		}
		ts := claudeTimestamp(record.Timestamp)
		switch record.Type {
		case "summary":
			if strings.TrimSpace(record.Summary) == "" {
				continue
			}
			if record.LeafUUID != "" && len(active) > 0 {
				if _, onChain := active[record.LeafUUID]; !onChain {
					continue
				}
			}
			key := strings.Join(strings.Fields(record.Summary), " ")
			if _, duplicate := seenSummaries[key]; duplicate {
				continue
			}
			seenSummaries[key] = struct{}{}
			addEvent(index, model.Event{
				ID:        eventIDFromContent("summary", result.SessionID, record.LeafUUID, record.Summary),
				Source:    model.Claude,
				SessionID: result.SessionID,
				Kind:      "summary",
				Role:      "assistant",
				Text:      record.Summary,
				Timestamp: ts,
			})
		case "user":
			if record.IsMeta {
				continue
			}
			text, blocks, err := decodeClaudeContent(record.Message)
			if err != nil {
				return claudeTranscript{}, fmt.Errorf("%s: line %d: %w", path, index+1, err)
			}
			if record.IsCompactSummary {
				summaryText := strings.TrimSpace(text)
				if summaryText == "" {
					summaryText = claudeBlocksText(blocks)
				}
				if summaryText != "" {
					key := strings.Join(strings.Fields(summaryText), " ")
					if _, duplicate := seenSummaries[key]; !duplicate {
						seenSummaries[key] = struct{}{}
						addEvent(index, model.Event{
							ID:        stableID(record.UUID, "compact-summary"),
							Source:    model.Claude,
							SessionID: result.SessionID,
							Kind:      "summary",
							Role:      "assistant",
							Text:      summaryText,
							Timestamp: ts,
						})
					}
				}
				continue
			}
			if blocks == nil {
				text = stripClaudeSyntheticUserContext(text)
				if text != "" {
					if firstUser == "" {
						firstUser = firstLine(visibleUserText(text), 100)
					}
					var userEvents []model.Event
					appendUserText(&userEvents, model.Claude, result.SessionID, stableID(record.UUID, "text", "0"), ts, text)
					for _, event := range userEvents {
						addEvent(index, event)
					}
				}
				continue
			}
			for blockIndex, block := range blocks {
				switch block.Type {
				case "text":
					blockText := stripClaudeSyntheticUserContext(block.Text)
					if blockText == "" {
						continue
					}
					if firstUser == "" {
						firstUser = firstLine(visibleUserText(blockText), 100)
					}
					var userEvents []model.Event
					appendUserText(&userEvents, model.Claude, result.SessionID, stableID(record.UUID, "text", strconv.Itoa(blockIndex)), ts, blockText)
					for _, event := range userEvents {
						addEvent(index, event)
					}
				case "tool_result":
					if block.ToolUseID != "" {
						if _, duplicate := seenToolResults[block.ToolUseID]; duplicate {
							continue
						}
						seenToolResults[block.ToolUseID] = struct{}{}
					}
					content := claudeToolResultText(block)
					if content == "" {
						continue
					}
					if block.IsError {
						content = "error: " + content
					}
					idPart := block.ToolUseID
					if idPart == "" {
						idPart = strconv.Itoa(blockIndex)
					}
					addEvent(index, model.Event{
						ID:        stableID(record.UUID, "tool_result", idPart),
						Source:    model.Claude,
						SessionID: result.SessionID,
						Kind:      "tool",
						Role:      "tool",
						Text:      content,
						Timestamp: ts,
					})
				}
			}
		case "assistant":
			text, blocks, err := decodeClaudeContent(record.Message)
			if err != nil {
				return claudeTranscript{}, fmt.Errorf("%s: line %d: %w", path, index+1, err)
			}
			if blocks == nil {
				if strings.TrimSpace(text) != "" {
					addEvent(index, model.Event{
						ID:        stableID(record.UUID, "text", "0"),
						Source:    model.Claude,
						SessionID: result.SessionID,
						Kind:      "message",
						Role:      "assistant",
						Text:      text,
						Timestamp: ts,
					})
				}
				continue
			}
			for blockIndex, block := range blocks {
				switch block.Type {
				case "text":
					if strings.TrimSpace(block.Text) == "" {
						continue
					}
					addEvent(index, model.Event{
						ID:        stableID(record.UUID, "text", strconv.Itoa(blockIndex)),
						Source:    model.Claude,
						SessionID: result.SessionID,
						Kind:      "message",
						Role:      "assistant",
						Text:      block.Text,
						Timestamp: ts,
					})
				case "tool_use", "server_tool_use":
					idPart := block.ID
					if idPart == "" {
						idPart = strconv.Itoa(blockIndex)
					}
					addEvent(index, model.Event{
						ID:        stableID(record.UUID, "tool_use", idPart),
						Source:    model.Claude,
						SessionID: result.SessionID,
						Kind:      "tool",
						Role:      "tool",
						Text:      toolText(block.Name, rawAny(block.Input)),
						Timestamp: ts,
					})
				}
			}
		case "system", "file-history-snapshot", "queue-operation", "progress", "last-prompt", "pr-link", "custom-title":
			continue
		default:
			if len(record.Message) > 0 {
				return claudeTranscript{}, fmt.Errorf("%s: line %d: unsupported claude record type %q", path, index+1, record.Type)
			}
		}
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].line == ordered[j].line {
			return ordered[i].order < ordered[j].order
		}
		return ordered[i].line < ordered[j].line
	})
	for _, item := range ordered {
		dedup.add(&result.Events, item.event)
	}
	if result.Title == "" {
		result.Title = firstUser
	}
	if result.Title == "" {
		result.Title = titleFromSummary
	}
	return result, nil
}

func activeClaudeLeaf(nodes []claudeNode) (string, bool) {
	for index := len(nodes) - 1; index >= 0; index-- {
		node := nodes[index]
		if !node.sidechain && (node.typ == "user" || node.typ == "assistant") {
			return node.uuid, true
		}
	}
	return "", false
}

func decodeClaudeContent(raw json.RawMessage) (string, []claudeBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var message claudeMessage
	if err := json.Unmarshal(raw, &message); err != nil {
		return "", nil, fmt.Errorf("invalid message: %w", err)
	}
	content := message.Content
	if len(content) == 0 || string(content) == "null" {
		return "", nil, nil
	}
	if content[0] == '"' {
		var text string
		if err := json.Unmarshal(content, &text); err != nil {
			return "", nil, fmt.Errorf("invalid message text: %w", err)
		}
		return text, nil, nil
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return "", nil, fmt.Errorf("invalid content blocks: %w", err)
	}
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n"), blocks, nil
}

func claudeBlocksText(blocks []claudeBlock) string {
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func claudeToolResultText(block claudeBlock) string {
	if len(block.Content) == 0 || string(block.Content) == "null" {
		return ""
	}
	if block.Content[0] == '"' {
		var text string
		if json.Unmarshal(block.Content, &text) == nil {
			return text
		}
	}
	var blocks []claudeBlock
	if json.Unmarshal(block.Content, &blocks) == nil {
		return claudeBlocksText(blocks)
	}
	return string(block.Content)
}

func stripClaudeSyntheticUserContext(text string) string {
	for _, tag := range []string{"system-reminder", "local-command-caveat"} {
		open := "<" + tag + ">"
		closeTag := "</" + tag + ">"
		for {
			start := strings.Index(text, open)
			if start < 0 {
				break
			}
			end := strings.Index(text[start:], closeTag)
			if end < 0 {
				text = text[:start]
				break
			}
			text = text[:start] + text[start+end+len(closeTag):]
		}
	}
	return strings.TrimSpace(text)
}

func claudeTimestamp(raw json.RawMessage) time.Time {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return time.Time{}
	}
	if text[0] == '"' {
		var value string
		if json.Unmarshal(raw, &value) != nil {
			return time.Time{}
		}
		return timestamp(value)
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return time.Time{}
	}
	absolute := value
	if absolute < 0 {
		absolute = -absolute
	}
	switch {
	case absolute >= 1e18:
		return time.Unix(0, int64(value)).UTC()
	case absolute >= 1e15:
		return time.Unix(0, int64(value*1e3)).UTC()
	case absolute >= 1e12:
		return time.UnixMilli(int64(value)).UTC()
	default:
		seconds := int64(value)
		nanoseconds := int64((value - float64(seconds)) * 1e9)
		return time.Unix(seconds, nanoseconds).UTC()
	}
}

func firstLine(text string, limit int) string {
	line := strings.TrimSpace(text)
	if newline := strings.IndexByte(line, '\n'); newline >= 0 {
		line = strings.TrimSpace(line[:newline])
	}
	runes := []rune(line)
	if limit > 0 && len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}
