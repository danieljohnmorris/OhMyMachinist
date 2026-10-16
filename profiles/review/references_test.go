package profiles

import (
	"strings"
	"testing"
	"testing/fstest"
)

// TestSkillReferenced detects references in a readable library.
func TestSkillReferencedDetectsReferences(t *testing.T) {
	fsys := fstest.MapFS{
		"default.yaml": &fstest.MapFile{Data: []byte("reviewers:\n  - name: reviewer\n    skills: [\"feature-dev:code-reviewer\"]\n")},
	}
	referenced, err := skillReferenced(fsys, "feature-dev:code-reviewer")
	if err != nil {
		t.Fatalf("skillReferenced: %v", err)
	}
	if !referenced {
		t.Fatal("skillReferenced = false, want true")
	}
}

// TestSkillReferencedParseError fails on a profile whose YAML cannot be
// parsed, naming the file, so a broken profile is treated as "in use"
// rather than silently unreferenced.
func TestSkillReferencedParseErrorNamesFile(t *testing.T) {
	fsys := fstest.MapFS{
		"broken.yaml": &fstest.MapFile{Data: []byte("reviewers: [unterminated")},
	}
	_, err := skillReferenced(fsys, "some-skill")
	if err == nil {
		t.Fatal("skillReferenced parsed a broken profile without error")
	}
	if !strings.Contains(err.Error(), "broken.yaml") {
		t.Fatalf("skillReferenced error %q does not name the file", err)
	}
}
