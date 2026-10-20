package metrics

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// gatherFamily returns one metric family from this instance's private registry.
// Tests never touch the global default registry, so they cannot interfere with
// each other or with the process under test.
func gatherFamily(t *testing.T, m *Metrics, name string) *dto.MetricFamily {
	t.Helper()

	family := findFamily(t, m, name)
	if family == nil {
		t.Fatalf("metric family %q is not exposed", name)
	}
	return family
}

func findFamily(t *testing.T, m *Metrics, name string) *dto.MetricFamily {
	t.Helper()

	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() == name {
			return family
		}
	}
	return nil
}

func TestObserveRequestCountsSuccessfulAndFailedRequests(t *testing.T) {
	m := New()

	m.ObserveRequest(http.MethodGet, "/tasks", http.StatusOK, 12*time.Millisecond)
	m.ObserveRequest(http.MethodPost, "/tasks", http.StatusBadRequest, 3*time.Millisecond)

	if got := testutil.ToFloat64(m.requestsTotal.WithLabelValues("GET", "/tasks", "200")); got != 1 {
		t.Fatalf("GET /tasks 200 counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.requestsTotal.WithLabelValues("POST", "/tasks", "400")); got != 1 {
		t.Fatalf("POST /tasks 400 counter = %v, want 1", got)
	}
}

func TestObserveRequestRecordsLatencyInSeconds(t *testing.T) {
	m := New()
	m.ObserveRequest(http.MethodGet, "/tasks", http.StatusOK, 250*time.Millisecond)

	family := gatherFamily(t, m, RequestLatencyHistogramName)
	if len(family.Metric) != 1 {
		t.Fatalf("histogram series = %d, want 1", len(family.Metric))
	}

	histogram := family.Metric[0].GetHistogram()
	if histogram.GetSampleCount() != 1 {
		t.Fatalf("sample count = %d, want 1", histogram.GetSampleCount())
	}
	if sum := histogram.GetSampleSum(); math.Abs(sum-0.25) > 1e-9 {
		t.Fatalf("sample sum = %v, want 0.25 seconds", sum)
	}

	var atQuarter, belowQuarter uint64
	for _, bucket := range histogram.Bucket {
		switch bucket.GetUpperBound() {
		case 0.1:
			belowQuarter = bucket.GetCumulativeCount()
		case 0.25:
			atQuarter = bucket.GetCumulativeCount()
		}
	}
	if belowQuarter != 0 {
		t.Fatalf("le=0.1 bucket = %d, want 0 for a 250ms request", belowQuarter)
	}
	if atQuarter != 1 {
		t.Fatalf("le=0.25 bucket = %d, want 1 for a 250ms request", atQuarter)
	}
}

func TestLabelsAreBounded(t *testing.T) {
	m := New()

	// A non-standard verb and an empty (unmatched) route must not create labels
	// chosen by a client.
	m.ObserveRequest("BREW", "", http.StatusNotFound, time.Millisecond)

	if got := testutil.ToFloat64(m.requestsTotal.WithLabelValues(OtherMethod, UnmatchedRoute, "404")); got != 1 {
		t.Fatalf("normalized counter = %v, want 1", got)
	}
	if got := testutil.CollectAndCount(m.requestsTotal); got != 1 {
		t.Fatalf("series = %d, want exactly one bounded series", got)
	}
}

func TestObserveRequestIgnoresTheMetricsPath(t *testing.T) {
	m := New()

	m.ObserveRequest(http.MethodGet, Path, http.StatusOK, time.Millisecond)

	if got := testutil.CollectAndCount(m.requestsTotal); got != 0 {
		t.Fatalf("series = %d, want scrape requests excluded", got)
	}
}

func TestHandlerServesPrometheusExposition(t *testing.T) {
	m := New()
	m.ObserveRequest(http.MethodGet, "/tasks", http.StatusOK, time.Millisecond)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	contentType := rec.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") || !strings.Contains(contentType, "version=0.0.4") {
		t.Fatalf("Content-Type = %q, want Prometheus text exposition", contentType)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"# TYPE " + RequestsTotalName + " counter",
		"# TYPE " + RequestLatencyHistogramName + " histogram",
		RequestsTotalName + `{method="GET",route="/tasks",status="200"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("exposition does not contain %q:\n%s", want, body)
		}
	}
	// promhttp's own handler metrics would indicate the endpoint instruments
	// itself; they are deliberately not registered.
	if strings.Contains(body, "promhttp_metric_handler") {
		t.Fatalf("exposition instruments the metrics handler:\n%s", body)
	}
}

func TestHandlerScrapesDoNotCountAsRequests(t *testing.T) {
	m := New()

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))
	}

	if got := testutil.CollectAndCount(m.requestsTotal); got != 0 {
		t.Fatalf("request series after scrapes = %d, want 0", got)
	}
}

func TestInstancesHaveIsolatedRegistries(t *testing.T) {
	first := New()
	second := New()

	first.ObserveRequest(http.MethodGet, "/tasks", http.StatusOK, time.Millisecond)

	if got := testutil.CollectAndCount(first.requestsTotal); got != 1 {
		t.Fatalf("first registry series = %d, want 1", got)
	}
	if got := testutil.CollectAndCount(second.requestsTotal); got != 0 {
		t.Fatalf("second registry series = %d, want 0", got)
	}
}
