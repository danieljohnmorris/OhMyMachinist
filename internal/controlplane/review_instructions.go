package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// maxReviewInstructionsBytes bounds per-project review instructions so the
// settings API and the review prompts stay small.
const maxReviewInstructionsBytes = 64 << 10

// DefaultReviewInstructions seeds the built-in MACH-26 review step. Projects
// override it per repository in Settings; a stored empty value reverts here.
const DefaultReviewInstructions = `# Built-in PR review

Every pull request is reviewed in three passes after it opens; findings are posted
as a PR comment and the verdict is recorded as the ` + "`machinist/review`" + ` commit status.

## Pass 1 — checks

Run types, lint, and build for every language in the diff. Failures that are new
relative to the base branch are blocking; pre-existing failures are reported only.

## Pass 2 — parallel review

Run these reviewers in parallel over the PR diff:

- Code review (feature-dev:code-reviewer)
- Silent-failure hunter (pr-review-toolkit:silent-failure-hunter)
- Test analysis (pr-review-toolkit:pr-test-analyzer)
- TypeScript diffs also get typescript-code-review and typescript-jsdoc; Rust diffs get rust-review.

Critical and bug findings are blocking.

## Pass 3 — docs

Check README/CLAUDE/AGENTS against the diff, commit doc fixes to the same branch,
and update a stale pull-request description.

## Routing rules (Daniel, Mac CLAUDE.md)

- Any .md/.mdx under src/content/ routes to the write-as-dan prose pass:
  read the style guide first, then run the editor pass, and report phrases
  considered, rejected, and kept.
- Everything else routes to the three code reviewers.
- A mixed pull request gets both passes. If the changed-file list cannot be read,
  fall back to the code reviewers.

## Amend loop

Blocking findings send the change back to amend on the same branch. Each amend
round counts against the retry budget (default 3 rounds); a pull request still
blocking after the budget is exhausted is reported BLOCKED and is not eligible
for auto-merge.
`

// ReviewInstruction is one project's stored reviewer instructions.
type ReviewInstruction struct {
	Repository   string `json:"repository"`
	Instructions string `json:"instructions"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// ReviewInstructions returns the stored instructions for a project.
func (s *Store) ReviewInstructions(ctx context.Context, repository string) (string, bool, error) {
	var instructions, updatedAt string
	err := s.db.QueryRowContext(ctx,
		`SELECT instructions, updated_at FROM review_instructions WHERE repository = ?`, repository,
	).Scan(&instructions, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return instructions, true, nil
}

// SaveReviewInstructions stores or replaces project instructions. An empty
// body deletes the override so the project falls back to the default.
func (s *Store) SaveReviewInstructions(ctx context.Context, repository, instructions string) error {
	instructions = strings.TrimRight(instructions, "\n")
	_, err := s.db.ExecContext(ctx, `
INSERT INTO review_instructions(repository, instructions, updated_at) VALUES(?,?,?)
ON CONFLICT(repository) DO UPDATE SET instructions=excluded.instructions, updated_at=excluded.updated_at`,
		repository, instructions, s.now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("save review instructions: %w", err)
	}
	return nil
}

// DeleteReviewInstructions removes a project override.
func (s *Store) DeleteReviewInstructions(ctx context.Context, repository string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM review_instructions WHERE repository = ?`, repository)
	if err != nil {
		return fmt.Errorf("delete review instructions: %w", err)
	}
	return nil
}

// AllReviewInstructions lists every stored project override.
func (s *Store) AllReviewInstructions(ctx context.Context) ([]ReviewInstruction, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repository, instructions, updated_at FROM review_instructions ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []ReviewInstruction{}
	for rows.Next() {
		var item ReviewInstruction
		if err := rows.Scan(&item.Repository, &item.Instructions, &item.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}

var reviewProjectPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

// resolveReviewProject maps a logical repository name to its GitHub owner/name
// slug when the control plane knows the mapping; everything is matched
// case-insensitively because the review step derives the key from origin.
func (s *Server) resolveReviewProject(input string) string {
	value := strings.TrimSpace(input)
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	if slug, ok := s.githubRepositories[value]; ok {
		return strings.ToLower(slug)
	}
	if slug, ok := s.githubRepositories[lower]; ok {
		return strings.ToLower(slug)
	}
	return lower
}

type reviewInstructionsResponse struct {
	Project      string `json:"project"`
	Instructions string `json:"instructions"`
	Custom       bool   `json:"custom"`
}

type reviewInstructionsListResponse struct {
	Default      string            `json:"default"`
	Instructions map[string]string `json:"instructions"`
}

func (s *Server) reviewInstructions(response http.ResponseWriter, request *http.Request) {
	project := request.URL.Query().Get("project")
	if strings.TrimSpace(project) == "" {
		overrides, err := s.store.AllReviewInstructions(request.Context())
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		instructions := map[string]string{}
		for _, item := range overrides {
			instructions[item.Repository] = item.Instructions
		}
		writeJSON(response, http.StatusOK, reviewInstructionsListResponse{Default: DefaultReviewInstructions, Instructions: instructions})
		return
	}
	if !reviewProjectPattern.MatchString(strings.TrimSpace(project)) {
		writeError(response, http.StatusBadRequest, errors.New("project must be a repository name or owner/name"))
		return
	}
	key := s.resolveReviewProject(project)
	stored, custom, err := s.store.ReviewInstructions(request.Context(), key)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	effective := stored
	if !custom {
		effective = DefaultReviewInstructions
	}
	writeJSON(response, http.StatusOK, reviewInstructionsResponse{Project: key, Instructions: effective, Custom: custom})
}

type saveReviewInstructionsRequest struct {
	Project      string `json:"project"`
	Instructions string `json:"instructions"`
}

func (s *Server) saveReviewInstructions(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input saveReviewInstructionsRequest
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	project := strings.TrimSpace(input.Project)
	if !reviewProjectPattern.MatchString(project) {
		writeError(response, http.StatusBadRequest, errors.New("project must be a repository name or owner/name"))
		return
	}
	if len(input.Instructions) > maxReviewInstructionsBytes {
		writeError(response, http.StatusBadRequest, fmt.Errorf("instructions must be at most %d bytes", maxReviewInstructionsBytes))
		return
	}
	key := s.resolveReviewProject(project)
	var err error
	if strings.TrimSpace(input.Instructions) == "" {
		err = s.store.DeleteReviewInstructions(request.Context(), key)
	} else {
		err = s.store.SaveReviewInstructions(request.Context(), key, input.Instructions)
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, reviewInstructionsResponse{Project: key, Instructions: strings.TrimRight(input.Instructions, "\n"), Custom: strings.TrimSpace(input.Instructions) != ""})
}
