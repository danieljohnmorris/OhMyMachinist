package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Minimal skills-library stub (MACH-32): install a reviewer SKILL.md by URL
// into the user skills directory so profile `skills:` references resolve by
// name. The shared loader (MACH-33) extends this with version pinning and
// caching; the on-disk convention stays the same.

var skillURLPattern = regexp.MustCompile(`^https?://(?:www\.)?github\.com/([\w.-]+)/([\w.-]+)(?:/(?:blob|raw)/([^/]+))?(?:/(.+))?/SKILL\.md/?$`)

var skillFrontmatterName = regexp.MustCompile(`(?m)^name:\s*['"]?([\w:.-]+)`)

func newReviewCommand(options *commandOptions) *cobra.Command {
	review := &cobra.Command{Use: "review", Short: "Manage review profiles and skills"}
	review.AddCommand(newInstallSkillCommand(options))
	return review
}

func newInstallSkillCommand(options *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "install-skill <github SKILL.md url>",
		Short: "Install a reviewer skill from a GitHub SKILL.md URL",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name, version, err := installSkill(args[0], 60*time.Second)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(options.stdout, "installed skill %s (version %s) into %s\n", name, version, skillUserDirectory())
			return err
		},
	}
}

func skillUserDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".agents", "skills")
}

func installSkill(rawURL string, timeout time.Duration) (string, string, error) {
	match := skillURLPattern.FindStringSubmatch(strings.TrimSpace(rawURL))
	if match == nil {
		return "", "", fmt.Errorf("unsupported skill URL: %s", rawURL)
	}
	owner, repo, ref, directory := match[1], match[2], match[3], match[4]
	if ref == "" {
		ref = "HEAD"
	}
	raw := "https://raw.githubusercontent.com/" + owner + "/" + repo + "/" + ref + "/"
	if directory != "" {
		raw += strings.Trim(directory, "/") + "/"
	}
	raw += "SKILL.md"
	client := &http.Client{Timeout: timeout}
	response, err := client.Get(raw)
	if err != nil {
		return "", "", fmt.Errorf("fetch %s: %w", raw, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("fetch %s: status %d", raw, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 512<<10))
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", raw, err)
	}
	name := repo
	if fm := skillFrontmatterName.FindSubmatch(body); fm != nil {
		name = string(fm[1])
	}
	key := strings.ToLower(strings.ReplaceAll(name, ":", "_"))
	target := filepath.Join(skillUserDirectory(), key)
	if err := os.MkdirAll(target, 0o755); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(target, "SKILL.md"), body, 0o644); err != nil {
		return "", "", err
	}
	return name, fmt.Sprintf("sha256:%x", sha256.Sum256(body)), nil
}
