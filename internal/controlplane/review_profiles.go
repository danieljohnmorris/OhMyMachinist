package controlplane

import (
	"context"
	"database/sql"

	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	review "github.com/owainlewis/machinist/profiles/review"
	"go.yaml.in/yaml/v3"
)

// maxReviewProfileBytes bounds profile bodies and extra instructions so the
// settings API and the review prompts stay small.
const maxReviewProfileBytes = 64 << 10

// settings keys and the shipped fallback profile.
const (
	settingReviewDefaultProfile       = "review.default_profile"
	settingReviewInstructionsMigrated = "review.instructions_migrated"
	defaultReviewProfileName          = "default"
)

// ProfileIssue is one validation problem with its source line number.
type ProfileIssue struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// ReviewProfile is one library profile (shipped or user-stored).
type ReviewProfile struct {
	Name      string `json:"name"`
	Body      string `json:"body,omitempty"`
	BuiltIn   bool   `json:"built_in"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ProjectReviewSetting is one project's review configuration: a library
// profile name, a full inline profile body, and/or free-text extra
// instructions appended to the resolved profile's reviewer prompts.
type ProjectReviewSetting struct {
	ProfileName       string
	ProfileBody       string
	ExtraInstructions string
}

// ShippedReviewProfiles returns the embedded shipped profiles by name.
func ShippedReviewProfiles() map[string]string {
	result := map[string]string{}
	entries, err := review.FS.ReadDir(".")
	if err != nil {
		return result
	}
	for _, entry := range entries {
		if entry.IsDir() || path.Ext(entry.Name()) != ".yaml" {
			continue
		}
		body, err := fs.ReadFile(review.FS, entry.Name())
		if err != nil {
			continue
		}
		name := reviewProfileNameFromBody(string(body), strings.TrimSuffix(entry.Name(), ".yaml"))
		result[name] = string(body)
	}
	return result
}

func reviewProfileNameFromBody(body, fallback string) string {
	var parsed struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal([]byte(body), &parsed); err == nil && strings.TrimSpace(parsed.Name) != "" {
		return strings.TrimSpace(parsed.Name)
	}
	return fallback
}

// ---------------------------------------------------------------------------
// Validation: mirrors deploy/factory/review/profile.py. Errors carry line
// numbers so the Settings editor can point at the offending YAML.

func validateProfileBody(body string) []ProfileIssue {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(body), &root); err != nil {
		line := 1
		var yamlError *yaml.TypeError
		if errors.As(err, &yamlError) {
			// fall through to generic line
		}
		if root.Line > 0 {
			line = root.Line
		}
		return []ProfileIssue{{Line: line, Message: "invalid YAML: " + err.Error()}}
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return []ProfileIssue{{Line: 1, Message: "profile must be a YAML mapping"}}
	}
	return validateProfileNode(root.Content[0])
}

func nodeLine(node *yaml.Node) int {
	if node == nil || node.Line < 1 {
		return 1
	}
	return node.Line
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Kind == yaml.ScalarNode && node.Content[index].Value == key {
			return node.Content[index+1]
		}
	}
	return nil
}

// normalizeProfileShape converts the lenient profile shapes the validator and
// the Python runtime accept — the `passes:` alias for `reviewers:`, list-form
// `routing:` and list-form `checks:` — into the canonical mapping form so the
// preview endpoint sees one shape. Canonical bodies pass through unchanged.
func normalizeProfileShape(body string) (string, error) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(body), &document); err != nil {
		return body, err
	}
	root := &document
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return body, nil
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return body, nil
	}

	// `passes` is an accepted alias for `reviewers` when reviewers is absent.
	if mappingValue(root, "reviewers") == nil {
		for index := 0; index+1 < len(root.Content); index += 2 {
			if root.Content[index].Kind == yaml.ScalarNode && root.Content[index].Value == "passes" {
				root.Content[index].Value = "reviewers"
				break
			}
		}
	}

	// List-form routing: entries carrying `when` are rules; a mapping entry
	// with a `fallback` list sets the fallback.
	if routing := mappingValue(root, "routing"); routing != nil && routing.Kind == yaml.SequenceNode {
		fallback := &yaml.Node{Kind: yaml.SequenceNode}
		rules := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range routing.Content {
			if item.Kind != yaml.MappingNode {
				continue
			}
			if hasMappingKey(item, "when") {
				rules.Content = append(rules.Content, item)
			}
			if itemFallback := mappingValue(item, "fallback"); itemFallback != nil && itemFallback.Kind == yaml.SequenceNode {
				fallback = itemFallback
			}
		}
		normalized := &yaml.Node{Kind: yaml.MappingNode}
		normalized.Content = append(normalized.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "fallback"}, fallback,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "rules"}, rules)
		replaceMappingValue(root, "routing", normalized)
	}

	// List-form checks: entries that are only policy keys set blocking and
	// missing_tool; everything else is a lane.
	if checks := mappingValue(root, "checks"); checks != nil && checks.Kind == yaml.SequenceNode {
		blocking, missingTool := "new_failures", "blocked"
		lanes := &yaml.Node{Kind: yaml.SequenceNode}
		for _, item := range checks.Content {
			if item.Kind == yaml.MappingNode && mappingValue(item, "lane") == nil && len(item.Content) <= 4 {
				policyOnly := true
				for index := 0; index+1 < len(item.Content); index += 2 {
					if key := item.Content[index].Value; key != "blocking" && key != "missing_tool" {
						policyOnly = false
						break
					}
				}
				if policyOnly {
					if value := mappingValue(item, "blocking"); value != nil && value.Kind == yaml.ScalarNode {
						blocking = value.Value
					}
					if value := mappingValue(item, "missing_tool"); value != nil && value.Kind == yaml.ScalarNode {
						missingTool = value.Value
					}
					continue
				}
			}
			lanes.Content = append(lanes.Content, item)
		}
		normalized := &yaml.Node{Kind: yaml.MappingNode}
		normalized.Content = append(normalized.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "blocking"}, &yaml.Node{Kind: yaml.ScalarNode, Value: blocking},
			&yaml.Node{Kind: yaml.ScalarNode, Value: "missing_tool"}, &yaml.Node{Kind: yaml.ScalarNode, Value: missingTool},
			&yaml.Node{Kind: yaml.ScalarNode, Value: "lanes"}, lanes)
		replaceMappingValue(root, "checks", normalized)
	}

	normalized, err := yaml.Marshal(root)
	if err != nil {
		return body, err
	}
	return string(normalized), nil
}

func hasMappingKey(node *yaml.Node, key string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Kind == yaml.ScalarNode && node.Content[index].Value == key {
			return true
		}
	}
	return false
}

func replaceMappingValue(root *yaml.Node, key string, value *yaml.Node) {
	for index := 0; index+1 < len(root.Content); index += 2 {
		if root.Content[index].Kind == yaml.ScalarNode && root.Content[index].Value == key {
			root.Content[index+1] = value
			return
		}
	}
}

func mappingKeys(node *yaml.Node) []string {
	keys := []string{}
	if node == nil || node.Kind != yaml.MappingNode {
		return keys
	}
	for index := 0; index < len(node.Content); index += 2 {
		keys = append(keys, node.Content[index].Value)
	}
	return keys
}

func globRegex(pattern string) (*regexp.Regexp, error) {
	if strings.TrimSpace(pattern) == "" {
		return nil, errors.New("empty glob")
	}
	parts := strings.Split(pattern, "/")
	var builder strings.Builder
	for index, part := range parts {
		if part == "**" {
			if index == len(parts)-1 {
				builder.WriteString(".+")
			} else {
				builder.WriteString("(?:[^/]+/)*")
				continue
			}
		} else {
			for _, char := range part {
				switch char {
				case '*':
					builder.WriteString("[^/]*")
				case '?':
					builder.WriteString("[^/]")
				default:
					builder.WriteString(regexp.QuoteMeta(string(char)))
				}
			}
		}
		if index != len(parts)-1 {
			builder.WriteString("/")
		}
	}
	return regexp.Compile("^" + builder.String() + "$")
}

func globMatches(pattern, target string) bool {
	regex, err := globRegex(pattern)
	if err != nil {
		return false
	}
	if !strings.Contains(pattern, "/") {
		if regex.MatchString(target[strings.LastIndex(target, "/")+1:]) {
			return true
		}
	}
	return regex.MatchString(target)
}

func validateGlobNode(node *yaml.Node, issues []ProfileIssue) []ProfileIssue {
	if node.Kind == yaml.ScalarNode {
		if _, err := globRegex(node.Value); err != nil || strings.TrimSpace(node.Value) == "" {
			issues = append(issues, ProfileIssue{Line: nodeLine(node), Message: fmt.Sprintf("invalid glob %q", node.Value)})
		}
		return issues
	}
	if node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			issues = validateGlobNode(item, issues)
		}
	}
	return issues
}

func validateProfileNode(root *yaml.Node) []ProfileIssue {
	issues := []ProfileIssue{}
	if root.Kind != yaml.MappingNode {
		return append(issues, ProfileIssue{Line: nodeLine(root), Message: "profile must be a YAML mapping"})
	}
	knownTop := map[string]bool{"name": true, "version": true, "description": true, "source": true, "extends": true, "extra_instructions": true, "scope": true, "checks": true, "concurrency": true, "reviewers": true, "passes": true, "routing": true, "reconcile": true, "docs": true, "blocking": true, "output": true, "skills": true}
	for index := 0; index < len(root.Content); index += 2 {
		key := root.Content[index]
		if !knownTop[key.Value] {
			issues = append(issues, ProfileIssue{Line: nodeLine(key), Message: fmt.Sprintf("unknown top-level key: %s", key.Value)})
		}
	}
	nameNode := mappingValue(root, "name")
	if nameNode == nil || nameNode.Kind != yaml.ScalarNode || strings.TrimSpace(nameNode.Value) == "" {
		issues = append(issues, ProfileIssue{Line: nodeLine(root), Message: "name is required"})
	}
	versionNode := mappingValue(root, "version")
	if versionNode == nil || versionNode.Kind != yaml.ScalarNode || versionNode.Value != "1" {
		issues = append(issues, ProfileIssue{Line: nodeLine(root), Message: "version must be 1"})
	}

	validateStringEnum := func(node *yaml.Node, allowed []string, what string) {
		if node == nil {
			return
		}
		if node.Kind != yaml.ScalarNode {
			issues = append(issues, ProfileIssue{Line: nodeLine(node), Message: fmt.Sprintf("%s must be %s", what, strings.Join(allowed, ", "))})
			return
		}
		for _, value := range allowed {
			if node.Value == value {
				return
			}
		}
		issues = append(issues, ProfileIssue{Line: nodeLine(node), Message: fmt.Sprintf("%s must be %s", what, strings.Join(allowed, ", "))})
	}
	validateMappingKeys := func(node *yaml.Node, allowed map[string]bool, what string) {
		if node == nil || node.Kind != yaml.MappingNode {
			return
		}
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if !allowed[key.Value] {
				issues = append(issues, ProfileIssue{Line: nodeLine(key), Message: fmt.Sprintf("unknown %s key: %s", what, key.Value)})
			}
		}
	}

	if scope := mappingValue(root, "scope"); scope != nil {
		if scope.Kind != yaml.MappingNode {
			issues = append(issues, ProfileIssue{Line: nodeLine(scope), Message: "scope must be a mapping"})
		} else {
			validateMappingKeys(scope, map[string]bool{"pin_head_sha": true, "lanes_from": true, "mode": true}, "scope")
			validateStringEnum(mappingValue(scope, "lanes_from"), []string{"diff", "repo"}, "scope.lanes_from")
			validateStringEnum(mappingValue(scope, "mode"), []string{"review_only", "amend"}, "scope.mode")
		}
	}

	laneNames := map[string]bool{}
	reviewerNames := map[string]bool{}
	if checks := mappingValue(root, "checks"); checks != nil {
		var lanes []*yaml.Node
		switch checks.Kind {
		case yaml.MappingNode:
			validateMappingKeys(checks, map[string]bool{"blocking": true, "missing_tool": true, "lanes": true}, "checks")
			validateStringEnum(mappingValue(checks, "blocking"), []string{"new_failures", "all"}, "checks.blocking")
			validateStringEnum(mappingValue(checks, "missing_tool"), []string{"blocked", "skip", "pass"}, "checks.missing_tool")
			if lanesNode := mappingValue(checks, "lanes"); lanesNode != nil {
				if lanesNode.Kind != yaml.SequenceNode {
					issues = append(issues, ProfileIssue{Line: nodeLine(lanesNode), Message: "checks.lanes must be a list"})
				} else {
					lanes = lanesNode.Content
				}
			}
		case yaml.SequenceNode:
			for _, item := range checks.Content {
				if item.Kind == yaml.MappingNode && mappingValue(item, "lane") == nil {
					onlyPolicy := true
					for _, key := range mappingKeys(item) {
						if key != "blocking" && key != "missing_tool" {
							onlyPolicy = false
						}
					}
					if onlyPolicy {
						continue
					}
				}
				lanes = append(lanes, item)
			}
		default:
			issues = append(issues, ProfileIssue{Line: nodeLine(checks), Message: "checks must be a mapping or a list of lanes"})
		}
		for _, lane := range lanes {
			if lane.Kind != yaml.MappingNode {
				issues = append(issues, ProfileIssue{Line: nodeLine(lane), Message: "each lane must be a mapping"})
				continue
			}
			validateMappingKeys(lane, map[string]bool{"lane": true, "when": true, "commands": true, "post_deploy": true}, "lane")
			if laneName := mappingValue(lane, "lane"); laneName != nil {
				if laneNames[laneName.Value] {
					issues = append(issues, ProfileIssue{Line: nodeLine(laneName), Message: fmt.Sprintf("duplicate lane: %s", laneName.Value)})
				}
				laneNames[laneName.Value] = true
			} else {
				issues = append(issues, ProfileIssue{Line: nodeLine(lane), Message: "lane requires a lane name"})
			}
			if when := mappingValue(lane, "when"); when != nil {
				if when.Kind != yaml.SequenceNode {
					issues = append(issues, ProfileIssue{Line: nodeLine(when), Message: "lane.when must be a list of globs"})
				} else {
					for _, pattern := range when.Content {
						issues = validateGlobNode(pattern, issues)
					}
				}
			}
			if commands := mappingValue(lane, "commands"); commands == nil || commands.Kind != yaml.SequenceNode {
				issues = append(issues, ProfileIssue{Line: nodeLine(lane), Message: "lane.commands must be a list"})
			} else {
				for _, command := range commands.Content {
					if command.Kind == yaml.ScalarNode {
						if strings.TrimSpace(command.Value) == "" {
							issues = append(issues, ProfileIssue{Line: nodeLine(command), Message: "command must not be empty"})
						}
						continue
					}
					if command.Kind != yaml.MappingNode {
						issues = append(issues, ProfileIssue{Line: nodeLine(command), Message: "commands must be strings or mappings"})
						continue
					}
					validateMappingKeys(command, map[string]bool{"run": true, "if_exists": true, "if_script": true, "if_configured": true, "optional": true, "else": true, "requires_authorization": true}, "command")
					run := mappingValue(command, "run")
					if run == nil || run.Kind != yaml.ScalarNode || strings.TrimSpace(run.Value) == "" {
						issues = append(issues, ProfileIssue{Line: nodeLine(command), Message: "command requires a run string"})
					}
				}
			}
		}
	}

	// `passes` is an accepted alias for `reviewers` (the Python runtime and the
	// preview endpoint both normalize it); validate whichever is present.
	reviewersNode := mappingValue(root, "reviewers")
	reviewersLabel := "reviewers"
	if reviewersNode == nil {
		reviewersNode = mappingValue(root, "passes")
		reviewersLabel = "passes"
	}
	if reviewersNode != nil {
		reviewers := reviewersNode
		if reviewers.Kind != yaml.SequenceNode {
			issues = append(issues, ProfileIssue{Line: nodeLine(reviewers), Message: reviewersLabel + " must be a list"})
		} else {
			for _, reviewer := range reviewers.Content {
				if reviewer.Kind != yaml.MappingNode {
					issues = append(issues, ProfileIssue{Line: nodeLine(reviewer), Message: "each reviewer must be a mapping"})
					continue
				}
				validateMappingKeys(reviewer, map[string]bool{"name": true, "skills": true, "skill": true, "prompt": true, "prompt_from": true, "model": true, "effort": true, "parallel": true, "when": true, "lanes": true, "read_only": true, "optional": true, "trigger": true, "routing_only": true}, "reviewer")
				if name := mappingValue(reviewer, "name"); name != nil && name.Kind == yaml.ScalarNode {
					if reviewerNames[name.Value] {
						issues = append(issues, ProfileIssue{Line: nodeLine(name), Message: fmt.Sprintf("duplicate reviewer: %s", name.Value)})
					}
					reviewerNames[name.Value] = true
				} else {
					issues = append(issues, ProfileIssue{Line: nodeLine(reviewer), Message: "reviewer requires a name"})
				}
				if mappingValue(reviewer, "skills") == nil && mappingValue(reviewer, "skill") == nil && mappingValue(reviewer, "prompt") == nil && mappingValue(reviewer, "prompt_from") == nil {
					issues = append(issues, ProfileIssue{Line: nodeLine(reviewer), Message: "reviewer requires skills or a prompt"})
				}
				validateStringEnum(mappingValue(reviewer, "effort"), []string{"low", "medium", "high"}, "effort")
				validateStringEnum(mappingValue(reviewer, "trigger"), []string{"on_request"}, "trigger")
				if lanes := mappingValue(reviewer, "lanes"); lanes != nil {
					if lanes.Kind != yaml.SequenceNode {
						issues = append(issues, ProfileIssue{Line: nodeLine(lanes), Message: "reviewer.lanes must be a list"})
					} else {
						for _, laneRef := range lanes.Content {
							if laneRef.Kind == yaml.ScalarNode && laneRef.Value != "" && !laneNames[laneRef.Value] {
								issues = append(issues, ProfileIssue{Line: nodeLine(laneRef), Message: fmt.Sprintf("reviewer references unknown lane: %s", laneRef.Value)})
							}
						}
					}
				}
			}
		}
	}

	if routing := mappingValue(root, "routing"); routing != nil {
		var rules []*yaml.Node
		switch routing.Kind {
		case yaml.SequenceNode:
			for _, item := range routing.Content {
				if item.Kind == yaml.MappingNode && mappingValue(item, "when") == nil {
					continue
				}
				rules = append(rules, item)
			}
		case yaml.MappingNode:
			validateMappingKeys(routing, map[string]bool{"fallback": true, "rules": true}, "routing")
			if rulesNode := mappingValue(routing, "rules"); rulesNode != nil && rulesNode.Kind == yaml.SequenceNode {
				rules = rulesNode.Content
			}
		default:
			issues = append(issues, ProfileIssue{Line: nodeLine(routing), Message: "routing must be a mapping or a list of rules"})
		}
		checkPassRefs := func(node *yaml.Node, what string) {
			if node == nil || node.Kind != yaml.SequenceNode {
				return
			}
			for _, passRef := range node.Content {
				if passRef.Kind == yaml.ScalarNode && passRef.Value != "" && !reviewerNames[passRef.Value] {
					issues = append(issues, ProfileIssue{Line: nodeLine(passRef), Message: fmt.Sprintf("%s references unknown pass: %s", what, passRef.Value)})
				}
			}
		}
		for _, rule := range rules {
			if rule.Kind != yaml.MappingNode {
				issues = append(issues, ProfileIssue{Line: nodeLine(rule), Message: "each routing rule must be a mapping"})
				continue
			}
			validateMappingKeys(rule, map[string]bool{"when": true, "passes": true}, "routing rule")
			passes := mappingValue(rule, "passes")
			if passes == nil || passes.Kind != yaml.SequenceNode {
				issues = append(issues, ProfileIssue{Line: nodeLine(rule), Message: "routing rule requires passes"})
			} else {
				checkPassRefs(passes, "routing")
			}
			if when := mappingValue(rule, "when"); when != nil && when.Kind != yaml.SequenceNode {
				issues = append(issues, ProfileIssue{Line: nodeLine(when), Message: "routing rule when must be a list of globs"})
			}
		}
		if routing.Kind == yaml.MappingNode {
			checkPassRefs(mappingValue(routing, "fallback"), "routing.fallback")
		} else {
			for _, item := range routing.Content {
				if item.Kind == yaml.MappingNode {
					checkPassRefs(mappingValue(item, "fallback"), "routing.fallback")
				}
			}
		}
	}

	for _, section := range []struct {
		key     string
		allowed map[string]bool
	}{
		{"reconcile", map[string]bool{"dedupe": true, "verify_against_code": true}},
		{"docs", map[string]bool{"enabled": true, "files": true, "update_pr_description": true}},
		{"output", map[string]bool{"pr_comment": true, "commit_status": true, "report": true}},
	} {
		node := mappingValue(root, section.key)
		if node == nil {
			continue
		}
		if node.Kind != yaml.MappingNode {
			issues = append(issues, ProfileIssue{Line: nodeLine(node), Message: fmt.Sprintf("%s must be a mapping", section.key)})
			continue
		}
		validateMappingKeys(node, section.allowed, section.key)
		if section.key == "output" {
			if report := mappingValue(node, "report"); report != nil && report.Kind == yaml.SequenceNode {
				for _, entry := range report.Content {
					switch entry.Value {
					case "head_sha", "findings", "checks", "passes", "docs", "blockers":
					default:
						issues = append(issues, ProfileIssue{Line: nodeLine(entry), Message: fmt.Sprintf("unknown report section: %s", entry.Value)})
					}
				}
			}
		}
	}

	if blocking := mappingValue(root, "blocking"); blocking != nil {
		if blocking.Kind != yaml.MappingNode {
			issues = append(issues, ProfileIssue{Line: nodeLine(blocking), Message: "blocking must be a mapping"})
		} else {
			validateMappingKeys(blocking, map[string]bool{"severities": true, "amend_rounds": true, "max_minutes": true}, "blocking")
			if rounds := mappingValue(blocking, "amend_rounds"); rounds != nil && (rounds.Kind != yaml.ScalarNode || !isNonNegativeInt(rounds.Value)) {
				issues = append(issues, ProfileIssue{Line: nodeLine(rounds), Message: "blocking.amend_rounds must be a non-negative integer"})
			}
			if minutes := mappingValue(blocking, "max_minutes"); minutes != nil && (minutes.Kind != yaml.ScalarNode || !isNonNegativeNumber(minutes.Value)) {
				issues = append(issues, ProfileIssue{Line: nodeLine(minutes), Message: "blocking.max_minutes must be a non-negative number"})
			}
		}
	}
	if concurrency := mappingValue(root, "concurrency"); concurrency != nil && (concurrency.Kind != yaml.ScalarNode || !isPositiveInt(concurrency.Value)) {
		issues = append(issues, ProfileIssue{Line: nodeLine(concurrency), Message: "concurrency must be a positive integer"})
	}
	return issues
}

var integerPattern = regexp.MustCompile(`^\d+$`)

func isNonNegativeInt(value string) bool { return integerPattern.MatchString(value) }

func isNonNegativeNumber(value string) bool {
	parsed, err := strconv.ParseFloat(value, 64)
	return err == nil && parsed >= 0
}

func isPositiveInt(value string) bool {
	return integerPattern.MatchString(value) && value != "0" && (len(value) == 1 || value[0] != '0')
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

// ---------------------------------------------------------------------------
// Store

func (s *Store) ReviewProfiles(ctx context.Context) ([]ReviewProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, body, updated_at FROM review_profiles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []ReviewProfile{}
	for rows.Next() {
		var item ReviewProfile
		if err := rows.Scan(&item.Name, &item.Body, &item.UpdatedAt); err != nil {
			return nil, err
		}
		list = append(list, item)
	}
	return list, rows.Err()
}

func (s *Store) SaveReviewProfile(ctx context.Context, name, body string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO review_profiles(name, body, updated_at) VALUES(?,?,?)
ON CONFLICT(name) DO UPDATE SET body=excluded.body, updated_at=excluded.updated_at`,
		name, strings.TrimRight(body, "\n"), s.now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("save review profile: %w", err)
	}
	return nil
}

