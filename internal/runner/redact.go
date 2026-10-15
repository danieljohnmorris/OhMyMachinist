package runner

import "regexp"

// secretPatterns mirror deploy/factory/review/core.py so every published
// surface (PR comment, run summary, live transcript) redacts the same token
// and environment shapes. Tool results routinely echo environment variables,
// auth headers and CLI invocations, which must never land in runs.events.
var secretPatterns = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}`), "[REDACTED]"},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{16,}`), "[REDACTED]"},
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "[REDACTED]"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), "[REDACTED]"},
	{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer|basic|token)\s+)\S+`), "${1}[REDACTED]"},
	{regexp.MustCompile(`(?i)\b((?:api[_-]?key|token|secret|password|passwd|client[_-]?secret)\s*[=:]\s*)[^\s'"]{8,}`), "${1}[REDACTED]"},
}

// redactSecrets masks credential-shaped strings in agent transcript text.
func redactSecrets(text string) string {
	if text == "" {
		return text
	}
	for _, entry := range secretPatterns {
		text = entry.pattern.ReplaceAllString(text, entry.replacement)
	}
	return text
}
