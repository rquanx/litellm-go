package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRobotsCompatibilityRoute(t *testing.T) {
	handler := withRobots(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	w := httptest.NewRecorder(); handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "User-agent: *\nDisallow: /\n" { t.Fatalf("unexpected robots response %d %q", w.Code, w.Body.String()) }
}
