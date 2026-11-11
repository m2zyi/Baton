// Package model defines the shared contracts between Terminus components.
package model

import "time"

type Agent string

const (
	Codex  Agent = "codex"
	Claude Agent = "claude"
)

type Session struct {
	ID        string    `json:"id"`
	Agent     Agent     `json:"agent"`
	CWD       string    `json:"cwd"`
	Path      string    `json:"path,omitempty"`
	Title     string    `json:"title,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	// Backend identifies the native history transport that produced this
	// session. Empty is a legacy manifest value; adapters may probe once and
	// callers should persist the resolved value.
	Backend string `json:"backend,omitempty"`
}

// Event.ID must be deterministic within an agent's native session.
// Kind is message, tool, summary, or handoff. Handoff events are receipts,
// never new human messages; the conversation journal must not ingest them.
type Event struct {
	ID        string    `json:"id"`
	Source    Agent     `json:"source"`
	SessionID string    `json:"session_id"`
	Kind      string    `json:"kind"`
	Role      string    `json:"role"`
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp,omitempty"`
}

type Entry struct {
	Seq   int64 `json:"seq"`
	Event Event `json:"event"`
}

type Binding struct {
	Session          Session `json:"session"`
	DeliveredThrough int64   `json:"delivered_through"`
}

type Pending struct {
	Packet    Packet    `json:"packet"`
	SessionID string    `json:"session_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

type Conversation struct {
	Version     int               `json:"version"`
	ID          string            `json:"id"`
	CWD         string            `json:"cwd"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	ActiveAgent Agent             `json:"active_agent,omitempty"`
	Bindings    map[Agent]Binding `json:"bindings"`
	Pending     *Pending          `json:"pending,omitempty"`
}

// Packet.Body is a bounded, quoted slice of history. Through is a journal
// sequence, NOT an assertion that the target has received it yet.
type Packet struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Target         Agent  `json:"target"`
	Through        int64  `json:"through"`
	Body           string `json:"body"`
	Omitted        int    `json:"omitted"`
}
