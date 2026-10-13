// Package profiles embeds the shipped review profiles. The YAML files in
// this directory are the canonical library served by the control plane and
// documented in README ("PR review profiles"); the worker's flat-install
// copy under deploy/factory/review/profiles must stay in sync (enforced by
// review.test_profiles in the Python test suite).
package profiles

import "embed"

// FS holds every shipped profile YAML.
//
//go:embed *.yaml
var FS embed.FS
