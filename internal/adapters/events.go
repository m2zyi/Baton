package adapters

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"terminus/internal/model"
)

func eventIDFromContent(prefix string, values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return prefix + ":" + hex.EncodeToString(sum[:8])
}

func timestamp(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func timeFromUnix(seconds int64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}

// appendUserText splits a root-level handoff receipt from a following real user
// request. The historical package becomes kind=handoff and is never emitted as
// an ordinary user message.
func appendUserText(out *[]model.Event, source model.Agent, sessionID, baseID string, ts time.Time, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	receipt, rest, _, isReceipt := SplitHandoffReceipt(text)
	if isReceipt {
		*out = append(*out, model.Event{
			ID:        stableID(baseID, "handoff"),
			Source:    source,
			SessionID: sessionID,
			Kind:      "handoff",
			Role:      "user",
			Text:      receipt,
			Timestamp: ts,
		})
		if rest != "" {
			*out = append(*out, model.Event{
				ID:        stableID(baseID, "request"),
				Source:    source,
				SessionID: sessionID,
				Kind:      "message",
				Role:      "user",
				Text:      rest,
				Timestamp: ts,
			})
		}
		return
	}
	*out = append(*out, model.Event{
		ID:        baseID,
		Source:    source,
		SessionID: sessionID,
		Kind:      "message",
		Role:      "user",
		Text:      text,
		Timestamp: ts,
	})
}

type deduper struct {
	seen map[string]struct{}
}

func newDeduper() *deduper {
	return &deduper{seen: make(map[string]struct{})}
}

func (d *deduper) add(out *[]model.Event, event model.Event) {
	if d.seen == nil {
		d.seen = make(map[string]struct{})
	}
	if _, exists := d.seen[event.ID]; exists {
		return
	}
	d.seen[event.ID] = struct{}{}
	*out = append(*out, event)
}

func jsonText(value json.RawMessage) string {
	if len(value) == 0 {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return string(value)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return string(value)
	}
	return string(encoded)
}

func textFromJSON(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func toolText(name string, input any) string {
	name = strings.TrimSpace(name)
	body := textFromJSON(input)
	switch {
	case name == "":
		return body
	case body == "" || body == "null":
		return name
	default:
		return name + "\n" + body
	}
}
