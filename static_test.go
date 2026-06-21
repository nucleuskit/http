package runtimehttp

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerHandleStaticServesFilesOutsideJSONEnvelope(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "app.txt"), "static asset")

	server := NewServer(WithMiddleware(RequestID()))
	server.HandleStatic("/assets/", dir)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/app.txt", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if body := recorder.Body.String(); body != "static asset" {
		t.Fatalf("expected raw static body, got %q", body)
	}
	if strings.Contains(recorder.Body.String(), `"code"`) {
		t.Fatalf("static response should not use JSON envelope: %s", recorder.Body.String())
	}
	if recorder.Header().Get(requestIDHeader) == "" {
		t.Fatal("expected server middleware to still apply")
	}
}

func TestServerHandleStaticRejectsPrefixEscape(t *testing.T) {
	root := t.TempDir()
	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(public, "app.txt"), "public")
	writeTestFile(t, filepath.Join(root, "secret.txt"), "secret")

	server := NewServer()
	server.HandleStatic("/assets/", public)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/assets/../secret.txt", nil))

	if recorder.Code == http.StatusOK || strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("static file server escaped prefix: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestWithFileServerRegistersStaticFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "asset.txt"), "from option")

	server := NewServer(WithFileServer("/files/", dir))
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/files/asset.txt", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Body.String() != "from option" {
		t.Fatalf("unexpected body: %q", recorder.Body.String())
	}
}

func writeTestFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
