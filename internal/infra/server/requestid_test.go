package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// echoRequestID registers a route that returns the identifier visible in the
// Gin context, so a test can compare what downstream processing sees with what
// the response carries.
func echoRequestID() RouteRegistrar {
	return routeRegistrarFunc(func(r gin.IRouter) {
		r.GET("/echo", func(c *gin.Context) {
			c.String(http.StatusOK, RequestID(c))
		})
	})
}

func TestRequestIDIsGeneratedWhenAbsent(t *testing.T) {
	rec := serve(newTestServer(t, nil, echoRequestID()), http.MethodGet, "/echo")

	id := rec.Header().Get(RequestIDHeader)
	if !validRequestID(id) {
		t.Fatalf("response %s = %q, want a generated valid id", RequestIDHeader, id)
	}
	if len(id) != 32 {
		t.Fatalf("generated id length = %d, want 32 hex characters: %q", len(id), id)
	}
	if strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("generated id = %q, want lowercase hexadecimal", id)
	}
	if body := rec.Body.String(); body != id {
		t.Fatalf("context id = %q, response header = %q; they must match", body, id)
	}
}

func TestGeneratedRequestIDsAreUnique(t *testing.T) {
	s := newTestServer(t, nil)

	first := serve(s, http.MethodGet, "/health/live").Header().Get(RequestIDHeader)
	second := serve(s, http.MethodGet, "/health/live").Header().Get(RequestIDHeader)

	if first == "" || second == "" {
		t.Fatalf("request ids = %q, %q; want two generated values", first, second)
	}
	if first == second {
		t.Fatalf("two requests received the same id %q", first)
	}
}

func TestValidIncomingRequestIDIsPropagated(t *testing.T) {
	const incoming = "trace-01:abc.DEF_9"

	s := newTestServer(t, nil, echoRequestID())
	rec := serveWithHeaders(s, http.MethodGet, "/echo", map[string]string{RequestIDHeader: incoming})

	if got := rec.Header().Get(RequestIDHeader); got != incoming {
		t.Fatalf("response %s = %q, want the incoming value", RequestIDHeader, got)
	}
	if body := rec.Body.String(); body != incoming {
		t.Fatalf("context id = %q, want %q", body, incoming)
	}
}

func TestInvalidIncomingRequestIDsAreReplaced(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "space", value: "not a request id"},
		{name: "slash", value: "trace/with/slashes"},
		{name: "semicolon", value: "trace;drop"},
		{name: "newline", value: "trace\ninjected"},
		{name: "leading punctuation", value: "-starts-with-a-separator"},
		{name: "too long", value: strings.Repeat("a", maxRequestIDLength+1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t, nil, echoRequestID())
			rec := serveWithHeaders(s, http.MethodGet, "/echo", map[string]string{RequestIDHeader: tc.value})

			got := rec.Header().Get(RequestIDHeader)
			if got == tc.value {
				t.Fatalf("invalid incoming id %q was propagated", tc.value)
			}
			if !validRequestID(got) {
				t.Fatalf("replacement id %q is not valid", got)
			}
			if body := rec.Body.String(); body != got {
				t.Fatalf("context id = %q, response header = %q", body, got)
			}
		})
	}
}

func TestRequestIDHeaderIsPresentOnErrorResponses(t *testing.T) {
	rec := serve(newTestServer(t, nil), http.MethodGet, "/unknown")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if id := rec.Header().Get(RequestIDHeader); !validRequestID(id) {
		t.Fatalf("404 response %s = %q, want a valid id", RequestIDHeader, id)
	}
}

func TestValidRequestID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{id: "abc", want: true},
		{id: "ABC-123", want: true},
		{id: "trace.01_abc:def", want: true},
		{id: strings.Repeat("a", maxRequestIDLength), want: true},
		{id: "", want: false},
		{id: "-leading", want: false},
		{id: "with space", want: false},
		{id: "with/slash", want: false},
		{id: strings.Repeat("a", maxRequestIDLength+1), want: false},
		{id: "ümlaut", want: false},
	}

	for _, tc := range tests {
		if got := validRequestID(tc.id); got != tc.want {
			t.Errorf("validRequestID(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestRequestIDWithoutMiddleware(t *testing.T) {
	if got := RequestID(nil); got != "" {
		t.Fatalf("RequestID(nil) = %q, want empty", got)
	}
}

// serveWithHeaders is serve with request headers, for propagation tests.
func serveWithHeaders(s *Server, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	s.buildRouter().ServeHTTP(rec, req)
	return rec
}
