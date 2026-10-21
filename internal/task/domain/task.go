// Package domain defines the Task aggregate: its fields, the allowed status
// values, the identifier format, and the invariants the rest of the service
// relies on. It imports nothing from the HTTP, storage, or configuration
// layers.
package domain

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Status is the lifecycle state of a task. The set is closed: any value not
// listed below is rejected by the API.
type Status string

const (
	StatusTodo       Status = "todo"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
)

var validStatuses = map[Status]struct{}{
	StatusTodo:       {},
	StatusInProgress: {},
	StatusDone:       {},
}

// Valid reports whether s is one of the defined statuses.
func (s Status) Valid() bool {
	_, ok := validStatuses[s]
	return ok
}

// ParseStatus converts an untrusted request value into a Status. An empty
// value selects StatusTodo, the default for newly created tasks. Any other
// value must match one of the defined statuses exactly.
func ParseStatus(raw string) (Status, error) {
	status := Status(strings.TrimSpace(raw))
	if status == "" {
		return StatusTodo, nil
	}
	if !status.Valid() {
		return "", &ValidationError{
			Field:   "status",
			Message: `status must be one of "todo", "in_progress", "done"`,
		}
	}
	return status, nil
}

// MaxAssigneeLength bounds the free-form assignee label. It is part of the
// aggregate's contract, so the domain, the list filter, and the database check
// constraint agree on the same limit.
const MaxAssigneeLength = 100

// ParseStatusFilter converts an optional list filter into a Status. Unlike
// ParseStatus, an empty value means "any status" and returns the zero Status
// rather than the write default, so a missing filter cannot silently select
// todo only.
func ParseStatusFilter(raw string) (Status, error) {
	status := Status(strings.TrimSpace(raw))
	if status == "" {
		return "", nil
	}
	if !status.Valid() {
		return "", &ValidationError{
			Field:   "status",
			Message: `status must be one of "todo", "in_progress", "done"`,
		}
	}
	return status, nil
}

// Task is a single unit of work owned by the service.
type Task struct {
	ID          string
	Title       string
	Description string
	Status      Status
	// Assignee is a free-form label naming who owns the task. It is stored
	// trimmed and empty means unassigned.
	Assignee string
}

// Validate checks the aggregate invariants. Titles are trimmed before the
// check, so a title of only whitespace is invalid.
func (t Task) Validate() error {
	if strings.TrimSpace(t.Title) == "" {
		return &ValidationError{Field: "title", Message: "title is required"}
	}
	if !t.Status.Valid() {
		return &ValidationError{
			Field:   "status",
			Message: `status must be one of "todo", "in_progress", "done"`,
		}
	}
	if utf8.RuneCountInString(t.Assignee) > MaxAssigneeLength {
		return &ValidationError{
			Field:   "assignee",
			Message: fmt.Sprintf("assignee must be at most %d characters", MaxAssigneeLength),
		}
	}
	return nil
}

// ValidationError reports a single invalid field. Transport adapters map it to
// a 400 response; the service returns it unchanged.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

// idPattern matches the canonical textual form of a UUID (8-4-4-4-12 hex
// digits), case-insensitively. IDs are generated as version 4 UUIDs; the
// pattern is deliberately limited to the shape the API can produce and accept.
var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// IsValidID reports whether id is syntactically a UUID. IDs with any other
// shape cannot exist in the store, so the HTTP layer rejects them as invalid
// input rather than reporting them as missing.
func IsValidID(id string) bool {
	return idPattern.MatchString(id)
}

// NewID returns a new random RFC 4122 version 4 UUID in canonical form.
//
// Identity is part of the aggregate's contract (its format and validity), so
// the generator lives here rather than in each driven adapter. It uses the
// standard library only and does not touch storage.
func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("domain: generate task id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
