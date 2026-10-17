// Package plane implements the Plane intake domain: webhook signature
// verification and payload parsing, label and approve-bridge rules,
// description-to-text extraction, spec hashing, project mapping, the coder
// prompt template, run-to-ticket state-sync decisions and alert payloads.
//
// The package is deliberately free of I/O so every rule is testable in
// isolation; the control plane supplies the HTTP client and database state.
package plane

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
)

// Defaults mirror the retired plane-intake.py configuration.
const (
	// DefaultURL is a neutral placeholder. The real Plane URL comes from the
	// [plane] config or PLANE_URL; no personal host is baked into the tool.
	DefaultURL            = "https://plane.example.com"
	DefaultWorkspace      = "dan"
	DefaultTriggerLabel   = "factory"
	DefaultApproveLabel   = "approved"
	DefaultFailedLabel    = "failed"
	DefaultProgressState  = "In Progress"
	DefaultReviewState    = "In Review"
	DefaultTriggerLabelID = "d9bf0176-64e3-4db6-a5d0-5047a935c858"
	DefaultSupervisor     = "LocalAI"
	// MaxBodyBytes caps a webhook payload, matching the legacy 2 MB limit.
	MaxBodyBytes = 2 << 20
)

// Label is one Plane label as carried on a work item.
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Issue is a Plane work item reduced to the fields intake cares about. It is
// decoded from either a webhook payload's data object or a REST issue object.
type Issue struct {
	ID                  string
	Name                string
	SequenceID          int64
	ProjectID           string
	ProjectIdentifier   string
	StateID             string
	StateName           string
	StateGroup          string
	Labels              []Label
	LabelIDs            []string
	DescriptionHTML     string
	DescriptionStripped string
	IsDraft             bool
	DeletedAt           string
	ArchivedAt          string
	URL                 string
}

// rawIssue is the wire shape shared by webhook data and REST responses.
type rawIssue struct {
	ID                  string          `json:"id"`
	Name                string          `json:"name"`
	SequenceID          json.Number     `json:"sequence_id"`
	Project             string          `json:"project"`
	ProjectID           string          `json:"project_id"`
	DescriptionHTML     string          `json:"description_html"`
	DescriptionStripped string          `json:"description_stripped"`
	IsDraft             bool            `json:"is_draft"`
	DeletedAt           string          `json:"deleted_at"`
	ArchivedAt          string          `json:"archived_at"`
	URL                 string          `json:"url"`
	State               json.RawMessage `json:"state"`
	StateID             string          `json:"state_id"`
	StateDetail         json.RawMessage `json:"state_detail"`
	Labels              json.RawMessage `json:"labels"`
	LabelDetails        json.RawMessage `json:"label_details"`
	LabelIDs            json.RawMessage `json:"label_ids"`
	ProjectDetail       struct {
		Identifier string `json:"identifier"`
	} `json:"project_detail"`
}

// ParseIssue decodes a Plane work item from a webhook data object or a REST
// issue object.
func ParseIssue(raw []byte) (Issue, error) {
	var decoded rawIssue
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Issue{}, fmt.Errorf("parse plane issue: %w", err)
	}
	return decoded.issue(), nil
}

func (raw rawIssue) issue() Issue {
	issue := Issue{
		ID:                  raw.ID,
		Name:                raw.Name,
		ProjectID:           firstNonEmpty(raw.Project, raw.ProjectID),
		ProjectIdentifier:   raw.ProjectDetail.Identifier,
		DescriptionHTML:     raw.DescriptionHTML,
		DescriptionStripped: raw.DescriptionStripped,
		IsDraft:             raw.IsDraft,
		DeletedAt:           normaliseNull(raw.DeletedAt),
		ArchivedAt:          normaliseNull(raw.ArchivedAt),
		URL:                 raw.URL,
	}
	if sequence, err := strconv.ParseInt(raw.SequenceID.String(), 10, 64); err == nil {
		issue.SequenceID = sequence
	}
	issue.StateID = firstNonEmpty(raw.StateID, stateFieldID(raw.State), stateFieldID(raw.StateDetail))
	issue.StateName, issue.StateGroup = stateNameGroup(raw.StateDetail)
	if issue.StateName == "" {
		issue.StateName, issue.StateGroup = stateNameGroup(raw.State)
	}
	issue.Labels = decodeLabels(raw.Labels, raw.LabelDetails)
	issue.LabelIDs = decodeLabelIDs(raw.LabelIDs)
	return issue
}

