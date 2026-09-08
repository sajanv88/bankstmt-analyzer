package ocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sajanv88/bankstmt-analyzer/internal/config"
)

// maxErrorBodyBytes caps how much of a failed response is kept for the
// error message. Enough to identify the fault, not enough to paste a whole
// document into a log line.
const maxErrorBodyBytes = 2048

// Client calls the Azure-hosted Mistral OCR deployment.
type Client struct {
	url        string
	apiKey     string
	model      string
	httpClient *http.Client
}

// NewClient builds a client from configuration. The HTTP client carries
// the configured timeout, so a hung upstream cannot pin a worker for the
// length of a queue lease.
func NewClient(cfg config.AzureOCR) (*Client, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	if endpoint == "" {
		return nil, fmt.Errorf("ocr: endpoint must not be empty")
	}
	return &Client{
		url:        endpoint + "/" + strings.TrimLeft(cfg.Path, "/"),
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		httpClient: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

// request is the OCR API's request body. The document travels as a data
// URI rather than a fetchable URL: the PDFs live in this service's own
// blob store, which the OCR service has no route to.
type request struct {
	Model              string   `json:"model"`
	Document           document `json:"document"`
	IncludeImageBase64 bool     `json:"include_image_base64"`
}

type document struct {
	Type        string `json:"type"`
	DocumentURL string `json:"document_url"`
}

// response mirrors Result field for field, so the decoded value converts
// straight across without copying members.
type response struct {
	Pages []Page `json:"pages"`
}

// Extract runs OCR over one PDF and returns its pages as markdown.
func (c *Client) Extract(ctx context.Context, pdf []byte) (Result, error) {
	if len(pdf) == 0 {
		return Result{}, fmt.Errorf("ocr: refusing to submit an empty document")
	}

	body, err := json.Marshal(request{
		Model: c.model,
		Document: document{
			Type:        "document_url",
			DocumentURL: "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdf),
		},
		IncludeImageBase64: false,
	})
	if err != nil {
		return Result{}, fmt.Errorf("ocr: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("ocr: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A transport failure is worth another attempt: the queue will
		// redeliver this step.
		return Result{}, &TransportError{Cause: err}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return Result{}, newAPIError(resp)
	}

	var decoded response
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return Result{}, fmt.Errorf("ocr: decode response: %w", err)
	}
	if len(decoded.Pages) == 0 {
		return Result{}, fmt.Errorf("ocr: the service returned no pages")
	}
	return Result(decoded), nil
}

// APIError is a non-2xx response from the OCR service.
type APIError struct {
	StatusCode int
	Status     string
	// Body is the truncated response body, kept for diagnosis.
	Body string
	// RetryAfter is the Retry-After header when the service sent one.
	RetryAfter time.Duration
	// URL is the request that failed, reported so a routing or model-name
	// mistake is readable from the error alone. The key travels in a
	// header, so nothing secret appears here.
	URL string
}

func newAPIError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	err := &APIError{
		StatusCode: resp.StatusCode,
		Status:     resp.Status,
		Body:       strings.TrimSpace(string(body)),
	}
	if resp.Request != nil && resp.Request.URL != nil {
		err.URL = resp.Request.URL.String()
	}
	if after, parseErr := time.ParseDuration(resp.Header.Get("Retry-After") + "s"); parseErr == nil {
		err.RetryAfter = after
	}
	return err
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("ocr: request failed with %s", e.Status)
	if e.URL != "" {
		msg += fmt.Sprintf(" for %s", e.URL)
	}
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Retryable reports whether another attempt could plausibly succeed: rate
// limiting and server-side faults are worth retrying, a rejected request is
// not. The pipeline uses this to decide whether to spend its retry budget.
func (e *APIError) Retryable() bool {
	return e.StatusCode == http.StatusTooManyRequests || e.StatusCode >= http.StatusInternalServerError
}

// TransportError is a failure to reach the service at all.
type TransportError struct{ Cause error }

func (e *TransportError) Error() string { return "ocr: " + e.Cause.Error() }
func (e *TransportError) Unwrap() error { return e.Cause }

// Retryable is always true: the request never reached the service, so
// nothing about it has been rejected.
func (e *TransportError) Retryable() bool { return true }
