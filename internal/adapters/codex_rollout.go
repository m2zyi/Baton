package adapters

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"terminus/internal/model"
)

type rolloutEnvelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type rolloutSessionMeta struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
	CWD       string `json:"cwd"`
	Timestamp string `json:"timestamp"`
	Title     string `json:"-"`
}

type rolloutResponseItem struct {
	Type      string           `json:"type"`
	ID        string           `json:"id"`
	Role      string           `json:"role"`
	Content   []rolloutContent `json:"content"`
	Name      string           `json:"name"`
	Arguments json.RawMessage  `json:"arguments"`
	Input     string           `json:"input"`
	CallID    string           `json:"call_id"`
	Output    json.RawMessage  `json:"output"`
	Action    json.RawMessage  `json:"action"`
	Query     string           `json:"query"`
}

type rolloutContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type rolloutEventMessage struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type rolloutCompacted struct {
	Message string `json:"message"`
}

type rolloutMessage struct {
	line     int
	order    int
	role     string
	text     string
	baseID   string
	ts       time.Time
	response bool
}

type orderedEvent struct {
	line  int
	order int
	event model.Event
}

func (a *codexAdapter) discoverRollouts(ctx context.Context, cwd string) ([]model.Session, error) {
	root := filepath.Join(a.home, "sessions")
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	byID := make(map[string]model.Session)
	visited := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		visited++
		if visited%32 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		meta, err := readRolloutMeta(path)
		if err != nil || meta.SessionID == "" {
			return nil
		}
		if cwd != "" && !sameDir(meta.CWD, cwd) {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		session := model.Session{
			ID:        meta.SessionID,
			Agent:     model.Codex,
			CWD:       meta.CWD,
			Path:      path,
			Title:     meta.Title,
			UpdatedAt: info.ModTime(),
		}
		if existing, ok := byID[session.ID]; !ok || session.UpdatedAt.After(existing.UpdatedAt) {
			byID[session.ID] = session
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}

	sessions := make([]model.Session, 0, len(byID))
	for _, session := range byID {
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].UpdatedAt.Equal(sessions[j].UpdatedAt) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt)
	})
	return sessions, nil
}

func readRolloutMeta(path string) (rolloutSessionMeta, error) {
	file, err := os.Open(path)
	if err != nil {
		return rolloutSessionMeta{}, err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var meta rolloutSessionMeta
	found := false
	for lineNumber := 0; lineNumber < 256; lineNumber++ {
		line, err := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 && json.Valid(trimmed) {
			var envelope rolloutEnvelope
			if json.Unmarshal(trimmed, &envelope) == nil && envelope.Type == "session_meta" && !found {
				if json.Unmarshal(envelope.Payload, &meta) != nil {
					return rolloutSessionMeta{}, fmt.Errorf("%s: invalid session_meta", path)
				}
				if meta.ID != "" {
					meta.SessionID = meta.ID
				} else if meta.SessionID == "" {
					meta.SessionID = meta.ID
				}
				found = true
			} else if found && envelope.Type == "response_item" {
				var item rolloutResponseItem
				if json.Unmarshal(envelope.Payload, &item) == nil && item.Type == "message" && strings.EqualFold(item.Role, "user") {
					for _, block := range item.Content {
						text := stripCodexSyntheticUserContext(block.Text)
						if title := strings.TrimSpace(visibleUserText(text)); title != "" {
							meta.Title = firstLine(title, 100)
							return meta, nil
						}
					}
				}
			} else if found && envelope.Type == "event_msg" {
				var event rolloutEventMessage
				if json.Unmarshal(envelope.Payload, &event) == nil && event.Type == "user_message" {
					text := stripCodexSyntheticUserContext(event.Message)
					if title := strings.TrimSpace(visibleUserText(text)); title != "" {
						meta.Title = firstLine(title, 100)
						return meta, nil
					}
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return rolloutSessionMeta{}, err
		}
	}
	if found {
		return meta, nil
	}
	return rolloutSessionMeta{}, nil
}

func readCodexRollout(s model.Session) ([]model.Event, error) {
	lines, err := readJSONLines(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read codex rollout %s: %w", s.Path, err)
	}
	sessionID := s.ID
	messages := make([]rolloutMessage, 0)
	fixed := make([]orderedEvent, 0)
	order := 0
	sawMeta := false

	for _, line := range lines {
		var envelope rolloutEnvelope
		if err := json.Unmarshal(line.data, &envelope); err != nil {
			return nil, fmt.Errorf("%s:%d: invalid codex record: %w", s.Path, line.number, err)
		}
		ts := timestamp(envelope.Timestamp)
		switch envelope.Type {
		case "session_meta":
			if sawMeta {
				continue
			}
			sawMeta = true
			var meta rolloutSessionMeta
			if json.Unmarshal(envelope.Payload, &meta) == nil {
				if meta.ID != "" {
					sessionID = meta.ID
				} else if meta.SessionID != "" {
					sessionID = meta.SessionID
				}
			}
		case "response_item":
			parsedMessages, parsedTools, err := parseRolloutResponseItem(line.number, &order, envelope.Payload, sessionID, ts)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: %w", s.Path, line.number, err)
			}
			messages = append(messages, parsedMessages...)
			fixed = append(fixed, parsedTools...)
		case "event_msg":
			var event rolloutEventMessage
			if json.Unmarshal(envelope.Payload, &event) == nil {
				switch event.Type {
				case "user_message":
					text := stripCodexSyntheticUserContext(event.Message)
					if strings.TrimSpace(text) != "" {
						order++
						messages = append(messages, rolloutMessage{
							line:   line.number,
							order:  order,
							role:   "user",
							text:   text,
							baseID: eventIDFromContent("event", sessionID, strconv.Itoa(line.number), "user", text),
							ts:     ts,
						})
					}
				case "agent_message":
					if strings.TrimSpace(event.Message) != "" {
						order++
						messages = append(messages, rolloutMessage{
							line:   line.number,
							order:  order,
							role:   "assistant",
							text:   event.Message,
							baseID: eventIDFromContent("event", sessionID, strconv.Itoa(line.number), "assistant", event.Message),
							ts:     ts,
						})
					}
				}
			}
		case "compacted":
			var compacted rolloutCompacted
			if json.Unmarshal(envelope.Payload, &compacted) == nil && strings.TrimSpace(compacted.Message) != "" {
				order++
				fixed = append(fixed, orderedEvent{
					line:  line.number,
					order: order,
					event: model.Event{
						ID:        eventIDFromContent("summary", sessionID, strconv.Itoa(line.number), compacted.Message),
						Source:    model.Codex,
						SessionID: sessionID,
						Kind:      "summary",
						Role:      "assistant",
						Text:      compacted.Message,
						Timestamp: ts,
					},
				})
			}
		}
	}

	responseSignatures := make(map[string]struct{})
	type responseLine struct {
		line int
		role string
	}
	responseTexts := make(map[responseLine][]string)
	for _, message := range messages {
		if message.response {
			responseSignatures[rolloutSignature(message.role, message.text)] = struct{}{}
			key := responseLine{line: message.line, role: message.role}
			responseTexts[key] = append(responseTexts[key], message.text)
		}
	}
	for key, texts := range responseTexts {
		responseSignatures[rolloutSignature(key.role, strings.Join(texts, "\n"))] = struct{}{}
		responseSignatures[rolloutSignature(key.role, strings.Join(texts, ""))] = struct{}{}
	}

	ordered := make([]orderedEvent, 0, len(fixed)+len(messages))
	ordered = append(ordered, fixed...)
	var userBuffer []model.Event
	for _, message := range messages {
		if !message.response {
			if _, covered := responseSignatures[rolloutSignature(message.role, message.text)]; covered {
				continue
			}
		}
		userBuffer = userBuffer[:0]
		switch message.role {
		case "user":
			appendUserText(&userBuffer, model.Codex, sessionID, message.baseID, message.ts, message.text)
			for index, event := range userBuffer {
				ordered = append(ordered, orderedEvent{line: message.line, order: message.order*10 + index, event: event})
			}
		case "assistant":
			ordered = append(ordered, orderedEvent{
				line:  message.line,
				order: message.order,
				event: model.Event{
					ID:        message.baseID,
					Source:    model.Codex,
					SessionID: sessionID,
					Kind:      "message",
					Role:      "assistant",
					Text:      message.text,
					Timestamp: message.ts,
				},
			})
		}
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].line == ordered[j].line {
			return ordered[i].order < ordered[j].order
		}
		return ordered[i].line < ordered[j].line
	})
	events := make([]model.Event, 0, len(ordered))
	dedup := newDeduper()
	for _, item := range ordered {
		dedup.add(&events, item.event)
	}
	return events, nil
}

