package web

import (
	"html/template"
	"testing"
)

// TestTemplatesParse mirrors the parse done in NewServer so a malformed
// template fails CI/build-time instead of panicking on first request.
func TestTemplatesParse(t *testing.T) {
	if _, err := template.ParseFS(webFS, "templates/*.html"); err != nil {
		t.Fatalf("template parse failed: %v", err)
	}
}
