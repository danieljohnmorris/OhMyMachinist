package issues

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// VerifyHMAC checks Linear/Plane-style SHA-256 webhook signatures. Accepted
// signatures are either raw hex or "sha256=" prefixed hex.
func VerifyHMAC(secret string, body []byte, signature string) bool {
	if secret == "" || signature == "" {
		return false
	}
	signature = strings.TrimPrefix(signature, "sha256=")
	expectedBytes, err := hex.DecodeString(signature)
	if err != nil || len(expectedBytes) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), expectedBytes)
}

type webhookIssue struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Key         string `json:"key"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

type webhookEnvelope struct {
	Data struct {
		Issue webhookIssue `json:"issue"`
	} `json:"data"`
	Issue webhookIssue `json:"issue"`
}

// ParseIssueWebhook extracts a source-neutral issue from either Plane's root
// issue payload or Linear's nested data payload. Callers must verify the
// signature before invoking it.
func ParseIssueWebhook(body []byte) (Issue, error) {
	var envelope webhookEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Issue{}, fmt.Errorf("parse issue webhook: %w", err)
	}
	payload := envelope.Issue
	if payload.ID == "" && payload.Identifier == "" && payload.Key == "" {
		payload = envelope.Data.Issue
	}
	key := firstNonEmpty(payload.Identifier, payload.ID, payload.Key)
	title := firstNonEmpty(payload.Title, payload.Name)
	if key == "" || title == "" {
		return Issue{}, fmt.Errorf("issue webhook is missing key or title")
	}
	return Issue{Key: key, Title: title, Description: payload.Description, URL: payload.URL}, nil
}

// WebhookHandler verifies a request before passing the issue to the submitter.
// Mount it behind an HTTPS proxy and keep the secret in local configuration.
type WebhookHandler struct {
	Secret          string
	SignatureHeader string
	Submit          func(Issue) error
}

func (h WebhookHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		response.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.Secret == "" || h.Submit == nil {
		response.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, 1<<20))
	if err != nil {
		http.Error(response, "read request body", http.StatusBadRequest)
		return
	}
	signatureHeader := h.SignatureHeader
	if signatureHeader == "" {
		signatureHeader = "X-Signature"
	}
	if !VerifyHMAC(h.Secret, body, request.Header.Get(signatureHeader)) {
		http.Error(response, "invalid signature", http.StatusUnauthorized)
		return
	}
	issue, err := ParseIssueWebhook(body)
	if err != nil {
		http.Error(response, err.Error(), http.StatusBadRequest)
		return
	}
	if err = h.Submit(issue); err != nil {
		http.Error(response, "submit issue", http.StatusInternalServerError)
		return
	}
	response.WriteHeader(http.StatusAccepted)
}
