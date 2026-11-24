package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDashboardExportWithRSCAssetDirectoryAndSiblingHTML(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "onboarding"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"onboarding.html": "<html>onboarding</html>",
		"onboarding/__next._full.txt": "next payload",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	handler := withDashboardAssets(http.NotFoundHandler(), root)
	for path, want := range map[string]string{
		"/ui/onboarding?token=fixture": "<html>onboarding</html>",
		"/ui/onboarding/__next._full.txt": "next payload",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Fatalf("%s returned %d %q", path, response.Code, response.Body.String())
		}
	}
}