// stateFieldID returns the UUID from a state field that is either a string or
// an object with an id.
func stateFieldID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var id string
	if err := json.Unmarshal(raw, &id); err == nil {
		return id
	}
	var named struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &named); err == nil {
		return named.ID
	}
	return ""
}

func stateNameGroup(raw json.RawMessage) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	var named struct {
		Name  string `json:"name"`
		Group string `json:"group"`
	}
	if err := json.Unmarshal(raw, &named); err == nil {
		return named.Name, named.Group
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		return name, ""
	}
	return "", ""
}

// decodeLabels flattens Plane's several label representations (objects with
// id/name) into a single deduplicated list.
func decodeLabels(fields ...json.RawMessage) []Label {
	seen := map[string]bool{}
	labels := []Label{}
	for _, field := range fields {
		if len(field) == 0 {
			continue
		}
		var entries []Label
		if err := json.Unmarshal(field, &entries); err == nil {
			for _, label := range entries {
				if label.ID == "" && label.Name == "" {
					continue
				}
				key := label.ID + "\x00" + label.Name
				if seen[key] {
					continue
				}
				seen[key] = true
				labels = append(labels, label)
			}
		}
	}
	return labels
}

// decodeLabelIDs flattens label_ids entries, which may be bare strings or
// objects.
func decodeLabelIDs(field json.RawMessage) []string {
	if len(field) == 0 {
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(field, &raw); err != nil {
		return nil
	}
	ids := []string{}
	for _, entry := range raw {
		var id string
		if err := json.Unmarshal(entry, &id); err == nil && id != "" {
			ids = append(ids, id)
			continue
		}
		var named struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(entry, &named); err == nil && named.ID != "" {
			ids = append(ids, named.ID)
		}
	}
	return ids
}

// LabelNames returns the lower-cased label names on an issue, substituting the
// trigger name when a label's UUID is one of the configured trigger IDs.
func LabelNames(issue Issue, trigger string, triggerIDs []string) []string {
	wanted := map[string]bool{}
	for _, id := range triggerIDs {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = true
		}
	}
	names := []string{}
	for _, label := range issue.Labels {
		if label.Name != "" {
			names = append(names, strings.ToLower(label.Name))
		}
		if wanted[label.ID] {
			names = append(names, strings.ToLower(trigger))
		}
	}
	for _, id := range issue.LabelIDs {
		if wanted[id] {
			names = append(names, strings.ToLower(trigger))
		}
	}
	return names
}

// Triggered reports whether an issue carries the trigger label, by name or by
// configured label UUID.
func Triggered(issue Issue, trigger string, triggerIDs []string) bool {
	for _, name := range LabelNames(issue, trigger, triggerIDs) {
		if name == strings.ToLower(trigger) {
			return true
		}
	}
	return false
}

// LabelPresent reports whether an issue carries the named label by name or by
// one of the given label UUIDs.
func LabelPresent(issue Issue, name string, ids []string) bool {
	for _, label := range issue.Labels {
		if strings.EqualFold(label.Name, name) {
			return true
		}
	}
	for _, id := range ids {
		if id == "" {
			continue
		}
		for _, label := range issue.Labels {
			if label.ID == id {
				return true
			}
		}
		for _, candidate := range issue.LabelIDs {
			if candidate == id {
				return true
			}
		}
	}
	return false
}

// Eligible reports whether a work item should be processed: not a draft, not
// deleted, and carrying the trigger label.
func Eligible(issue Issue, trigger string, triggerIDs []string) bool {
	if issue.IsDraft {
		return false
	}
	if issue.DeletedAt != "" || issue.ArchivedAt != "" {
		return false
	}
	return Triggered(issue, trigger, triggerIDs)
}

