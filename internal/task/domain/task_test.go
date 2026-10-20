package domain

import (
	"errors"
	"testing"
)

func TestParseStatus(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		want      Status
		wantField string
	}{
		{name: "empty selects default", raw: "", want: StatusTodo},
		{name: "whitespace selects default", raw: "   ", want: StatusTodo},
		{name: "todo", raw: "todo", want: StatusTodo},
		{name: "in progress", raw: "in_progress", want: StatusInProgress},
		{name: "done", raw: "done", want: StatusDone},
		{name: "surrounding whitespace is tolerated", raw: " done ", want: StatusDone},
		{name: "unknown value", raw: "pending", wantField: "status"},
		{name: "values are case sensitive", raw: "TODO", wantField: "status"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseStatus(tc.raw)
			if tc.wantField != "" {
				var verr *ValidationError
				if !errors.As(err, &verr) {
					t.Fatalf("ParseStatus(%q) error = %v, want *ValidationError", tc.raw, err)
				}
				if verr.Field != tc.wantField {
					t.Fatalf("error field = %q, want %q", verr.Field, tc.wantField)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseStatus(%q) unexpected error: %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("ParseStatus(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestStatusValid(t *testing.T) {
	for _, status := range []Status{StatusTodo, StatusInProgress, StatusDone} {
		if !status.Valid() {
			t.Errorf("Status(%q).Valid() = false, want true", status)
		}
	}
	for _, status := range []Status{"", "pending", "DONE", "in-progress"} {
		if status.Valid() {
			t.Errorf("Status(%q).Valid() = true, want false", status)
		}
	}
}

func TestTaskValidate(t *testing.T) {
	tests := []struct {
		name      string
		task      Task
		wantField string
	}{
		{
			name: "valid task",
			task: Task{Title: "write tests", Status: StatusTodo},
		},
		{
			name: "valid task with description",
			task: Task{Title: "write tests", Description: "cover the service", Status: StatusInProgress},
		},
		{
			name:      "empty title",
			task:      Task{Title: "", Status: StatusTodo},
			wantField: "title",
		},
		{
			name:      "whitespace title",
			task:      Task{Title: " \t\n ", Status: StatusTodo},
			wantField: "title",
		},
		{
			name:      "invalid status",
			task:      Task{Title: "write tests", Status: "pending"},
			wantField: "status",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.task.Validate()
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("Validate() unexpected error: %v", err)
				}
				return
			}

			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Validate() error = %v, want *ValidationError", err)
			}
			if verr.Field != tc.wantField {
				t.Fatalf("error field = %q, want %q", verr.Field, tc.wantField)
			}
		})
	}
}

func TestValidationErrorMessage(t *testing.T) {
	err := &ValidationError{Field: "title", Message: "title is required"}
	if got, want := err.Error(), "title: title is required"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestNewIDGeneratesUniqueVersion4UUIDs(t *testing.T) {
	seen := make(map[string]struct{}, 100)

	for i := 0; i < 100; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID() unexpected error: %v", err)
		}
		if !IsValidID(id) {
			t.Fatalf("NewID() = %q, want a canonical UUID", id)
		}
		if id[14] != '4' {
			t.Fatalf("NewID() = %q, want version 4", id)
		}
		if variant := id[19]; variant != '8' && variant != '9' && variant != 'a' && variant != 'b' {
			t.Fatalf("NewID() = %q, want the RFC 4122 variant", id)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("NewID() returned duplicate %q", id)
		}
		seen[id] = struct{}{}
	}
}

func TestIsValidID(t *testing.T) {
	valid := []string{
		"6ba7b810-9dad-41d1-80b4-00c04fd430c8",
		"6BA7B810-9DAD-41D1-80B4-00C04FD430C8",
	}
	invalid := []string{
		"",
		"not-a-uuid",
		"6ba7b8109dad41d180b400c04fd430c8",
		"6ba7b810-9dad-41d1-80b4-00c04fd430c",
		"6ba7b810-9dad-41d1-80b4-00c04fd430c8-extra",
		" 6ba7b810-9dad-41d1-80b4-00c04fd430c8",
	}

	for _, id := range valid {
		if !IsValidID(id) {
			t.Errorf("IsValidID(%q) = false, want true", id)
		}
	}
	for _, id := range invalid {
		if IsValidID(id) {
			t.Errorf("IsValidID(%q) = true, want false", id)
		}
	}
}
