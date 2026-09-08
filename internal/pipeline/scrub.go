package pipeline

import (
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// maxErrorMessageBytes bounds an error message before it is logged or
	// carried through the queue. Upstream services can return a whole
	// document in an error body.
	maxErrorMessageBytes = 400
	// maxFailureReasonBytes bounds what is written to uploads.failure_reason
	// and therefore served to clients.
	maxFailureReasonBytes = 500
	// redacted replaces any configured secret found in a message.
	redacted = "[redacted]"
)

// scrubber turns an upstream error into a message that is safe to log, to
// carry through the queue, and to serve to a client.
//
// Three things it removes: configured secrets, which upstream error bodies
// and connection errors do quote; newlines, which would let a multi-line
// upstream body break a log line into several; and length, since a stack
// trace or an echoed document belongs in neither a log field nor an API
// response.
type scrubber struct {
	secrets []string
}

func newScrubber(secrets []string) scrubber {
	cleaned := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		// Very short values would match everywhere and redact the whole
		// message; they are not credentials worth protecting anyway.
		if len(strings.TrimSpace(secret)) >= 8 {
			cleaned = append(cleaned, secret)
		}
	}
	// Longest first, so a secret that contains another (an API key inside
	// a DSN, say) is replaced as a whole rather than leaving a fragment.
	sort.Slice(cleaned, func(i, j int) bool { return len(cleaned[i]) > len(cleaned[j]) })
	return scrubber{secrets: cleaned}
}

// message renders err safely. A nil error becomes a fixed placeholder
// rather than an empty string, because the saga can report a failure whose
// cause did not survive the round trip through the queue.
func (s scrubber) message(err error) string {
	if err == nil {
		return "the step failed without reporting a reason"
	}
	return s.clean(err.Error())
}

func (s scrubber) clean(msg string) string {
	for _, secret := range s.secrets {
		msg = strings.ReplaceAll(msg, secret, redacted)
	}
	msg = strings.Join(strings.Fields(msg), " ")
	return truncate(msg, maxErrorMessageBytes)
}

// truncate shortens s to at most limit bytes without splitting a rune.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const ellipsis = "..."
	cut := limit - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
