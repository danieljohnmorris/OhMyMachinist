package factory

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFactoryWorkflowDryRun(t *testing.T) {
	script, err := filepath.Abs("delivery.sh")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(script, "dry-run")
	command.Env = append(os.Environ(),
		"FACTORY_ISSUE_KEY=ABC-12", "FACTORY_ISSUE_TITLE=Add archive page",
		"FACTORY_PROMPT_FILE=pr-body.md", "FACTORY_REVIEW_COMMAND=review",
		"FACTORY_COMMENT_COMMAND=comment", "FACTORY_SCREENSHOT_DIR=",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("dry run: %v: %s", err, output)
	}
	text := string(output)
	for _, expected := range []string{"[dry-run] git switch", "[dry-run] adversarial review", "[dry-run] comment on"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("dry-run output missing %q: %s", expected, text)
		}
	}
	if strings.Contains(text, "merge") || strings.Contains(text, "deploy") {
		t.Fatalf("dry-run exposed a forbidden action: %s", text)
	}
}
