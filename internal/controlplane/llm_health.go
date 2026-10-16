package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"strings"
	"time"
)

// LLM endpoint health (MACH-69): "port open" is not enough. On 16 Oct the
// endpoint came back serving a different model and silently answered requests
// for deepseek-v4.1-flash with GLM-5.3, so health means: the endpoint answers
// /v1/models AND serves the expected model id. An optional tiny completion
// proves inference works, not just listing.

// llmHealthResult is one probe outcome.
type llmHealthResult struct {
	// Healthy means the endpoint may be used for the automatic requeue.
	Healthy bool
	// Mismatch means the endpoint answered but does not serve the expected
	// model id. Logged always; blocks the requeue when configured to.
	Mismatch bool
	// Detail carries the human-readable probe outcome for logs and alerts.
	Detail string
	// Models lists the model ids the endpoint reported serving.
	Models []string
}

// probeModelEndpoint probes the configured model endpoint. An unconfigured
// endpoint is never healthy: classification still runs, but the job is left
// for a human.
func (s *Server) probeModelEndpoint(ctx context.Context) llmHealthResult {
	endpoint := s.llmUnavailable.Endpoint
	if endpoint == "" {
		return llmHealthResult{Detail: "no model endpoint configured"}
	}
	timeout, err := s.health.RequestTimeout()
	if err != nil {
		timeout = 10 * time.Second
	}
	base := strings.TrimRight(endpoint, "/")
	models, err := fetchModelIDs(ctx, base+"/v1/models", timeout)
	if err != nil {
		return llmHealthResult{Detail: fmt.Sprintf("models probe failed: %v", err)}
	}
	result := llmHealthResult{Models: models}
	if expected := s.llmUnavailable.Model; expected != "" && !slices.Contains(models, expected) {
		result.Mismatch = true
		result.Detail = fmt.Sprintf("model mismatch: endpoint serves %v, expected %q", models, expected)
		log.Printf("llm health: %s", result.Detail)
		// A mismatch means requests for the expected model are silently
		// answered by a different model; only an operator-configured
		// override lets a requeue proceed on it.
		if s.llmUnavailable.Blocking() {
			return result
		}
	}
	if s.llmUnavailable.ProbeCompletion {
		if err := probeTinyCompletion(ctx, base+"/v1/chat/completions", timeout, s.llmUnavailable.Model); err != nil {
			result.Detail = fmt.Sprintf("completion probe failed: %v", err)
			return result
		}
	}
	result.Healthy = true
	result.Detail = fmt.Sprintf("endpoint healthy, serves %v", models)
	return result
}

// fetchModelIDs reads the model ids served by an OpenAI-compatible endpoint.
func fetchModelIDs(ctx context.Context, url string, timeout time.Duration) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, response.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}
	models := make([]string, 0, len(body.Data))
	for _, model := range body.Data {
		if model.ID != "" {
			models = append(models, model.ID)
		}
	}
	return models, nil
}

// probeTinyCompletion sends a one-token completion to prove the endpoint
// answers inference for the expected model.
func probeTinyCompletion(ctx context.Context, url string, timeout time.Duration, model string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s: status %d", url, response.StatusCode)
	}
	return nil
}
