# Fletcher iOS app - first-cut UI mockup

A static HTML/CSS mockup of the Milestone 7 SwiftUI iOS client (see
`docs/ROADMAP.md`, Milestone 7). It is a design reference, not shipping code: it
pins the visual direction and information architecture before the SwiftUI build,
and is built to render near-natively in iOS Safari (`-apple-system` font,
dark-mode semantic colors, safe-area insets).

## Preview

It is a single self-contained `index.html` with no build step:

```
python3 -m http.server 8723 --bind 0.0.0.0
```

Then open `http://<your-box-ip>:8723` in Safari on the phone (over the LAN or a
WireGuard / Tailscale tunnel). "Add to Home Screen" runs it full-screen without
Safari chrome, which is closest to the real app.

## What it captures (the approved first cut)

- **Sessions** tab as the home: each session is a card with a recent-activity
  terminal preview (LIVE or hibernated), one tap into the terminal.
- **Terminal**: full-screen Claude session with the pinned accessory key-row
  (esc, sticky ctrl, tab, arrows, `|` `/` `~` `-`) - the make-or-break detail for
  a touch terminal.
- **New** (unified create): a trigger segment - Interactive / Run once /
  Scheduled - reflecting DESIGN.md §4 (one primitive, many hats), not three
  separate create flows.
- **Inbox**: results split into "Needs you" (approvals) and "Results"
  (structured cards plus a generic last-output fallback).
- **Pairing**: one-code onboarding (scan -> WireGuard tunnel up -> signed in),
  reflecting the unified pairing payload in Milestone 7.
- **Settings**: live daemon settings (idle auto-stop, max sessions, max disk) as
  editable rows.
- Visual system: native-iOS, dark-first, system indigo accent, green / orange /
  gray for status, a liquid-glass bottom tab bar.

## Deferred to implementation time

- Wiring the remaining setting pickers (max sessions / max disk).
- How a job result becomes a structured inbox card. The proposed direction is the
  §4 "sink" plus a `fletcher.report` MCP tool for rich cards and a generic
  name / exit-code / output-tail fallback for any job - to be validated when the
  inbox is actually built.