// NeedsApproveBridge reports whether an issue carries the approve label but
// not the trigger label, and so should have the trigger label added.
func NeedsApproveBridge(issue Issue, approve string, approveIDs []string, trigger string, triggerIDs []string) bool {
	if issue.IsDraft || issue.DeletedAt != "" || issue.ArchivedAt != "" {
		return false
	}
	if !LabelPresent(issue, approve, approveIDs) {
		return false
	}
	return !LabelPresent(issue, trigger, triggerIDs)
}

// Key is the human ticket key, "IDENT-42".
func Key(identifier string, sequence int64) string {
	return identifier + "-" + strconv.FormatInt(sequence, 10)
}

// Title combines a ticket key with its name for the job title.
func Title(key, name string) string {
	key = strings.TrimSpace(key)
	name = strings.TrimSpace(name)
	switch {
	case key == "":
		return name
	case name == "":
		return key
	default:
		return key + ": " + name
	}
}

// Spec extracts the ticket specification: the rendered description HTML when
// present, else Plane's stripped plain-text description.
func Spec(issue Issue) string {
	if text := HTMLToText(issue.DescriptionHTML); text != "" {
		return text
	}
	return issue.DescriptionStripped
}

// SpecHash is the eight-character hash of the ticket name and spec. A change
// to either produces a new job; any other edit leaves the hash unchanged.
func SpecHash(name, spec string) string {
	sum := sha256.Sum256([]byte(name + "\n" + spec))
	return hex.EncodeToString(sum[:])[:8]
}

// RepoMap resolves a Plane project to a Machinist repository. Repositories map
// project id or identifier to a repository; Identifiers maps project id to
// identifier. An empty repository means the project is unmapped.
type RepoMap struct {
	Repositories map[string]string
	Identifiers  map[string]string
	Default      string
}

// RepoFor returns the repository for a project, matching the project UUID
// first, then the identifier, then the default. ok is false when the project
// has no mapping and no default is configured.
func (m RepoMap) RepoFor(projectID, identifier string) (string, bool) {
	if repo := strings.TrimSpace(m.Repositories[projectID]); repo != "" {
		return repo, true
	}
	if repo := strings.TrimSpace(m.Repositories[identifier]); repo != "" {
		return repo, true
	}
	if repo := strings.TrimSpace(m.Default); repo != "" {
		return repo, true
	}
	return "", false
}

// IdentifierFor returns the identifier recorded for a project UUID, falling
// back to the project UUID's short prefix.
func (m RepoMap) IdentifierFor(projectID string) string {
	if identifier := m.Identifiers[projectID]; identifier != "" {
		return identifier
	}
	if len(projectID) > 8 {
		return projectID[:8]
	}
	return projectID
}

// SupervisorMap routes PR-opened alerts to a ticket supervisor bot. The ticket
// key's identifier prefix selects the bot; "_default" is the fallback.
type SupervisorMap map[string]string

// SupervisorFor returns the supervisor bound to a ticket key, or the default.
func (m SupervisorMap) SupervisorFor(key string) string {
	identifier := strings.ToUpper(strings.SplitN(key, "-", 2)[0])
	if supervisor := strings.TrimSpace(m[identifier]); supervisor != "" {
		return supervisor
	}
	if supervisor := strings.TrimSpace(m["_default"]); supervisor != "" {
		return supervisor
	}
	return DefaultSupervisor
}

// VerifySignature checks Plane's HMAC-SHA256 X-Plane-Signature, tolerating the
// re-serialised body Plane CE signs in some SDKs. Both raw hex and a
// "sha256=" prefix are accepted.
func VerifySignature(secret string, body []byte, header string) bool {
	if secret == "" || header == "" {
		return false
	}
	header = strings.TrimPrefix(strings.TrimSpace(header), "sha256=")
	expected, err := hex.DecodeString(header)
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	candidates := [][]byte{body}
	for _, candidate := range reserializations(body) {
		duplicate := false
		for _, existing := range candidates {
			if bytes.Equal(existing, candidate) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			candidates = append(candidates, candidate)
		}
	}
	for _, candidate := range candidates {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(candidate)
		if hmac.Equal(mac.Sum(nil), expected) {
			return true
		}
	}
	return false
}

