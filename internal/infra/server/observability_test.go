package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/aarashgoodarzi/task-manager/internal/infra/metrics"
)

type observation struct {
	method   string
	route    string
	status   int
	duration time.Duration
}

// recordingInstrumentation is a hand-written fake, matching the repository's
// habit of declaring and faking narrow ports instead of using a mock library.
type recordingInstrumentation struct {
	mu           sync.Mutex
	observations []observation
}

func (r *recordingInstrumentation) ObserveRequest(method, route string, status int, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.observations = append(r.observations, observation{
		method:   method,
		route:    route,
		status:   status,
		duration: duration,
	})
}

func (r *recordingInstrumentation) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(w, "metric_stub 1\n")
	})
}

func (r *recordingInstrumentation) recorded() []observation {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]observation, len(r.observations))
	copy(out, r.observations)
	return out
}

type routeRegistrarFunc func(r gin.IRouter)

func (f routeRegistrarFunc) RegisterRoutes(r gin.IRouter) {
	f(r)
}

func TestMetricsEndpointIsServedWithoutInstrumentingItself(t *testing.T) {
	instr := &recordingInstrumentation{}
	s := newTestServerWith(t, nil, instr, discardLogger())

	rec := serve(s, http.MethodGet, metrics.Path)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "metric_stub") {
		t.Fatalf("body = %q, want the instrumentation handler's output", rec.Body.String())
	}
	if got := instr.recorded(); len(got) != 0 {
		t.Fatalf("scrape was instrumented: %+v", got)
	}
}

func TestMetricsEndpointIsAbsentWithoutInstrumentation(t *testing.T) {
	rec := serve(newTestServer(t, nil), http.MethodGet, metrics.Path)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no instrumentation is wired", rec.Code)
	}
}

func TestRequestMetricsUseRegisteredRoutePattern(t *testing.T) {
	const concretePath = "/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8"

	instr := &recordingInstrumentation{}
	s := newTestServerWith(t, nil, instr, discardLogger(), routeRegistrarFunc(func(r gin.IRouter) {
		r.GET("/tasks/:id", func(c *gin.Context) {
			c.Status(http.StatusNoContent)
		})
	}))

	rec := serve(s, http.MethodGet, concretePath)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	got := instr.recorded()
	if len(got) != 1 {
		t.Fatalf("observations = %d, want 1", len(got))
	}
	if got[0].route != "/tasks/:id" {
		t.Fatalf("route label = %q, want the registered pattern", got[0].route)
	}
	if strings.Contains(got[0].route, "6ba7b810") {
		t.Fatalf("route label %q contains the concrete id", got[0].route)
	}
	if got[0].method != http.MethodGet || got[0].status != http.StatusNoContent {
		t.Fatalf("observation = %+v, want GET/204", got[0])
	}
	if got[0].duration < 0 {
		t.Fatalf("duration = %v, want a non-negative measurement", got[0].duration)
	}
}

func TestUnmatchedRequestsShareOneBoundedRouteLabel(t *testing.T) {
	instr := &recordingInstrumentation{}
	s := newTestServerWith(t, nil, instr, discardLogger())

	for _, path := range []string{"/one", "/two/three"} {
		if rec := serve(s, http.MethodGet, path); rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404", path, rec.Code)
		}
	}
	// A known path with an unsupported method is also an unmatched route.
	if rec := serve(s, http.MethodPut, "/health"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /health status = %d, want 405", rec.Code)
	}

	got := instr.recorded()
	if len(got) != 3 {
		t.Fatalf("observations = %d, want 3", len(got))
	}
	for _, obs := range got {
		if obs.route != metrics.UnmatchedRoute {
			t.Fatalf("route label = %q, want %q", obs.route, metrics.UnmatchedRoute)
		}
	}
	if got[0].status != http.StatusNotFound || got[2].status != http.StatusMethodNotAllowed {
		t.Fatalf("statuses = %d, %d, want 404 and 405", got[0].status, got[2].status)
	}
}

func TestRequestLogIncludesCorrelationFields(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	instr := &recordingInstrumentation{}
	s := newTestServerWith(t, nil, instr, log, routeRegistrarFunc(func(r gin.IRouter) {
		r.GET("/tasks/:id", func(c *gin.Context) {
			c.Status(http.StatusNoContent)
		})
	}))

	rec := serve(s, http.MethodGet, "/tasks/6ba7b810-9dad-41d1-80b4-00c04fd430c8")
	id := rec.Header().Get(RequestIDHeader)

	entry := decodeLogEntry(t, &buf)
	if entry["msg"] != "request" {
		t.Fatalf("msg = %v, want request", entry["msg"])
	}
	if entry["request_id"] != id {
		t.Fatalf("logged request_id = %v, want response header %q", entry["request_id"], id)
	}
	if entry["route"] != "/tasks/:id" {
		t.Fatalf("logged route = %v, want the registered pattern", entry["route"])
	}
	if entry["status"] != float64(http.StatusNoContent) {
		t.Fatalf("logged status = %v, want 204", entry["status"])
	}
	if entry["method"] != http.MethodGet {
		t.Fatalf("logged method = %v, want GET", entry["method"])
	}
	if _, ok := entry["duration"]; !ok {
		t.Fatalf("log entry has no duration: %v", entry)
	}
}

func TestPanicIsLoggedAndObservedAsA500(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	// gin.Recovery prints a stack trace to this writer; keep the test output
	// readable without changing the recovery behavior itself.
	originalWriter := gin.DefaultErrorWriter
	gin.DefaultErrorWriter = io.Discard
	t.Cleanup(func() { gin.DefaultErrorWriter = originalWriter })

	instr := &recordingInstrumentation{}
	s := newTestServerWith(t, nil, instr, log, routeRegistrarFunc(func(r gin.IRouter) {
		r.GET("/panic", func(*gin.Context) {
			panic("boom")
		})
	}))

	rec := serve(s, http.MethodGet, "/panic")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	id := rec.Header().Get(RequestIDHeader)
	if !validRequestID(id) {
		t.Fatalf("response request id = %q, want a generated valid id", id)
	}

	got := instr.recorded()
	if len(got) != 1 {
		t.Fatalf("observations = %d, want the panicked request to be observed", len(got))
	}
	if got[0].status != http.StatusInternalServerError || got[0].route != "/panic" {
		t.Fatalf("observation = %+v, want 500 on /panic", got[0])
	}

	entry := decodeLogEntry(t, &buf)
	if entry["request_id"] != id {
		t.Fatalf("logged request_id = %v, want %q", entry["request_id"], id)
	}
	if entry["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("logged status = %v, want 500", entry["status"])
	}
	if entry["panic"] != true {
		t.Fatalf("logged panic = %v, want true", entry["panic"])
	}
	if _, ok := entry["duration"]; !ok {
		t.Fatalf("panic log entry has no duration: %v", entry)
	}
}

// decodeLogEntry parses the last JSON log line. The middleware writes one line
// per request; parsing the last line keeps the helper independent of any setup
// messages.
func decodeLogEntry(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		t.Fatalf("no log output; logs = %q", buf.String())
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("decode log line: %v (line = %q)", err, lines[len(lines)-1])
	}
	return entry
}
