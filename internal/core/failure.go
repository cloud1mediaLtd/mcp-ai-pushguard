package core

import (
	"regexp"
	"strings"
)

// A failed job's logs are mostly success. A Go test suite that fails one
// assertion still prints several hundred "ok" lines, and the tail an agent or a
// human gets back is whatever happened to run last -- which is usually not the
// thing that failed.
//
// So a blind tail makes the caller grep the log themselves to answer the only
// question they asked: why did this fail? These are the markers that answer it.
//
// Ordered most specific first. The first pattern that matches anything wins,
// so a run with a real assertion failure reports that rather than the generic
// "make: *** [target] Error 2" it also emitted.
type failureMarker struct {
	pattern *regexp.Regexp
	// Every match must contain at least one of these literals. This skips
	// regex work on ordinary passing output, both for whole logs and lines.
	literals []string
}

func (m failureMarker) candidate(text string) bool {
	for _, literal := range m.literals {
		if strings.Contains(text, literal) {
			return true
		}
	}
	return false
}

var failureMarkers = []failureMarker{
	// Go test: the failing test and its message.
	{regexp.MustCompile(`^\s*--- FAIL: `), []string{"--- FAIL: "}},
	// Go build/vet/lint diagnostics: path:line:col: message.
	{regexp.MustCompile(`^[^\s:]+\.go:\d+:\d+: `), []string{".go:"}},
	// Node/vitest/jest.
	{regexp.MustCompile(`^\s*(?:✗|×|FAIL|●) `), []string{"✗ ", "× ", "FAIL ", "● "}},
	// Package-level Go failures, after the specific ones above.
	{regexp.MustCompile(`^(?:FAIL|ok)?\s*FAIL\s`), []string{"FAIL"}},
	// Panics and fatals.
	{regexp.MustCompile(`^(?:panic:|fatal error:|fatal: )`), []string{"panic:", "fatal error:", "fatal: "}},
	// A tool that failed for its own reasons rather than because the code is
	// wrong -- "Error: parallel golangci-lint is running", a missing binary, a
	// database that would not start. Ranked here because it is a real cause and
	// make is not, but below the code-level markers: a run with BOTH a failing
	// test and a noisy tool error is better summarised by the failing test.
	{regexp.MustCompile(`^(?:Error|ERROR|error): `), []string{"Error: ", "ERROR: ", "error: "}},
	// Make, last: it is the consequence of one of the above, not the cause.
	{regexp.MustCompile(`^make(?:\[\d+\])?: \*\*\* `), []string{"make"}},
}

// FailureContext is how many lines after a matched marker are worth keeping.
// A Go test failure puts its message on the following line or two; more than
// this and unrelated output starts bleeding in.
const FailureContext = 3

// ExtractFailure returns the lines of log that explain a failure, or "" when
// none of the markers match.
//
// It returns at most maxLines, taking the LAST matches rather than the first:
// a run that fails many packages is best summarised by where it gave up, and
// truncating from the front would bury that behind the earliest failure.
func ExtractFailure(logs string, maxLines int) string {
	if strings.TrimSpace(logs) == "" || maxLines <= 0 {
		return ""
	}
	for _, marker := range failureMarkers {
		if !marker.candidate(logs) {
			continue
		}
		type span struct{ start, end int }
		var windows []span
		// Only the ends of the current line and its three following context
		// lines are needed. Avoid allocating an entry for every line in a log.
		var recentEnds [FailureContext + 1]int
		used, nextStart, nextRow := 0, len(logs)+1, -1
		row := 0
		for end := len(logs); end >= 0; row++ {
			start := strings.LastIndexByte(logs[:end], '\n') + 1
			recentEnds[row%len(recentEnds)] = end
			if line := logs[start:end]; marker.candidate(line) && marker.pattern.MatchString(line) {
				count := min(row+1, FailureContext+1)
				windowEnd := recentEnds[(row-count+1)%len(recentEnds)]
				if nextRow >= 0 && row-nextRow <= count {
					count = row - nextRow
					windowEnd = nextStart - 1
				}
				cost := count
				if len(windows) == 0 && count > maxLines {
					cost = maxLines
					windowEnd = recentEnds[(row-cost+1)%len(recentEnds)]
				} else if len(windows) > 0 && windowEnd+1 < nextStart {
					cost++ // Omission separators count toward the line budget.
				}
				if cost > maxLines-used {
					break
				}
				windows = append(windows, span{start, windowEnd})
				used += cost
				nextStart, nextRow = start, row
				if used == maxLines {
					break
				}
			}
			end = start - 1
		}
		if len(windows) == 0 {
			continue
		}

		var out strings.Builder
		previousEnd := -1
		for i := len(windows) - 1; i >= 0; i-- {
			window := windows[i]
			if previousEnd >= 0 {
				out.WriteByte('\n')
				if window.start > previousEnd+1 {
					out.WriteString("  …\n")
				}
			}
			out.WriteString(logs[window.start:window.end])
			previousEnd = window.end
		}
		return out.String()
	}
	return ""
}

// namedFailure matches the header line of one Go test failure.
var namedFailure = regexp.MustCompile(`(?m)^\s*--- FAIL: (\S+)`)

// FailureNames lists every distinct failing test in a log, in order.
//
// ExtractFailure returns a bounded WINDOW — it drops from the front once the
// line budget is spent, so the last failure always survives and earlier ones
// silently do not. That is the right trade for a screenful of output and the
// wrong one for "what broke", and nothing said which had happened.
//
// It cost a real hour: two agents read the same failed job through
// `pushguard status`, one reported three failures and the other four, and the
// log actually held five. Each had a different subset and each believed it was
// the whole. A summary that can omit items must say that it did.
func FailureNames(logs string) []string {
	seen := map[string]bool{}
	names := make([]string, 0, 4)
	for _, m := range namedFailure.FindAllStringSubmatch(logs, -1) {
		// Subtests report as Parent/Child; the parent line is enough.
		name := m[1]
		if i := strings.Index(name, "/"); i > 0 {
			name = name[:i]
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}