// reserializations returns order-preserving normalisations of a JSON body: a
// compact form (no spaces after ":" or ",") and a spaced form, matching the
// two serialisations Plane CE and its SDKs may sign. Key order is preserved so
// a signature made over json.dumps(payload) still verifies.
func reserializations(body []byte) [][]byte {
	return [][]byte{reserialize(body, false), reserialize(body, true)}
}

// reserialize re-emits JSON with normalised separators, copying strings and
// numbers verbatim.
func reserialize(body []byte, spaced bool) []byte {
	colon, comma := []byte(":"), []byte(",")
	if spaced {
		colon, comma = []byte(": "), []byte(", ")
	}
	out := make([]byte, 0, len(body)+8)
	inString := false
	escaped := false
	for index := range body {
		character := body[index]
		if inString {
			out = append(out, character)
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
			out = append(out, character)
		case ' ', '\t', '\n', '\r':
			// Skip insignificant whitespace.
		case ':':
			out = append(out, colon...)
		case ',':
			out = append(out, comma...)
		default:
			out = append(out, character)
		}
	}
	return out
}

// Event is the classification of a webhook delivery.
type Event struct {
	Name     string
	Action   string
	Delivery string
}

// envelope is the Plane CE webhook frame.
type envelope struct {
	Event  string          `json:"event"`
	Action string          `json:"action"`
	Data   json.RawMessage `json:"data"`
}

// ParseWebhook classifies a Plane delivery and extracts its work item. It
// accepts CE v1 (event "issue", action create/update) and v2
// ("workitem.created"/"workitem.updated"). ok is false for deliveries that do
// not describe an issue change.
func ParseWebhook(body []byte, delivery string) (Event, Issue, bool, error) {
	var frame envelope
	if err := json.Unmarshal(body, &frame); err != nil {
		return Event{}, Issue{}, false, fmt.Errorf("parse plane webhook: %w", err)
	}
	event := Event{Name: frame.Event, Action: frame.Action, Delivery: delivery}
	if !isIssueEvent(frame.Event, frame.Action) {
		return event, Issue{}, false, nil
	}
	data := frame.Data
	if len(data) == 0 {
		return event, Issue{}, false, fmt.Errorf("plane webhook has no data")
	}
	issue, err := ParseIssue(data)
	if err != nil {
		return event, Issue{}, false, err
	}
	return event, issue, true, nil
}

// isIssueEvent reports whether a delivery describes an issue create or update.
func isIssueEvent(event, action string) bool {
	switch strings.ToLower(action) {
	case "create", "update", "created", "updated":
		return event == "issue" || strings.HasPrefix(event, "workitem.")
	}
	if strings.HasSuffix(event, ".created") || strings.HasSuffix(event, ".updated") {
		return strings.HasPrefix(event, "workitem.")
	}
	return false
}

// prMarker matches the "deliver-pr: PR: <url>" marker printed by the delivery
// step.
var prMarker = regexp.MustCompile(`deliver-pr: PR: (\S+)`)

