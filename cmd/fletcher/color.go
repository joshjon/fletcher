package main

import (
	"io"
	"os"
)

// colorDisabled is true when ANSI escape codes should be suppressed.
// Resolved once at package init from NO_COLOR (per https://no-color.org/)
// and from whether stdout is a TTY. Treated as a global because every
// rendering path in the CLI shares it - we never colour for one
// command and not another.
var colorDisabled = resolveColorDisabled()

func resolveColorDisabled() bool {
	if os.Getenv("NO_COLOR") != "" {
		return true
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return true
	}
	// A character device is a TTY; pipes / files / sockets aren't.
	return fi.Mode()&os.ModeCharDevice == 0
}

// disableColor lets early-running code (e.g. an `--output json` flag
// handler) suppress colour for the rest of the command's lifetime.
func disableColor() { colorDisabled = true }

// ANSI escape codes for the small palette the CLI uses. We deliberately
// stick to the basic 8-colour set so the output reads on any sensible
// terminal, including SSH sessions with terminfo set to vt100/xterm.
const (
	codeReset  = "\x1b[0m"
	codeBold   = "\x1b[1m"
	codeDim    = "\x1b[2m"
	codeRed    = "\x1b[31m"
	codeGreen  = "\x1b[32m"
	codeYellow = "\x1b[33m"
	codeBlue   = "\x1b[34m"
	codeGray   = "\x1b[90m"
)

func colorize(code, s string) string {
	if colorDisabled {
		return s
	}
	return code + s + codeReset
}

func red(s string) string    { return colorize(codeRed, s) }
func green(s string) string  { return colorize(codeGreen, s) }
func yellow(s string) string { return colorize(codeYellow, s) }
func blue(s string) string   { return colorize(codeBlue, s) }
func gray(s string) string   { return colorize(codeGray, s) }
func bold(s string) string   { return colorize(codeBold, s) }
func dim(s string) string    { return colorize(codeDim, s) }

// suppressColorIf disables colour when w isn't a TTY (e.g. piped to
// a file). Use this when a single command path needs to override the
// global default - for example, JSON output mode should always be
// uncoloured regardless of NO_COLOR / TTY state.
func suppressColorIf(w io.Writer) {
	if f, ok := w.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return
		}
	}
	disableColor()
}
