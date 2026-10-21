package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aarashgoodarzi/task-manager/internal/task/domain"
)

// cursorVersion is the only payload version this binary accepts. A future
// change that cannot decode version 1 must bump it and reject older values
// rather than guessing.
const cursorVersion = 1

// cursorPayload is the JSON object encoded inside an opaque list cursor. The
// after_id is the exclusive lower bound for ORDER BY id. Status, assignee, and
// page_size are bound into the cursor so a client cannot silently continue a
// walk under a different filter or page size.
//
// Base64 makes the value opaque to clients; it is encoding, not encryption or
// authentication. Validation of the decoded fields is what enforces the
// contract.
type cursorPayload struct {
	V        int    `json:"v"`
	AfterID  string `json:"after_id"`
	Status   string `json:"status"`
	Assignee string `json:"assignee"`
	PageSize int    `json:"page_size"`
}

// EncodeCursor builds an opaque cursor that continues a list after afterID
// under the given normalized query. afterID must be a valid task identifier.
func EncodeCursor(afterID string, query ListQuery) (string, error) {
	if !domain.IsValidID(afterID) {
		return "", fmt.Errorf("service: encode cursor: invalid after id")
	}
	raw, err := json.Marshal(cursorPayload{
		V:        cursorVersion,
		AfterID:  afterID,
		Status:   string(query.Status),
		Assignee: query.Assignee,
		PageSize: query.PageSize,
	})
	if err != nil {
		return "", fmt.Errorf("service: encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeCursor parses an opaque cursor and returns the exclusive after-id bound.
// The decoded filter and page size must match query; a mismatch is a client
// error, not a silent first page. Malformed input is also a client error.
func DecodeCursor(cursor string, query ListQuery) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var payload cursorPayload
	if err := dec.Decode(&payload); err != nil {
		return "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}
	// Reject trailing junk after the single JSON object.
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}

	if payload.V != cursorVersion {
		return "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}
	if !domain.IsValidID(payload.AfterID) {
		return "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}
	if payload.PageSize < 1 || payload.PageSize > MaxPageSize {
		return "", &domain.ValidationError{Field: "cursor", Message: "must be a valid cursor"}
	}

	if domain.Status(payload.Status) != query.Status ||
		payload.Assignee != query.Assignee ||
		payload.PageSize != query.PageSize {
		return "", &domain.ValidationError{
			Field:   "cursor",
			Message: "is incompatible with the current query",
		}
	}

	return payload.AfterID, nil
}
