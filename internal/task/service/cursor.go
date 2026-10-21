package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
)

// cursorVersion is the only payload version this binary accepts. Version 3
// binds both created_at and id for FIFO keyset pagination; version 1 (id-only
// ascending) and version 2 (LIFO) cursors are rejected rather than
// reinterpreted.
const cursorVersion = 3

// cursorPayload is the JSON object encoded inside an opaque list cursor. The
// after_created_at / after_id pair is the exclusive lower bound for
// ORDER BY created_at ASC, id ASC: the next page continues with rows
// strictly newer than that position. Status, assignee, and page_size are
// bound into the cursor so a client cannot silently continue a walk under a
// different filter or page size.
//
// Base64 makes the value opaque to clients; it is encoding, not encryption or
// authentication. Validation of the decoded fields is what enforces the
// contract.
type cursorPayload struct {
	V              int    `json:"v"`
	AfterCreatedAt string `json:"after_created_at"`
	AfterID        string `json:"after_id"`
	Status         string `json:"status"`
	Assignee       string `json:"assignee"`
	PageSize       int    `json:"page_size"`
}

// EncodeCursor builds an opaque cursor that continues a FIFO list after the
// given created_at/id position under the normalized query. afterID must be a
// valid task identifier; createdAt must be non-zero.
func EncodeCursor(createdAt time.Time, afterID string, query ListQuery) (string, error) {
	if createdAt.IsZero() {
		return "", fmt.Errorf("service: encode cursor: missing created_at")
	}
	if !domain.IsValidID(afterID) {
		return "", fmt.Errorf("service: encode cursor: invalid after id")
	}
	raw, err := json.Marshal(cursorPayload{
		V:              cursorVersion,
		AfterCreatedAt: createdAt.UTC().Format(time.RFC3339Nano),
		AfterID:        afterID,
		Status:         string(query.Status),
		Assignee:       query.Assignee,
		PageSize:       query.PageSize,
	})
	if err != nil {
		return "", fmt.Errorf("service: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCursor parses an opaque cursor and returns the exclusive FIFO keyset
// bound (created_at, id). The decoded filter and page size must match query;
// a mismatch is a client error, not a silent first page. Malformed input is
// also a client error.
func DecodeCursor(cursor string, query ListQuery) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var payload cursorPayload
	if err := dec.Decode(&payload); err != nil {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}
	// Reject trailing junk after the single JSON object.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}

	if payload.V != cursorVersion {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, payload.AfterCreatedAt)
	if err != nil || createdAt.IsZero() {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}
	if !domain.IsValidID(payload.AfterID) {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}
	if payload.PageSize < 1 || payload.PageSize > MaxPageSize {
		return time.Time{}, "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}

	if domain.Status(payload.Status) != query.Status ||
		payload.Assignee != query.Assignee ||
		payload.PageSize != query.PageSize {
		return time.Time{}, "", &domain.ValidationError{
			Field:   "cursor",
			Message: "is incompatible with the current query",
		}
	}

	return createdAt.UTC(), payload.AfterID, nil
}
