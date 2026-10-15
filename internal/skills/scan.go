package skills

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// ScanResult reports the outcome of a secret scan of one staged skill version.
type ScanResult struct {
	// Scanned is true when the scanner actually ran. It is false when gitleaks
	// is unavailable, which the library records as scanned: false.
	Scanned bool
	// Findings holds a short, human-readable list of detected secrets.
	Findings []string
}

// Scanner scans a staged skill directory for secrets. The library rejects a
// version when the scan reports findings.
type Scanner func(ctx context.Context, dir string) (ScanResult, error)

// GitleaksScanner runs gitleaks over a staged directory. A missing binary
// returns Scanned=false so the caller can warn and continue; findings are
// reported when gitleaks exits non-zero for a leak.
func GitleaksScanner(ctx context.Context, dir string) (ScanResult, error) {
	binary, err := exec.LookPath("gitleaks")
	if err != nil {
		return ScanResult{Scanned: false}, nil
	}
	// gitleaks renamed `detect` to `dir` in v8.19; try the newer form first and
	// fall back on an unknown-command error.
	forms := [][]string{
		{"dir", dir, "--no-banner"},
		{"detect", "--no-git", "--source", dir, "--no-banner"},
	}
	var output string
	for _, form := range forms {
		command := exec.CommandContext(ctx, binary, form...)
		var buffer bytes.Buffer
		command.Stdout = &buffer
		command.Stderr = &buffer
		err := command.Run()
		if err == nil {
			return ScanResult{Scanned: true}, nil
		}
		output = buffer.String()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			switch exit.ExitCode() {
			case 1:
				if unknownCommand(output) {
					continue
				}
				return ScanResult{Scanned: true, Findings: scanFindings(output)}, nil
			case 2:
				if unknownCommand(output) {
					continue
				}
			}
		}
		return ScanResult{}, fmt.Errorf("gitleaks: %w: %s", err, strings.TrimSpace(output))
	}
	return ScanResult{}, fmt.Errorf("gitleaks could not be run: %s", strings.TrimSpace(output))
}

func unknownCommand(output string) bool {
	return strings.Contains(output, "unknown command") || strings.Contains(output, "unknown flag")
}

func scanFindings(output string) []string {
	var findings []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		findings = append(findings, trimmed)
		if len(findings) >= 20 {
			break
		}
	}
	if len(findings) == 0 {
		findings = []string{"gitleaks reported findings"}
	}
	return findings
}
