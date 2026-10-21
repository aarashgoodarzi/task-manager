package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
)

func TestEncodeDecodeCursorRoundTrip(t *testing.T) {
	query := ListQuery{
		Status:   domain.StatusDone,
		Assignee: "alice",
		PageSize: 10,
	}
	afterID := "6f1c2a4e-8d3b-4f5a-9c7e-1a2b3c4d5e6f"

	cursor, err := EncodeCursor(afterID, query)
	if err != nil {
		t.Fatalf("EncodeCursor() unexpected error: %v", err)
	}
	if cursor == "" || strings.Contains(cursor, afterID) {
		t.Fatalf("cursor = %q, want an opaque non-empty token", cursor)
	}

	got, err := DecodeCursor(cursor, query)
	if err != nil {
		t.Fatalf("DecodeCursor() unexpected error: %v", err)
	}
	if got != afterID {
		t.Fatalf("after id = %q, want %q", got, afterID)
	}
}

func TestDecodeCursorRejectsMalformed(t *testing.T) {
	query := ListQuery{PageSize: DefaultPageSize}
	validID := "6f1c2a4e-8d3b-4f5a-9c7e-1a2b3c4d5e6f"

	wrongVersion, err := json.Marshal(cursorPayload{
		V: 99, AfterID: validID, PageSize: DefaultPageSize,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	tests := []struct {
		name   string
		cursor string
	}{
		{name: "not base64", cursor: "%%%"},
		{name: "not json", cursor: base64.RawURLEncoding.EncodeToString([]byte("nope"))},
		{name: "wrong version", cursor: base64.RawURLEncoding.EncodeToString(wrongVersion)},
		{name: "invalid after id", cursor: mustEncodePayload(t, cursorPayload{
			V: cursorVersion, AfterID: "not-a-uuid", PageSize: DefaultPageSize,
		})},
		{name: "unknown field", cursor: base64.RawURLEncoding.EncodeToString([]byte(
			`{"v":1,"after_id":"6f1c2a4e-8d3b-4f5a-9c7e-1a2b3c4d5e6f","status":"","assignee":"","page_size":20,"extra":true}`,
		))},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeCursor(tc.cursor, query)
			var validation *domain.ValidationError
			if !errors.As(err, &validation) || validation.Field != "cursor" {
				t.Fatalf("DecodeCursor() error = %v, want cursor validation error", err)
			}
			if validation.Message != "must be a valid cursor" {
				t.Fatalf("message = %q, want malformed-cursor wording", validation.Message)
			}
		})
	}
}

func TestDecodeCursorRejectsIncompatibleQuery(t *testing.T) {
	base := ListQuery{Status: domain.StatusTodo, Assignee: "alice", PageSize: 5}
	cursor, err := EncodeCursor("6f1c2a4e-8d3b-4f5a-9c7e-1a2b3c4d5e6f", base)
	if err != nil {
		t.Fatalf("EncodeCursor() unexpected error: %v", err)
	}

	tests := []struct {
		name  string
		query ListQuery
	}{
		{name: "different status", query: ListQuery{Status: domain.StatusDone, Assignee: "alice", PageSize: 5}},
		{name: "different assignee", query: ListQuery{Status: domain.StatusTodo, Assignee: "bob", PageSize: 5}},
		{name: "different page size", query: ListQuery{Status: domain.StatusTodo, Assignee: "alice", PageSize: 10}},
		{name: "missing filter", query: ListQuery{PageSize: 5}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodeCursor(cursor, tc.query)
			var validation *domain.ValidationError
			if !errors.As(err, &validation) || validation.Field != "cursor" {
				t.Fatalf("DecodeCursor() error = %v, want cursor validation error", err)
			}
			if validation.Message != "is incompatible with the current query" {
				t.Fatalf("message = %q, want incompatibility wording", validation.Message)
			}
		})
	}
}

func TestNormalizeEmptyCursorIsFirstPage(t *testing.T) {
	got, err := ListQuery{}.Normalize()
	if err != nil {
		t.Fatalf("Normalize() unexpected error: %v", err)
	}
	if got.AfterID != "" || got.Cursor != "" || got.PageSize != DefaultPageSize {
		t.Fatalf("got = %+v, want first-page defaults", got)
	}
}

func TestNormalizeBindsDecodedCursor(t *testing.T) {
	afterID := "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	query := ListQuery{Status: domain.StatusDone, Assignee: "alice", PageSize: 2}
	cursor, err := EncodeCursor(afterID, query)
	if err != nil {
		t.Fatalf("EncodeCursor() unexpected error: %v", err)
	}

	got, err := ListQuery{Status: domain.StatusDone, Assignee: "alice", PageSize: 2, Cursor: cursor}.Normalize()
	if err != nil {
		t.Fatalf("Normalize() unexpected error: %v", err)
	}
	if got.AfterID != afterID {
		t.Fatalf("AfterID = %q, want %q", got.AfterID, afterID)
	}
}

func mustEncodePayload(t *testing.T, payload cursorPayload) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