// ParsePullRequest scans a run event stream for the delivered PR URL.
func ParsePullRequest(events string) (string, bool) {
	for _, line := range strings.Split(events, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			Encoding string `json:"encoding"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Type != "process.output" {
			continue
		}
		data := event.Data
		if event.Encoding == "base64" {
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				continue
			}
			data = string(decoded)
		}
		if match := prMarker.FindStringSubmatch(data); match != nil {
			return match[1], true
		}
	}
	return "", false
}

// Sync phases applied to a Plane ticket from a Machinist run.
const (
	PhaseStart  = "start"
	PhaseReview = "review"
	PhaseFailed = "failed"
)

// PhaseFor maps a run state to the ticket sync phase, if any.
func PhaseFor(runState string) (string, bool) {
	switch runState {
	case "running":
		return PhaseStart, true
	case "succeeded":
		return PhaseReview, true
	case "failed":
		return PhaseFailed, true
	}
	return "", false
}

// StartMovesState reports whether a run start should move the ticket to the
// progress state: only from a backlog or unstarted ticket.
func StartMovesState(group string) bool {
	group = strings.ToLower(group)
	return group == "backlog" || group == "unstarted"
}

// ReviewMovesState reports whether a delivered run should move the ticket to
// the review state. Done and Cancelled tickets are never touched.
func ReviewMovesState(group string) bool {
	group = strings.ToLower(group)
	return group != "completed" && group != "cancelled"
}

// FailedLabelPatch returns the new label list with the failed label added, and
// whether the patch is needed.
func FailedLabelPatch(labels []string, failedID string) ([]string, bool) {
	if failedID == "" {
		return labels, false
	}
	for _, label := range labels {
		if label == failedID {
			return labels, false
		}
	}
	patched := append(append([]string{}, labels...), failedID)
	return patched, true
}

// DropFailedLabelPatch returns the new label list with the failed label
// removed, and whether the patch is needed.
func DropFailedLabelPatch(labels []string, failedID string) ([]string, bool) {
	if failedID == "" {
		return labels, false
	}
	patched := make([]string, 0, len(labels))
	removed := false
	for _, label := range labels {
		if label == failedID {
			removed = true
			continue
		}
		patched = append(patched, label)
	}
	return patched, removed
}

// StateAlert is the state-change alert payload written to the alert spool.
type StateAlert struct {
	Type       string         `json:"type"`
	DetectedAt string         `json:"detected_at"`
	Event      string         `json:"event"`
	Action     string         `json:"action"`
	Delivery   string         `json:"delivery"`
	Plane      StateAlertItem `json:"plane"`
	StateFrom  *StateSnapshot `json:"state_from"`
	StateTo    StateSnapshot  `json:"state_to"`
}

// StateAlertItem identifies the ticket in a state alert.
type StateAlertItem struct {
	Key       string `json:"key"`
	IssueID   string `json:"issue_id"`
	ProjectID string `json:"project_id"`
	Repo      string `json:"repo"`
	URL       string `json:"url"`
	Name      string `json:"name"`
}

// StateSnapshot is a ticket state before or after a transition.
type StateSnapshot struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"`
}

// PROpenedAlert is the PR-opened notification written to the alert spool.
type PROpenedAlert struct {
	Type        string         `json:"type"`
	Timestamp   string         `json:"ts"`
	Supervisor  string         `json:"supervisor"`
	Notify      string         `json:"notify"`
	PlaneKey    string         `json:"plane_key"`
	PlaneURL    string         `json:"plane_url"`
	PRURL       string         `json:"pr_url"`
	JobID       string         `json:"job_id"`
	Repo        string         `json:"repo"`
	MachinistUI string         `json:"machinist_run"`
	Plane       StateAlertItem `json:"plane"`
	Message     string         `json:"message"`
}

// NewPROpenedAlert builds the PR-opened notification. Auto-merge projects drop
// the "do not merge without Daniel" sentence.
func NewPROpenedAlert(supervisor, key, planeURL, prURL, jobID, repo, runURL, timestamp string, autoMerge bool) PROpenedAlert {
	message := fmt.Sprintf("%s: Machinist run %s succeeded and opened %s (Plane: %s). Review only; do not merge without Daniel.", key, jobID, prURL, planeURL)
	if autoMerge {
		message = fmt.Sprintf("%s: Machinist run %s succeeded and opened %s (Plane: %s).", key, jobID, prURL, planeURL)
	}
	return PROpenedAlert{
		Type: "machinist.pr_opened", Timestamp: timestamp, Supervisor: supervisor,
		Notify:   "SendToAgent -> " + supervisor + " (not Daniel)",
		PlaneKey: key, PlaneURL: planeURL, PRURL: prURL, JobID: jobID, Repo: repo,
		MachinistUI: runURL, Plane: StateAlertItem{Key: key}, Message: message,
	}
}

// BrowseURL is the human-facing ticket URL for a key.
func BrowseURL(baseURL, workspace, key string) string {
	return strings.TrimRight(baseURL, "/") + "/" + workspace + "/browse/" + key + "/"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// normaliseNull maps Plane's stringified nulls to an empty string.
func normaliseNull(value string) string {
	if value == "None" || value == "null" {
		return ""
	}
	return value
}

// htmlEscape escapes a value for embedding in comment HTML.
func htmlEscape(value string) string { return html.EscapeString(value) }
