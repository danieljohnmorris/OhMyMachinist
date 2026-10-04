package factory

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

type projectInput struct {
	RequestID string `json:"request_id"`
	Name      string `json:"name"`
	Host      string `json:"host"`
	Source    string `json:"source"`
	Path      string `json:"path"`
	GitURL    string `json:"git_url"`
	GitHub    string `json:"github"`
}

func projectSource(p config.FactoryProject) string {
	if p.Source == "git" {
		return "git"
	}
	return "folder"
}
func publicProject(key string, p config.FactoryProject) map[string]any {
	return map[string]any{"id": key, "name": p.Name, "host": p.Host, "path": p.Path, "source": projectSource(p), "github": p.GitHub}
}
func validGitURL(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, "\x00\r\n\t ") || strings.HasPrefix(raw, "-") {
		return false
	}
	// SSH's familiar git@host:path syntax contains a login, never a password.
	if strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "://") {
		host, path, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":")
		return ok && host != "" && path != "" && !strings.HasPrefix(host, "-") && !strings.ContainsAny(host, "/:@")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || strings.HasPrefix(u.Hostname(), "-") || u.Path == "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "ssh" {
		return false
	}
	if u.User != nil {
		if u.Scheme != "ssh" || u.User.Username() != "git" {
			return false
		}
		if _, ok := u.User.Password(); ok {
			return false
		}
	}
	return true
}

var githubPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func githubFromOrigin(origin string) string {
	var path string
	if strings.HasPrefix(origin, "git@github.com:") {
		path = strings.TrimPrefix(origin, "git@github.com:")
	} else {
		u, e := url.Parse(origin)
		if e != nil || u.Host != "github.com" || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" {
			return ""
		}
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || !githubPart.MatchString(parts[0]) || !githubPart.MatchString(parts[1]) {
		return ""
	}
	return path
}
func (s *Service) addProject(w http.ResponseWriter, r *http.Request) {
	var in projectInput
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Host == "" {
		in.Host = "local"
	}
	if in.RequestID == "" || in.Name == "" || !filepath.IsAbs(in.Path) || strings.ContainsAny(in.Path, "\x00\r\n") {
		fail(w, errors.New("request_id, name and an absolute repository path are required"))
		return
	}
	if in.Source != "folder" && in.Source != "git" {
		fail(w, errors.New("source must be folder or git"))
		return
	}
	if in.Source == "git" && !validGitURL(in.GitURL) {
		fail(w, errors.New("use an HTTPS or SSH Git URL without embedded credentials"))
		return
	}
	if in.Source == "folder" && in.GitURL != "" {
		fail(w, errors.New("git_url is only used when cloning a repository"))
		return
	}
	if in.GitHub != "" && (len(strings.Split(in.GitHub, "/")) != 2 || strings.ContainsAny(in.GitHub, " \r\n\x00")) {
		fail(w, errors.New("github must be owner/repository"))
		return
	}
	in.Path = filepath.Clean(in.Path)
	s.mu.Lock()
	if !s.cfg.Enabled || s.closed {
		s.mu.Unlock()
		fail(w, errors.New("factory is unavailable"))
		return
	}
	requestKey := "project:" + in.RequestID
	if key := s.requests[requestKey]; key != "" {
		p := s.cfg.Projects[key]
		s.mu.Unlock()
		jsonReply(w, 200, map[string]any{"project": publicProject(key, p)})
		return
	}
	h, ok := s.cfg.Hosts[in.Host]
	if !ok {
		s.mu.Unlock()
		fail(w, errors.New("host is not configured"))
		return
	}
	for _, p := range s.cfg.Projects {
		if p.Host == in.Host && filepath.Clean(p.Path) == in.Path {
			s.mu.Unlock()
			fail(w, errors.New("this repository is already a project on that host"))
			return
		}
	}
	if s.provisioning {
		s.mu.Unlock()
		fail(w, errors.New("another repository is being prepared; retry with the same request_id"))
		return
	}
	s.provisioning = true
	s.mu.Unlock()
	p := config.FactoryProject{Name: in.Name, Host: in.Host, Path: in.Path, Source: in.Source, GitURL: in.GitURL, GitHub: in.GitHub}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	err := prepareProject(ctx, p, h)
	if err == nil && p.GitHub == "" {
		origin := p.GitURL
		if p.Source != "git" {
			origin, _ = projectGit(ctx, p, h, p.Path, "config", "--get", "remote.origin.url")
		}
		p.GitHub = githubFromOrigin(origin)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provisioning = false
	if err != nil {
		fail(w, err)
		return
	}
	if s.closed {
		fail(w, errors.New("factory stopped while preparing project; retry after restart"))
		return
	}
	key := id("p_")
	// Persist the project and its retry key together before making it visible.
	if err = s.commitRecords([]recordWrite{{"project", key, p}, {"request", requestKey, key}}, "", Event{}); err != nil {
		fail(w, err)
		return
	}
	s.commandMu.Lock()
	s.cfg.Projects[key] = p
	s.commandMu.Unlock()
	s.requests[requestKey] = key
	s.signal()
	jsonReply(w, 201, map[string]any{"project": publicProject(key, p)})
}

// Commands use an explicit host snapshot, so provisioning never needs a project-map mutation.
func projectCommand(ctx context.Context, p config.FactoryProject, h config.FactoryHost, args ...string) (string, error) {
	cmd := hostCommand(ctx, p, h, "", args)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("repository setup failed: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}
func hostCommand(ctx context.Context, p config.FactoryProject, h config.FactoryHost, dir string, args []string) *exec.Cmd {
	if p.Host != "" && p.Host != "local" {
		command := shellArgs(args)
		if dir != "" {
			command = "cd " + shellQuote(dir) + " && " + command
		}
		return exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", h.SSH, command)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	return cmd
}
func projectGit(ctx context.Context, p config.FactoryProject, h config.FactoryHost, dir string, args ...string) (string, error) {
	cmd := hostCommand(ctx, p, h, dir, append([]string{"git"}, args...))
	b, e := cmd.CombinedOutput()
	if e != nil {
		return "", fmt.Errorf("git: %s (%w)", strings.TrimSpace(string(b)), e)
	}
	return strings.TrimSpace(string(b)), nil
}
func prepareProject(ctx context.Context, p config.FactoryProject, h config.FactoryHost) error {
	exists, err := projectCommand(ctx, p, h, "sh", "-c", `if [ -e "$1" ] || [ -L "$1" ]; then printf present; else printf missing; fi`, "machinist", p.Path)
	if err != nil {
		return err
	}
	if p.Source == "git" {
		if exists == "missing" {
			if _, err = projectCommand(ctx, p, h, "git", "clone", "--", p.GitURL, p.Path); err != nil {
				return err
			}
		} else {
			origin, e := projectCommand(ctx, p, h, "git", "-C", p.Path, "config", "--get", "remote.origin.url")
			if e != nil {
				return errors.New("destination exists and is not the requested repository; choose another folder")
			}
			if origin != p.GitURL {
				return errors.New("destination already exists with a different Git origin; choose another folder")
			}
		}
	} else if exists != "present" {
		return errors.New("repository folder does not exist on the selected host")
	}
	top, err := projectCommand(ctx, p, h, "git", "-C", p.Path, "rev-parse", "--show-toplevel")
	if err != nil {
		return errors.New("choose a Git repository folder")
	}
	// Reject nested folders. Resolve symlinks on the execution host before comparing.
	canonical, err := projectCommand(ctx, p, h, "sh", "-c", `cd "$1" && pwd -P`, "machinist", p.Path)
	if err != nil {
		return err
	}
	if top != canonical {
		return errors.New("choose the repository root folder")
	}
	head, err := projectCommand(ctx, p, h, "git", "-C", p.Path, "rev-parse", "--verify", "HEAD")
	if err != nil || head == "" {
		return errors.New("repository needs at least one commit before it can be added")
	}
	return nil
}