func (s *Store) ProjectReviewSetting(ctx context.Context, repository string) (ProjectReviewSetting, bool, error) {
	var setting ProjectReviewSetting
	err := s.db.QueryRowContext(ctx,
		`SELECT profile_name, profile_body, extra_instructions FROM project_review_settings WHERE repository = ?`, repository,
	).Scan(&setting.ProfileName, &setting.ProfileBody, &setting.ExtraInstructions)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectReviewSetting{}, false, nil
	}
	if err != nil {
		return ProjectReviewSetting{}, false, err
	}
	return setting, true, nil
}

func (s *Store) SaveProjectReviewSetting(ctx context.Context, repository string, setting ProjectReviewSetting) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO project_review_settings(repository, profile_name, profile_body, extra_instructions, updated_at) VALUES(?,?,?,?,?)
ON CONFLICT(repository) DO UPDATE SET profile_name=excluded.profile_name, profile_body=excluded.profile_body, extra_instructions=excluded.extra_instructions, updated_at=excluded.updated_at`,
		repository, setting.ProfileName, setting.ProfileBody, setting.ExtraInstructions, s.now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("save project review setting: %w", err)
	}
	return nil
}

func (s *Store) DeleteProjectReviewSetting(ctx context.Context, repository string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM project_review_settings WHERE repository = ?`, repository)
	if err != nil {
		return fmt.Errorf("delete project review setting: %w", err)
	}
	return nil
}

