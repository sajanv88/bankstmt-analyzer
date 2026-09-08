// Package ocr talks to the Azure-hosted Mistral OCR deployment, turning a
// statement PDF into markdown, one entry per page.
package ocr

import (
	"strings"
)

// Page is one page of a document as markdown.
type Page struct {
	// Index is the page's zero-based position as the service reported it.
	Index int `json:"index"`
	// Markdown is the extracted page content.
	Markdown string `json:"markdown"`
}

// Result is everything the OCR service returned for one document.
type Result struct {
	Pages []Page
}

// PageCount is how many pages the service extracted.
func (r Result) PageCount() int { return len(r.Pages) }

// Markdown joins the pages into one document, separated by a horizontal
// rule so the model can still see where each page began.
func (r Result) Markdown() string {
	if len(r.Pages) == 0 {
		return ""
	}
	parts := make([]string, 0, len(r.Pages))
	for _, page := range r.Pages {
		parts = append(parts, strings.TrimSpace(page.Markdown))
	}
	return strings.Join(parts, "\n\n---\n\n")
}
