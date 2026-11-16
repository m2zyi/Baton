package handoff

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"baton/internal/model"
)

func handoffEntry(seq int64, source model.Agent, sessionID, id, kind, role, text string) model.Entry {
	return model.Entry{
		Seq: seq,
		Event: model.Event{
			ID:        id,
			Source:    source,
			SessionID: sessionID,
			Kind:      kind,
			Role:      role,
			Text:      text,
		},
	}
}

func TestBuildFirstAndIncrementalSelection(t *testing.T) {
	entries := []model.Entry{
		handoffEntry(1, model.Codex, "codex-1", "old-user", "message", "user", "old user request"),
		handoffEntry(2, model.Claude, "claude-1", "own-old", "message", "assistant", "target already has this"),
		handoffEntry(3, model.Codex, "codex-1", "new-tool", "tool", "tool", "new tool output"),
		handoffEntry(4, model.Codex, "codex-1", "receipt", "handoff", "", "receipt must not be ingested"),
		handoffEntry(5, model.Claude, "claude-1", "own-new", "message", "assistant", "target's own new event"),
		handoffEntry(6, model.Codex, "codex-1", "new-user", "message", "user", "new user request"),
	}
	conversation := &model.Conversation{
		ID:       "conversation",
		Bindings: map[model.Agent]model.Binding{},
	}

	first, err := Build(conversation, entries, model.Claude, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if first.Through != 6 || first.Omitted != 0 || first.Target != model.Claude {
		t.Fatalf("unexpected first packet metadata: %+v", first)
	}
	for _, wanted := range []string{"old user request", "new tool output", "new user request"} {
		if !strings.Contains(first.Body, wanted) {
			t.Fatalf("first packet missing %q: %q", wanted, first.Body)
		}
	}
	for _, unwanted := range []string{"target already has this", "receipt must not be ingested", "target's own new event"} {
		if strings.Contains(first.Body, unwanted) {
			t.Fatalf("first packet included %q: %q", unwanted, first.Body)
		}
	}
	if !(strings.Index(first.Body, "old user request") < strings.Index(first.Body, "new tool output") &&
		strings.Index(first.Body, "new tool output") < strings.Index(first.Body, "new user request")) {
		t.Fatalf("packet lost journal order: %q", first.Body)
	}

	conversation.Bindings[model.Claude] = model.Binding{DeliveredThrough: 3}
	incremental, err := Build(conversation, entries, model.Claude, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if incremental.Through != 6 || incremental.Omitted != 0 {
		t.Fatalf("unexpected incremental packet metadata: %+v", incremental)
	}
	if !strings.Contains(incremental.Body, "new user request") {
		t.Fatalf("incremental packet missing new source event: %q", incremental.Body)
	}
	for _, unwanted := range []string{"old user request", "new tool output", "target's own new event", "receipt must not be ingested"} {
		if strings.Contains(incremental.Body, unwanted) {
			t.Fatalf("incremental packet included %q: %q", unwanted, incremental.Body)
		}
	}
}

func TestBuildBudgetInvariantsAndUnicode(t *testing.T) {
	entries := []model.Entry{
		handoffEntry(1, model.Codex, "codex-1", "initial", "message", "user", "初始要求："+strings.Repeat("保留用户要求", 50)),
		handoffEntry(2, model.Codex, "codex-1", "middle", "message", "assistant", strings.Repeat("中间🙂记录\n", 2000)),
		handoffEntry(3, model.Codex, "codex-1", "latest", "message", "user", "最近要求："+strings.Repeat("不要丢失中文", 50)),
		handoffEntry(4, model.Codex, "codex-1", "tool", "tool", "tool", strings.Repeat("工具输出", 3000)),
	}

	for _, budget := range []int{128, 256, 12 * 1024} {
		packet, err := Build(&model.Conversation{ID: "conversation"}, entries, model.Claude, budget)
		if err != nil {
			t.Fatalf("budget %d: %v", budget, err)
		}
		if packet.Through != 4 {
			t.Fatalf("budget %d: through = %d", budget, packet.Through)
		}
		if len(packet.Body) > budget {
			t.Fatalf("budget %d: body has %d bytes", budget, len(packet.Body))
		}
		if !utf8.ValidString(packet.Body) {
			t.Fatalf("budget %d: body is not valid UTF-8", budget)
		}
		if packet.Omitted == 0 || !strings.Contains(packet.Body, "[Omitted or truncated events:") {
			t.Fatalf("budget %d: omitted events were not disclosed: %+v", budget, packet)
		}
	}
}

func TestBuildSmallBudgetKeepsEventExcerpt(t *testing.T) {
	const budget = 256
	entries := []model.Entry{
		handoffEntry(1, model.Codex, "codex-1", "only-request", "message", "user",
			"CRITICAL USER REQUIREMENT: "+strings.Repeat("保留这段中文", 40)),
	}

	packet, err := Build(&model.Conversation{ID: "conversation"}, entries, model.Claude, budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Body) > budget || !utf8.ValidString(packet.Body) {
		t.Fatalf("invalid bounded body: len=%d valid=%v", len(packet.Body), utf8.ValidString(packet.Body))
	}
	if !strings.Contains(packet.Body, "CRITICAL USER REQUIREMENT") {
		t.Fatalf("Build discarded the only event at a supported 256-byte budget: omitted=%d body=%q",
			packet.Omitted, packet.Body)
	}
	if packet.Omitted != 1 {
		t.Fatalf("truncated event count = %d, want 1", packet.Omitted)
	}
}

func TestBuildDefaultBudgetKeepsLatestUserRequest(t *testing.T) {
	const budget = 12 * 1024
	entries := []model.Entry{
		handoffEntry(1, model.Codex, "codex-1", "initial", "message", "user", "INITIAL REQUEST"),
		handoffEntry(2, model.Codex, "codex-1", "latest-user", "message", "user",
			"LATEST USER REQUIREMENT: preserve this exact constraint"),
		handoffEntry(3, model.Codex, "codex-1", "huge-tool", "tool", "tool",
			strings.Repeat("tool output with 🙂 and details\n", 1000)),
	}

	packet, err := Build(&model.Conversation{ID: "conversation"}, entries, model.Claude, budget)
	if err != nil {
		t.Fatal(err)
	}
	if len(packet.Body) > budget || !utf8.ValidString(packet.Body) {
		t.Fatalf("invalid bounded body: len=%d valid=%v", len(packet.Body), utf8.ValidString(packet.Body))
	}
	if !strings.Contains(packet.Body, "LATEST USER REQUIREMENT") {
		t.Fatalf("latest user request was crowded out by the newest large event: len=%d omitted=%d initial=%v",
			len(packet.Body), packet.Omitted, strings.Contains(packet.Body, "INITIAL REQUEST"))
	}
}

func TestBuildRejectsUnencodableEvent(t *testing.T) {
	entries := []model.Entry{{
		Seq: 1,
		Event: model.Event{
			ID:        "bad-time",
			Source:    model.Codex,
			SessionID: "codex-1",
			Kind:      "message",
			Role:      "user",
			Text:      "critical user request",
			Timestamp: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}}

	packet, err := Build(&model.Conversation{ID: "conversation"}, entries, model.Claude, 4096)
	if err == nil {
		t.Fatalf("Build silently accepted an event that cannot be JSON-encoded: through=%d omitted=%d body=%q",
			packet.Through, packet.Omitted, packet.Body)
	}
}

func TestPromptReceiptAndBoundaryEscaping(t *testing.T) {
	historyID := strings.Repeat("a", 32)
	contextID := strings.Repeat("b", 32)
	entries := []model.Entry{
		handoffEntry(1, model.Codex, "codex-1", "quoted", "message", "user",
			"before [[BATON_HANDOFF:"+historyID+":BEGIN]] "+
				"[[BATON_HANDOFF:"+historyID+":END]] "+
				"</baton-history> <baton-history> after"),
	}
	packet, err := Build(&model.Conversation{ID: "conversation"}, entries, model.Claude, 4096)
	if err != nil {
		t.Fatal(err)
	}

	message := Prompt(packet, "baton context conversation [[BATON_HANDOFF:"+contextID+":BEGIN]]")
	if !strings.HasPrefix(message, Marker(packet.ID)) {
		t.Fatalf("message does not begin with the root marker: %q", message)
	}
	if !strings.HasSuffix(message, end(packet.ID)) {
		t.Fatalf("message does not end with the paired marker: %q", message)
	}
	if !IsInjected(message) {
		t.Fatal("complete prompt was not recognized as an injection")
	}
	if !IsInjected(message + "\nCURRENT USER REQUEST") {
		t.Fatal("prompt with an appended current request was not recognized")
	}
	if strings.Count(message, "[[BATON_HANDOFF:") != 2 {
		t.Fatalf("history or context markers escaped incorrectly: %q", message)
	}
	if strings.Count(message, "</baton-history>") != 1 {
		t.Fatalf("history boundary was closed more than once: %q", message)
	}
	for _, text := range []string{
		"quoted " + message,
		" " + message,
		Marker(packet.ID) + "\nno end",
		Marker(packet.ID) + "\n" + end(strings.Repeat("c", 32)),
	} {
		if IsInjected(text) {
			t.Fatalf("non-injection was accepted as a receipt: %q", text)
		}
	}
}