// Setting reads one instance setting; the second result is false when unset.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (s *Store) SaveSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO settings(key, value) VALUES(?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("save setting: %w", err)
	}
	return nil
}

// migrateReviewInstructions copies the MACH-26 free-text review instructions
// into each project's profile setting as extra_instructions so nothing is
// lost. Runs once, guarded by a settings marker.
func (s *Store) migrateReviewInstructions(ctx context.Context) error {
	if _, done, err := s.Setting(ctx, settingReviewInstructionsMigrated); err != nil || done {
		return err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT repository, instructions FROM review_instructions`)
	if err != nil {
		return err // table may not exist on fresh databases
	}
	defer rows.Close()
	type pair struct{ repository, instructions string }
	var legacy []pair
	for rows.Next() {
		var item pair
		if err := rows.Scan(&item.repository, &item.instructions); err != nil {
			return err
		}
		legacy = append(legacy, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range legacy {
		setting, exists, err := s.ProjectReviewSetting(ctx, item.repository)
		if err != nil {
			return err
		}
		if exists && setting.ExtraInstructions != "" {
			continue
		}
		setting.ExtraInstructions = item.instructions
		if err := s.SaveProjectReviewSetting(ctx, item.repository, setting); err != nil {
			return err
		}
	}
	return s.SaveSetting(ctx, settingReviewInstructionsMigrated, "1")
}

// ---------------------------------------------------------------------------
// Resolution

// resolveReviewProfileForProject computes the effective profile: project
// setting > global default (settings key, then config, then shipped default).
func (s *Server) resolveReviewProfileForProject(ctx context.Context, repository string) (source, name, body, extra string, err error) {
	setting, exists, err := s.store.ProjectReviewSetting(ctx, repository)
	if err != nil {
		return "", "", "", "", err
	}
	if exists {
		if strings.TrimSpace(setting.ProfileBody) != "" {
			name := strings.TrimSpace(setting.ProfileName)
			if name == "" {
				name = reviewProfileNameFromBody(setting.ProfileBody, "custom")
			}
			return "project", name, setting.ProfileBody, setting.ExtraInstructions, nil
		}
		if name = strings.TrimSpace(setting.ProfileName); name != "" {
			if profileBody, ok := s.lookupReviewProfile(ctx, name); ok {
				return "project", name, profileBody, setting.ExtraInstructions, nil
			}
		}
	}
	defaultName, err := s.defaultReviewProfileName(ctx)
	if err != nil {
		return "", "", "", "", err
	}
	if profileBody, ok := s.lookupReviewProfile(ctx, defaultName); ok {
		source := "global"
		if defaultName == defaultReviewProfileName {
			source = "default" // shipped fallback, nothing configured
		}
		return source, defaultName, profileBody, setting.ExtraInstructions, nil
	}
	shipped := ShippedReviewProfiles()
	if body, ok := shipped[defaultReviewProfileName]; ok {
		return "default", defaultReviewProfileName, body, setting.ExtraInstructions, nil
	}
	return "", "", "", "", errors.New("no review profile available")
}

// lookupReviewProfile finds a named profile: user library, then shipped.
func (s *Server) lookupReviewProfile(ctx context.Context, name string) (string, bool) {
	stored, err := s.store.ReviewProfiles(ctx)
	if err == nil {
		for _, profile := range stored {
			if profile.Name == name {
				return profile.Body, true
			}
		}
	}
	shipped := ShippedReviewProfiles()
	body, ok := shipped[name]
	return body, ok
}

func (s *Server) defaultReviewProfileName(ctx context.Context) (string, error) {
	if value, ok, err := s.store.Setting(ctx, settingReviewDefaultProfile); err != nil {
		return "", err
	} else if ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value), nil
	}
	if s.reviewDefaultProfile != "" {
		return s.reviewDefaultProfile, nil
	}
	return defaultReviewProfileName, nil
}

// ---------------------------------------------------------------------------
// HTTP handlers

type reviewProfileResponse struct {
	Project           string `json:"project"`
	Source            string `json:"source"`
	ProfileName       string `json:"profile_name"`
	Profile           string `json:"profile"`
	ExtraInstructions string `json:"extra_instructions"`
}

func (s *Server) getReviewProfile(response http.ResponseWriter, request *http.Request) {
	project := s.resolveReviewProject(request.URL.Query().Get("project"))
	source, name, body, extra, err := s.resolveReviewProfileForProject(request.Context(), project)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, reviewProfileResponse{
		Project: project, Source: source, ProfileName: name, Profile: body, ExtraInstructions: extra,
	})
}

type reviewProfileListResponse struct {
	Default  string          `json:"default"`
	Profiles []ReviewProfile `json:"profiles"`
}

func (s *Server) listReviewProfiles(response http.ResponseWriter, request *http.Request) {
	stored, err := s.store.ReviewProfiles(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	shipped := ShippedReviewProfiles()
	for name, body := range shipped {
		stored = append(stored, ReviewProfile{Name: name, Body: body, BuiltIn: true})
	}
	defaultName, err := s.defaultReviewProfileName(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, reviewProfileListResponse{Default: defaultName, Profiles: stored})
}

type saveReviewProfileRequest struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

func (s *Server) saveReviewProfile(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input saveReviewProfileRequest
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	name := strings.TrimSpace(input.Name)
	if !reviewProjectPattern.MatchString(name) || strings.Contains(name, "/") {
		writeError(response, http.StatusBadRequest, errors.New("profile name must be a simple identifier"))
		return
	}
	if len(input.Body) > maxReviewProfileBytes {
		writeError(response, http.StatusBadRequest, fmt.Errorf("profile body must be at most %d bytes", maxReviewProfileBytes))
		return
	}
	if issues := validateProfileBody(input.Body); len(issues) > 0 {
		writeJSON(response, http.StatusBadRequest, map[string]any{"errors": issues})
		return
	}
	if err := s.store.SaveReviewProfile(request.Context(), name, input.Body); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, ReviewProfile{Name: name, Body: strings.TrimRight(input.Body, "\n")})
}

type saveProjectReviewProfileRequest struct {
	Project           string `json:"project"`
	ProfileName       string `json:"profile_name"`
	ProfileBody       string `json:"profile_body"`
	ExtraInstructions string `json:"extra_instructions"`
}

func (s *Server) saveProjectReviewProfile(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input saveProjectReviewProfileRequest
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	project := strings.TrimSpace(input.Project)
	if !reviewProjectPattern.MatchString(project) {
		writeError(response, http.StatusBadRequest, errors.New("project must be a repository name or owner/name"))
		return
	}
	if len(input.ProfileBody) > maxReviewProfileBytes || len(input.ExtraInstructions) > maxReviewProfileBytes {
		writeError(response, http.StatusBadRequest, fmt.Errorf("profile body and extra instructions must each be at most %d bytes", maxReviewProfileBytes))
		return
	}
	if strings.TrimSpace(input.ProfileBody) != "" {
		if issues := validateProfileBody(input.ProfileBody); len(issues) > 0 {
			writeJSON(response, http.StatusBadRequest, map[string]any{"errors": issues})
			return
		}
	}
	key := s.resolveReviewProject(project)
	if strings.TrimSpace(input.ProfileName) == "" && strings.TrimSpace(input.ProfileBody) == "" && strings.TrimSpace(input.ExtraInstructions) == "" {
		if err := s.store.DeleteProjectReviewSetting(request.Context(), key); err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
	} else if err := s.store.SaveProjectReviewSetting(request.Context(), key, ProjectReviewSetting{
		ProfileName:       strings.TrimSpace(input.ProfileName),
		ProfileBody:       strings.TrimRight(input.ProfileBody, "\n"),
		ExtraInstructions: strings.TrimRight(input.ExtraInstructions, "\n"),
	}); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	source, name, body, extra, err := s.resolveReviewProfileForProject(request.Context(), key)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, reviewProfileResponse{Project: key, Source: source, ProfileName: name, Profile: body, ExtraInstructions: extra})
}

type reviewDefaultProfileRequest struct {
	Name string `json:"name"`
}

func (s *Server) getReviewDefaultProfile(response http.ResponseWriter, request *http.Request) {
	name, err := s.defaultReviewProfileName(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, reviewDefaultProfileRequest{Name: name})
}

func (s *Server) saveReviewDefaultProfile(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input reviewDefaultProfileRequest
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	name := strings.TrimSpace(input.Name)
	if _, ok := s.lookupReviewProfile(request.Context(), name); !ok {
		writeError(response, http.StatusBadRequest, fmt.Errorf("unknown review profile: %s", name))
		return
	}
	if err := s.store.SaveSetting(request.Context(), settingReviewDefaultProfile, name); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, input)
}

// ---------------------------------------------------------------------------
// Preview: resolved profile, lanes, reviewers and routing for a file list.
// No execution.

type reviewPreviewRequest struct {
	Project     string   `json:"project"`
	RepoProfile string   `json:"repo_profile"`
	Files       []string `json:"files"`
}

type reviewPreviewLane struct {
	Lane   string   `json:"lane"`
	Files  []string `json:"files"`
	Checks []string `json:"checks"`
}

type reviewPreviewPass struct {
	Name   string   `json:"name"`
	Skills []string `json:"skills"`
	Reason string   `json:"reason"`
}

type reviewPreviewResponse struct {
	Source    string              `json:"source"`
	Name      string              `json:"name"`
	Profile   string              `json:"profile"`
	Lanes     []reviewPreviewLane `json:"lanes"`
	Reviewers []reviewPreviewPass `json:"reviewers"`
	Notes     []string            `json:"notes,omitempty"`
}

func (s *Server) previewReviewProfile(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input reviewPreviewRequest
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	project := s.resolveReviewProject(input.Project)
	source, name, body, _, err := s.resolveReviewProfileForProject(request.Context(), project)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if strings.TrimSpace(input.RepoProfile) != "" {
		// A repository profile at the base ref outranks the stored setting.
		body = input.RepoProfile
		source = "repo"
		name = reviewProfileNameFromBody(body, "repo")
	}
	if issues := validateProfileBody(body); len(issues) > 0 {
		writeJSON(response, http.StatusBadRequest, map[string]any{"errors": issues})
		return
	}
	var parsed struct {
		Scope struct {
			LanesFrom string `yaml:"lanes_from"`
		} `yaml:"scope"`
		Checks struct {
			Lanes []struct {
				Lane     string   `yaml:"lane"`
				When     []string `yaml:"when"`
				Commands []any    `yaml:"commands"`
			} `yaml:"lanes"`
		} `yaml:"checks"`
		Reviewers []struct {
			Name        string   `yaml:"name"`
			Skills      []string `yaml:"skills"`
			Skill       string   `yaml:"skill"`
			Lanes       []string `yaml:"lanes"`
			Trigger     string   `yaml:"trigger"`
			RoutingOnly bool     `yaml:"routing_only"`
		} `yaml:"reviewers"`
		Passes []struct {
			Name        string   `yaml:"name"`
			Skills      []string `yaml:"skills"`
			Skill       string   `yaml:"skill"`
			Lanes       []string `yaml:"lanes"`
			Trigger     string   `yaml:"trigger"`
			RoutingOnly bool     `yaml:"routing_only"`
		} `yaml:"passes"`
		Routing struct {
			Fallback []string `yaml:"fallback"`
			Rules    []struct {
				When   []string `yaml:"when"`
				Passes []string `yaml:"passes"`
			} `yaml:"rules"`
		} `yaml:"routing"`
	}
	normalizedBody, err := normalizeProfileShape(body)
	if err != nil {
		writeError(response, http.StatusBadRequest, fmt.Errorf("cannot parse profile: %w", err))
		return
	}
	if err := yaml.Unmarshal([]byte(normalizedBody), &parsed); err != nil {
		writeError(response, http.StatusBadRequest, fmt.Errorf("cannot parse profile: %w", err))
		return
	}
	reviewers := parsed.Reviewers
	if len(reviewers) == 0 {
		reviewers = nil
		for _, pass := range parsed.Passes {
			reviewers = append(reviewers, pass)
		}
	}
	files := make([]string, 0, len(input.Files))
	for _, file := range input.Files {
		if strings.TrimSpace(file) != "" {
			files = append(files, strings.TrimSpace(file))
		}
	}
	filesReadable := len(files) > 0
	var notes []string
	if !filesReadable {
		notes = append(notes, "no file list given; showing the fallback reviewer set")
	}

	preview := reviewPreviewResponse{Source: source, Name: name, Profile: body, Lanes: []reviewPreviewLane{}, Reviewers: []reviewPreviewPass{}, Notes: notes}
	laneFiles := map[string][]string{}
	for _, lane := range parsed.Checks.Lanes {
		matched := []string{}
		for _, file := range files {
			for _, pattern := range lane.When {
				if globMatches(pattern, file) {
					matched = append(matched, file)
					break
				}
			}
		}
		laneFiles[lane.Lane] = matched
		checkNames := []string{}
		for _, command := range lane.Commands {
			switch typed := command.(type) {
			case string:
				checkNames = append(checkNames, typed)
			case map[string]any:
				if run, ok := typed["run"].(string); ok {
					checkNames = append(checkNames, run)
				}
			}
		}
		preview.Lanes = append(preview.Lanes, reviewPreviewLane{Lane: lane.Lane, Files: matched, Checks: checkNames})
	}

	selected := map[string]string{}
	addPass := func(name, reason string) {
		if name == "" {
			return
		}
		if _, seen := selected[name]; !seen {
			selected[name] = reason
		}
	}
	if !filesReadable {
		for _, name := range parsed.Routing.Fallback {
			addPass(name, "fallback")
		}
	} else {
		for _, rule := range parsed.Routing.Rules {
			matched := false
			for _, file := range files {
				for _, pattern := range rule.When {
					if globMatches(pattern, file) {
						matched = true
					}
				}
			}
			if matched {
				for _, pass := range rule.Passes {
					addPass(pass, "routing")
				}
			}
		}
		for _, reviewer := range reviewers {
			switch {
			case reviewer.Trigger == "on_request" || reviewer.RoutingOnly:
				continue
			case len(reviewer.Lanes) > 0:
				for _, lane := range reviewer.Lanes {
					if len(laneFiles[lane]) > 0 {
						addPass(reviewer.Name, "lane: "+lane)
					}
				}
			case len(parsed.Routing.Fallback) == 0:
				addPass(reviewer.Name, "profile default")
			default:
				for _, fallback := range parsed.Routing.Fallback {
					if fallback == reviewer.Name {
						addPass(reviewer.Name, "fallback")
					}
				}
			}
		}
	}
	for _, reviewer := range reviewers {
		if reason, ok := selected[reviewer.Name]; ok {
			skills := reviewer.Skills
			if len(skills) == 0 && reviewer.Skill != "" {
				skills = []string{reviewer.Skill}
			}
			preview.Reviewers = append(preview.Reviewers, reviewPreviewPass{Name: reviewer.Name, Skills: skills, Reason: reason})
		}
	}
	writeJSON(response, http.StatusOK, preview)
}

// ---------------------------------------------------------------------------
// Legacy migration: the MACH-26 markdown blob lives on only as the
// extra_instructions column migrated by migrateReviewInstructions; it is no
// longer served.
