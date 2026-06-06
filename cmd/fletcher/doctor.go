package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/joshjon/fletcher/internal/doctor"
)

func doctorCmd() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "diagnose this Fletcher install and surface an action plan",
		Description: `Runs a battery of checks against the daemon, the host
networking stack, and the upstream providers, then prints a
prioritised action plan when anything needs your attention.

Most fixes are operator-side (router config, ISP, network
interfaces), so the doctor diagnoses and explains rather than
auto-fixing. Re-run after each change to confirm.`,
		Flags: []cli.Flag{
			socketFlag(),
			&cli.IntFlag{
				Name:    "wireguard-port",
				Usage:   "UDP port to probe for UPnP forwarding (matches the daemon's listen port)",
				Sources: cli.EnvVars("FLETCHER_WIREGUARD_PORT"),
				Value:   51820,
			},
			&cli.BoolFlag{
				Name:  "quiet",
				Usage: "suppress per-check rows; print only the summary + plan. Exits non-zero on any failure.",
			},
			&cli.StringFlag{
				Name:    "output",
				Aliases: []string{"o"},
				Value:   "text",
				Usage:   "output format (text, json)",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			checkers := []doctor.Checker{
				doctor.CheckDaemon(cmd.String("socket")),
				doctor.CheckTUN(),
				doctor.CheckKVM(),
				doctor.CheckFirecrackerVMM(),
				doctor.CheckRuntimeReady(cmd.String("socket")),
				doctor.CheckDefaultRoutes(),
				doctor.CheckPublicIP(),
				doctor.CheckUPnP(cmd.Int("wireguard-port")),
				doctor.CheckPublicEndpoint(cmd.String("socket")),
				doctor.CheckProviderReachability(),
			}
			results := doctor.Run(ctx, checkers)

			if cmd.String("output") == "json" {
				disableColor() // structured output should never contain ANSI escapes
				return renderDoctorJSON(os.Stdout, results)
			}
			renderDoctorText(os.Stdout, results, cmd.Bool("quiet"))
			if doctor.Summarise(results).Fail > 0 {
				return cli.Exit("", 1)
			}
			return nil
		},
	}
}

// renderDoctorText prints the human-readable diagnostic table plus
// the action-plan section. Categories are emitted in a fixed order so
// the layout is stable across runs.
func renderDoctorText(w io.Writer, results []doctor.Result, quiet bool) {
	order := []doctor.Category{
		doctor.CategoryDaemon,
		doctor.CategoryHost,
		doctor.CategoryNetwork,
		doctor.CategoryRouter,
		doctor.CategoryReachability,
		doctor.CategoryProviders,
	}
	byCategory := make(map[doctor.Category][]doctor.Result, len(order))
	for _, r := range results {
		byCategory[r.Category] = append(byCategory[r.Category], r)
	}

	if !quiet {
		for _, cat := range order {
			rs := byCategory[cat]
			if len(rs) == 0 {
				continue
			}
			fmt.Fprintln(w, bold(string(cat)))
			for _, r := range rs {
				icon := statusIcon(r.Status)
				fmt.Fprintf(w, "  %s  %s\n", icon, r.Name)
				if r.Detail != "" {
					fmt.Fprintf(w, "      %s\n", dim(r.Detail))
				}
			}
			fmt.Fprintln(w)
		}
	}

	s := doctor.Summarise(results)
	fmt.Fprintf(w, "Summary: %s, %s, %s",
		green(fmt.Sprintf("%d ok", s.OK)),
		yellow(fmt.Sprintf("%d warnings", s.Warn)),
		red(fmt.Sprintf("%d issues", s.Fail)),
	)
	if s.Skip > 0 {
		fmt.Fprintf(w, ", %s", gray(fmt.Sprintf("%d skipped", s.Skip)))
	}
	fmt.Fprintln(w, ".")

	plan := doctor.CollectPlan(results)
	if len(plan) == 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, green("Nothing to do."))
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, gray(strings.Repeat("-", 60)))
	fmt.Fprintln(w, bold("WHAT TO DO NEXT"))
	fmt.Fprintln(w)
	renderPlan(w, plan)
}

