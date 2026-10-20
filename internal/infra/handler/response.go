// Package handler contains shared HTTP delivery code. It defines the single
// JSON error envelope used by the HTTP adapters, so every API error has the
// same shape.
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Error codes used in response bodies. They are stable identifiers clients can
// branch on; the HTTP status code carries the same information at a coarser
// level.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeValidationFailed = "validation_error"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternal         = "internal_error"
)

// Response is the envelope for every non-2xx API response.
type Response struct {
	Error Detail `json:"error"`
}

// Detail describes one error. Fields is present only for validation failures
// and maps a field name to a human-readable reason.
type Detail struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// Write aborts the request and writes the error envelope with the given status.
func Write(c *gin.Context, status int, code, message string, fields map[string]string) {
	c.AbortWithStatusJSON(status, Response{
		Error: Detail{Code: code, Message: message, Fields: fields},
	})
}

// Internal is a convenience for the only status whose message must never
// include the underlying error.
func Internal(c *gin.Context) {
	Write(c, http.StatusInternalServerError, CodeInternal, "internal server error", nil)
}