func rolloutSignature(role, text string) string {
	return role + "\x00" + strings.Join(strings.Fields(text), " ")
}

func parseRolloutResponseItem(line int, order *int, raw json.RawMessage, sessionID string, ts time.Time) ([]rolloutMessage, []orderedEvent, error) {
	var item rolloutResponseItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, nil, fmt.Errorf("invalid response_item: %w", err)
	}
	baseID := item.ID
	if baseID == "" {
		baseID = eventIDFromContent("response", sessionID, strconv.Itoa(line), item.Type, item.CallID)
	}

	var messages []rolloutMessage
	var tools []orderedEvent
	addTool := func(kind, text, suffix string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		*order = *order + 1
		tools = append(tools, orderedEvent{
			line:  line,
			order: *order,
			event: model.Event{
				ID:        stableID(baseID, suffix),
				Source:    model.Codex,
				SessionID: sessionID,
				Kind:      kind,
				Role:      "tool",
				Text:      text,
				Timestamp: ts,
			},
		})
	}

	switch item.Type {
	case "message":
		role := strings.ToLower(item.Role)
		if role != "user" && role != "assistant" {
			return nil, nil, nil
		}
		for blockIndex, block := range item.Content {
			if block.Type != "input_text" && block.Type != "output_text" && block.Type != "text" {
				continue
			}
			text := block.Text
			if role == "user" {
				text = stripCodexSyntheticUserContext(text)
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			*order = *order + 1
			messages = append(messages, rolloutMessage{
				line:     line,
				order:    *order,
				role:     role,
				text:     text,
				baseID:   stableID(baseID, "block", strconv.Itoa(blockIndex)),
				ts:       ts,
				response: true,
			})
		}
	case "reasoning":
		return nil, nil, nil
	case "function_call":
		addTool("tool", toolText(item.Name, parseArguments(item.Arguments)), "call")
	case "custom_tool_call":
		addTool("tool", toolText(item.Name, item.Input), "call")
	case "local_shell_call":
		addTool("tool", toolText("localShell", rawAny(item.Action)), "call")
	case "web_search_call":
		addTool("tool", "webSearch\n"+item.Query, "call")
	case "image_generation_call":
		addTool("tool", "imageGeneration", "call")
	case "function_call_output", "custom_tool_call_output":
		addTool("tool", toolText(item.Name, parseArguments(item.Output)), "output")
	}
	return messages, tools, nil
}

func parseArguments(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	if text, ok := value.(string); ok {
		var nested any
		if json.Unmarshal([]byte(text), &nested) == nil {
			return nested
		}
		return text
	}
	return value
}
