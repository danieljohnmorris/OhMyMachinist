package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var projectKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)
var projectTitlePattern = regexp.MustCompile(`^([A-Z][A-Z0-9]{1,9})-\d+\b`)

const jobMetadataSchema = `
CREATE TABLE IF NOT EXISTS job_metadata (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
 metadata TEXT NOT NULL);`

type JobScreenshot struct {
	Label        string `json:"label,omitempty"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	URL          string `json:"url,omitempty"`
	ArtifactID   string `json:"artifact_id,omitempty"`
}

type JobMetadata struct {
	Project       string          `json:"project,omitempty"`
	IssueURL      string          `json:"issue_url,omitempty"`
	Branch        string          `json:"branch,omitempty"`
	PRURL         string          `json:"pr_url,omitempty"`
	PreviewURL    string          `json:"preview_url,omitempty"`
	ReviewVerdict string          `json:"review_verdict,omitempty"`
	ReviewSummary string          `json:"review_summary,omitempty"`
	Screenshots   []JobScreenshot `json:"screenshots,omitempty"`
}

var ErrJobMetadataInvalid = errors.New("invalid job metadata")

func NormalizeJobMetadata(raw map[string]any) (*JobMetadata, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("encode job metadata: %w", err)
	}
	var metadata JobMetadata
	if verdict, ok := raw["review_verdict"]; ok && verdict != nil {
		if _, isString := verdict.(string); !isString {
			encoded, verdictErr := json.Marshal(verdict)
			if verdictErr != nil {
				return nil, fmt.Errorf("%w: invalid review verdict", ErrJobMetadataInvalid)
			}
			raw["review_verdict"] = string(encoded)
		}
		body, err = json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("encode job metadata: %w", err)
		}
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		return nil, fmt.Errorf("%w: expected a job metadata object", ErrJobMetadataInvalid)
	}
	for _, value := range []string{metadata.IssueURL, metadata.PRURL, metadata.PreviewURL} {
		if value == "" {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("%w: URLs must be HTTP(S)", ErrJobMetadataInvalid)
		}
	}
	for _, screenshot := range metadata.Screenshots {
		if screenshot.ThumbnailURL != "" {
			parsed, err := url.Parse(screenshot.ThumbnailURL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return nil, fmt.Errorf("%w: screenshot thumbnails must be HTTP(S)", ErrJobMetadataInvalid)
			}
		}
		if screenshot.URL != "" {
			parsed, err := url.Parse(screenshot.URL)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return nil, fmt.Errorf("%w: screenshots must be HTTP(S)", ErrJobMetadataInvalid)
			}
		}
	}
	if metadata.Project != "" && !projectKeyPattern.MatchString(metadata.Project) {
		return nil, fmt.Errorf("%w: project must match [A-Z][A-Z0-9]{1,9}", ErrJobMetadataInvalid)
	}
	if metadata.IssueURL == "" && metadata.Branch == "" && metadata.PRURL == "" && metadata.PreviewURL == "" &&
		metadata.ReviewVerdict == "" && metadata.ReviewSummary == "" && len(metadata.Screenshots) == 0 {
		if metadata.Project == "" {
			return nil, nil
		}
	}
	return &metadata, nil
}

func ResolveProjectKey(explicit, title string) (string, error) {
	if explicit != "" {
		if !projectKeyPattern.MatchString(explicit) {
			return "", fmt.Errorf("project must match [A-Z][A-Z0-9]{1,9}; got %q", explicit)
		}
		return explicit, nil
	}
	if match := projectTitlePattern.FindStringSubmatch(strings.TrimSpace(title)); match != nil {
		return match[1], nil
	}
	return "", nil
}

func saveJobMetadata(ctx context.Context, tx *sql.Tx, jobID string, raw map[string]any) error {
	var existingProject string
	var existing string
	err := tx.QueryRowContext(ctx, `SELECT metadata FROM job_metadata WHERE job_id=?`, jobID).Scan(&existing)
	if err == nil {
		var prior JobMetadata
		if decodeErr := json.Unmarshal([]byte(existing), &prior); decodeErr == nil {
			existingProject = prior.Project
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existingProject != "" && (raw == nil || raw["project"] == nil) {
		if raw == nil {
			raw = map[string]any{}
		}
		raw["project"] = existingProject
	}
	metadata, err := NormalizeJobMetadata(raw)
	if err != nil || metadata == nil {
		return err
	}
	body, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO job_metadata(job_id,metadata) VALUES(?,?)
ON CONFLICT(job_id) DO UPDATE SET metadata=excluded.metadata`, jobID, string(body))
	return err
}

func loadJobMetadata(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, jobs []Job) error {
	rows, err := q.QueryContext(ctx, `SELECT job_id,metadata FROM job_metadata`)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]*Job, len(jobs))
	for index := range jobs {
		byID[jobs[index].ID] = &jobs[index]
	}
	for rows.Next() {
		var jobID, raw string
		if err := rows.Scan(&jobID, &raw); err != nil {
			return err
		}
		if job := byID[jobID]; job != nil {
			var metadata JobMetadata
			if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
				return fmt.Errorf("decode job metadata: %w", err)
			}
			job.Metadata = &metadata
		}
	}
	return rows.Err()
}

func isBlankMetadata(value string) bool { return strings.TrimSpace(value) == "" }
