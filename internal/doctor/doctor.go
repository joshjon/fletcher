// Package doctor runs a battery of checks against a Fletcher install
// and produces both a human-readable diagnostic and a prioritised
// action plan the operator can follow. It's the implementation behind
// `fletcher doctor`. See DESIGN.md §13 Phase 16.
//
// Each check returns a Result that may contribute a PlanStep to the
// renderer; the renderer dedupes and orders steps so the operator
// sees one coherent plan instead of N independent fix hints.
package doctor

import (
	"context"
	"sort"
)

// Status is the verdict of one check.
type Status int

// Status values, ordered from best to worst for sorting.
const (
	StatusOK Status = iota
	StatusWarn
	StatusFail
	StatusSkip // platform doesn't support this check (e.g. /dev/net/tun on macOS dev)
)

// String renders a Status as the short label used in the CLI output.
func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	case StatusSkip:
		return "skip"
	}
	return "unknown"
}

// Category groups results in the rendered table. Each category is a
// header in the CLI output; results within a category are listed in
// the order the checks ran.
type Category string

// Category values used by Fletcher's checks today.
const (
	CategoryDaemon       Category = "DAEMON"
	CategoryHost         Category = "HOST"
	CategoryNetwork      Category = "NETWORK"
	CategoryRouter       Category = "ROUTER"
	CategoryReachability Category = "PUBLIC REACHABILITY"
	CategoryProviders    Category = "MODEL PROVIDERS"
)

// Result is one check's outcome.
type Result struct {
	// Category groups this result under a header in the rendered output.
	Category Category
	// Name is the short label that appears next to the status icon
	// (e.g. "Running", "Public IP", "UPnP IGD reachable").
	Name string
	// Status is the verdict.
	Status Status
	// Detail is a one-line elaboration shown under Name when non-empty.
	Detail string
	// Plan, when non-nil, contributes to the action-plan section. Multiple
	// results pointing at the same underlying fix should share a Plan with
	// the same ID so the renderer dedupes them.
	Plan *PlanStep
}

// Priority orders plan steps. Blockers come before followups.
type Priority int

// Priority values.
const (
	PriorityBlocker  Priority = 0
	PriorityFollowup Priority = 1
)

// PlanStep is one entry in the "WHAT TO DO NEXT" section.
type PlanStep struct {
	// ID dedupes related steps. Multiple Results contributing the same
	// ID collapse into one rendered step (e.g. UPnP-failed and
	// no-public-endpoint both point at "configure-endpoint").
	ID string
	// Priority controls ordering: blockers first, then followups.
	Priority Priority
	// Title is the imperative summary ("Get a public endpoint working").
	Title string
	// Why is one or two sentences explaining the consequence of leaving
	// this unfixed. Helps the user decide whether to act.
	Why string
	// Options is a set of alternative ways to fix this; rendered as
	// labelled A/B/C blocks. A step with one Option still renders
	// cleanly without the A/B/C labels.
	Options []PlanOption
}

// PlanOption is one of several alternative remediations for a step.
type PlanOption struct {
	// Label is the short header ("Enable UPnP on your router").
	Label string
	// Steps are concrete copy-pasteable lines. Use placeholders
	// (`<your-public-ip>`) or commands that print the user's value
	// at runtime (`ip route | awk '/default/{print $3}'`) instead of
	// hard-coded IPs / brand-specific paths.
	Steps []string
}

// Checker runs one diagnostic and returns its Result.
type Checker interface {
	Check(ctx context.Context) Result
}

// CheckerFunc adapts a free function into a Checker.
type CheckerFunc func(ctx context.Context) Result

// Check satisfies the Checker interface.
func (f CheckerFunc) Check(ctx context.Context) Result { return f(ctx) }

// Run executes every checker in order and returns the collected
// results. Checkers are run sequentially because some probes
// (especially the UPnP one) take seconds and contention doesn't help.
func Run(ctx context.Context, checkers []Checker) []Result {
	out := make([]Result, 0, len(checkers))
	for _, c := range checkers {
		out = append(out, c.Check(ctx))
	}
	return out
}

// CollectPlan extracts the de-duplicated, ordered list of PlanSteps
// from a set of results. Steps with the same ID are merged (the first
// occurrence wins on Title / Why, options are unioned). Output order
// is blocker-first, then followup, then insertion order within a
// priority bucket.
func CollectPlan(results []Result) []PlanStep {
	type entry struct {
		step  PlanStep
		order int
	}
	byID := make(map[string]*entry)
	order := 0
	for _, r := range results {
		if r.Plan == nil {
			continue
		}
		if existing, ok := byID[r.Plan.ID]; ok {
			existing.step.Options = mergeOptions(existing.step.Options, r.Plan.Options)
			continue
		}
		byID[r.Plan.ID] = &entry{step: *r.Plan, order: order}
		order++
	}
	out := make([]PlanStep, 0, len(byID))
	for _, e := range byID {
		out = append(out, e.step)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ei := byID[out[i].ID]
		ej := byID[out[j].ID]
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return ei.order < ej.order
	})
	return out
}

// mergeOptions concatenates options from two steps, skipping duplicates
// by label. Order is preserved.
func mergeOptions(a, b []PlanOption) []PlanOption {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]PlanOption, 0, len(a)+len(b))
	for _, o := range a {
		if seen[o.Label] {
			continue
		}
		seen[o.Label] = true
		out = append(out, o)
	}
	for _, o := range b {
		if seen[o.Label] {
			continue
		}
		seen[o.Label] = true
		out = append(out, o)
	}
	return out
}

// Summary counts results by status. Useful for the trailing
// "X ok, Y warnings, Z issues" line and for `--quiet` exit codes.
type Summary struct {
	OK   int
	Warn int
	Fail int
	Skip int
}

// Summarise tallies a result set.
func Summarise(results []Result) Summary {
	var s Summary
	for _, r := range results {
		switch r.Status {
		case StatusOK:
			s.OK++
		case StatusWarn:
			s.Warn++
		case StatusFail:
			s.Fail++
		case StatusSkip:
			s.Skip++
		}
	}
	return s
}
