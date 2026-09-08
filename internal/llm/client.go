package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
)

// maxErrorBodyBytes caps how much of a failed response is kept for the
// error message.
const maxErrorBodyBytes = 2048

// Statement is one bank statement's OCR output, ready to be presented to
// the model.
type Statement struct {
	// Filename is carried for the operator's benefit; it is not sent to
	// the model, which has no use for a client-supplied name.
	Filename string
	Markdown string
}

// Client calls the Azure OpenAI chat completions deployment.
type Client struct {
	url          string
	apiKey       string
	systemPrompt string
	httpClient   *http.Client
}

// NewClient builds a client for the configured deployment. systemPrompt is
// injected rather than read here so that the caller owns where it comes
// from and a test can supply its own.
func NewClient(cfg config.AzureOpenAI, systemPrompt string) (*Client, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("llm: endpoint must not be empty")
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return nil, fmt.Errorf("llm: the system prompt must not be empty")
	}

	target := fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
		endpoint, url.PathEscape(cfg.Deployment), url.QueryEscape(cfg.APIVersion))

	return &Client{
		url:          target,
		apiKey:       cfg.APIKey,
		systemPrompt: systemPrompt,
		httpClient:   &http.Client{Timeout: cfg.Timeout},
	}, nil
}

type chatRequest struct {
	Messages []message `json:"messages"`
	// Temperature is zero so the same statements produce the same
	// analysis: this is an extraction task, not a creative one.
	Temperature    float64        `json:"temperature"`
	ResponseFormat responseFormat `json:"response_format"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatResponse struct {
	Choices []struct {
		FinishReason string  `json:"finish_reason"`
		Message      message `json:"message"`
	} `json:"choices"`
}

// Analyze sends the statements to the model and returns the decoded
// analysis.
func (c *Client) Analyze(ctx context.Context, statements []Statement) (*Response, error) {
	if len(statements) == 0 {
		return nil, fmt.Errorf("llm: refusing to analyse zero statements")
	}

	body, err := json.Marshal(chatRequest{
		Messages: []message{
			{Role: "system", Content: c.systemPrompt},
			{Role: "user", Content: BuildUserMessage(statements)},
		},
		Temperature:    0,
		ResponseFormat: responseFormat{Type: "json_object"},
	})
	if err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("api-key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &TransportError{Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, newAPIError(resp)
	}

	var decoded chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("llm: decode response envelope: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return nil, &InvalidResponseError{Reason: "the reply contained no choices"}
	}
	choice := decoded.Choices[0]
	if choice.FinishReason == "length" {
		// A truncated reply is invalid JSON in a way that is worth naming
		// precisely: the fix is a shorter input or a larger limit, not a
		// retry.
		return nil, &InvalidResponseError{Reason: "the reply was truncated by the model's output limit"}
	}

	return parseResponse([]byte(choice.Message.Content))
}

// BuildUserMessage lays the statements out for the model, one labelled
// block each, in the order they were uploaded.
func BuildUserMessage(statements []Statement) string {
	var b strings.Builder
	for i, statement := range statements {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "--- STATEMENT %d ---\n%s", i+1, statement.Markdown)
	}
	return b.String()
}

// APIError is a non-2xx response from the deployment.
type APIError struct {
	StatusCode int
	Status     string
	Body       string
	RetryAfter time.Duration
}

func newAPIError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	err := &APIError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       strings.TrimSpace(string(body)),
	}
	if after, parseErr := time.ParseDuration(resp.Header.Get("Retry-After") + "s"); parseErr == nil {
		err.RetryAfter = after
	}
	return err
}

func (e *APIError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("llm: request failed with %s", e.Status)
	}
	return fmt.Sprintf("llm: request failed with %s: %s", e.Status, e.Body)
}

// Retryable reports whether another attempt could plausibly succeed.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= http.StatusInternalServerError
}

// TransportError is a failure to reach the deployment at all.
type TransportError struct{ Cause error }

func (e *TransportError) Error() string { return "llm: " + e.Cause.Error() }
func (e *TransportError) Unwrap() error { return e.Cause }

// Retryable is always true: the request never reached the deployment.
func (e *TransportError) Retryable() bool { return true }