func renderPlan(w io.Writer, plan []doctor.PlanStep) {
	for i, step := range plan {
		label := green("follow-up")
		if step.Priority == doctor.PriorityBlocker {
			label = red("blocker")
		}
		fmt.Fprintf(w, "%d. %s (%s)\n", i+1, bold(step.Title), label)
		if step.Why != "" {
			fmt.Fprintln(w)
			for _, line := range wrap(step.Why, 70) {
				fmt.Fprintf(w, "   %s\n", dim(line))
			}
		}
		fmt.Fprintln(w)
		multiple := len(step.Options) > 1
		for j, opt := range step.Options {
			prefix := "   "
			if multiple {
				prefix = fmt.Sprintf("   %s. ", string(rune('A'+j)))
			}
			fmt.Fprintf(w, "%s%s\n", prefix, bold(opt.Label))
			for _, s := range opt.Steps {
				// Lines starting with "#" are explanatory comments inside
				// the recipe; dim them so the actual commands stand out.
				if strings.HasPrefix(strings.TrimSpace(s), "#") {
					fmt.Fprintf(w, "        %s\n", dim(s))
				} else {
					fmt.Fprintf(w, "        %s\n", s)
				}
			}
			fmt.Fprintln(w)
		}
	}
}

// wrap breaks s into lines no longer than width, breaking at spaces.
// Crude but sufficient for short "Why" paragraphs.
func wrap(s string, width int) []string {
	if len(s) <= width {
		return []string{s}
	}
	var out []string
	for len(s) > width {
		cut := strings.LastIndex(s[:width], " ")
		if cut <= 0 {
			cut = width
		}
		out = append(out, s[:cut])
		s = strings.TrimLeft(s[cut:], " ")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

// statusIcon renders the per-result status as a single coloured glyph.
// Unicode symbols here are deliberate UI affordances (not punctuation,
// which the CLAUDE.md writing rule restricts to ASCII).
func statusIcon(s doctor.Status) string {
	switch s {
	case doctor.StatusOK:
		return green("✓")
	case doctor.StatusWarn:
		return yellow("⚠")
	case doctor.StatusFail:
		return red("✗")
	case doctor.StatusSkip:
		return gray("○")
	}
	return "?"
}

// renderDoctorJSON emits a structured form suitable for monitoring /
// CI consumers. The fields mirror the text renderer's organisation.
func renderDoctorJSON(w io.Writer, results []doctor.Result) error {
	type plan struct {
		ID       string                   `json:"id"`
		Priority string                   `json:"priority"`
		Title    string                   `json:"title"`
		Why      string                   `json:"why,omitempty"`
		Options  []map[string]interface{} `json:"options,omitempty"`
	}
	type out struct {
		Results []map[string]interface{} `json:"results"`
		Summary map[string]int           `json:"summary"`
		Plan    []plan                   `json:"plan"`
	}

	o := out{Summary: map[string]int{}}
	for _, r := range results {
		o.Results = append(o.Results, map[string]interface{}{
			"category": string(r.Category),
			"name":     r.Name,
			"status":   r.Status.String(),
			"detail":   r.Detail,
		})
	}
	s := doctor.Summarise(results)
	o.Summary["ok"] = s.OK
	o.Summary["warn"] = s.Warn
	o.Summary["fail"] = s.Fail
	o.Summary["skip"] = s.Skip

	for _, p := range doctor.CollectPlan(results) {
		priority := "blocker"
		if p.Priority != doctor.PriorityBlocker {
			priority = "follow-up"
		}
		opts := make([]map[string]interface{}, 0, len(p.Options))
		for _, op := range p.Options {
			opts = append(opts, map[string]interface{}{
				"label": op.Label,
				"steps": op.Steps,
			})
		}
		o.Plan = append(o.Plan, plan{
			ID:       p.ID,
			Priority: priority,
			Title:    p.Title,
			Why:      p.Why,
			Options:  opts,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(o)
}
