package board

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"oddsboard/internal/journal"
	"oddsboard/internal/market"
)

// summarize explains a decision in plain words for the audit trail, e.g.
// "Falcons 1.85 → 1.80" or "Market suspended; trading locked: market
// suspended". Support staff read these to tell a customer what happened.
func summarize(def market.Definition, kind journal.Kind, res market.Result, before, after market.State) string {
	switch res.Verdict {
	case market.VerdictRejected:
		return "Rejected: " + res.Reason
	case market.VerdictDuplicate:
		return fmt.Sprintf("Duplicate of seq %d, ignored", res.Seq)
	case market.VerdictConflict:
		return "Conflict: " + res.Reason + "; trading locked until the state is re-proven"
	}
	var parts []string
	switch {
	case kind == journal.KindSnapshot:
		parts = append(parts, fmt.Sprintf("Resync snapshot through seq %d", res.Seq))
	case res.Verdict == market.VerdictLate:
		parts = append(parts, fmt.Sprintf("Late seq %d (%s)", res.Seq, res.Reason))
	}
	for _, ch := range res.Changes {
		parts = append(parts, describeChange(def, ch))
	}
	if len(res.Changes) == 0 {
		parts = append(parts, "no change")
	}
	parts = append(parts, tradability(before, after)...)
	return strings.Join(parts, "; ")
}

func describeChange(def market.Definition, ch market.Change) string {
	if ch.Field == "status" {
		switch market.Status(ch.To) {
		case market.StatusOpen:
			if ch.From == "" {
				return "Market open"
			}
			return "Market reopened"
		case market.StatusSuspended:
			return "Market suspended"
		case market.StatusClosed:
			return "Market closed"
		}
		return "Status " + ch.To
	}
	name := strings.TrimPrefix(ch.Field, "odds.")
	for _, o := range def.Outcomes {
		if string(o.ID) == name {
			name = o.Name
		}
	}
	if ch.From == "" {
		return fmt.Sprintf("%s %s", name, ch.To)
	}
	return fmt.Sprintf("%s %s → %s", name, ch.From, ch.To)
}

func tradability(before, after market.State) []string {
	var out []string
	if len(after.Gaps) > 0 && !slices.Equal(before.Gaps, after.Gaps) {
		out = append(out, "missing "+describeRanges(after.Gaps))
	}
	switch {
	case !before.Tradable && after.Tradable:
		out = append(out, "now tradable")
	case before.Tradable && !after.Tradable:
		out = append(out, "trading locked: "+describeReasons(after.Reasons))
	}
	return out
}

func healthSummary(before, after market.State, staleAfter time.Duration) string {
	var parts []string
	switch {
	case after.Feed == market.FeedStale && before.Feed != market.FeedStale:
		parts = append(parts, fmt.Sprintf("Feed stale: no new message for over %s", staleAfter))
	case after.Feed != before.Feed:
		parts = append(parts, "Feed "+string(after.Feed))
	default:
		parts = append(parts, "State re-evaluated")
	}
	return strings.Join(append(parts, tradability(before, after)...), "; ")
}

func describeRanges(rs market.Ranges) string {
	var parts []string
	for _, r := range rs {
		if r.From == r.To {
			parts = append(parts, fmt.Sprintf("seq %d", r.From))
		} else {
			parts = append(parts, fmt.Sprintf("seq %d–%d", r.From, r.To))
		}
	}
	return strings.Join(parts, ", ")
}

var reasonText = map[market.Reason]string{
	market.ReasonNoData:      "no data yet",
	market.ReasonIncomplete:  "incomplete data",
	market.ReasonSequenceGap: "missing feed messages",
	market.ReasonConflict:    "conflicting feed data",
	market.ReasonFeedStale:   "feed stale",
	market.ReasonSuspended:   "market suspended",
	market.ReasonClosed:      "market closed",
}

func describeReasons(rs []market.Reason) string {
	var parts []string
	for _, r := range rs {
		if t, ok := reasonText[r]; ok {
			parts = append(parts, t)
		} else {
			parts = append(parts, string(r))
		}
	}
	return strings.Join(parts, ", ")
}
