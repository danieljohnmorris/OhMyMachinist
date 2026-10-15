package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// GitHub talks to the GitHub REST API and raw content host. Tests point the
// base URLs at a fake server; the token is read from the environment only and
// is never recorded, logged, or embedded in a source URL.
type GitHub struct {
	APIBase string
	RawBase string
	Token   string
	Client  *http.Client
}

const (
	defaultGitHubAPIBase = "https://api.github.com"
	defaultGitHubRawBase = "https://raw.githubusercontent.com"
	githubTokenEnv       = "GITHUB_TOKEN"
	maxGitHubResponse    = 4 << 20
)

func defaultGitHub() *GitHub {
	return &GitHub{
		APIBase: defaultGitHubAPIBase,
		RawBase: defaultGitHubRawBase,
		Token:   strings.TrimSpace(os.Getenv(githubTokenEnv)),
		Client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (g *GitHub) httpClient() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 60 * time.Second}
}

var githubURLPattern = regexp.MustCompile(`^https?://(?:www\.)?github\.com/([\w.-]+)/([\w.-]+)(?:/(blob|raw|tree)/([^/]+)(?:/(.*))?)?/?$`)

type githubSource struct {
	owner string
	repo  string
	ref   string
	dir   string
}

func isGitHubURL(input string) bool {
	return githubURLPattern.MatchString(stripGitHubSuffix(input))
}

// stripGitHubSuffix removes a query/fragment and an optional trailing @ref so
// the URL shape can be matched.
func stripGitHubSuffix(input string) string {
	trimmed := strings.TrimSpace(input)
	if at := strings.LastIndex(trimmed, "@"); at > strings.LastIndex(trimmed, "/") {
		trimmed = trimmed[:at]
	}
	if index := strings.IndexAny(trimmed, "?#"); index >= 0 {
		trimmed = trimmed[:index]
	}
	return trimmed
}

func parseGitHubSource(raw string) (githubSource, error) {
	trimmed := strings.TrimSpace(raw)
	ref := ""
	if at := strings.LastIndex(trimmed, "@"); at > strings.LastIndex(trimmed, "/") {
		ref = strings.TrimSpace(trimmed[at+1:])
		trimmed = trimmed[:at]
	}
	match := githubURLPattern.FindStringSubmatch(trimmed)
	if match == nil {
		return githubSource{}, fmt.Errorf("unsupported GitHub skill URL: %s", raw)
	}
	owner, repo, kind, urlRef, rest := match[1], match[2], match[3], match[4], match[5]
	if urlRef == "" {
		urlRef = "HEAD"
	}
	if ref == "" {
		ref = urlRef
	}
	directory := strings.Trim(strings.TrimSpace(rest), "/")
	switch {
	case directory == "SKILL.md":
		directory = ""
	case strings.HasSuffix(directory, "/SKILL.md"):
		directory = strings.Trim(strings.TrimSuffix(directory, "/SKILL.md"), "/")
	case kind == "blob" || kind == "raw":
		return githubSource{}, fmt.Errorf("GitHub skill URL must point at SKILL.md: %s", raw)
	}
	return githubSource{owner: owner, repo: repo, ref: ref, dir: directory}, nil
}

func (g *GitHub) request(ctx context.Context, endpoint, accept string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", "machinist")
	if g.Token != "" {
		request.Header.Set("Authorization", "Bearer "+g.Token)
	}
	response, err := g.httpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", endpoint, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxGitHubResponse+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", endpoint, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request %s: status %d", endpoint, response.StatusCode)
	}
	return body, nil
}

// ResolveRef resolves a branch, tag, or SHA to a commit SHA.
func (g *GitHub) ResolveRef(ctx context.Context, owner, repo, ref string) (string, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/commits/%s", strings.TrimSuffix(g.APIBase, "/"), owner, repo, url.PathEscape(ref))
	body, err := g.request(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	var payload struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", owner+"/"+repo, ref, err)
	}
	sha := strings.TrimSpace(payload.SHA)
	if sha == "" {
		return "", fmt.Errorf("GitHub did not resolve %s@%s to a commit", owner+"/"+repo, ref)
	}
	return sha, nil
}

type gitHubTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
}

// Tree lists every path in the repository at a commit.
func (g *GitHub) Tree(ctx context.Context, owner, repo, sha string) ([]gitHubTreeEntry, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/git/trees/%s?recursive=1", strings.TrimSuffix(g.APIBase, "/"), owner, repo, url.PathEscape(sha))
	body, err := g.request(ctx, endpoint, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Tree []gitHubTreeEntry `json:"tree"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("list tree %s@%s: %w", owner+"/"+repo, sha, err)
	}
	return payload.Tree, nil
}

// Fetch reads one file at a commit from the raw content host.
func (g *GitHub) Fetch(ctx context.Context, owner, repo, sha, path string) ([]byte, error) {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		segments[index] = url.PathEscape(segment)
	}
	endpoint := fmt.Sprintf("%s/%s/%s/%s/%s", strings.TrimSuffix(g.RawBase, "/"), owner, repo, url.PathEscape(sha), strings.Join(segments, "/"))
	return g.request(ctx, endpoint, "application/vnd.github.raw")
}

// fetchGitHub resolves a GitHub source to a commit SHA and downloads the whole
// skill directory at that commit.
func (l *Library) fetchGitHub(ctx context.Context, rawURL string) (fetched, error) {
	parsed, err := parseGitHubSource(rawURL)
	if err != nil {
		return fetched{}, err
	}
	client := l.github()
	sha, err := client.ResolveRef(ctx, parsed.owner, parsed.repo, parsed.ref)
	if err != nil {
		return fetched{}, fmt.Errorf("resolve GitHub ref: %w", err)
	}
	entries, err := client.Tree(ctx, parsed.owner, parsed.repo, sha)
	if err != nil {
		return fetched{}, fmt.Errorf("list GitHub skill directory: %w", err)
	}
	prefix := ""
	if parsed.dir != "" {
		prefix = parsed.dir + "/"
	}
	files := map[string][]byte{}
	total := 0
	for _, entry := range entries {
		if entry.Type != "blob" || !strings.HasPrefix(entry.Path, prefix) {
			continue
		}
		relative := strings.TrimPrefix(entry.Path, prefix)
		if relative == "" {
			continue
		}
		body, err := client.Fetch(ctx, parsed.owner, parsed.repo, sha, entry.Path)
		if err != nil {
			return fetched{}, err
		}
		if err := storeSkillFile(files, relative, body); err != nil {
			return fetched{}, err
		}
		total += len(body)
		if total > maxSkillTotal {
			return fetched{}, fmt.Errorf("skill exceeds %d bytes in total", maxSkillTotal)
		}
	}
	if _, ok := files["SKILL.md"]; !ok {
		directory := parsed.dir
		if directory == "" {
			directory = "."
		}
		return fetched{}, fmt.Errorf("GitHub skill %s@%s has no SKILL.md at %s", parsed.owner+"/"+parsed.repo, sha, directory)
	}
	return fetched{source: rawURL, version: sha, resolved: sha, files: files}, nil
}
