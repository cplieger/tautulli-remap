package main

import (
	"os"
	"strings"
	"testing"
)

// docPages are the pages that describe the health file or the tmpfs.
var docPages = []string{"README.md", "docs/configuration.md", "docs/hardening.md", "docs/how-it-works.md", "docs/monitoring.md", "compose.yaml"}

// TestDocs_name_the_health_file_the_app_writes pins the health file the docs
// name to the one the resident process writes and the health subcommand
// reads. The run lock keeps /tmp, so a page may name /tmp, but a sentence
// that names /tmp and health must name the health file's own path too.
func TestDocs_name_the_health_file_the_app_writes(t *testing.T) {
	raw, err := os.ReadFile("docs/monitoring.md")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	_, section, found := strings.Cut(string(raw), "\n## Knowing the app is alive\n")
	if !found {
		t.Fatal("docs/monitoring.md has no Knowing the app is alive section")
	}
	section, _, _ = strings.Cut(section, "\n## ")
	if !strings.Contains(section, "`"+markerPath+"`") {
		t.Errorf("docs/monitoring.md Knowing the app is alive does not name the health file %s", markerPath)
	}
	for _, path := range docPages {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		text := strings.Join(strings.Fields(string(raw)), " ")
		for sentence := range strings.SplitSeq(text, ". ") {
			if strings.Contains(sentence, "/tmp") && strings.Contains(strings.ToLower(sentence), "health") && !strings.Contains(sentence, markerPath) {
				t.Errorf("%s puts the health file in /tmp: %q", path, sentence)
			}
		}
	}
}
