package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// RequestIDHeader is the request and response header carrying the
	// correlation identifier.
	RequestIDHeader = "X-Request-ID"
	// RequestIDContextKey is the Gin context key the identifier is stored
	// under. Downstream middleware and handlers read it with RequestID(c) or
	// c.GetString(RequestIDContextKey).
	RequestIDContextKey = "request_id"

	// maxRequestIDLength bounds the accepted value, so a client cannot force an
	// arbitrarily large identifier into every log line and response.
	maxRequestIDLength = 64
)

// requestIDPattern accepts short, log-safe, header-safe identifiers: letters,
// digits, and the separators commonly used by proxies and tracing formats
// (W3C trace context uses "-"). It excludes whitespace and control characters,
// which could otherwise forge log lines or split headers.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)

// RequestID returns the correlation identifier assigned to the current
// request, or "" when the middleware did not run.
func RequestID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return c.GetString(RequestIDContextKey)
}

// requestIDMiddleware propagates a valid incoming X-Request-ID or generates a
// new one, stores it in the Gin context, and echoes it on the response. The
// header is set before the handler runs, so it is present on success, error,
// and panic responses alike.
func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(RequestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
		}

		c.Set(RequestIDContextKey, id)
		c.Header(RequestIDHeader, id)
		c.Next()
	}
}

// validRequestID reports whether an incoming value is safe and bounded enough
// to propagate. Anything else is replaced rather than trusted.
func validRequestID(id string) bool {
	return len(id) > 0 && len(id) <= maxRequestIDLength && requestIDPattern.MatchString(id)
}

// fallbackRequestIDSeq distinguishes the identifiers generated when the
// entropy source is unavailable. Correlation IDs need to be unique, not
// unguessable, so a timestamp plus a counter is an acceptable degraded mode.
var fallbackRequestIDSeq atomic.Uint64

// newRequestID returns a random 128-bit identifier in lowercase hexadecimal.
// crypto/rand is already used for task identifiers; the same source avoids a
// second randomness dependency and keeps collisions negligible.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}

	// Never fail a request because entropy is unavailable; gin.Recovery would
	// turn that into a 500 for every call.
	return fmt.Sprintf("fallback-%x-%x", time.Now().UnixNano(), fallbackRequestIDSeq.Add(1))
}
