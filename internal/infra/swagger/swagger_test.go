package swagger

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/aarashgoodarzi/task-manager/api"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func newTestRouter() *gin.Engine {
	r := gin.New()
	r.HandleMethodNotAllowed = true
	New().RegisterRoutes(r)
	return r
}

func serve(r http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	r.ServeHTTP(rec, req)
	return rec
}

func TestOpenAPIIsServed(t *testing.T) {
	rec := serve(newTestRouter(), http.MethodGet, SpecPath)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != specContentType {
		t.Fatalf("Content-Type = %q, want %q", got, specContentType)
	}
	if !bytes.Equal(rec.Body.Bytes(), api.OpenAPI) {
		t.Fatal("served document differs from the embedded contract")
	}
	if !strings.HasPrefix(rec.Body.String(), "openapi: 3.0.3") {
		t.Fatalf("document does not start with an OpenAPI version: %q", rec.Body.String()[:40])
	}
	for _, path := range []string{"/tasks", "/health"} {
		if !strings.Contains(rec.Body.String(), path) {
			t.Fatalf("document does not mention %q", path)
		}
	}
	// Creation is documented on /task, and only there; the bare substring
	// check above cannot distinguish the two paths.
	if !strings.Contains(rec.Body.String(), "\n  /task:\n") {
		t.Fatal("document does not define the creation path /task")
	}
}

func TestSwaggerUIIsServedAtBasePath(t *testing.T) {
	rec := serve(newTestRouter(), http.MethodGet, UIPath)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("Content-Type = %q, want HTML", got)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"Task Manager API",
		"/swagger/openapi.yaml",
		"/swagger/swagger-ui.css",
		"/swagger/swagger-ui-bundle.js",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("UI does not reference %q", want)
		}
	}
	for _, external := range []string{"cdn.jsdelivr.net", "unpkg.com"} {
		if strings.Contains(body, external) {
			t.Fatalf("UI references external asset host %q", external)
		}
	}
}

func TestSwaggerUIIndexAlias(t *testing.T) {
	rec := serve(newTestRouter(), http.MethodGet, UIPath+"index.html")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Task Manager API") {
		t.Fatal("index alias did not serve the Swagger UI")
	}
}

func TestSwaggerAssetsAreEmbedded(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		contentType string
	}{
		{name: "css", path: UIPath + "swagger-ui.css", contentType: "text/css"},
		{name: "javascript", path: UIPath + "swagger-ui-bundle.js", contentType: "javascript"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(newTestRouter(), http.MethodGet, tc.path)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, tc.contentType) {
				t.Fatalf("Content-Type = %q, want %q", got, tc.contentType)
			}
			if rec.Body.Len() == 0 {
				t.Fatal("asset body is empty")
			}
		})
	}
}

func TestSwaggerRoutesAreReadOnly(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "spec", path: SpecPath},
		{name: "index", path: UIPath},
		{name: "index alias", path: UIPath + "index.html"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := serve(newTestRouter(), http.MethodPost, tc.path)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestUnknownSwaggerAssetIsNotFound(t *testing.T) {
	rec := serve(newTestRouter(), http.MethodGet, UIPath+"does-not-exist")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}

func TestSwaggerBasePathRedirectsToSlash(t *testing.T) {
	rec := serve(newTestRouter(), http.MethodGet, "/swagger")

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if got, want := rec.Header().Get("Location"), UIPath; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
}
