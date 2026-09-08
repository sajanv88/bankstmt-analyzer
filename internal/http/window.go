package http

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	// monthLayout is the YYYY-MM format the `from` and `to` parameters use
	// and that the response echoes back.
	monthLayout = "2006-01"
	// defaultWindowMonths is how many months the visualization covers when
	// the request does not say.
	defaultWindowMonths = 3
	// maxWindowMonths caps the `months` parameter.
	maxWindowMonths = 12
)

// monthWindow is an inclusive range of whole calendar months. Both bounds
// are the first day of their month in UTC; the SQL window is derived from
// them so a caller cannot express a partial month.
type monthWindow struct {
	from time.Time
	to   time.Time
}

// periodStart is the first day of the window's first month.
func (w monthWindow) periodStart() time.Time { return w.from }

// periodEnd is the last day of the window's final month. The transaction
// queries are inclusive at both ends, so this has to be the month's final
// day rather than the first day of the next one.
func (w monthWindow) periodEnd() time.Time { return w.to.AddDate(0, 1, -1) }

// period renders the window for the response body.
func (w monthWindow) period() Period {
	return Period{From: w.from.Format(monthLayout), To: w.to.Format(monthLayout)}
}

// resolveWindow works out which months to report on.
//
// The default window is the last defaultWindowMonths months that carry
// data, counting back from anchor. `months` widens or narrows that count;
// `from` and `to` override it outright. Supplying only one of the two
// anchors the window at that end and derives the other from `months`.
func resolveWindow(query url.Values, anchor time.Time) (monthWindow, *Problem) {
	months, problem := parseMonths(query.Get("months"))
	if problem != nil {
		return monthWindow{}, problem
	}

	from, hasFrom, problem := parseMonthParam(query, "from")
	if problem != nil {
		return monthWindow{}, problem
	}
	to, hasTo, problem := parseMonthParam(query, "to")
	if problem != nil {
		return monthWindow{}, problem
	}

	switch {
	case hasFrom && hasTo:
		// Both supplied: `months` is irrelevant, the range is explicit.
	case hasFrom:
		to = maxMonth(from, truncateToMonth(anchor))
	case hasTo:
		from = to.AddDate(0, -(months - 1), 0)
	default:
		to = truncateToMonth(anchor)
		from = to.AddDate(0, -(months - 1), 0)
	}

	if from.After(to) {
		p := NewProblem(http.StatusBadRequest, "`from` must not be later than `to`.")
		p.InvalidParams = []InvalidParam{
			{Name: "from", Reason: "later than to"},
		}
		return monthWindow{}, p
	}
	return monthWindow{from: from, to: to}, nil
}

func parseMonths(raw string) (int, *Problem) {
	if raw == "" {
		return defaultWindowMonths, nil
	}
	months, err := strconv.Atoi(raw)
	if err != nil || months < 1 || months > maxWindowMonths {
		p := NewProblem(http.StatusBadRequest,
			fmt.Sprintf("`months` must be a whole number between 1 and %d.", maxWindowMonths))
		p.InvalidParams = []InvalidParam{
			{Name: "months", Reason: fmt.Sprintf("must be between 1 and %d", maxWindowMonths)},
		}
		return 0, p
	}
	return months, nil
}

// parseMonthParam reads an optional YYYY-MM parameter.
func parseMonthParam(query url.Values, name string) (time.Time, bool, *Problem) {
	raw := query.Get(name)
	if raw == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.ParseInLocation(monthLayout, raw, time.UTC)
	if err != nil {
		p := NewProblem(http.StatusBadRequest,
			fmt.Sprintf("`%s` must be a month in YYYY-MM format.", name))
		p.InvalidParams = []InvalidParam{{Name: name, Reason: "expected YYYY-MM"}}
		return time.Time{}, false, p
	}
	return parsed, true, nil
}

// truncateToMonth reduces t to the first day of its month in UTC.
func truncateToMonth(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func maxMonth(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
