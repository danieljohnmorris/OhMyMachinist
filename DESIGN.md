---
name: Machinist
description: Control plane for the open-source software factory
colors:
  primary: "#1d6683"
  primary-foreground: "#f8fcfd"
  success: "#326b51"
  warning: "#8a651b"
  danger: "#a54237"
  paper: "#f3f4f1"
  paper-surface: "#fafbf8"
  paper-muted: "#e4e7e4"
  ink: "#1a1d1e"
  ink-muted: "#626a6b"
  hairline: "#c7cdca"
  primary-dark: "#66a9c2"
  primary-foreground-dark: "#102128"
  success-dark: "#78ad91"
  warning-dark: "#d0a75c"
  danger-dark: "#df8075"
  night: "#151819"
  night-surface: "#1c2021"
  night-muted: "#252a2b"
  night-ink: "#edf1ef"
  night-ink-muted: "#9ba4a4"
  night-hairline: "#373e3e"
typography:
  headline:
    fontFamily: "Manrope Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "2.25rem"
    lineHeight: "2.5rem"
  title:
    fontFamily: "Manrope Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "1.375rem"
    lineHeight: "1.75rem"
  body:
    fontFamily: "Manrope Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.875rem"
    lineHeight: "1.25rem"
  label:
    fontFamily: "Manrope Variable, ui-sans-serif, system-ui, sans-serif"
    fontSize: "0.75rem"
    fontWeight: 600
    lineHeight: "1rem"
  mono:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
    fontSize: "0.8125rem"
rounded:
  sm: "4px"
  md: "6px"
  lg: "8px"
  full: "9999px"
components:
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.primary-foreground}"
    rounded: "{rounded.lg}"
    height: "36px"
    padding: "0 14px"
  button-outline:
    textColor: "{colors.ink}"
    rounded: "{rounded.lg}"
    height: "36px"
  button-ghost:
    textColor: "{colors.ink-muted}"
    rounded: "{rounded.lg}"
  card:
    backgroundColor: "{colors.paper-surface}"
    rounded: "{rounded.sm}"
  badge-neutral:
    backgroundColor: "{colors.paper-muted}"
    textColor: "{colors.ink-muted}"
    typography: "{typography.label}"
    rounded: "{rounded.full}"
    padding: "2px 8px"
  badge-active:
    textColor: "{colors.primary}"
    rounded: "{rounded.full}"
  badge-success:
    textColor: "{colors.success}"
    rounded: "{rounded.full}"
  badge-warning:
    textColor: "{colors.warning}"
    rounded: "{rounded.full}"
  badge-danger:
    textColor: "{colors.danger}"
    rounded: "{rounded.full}"
  alert:
    textColor: "{colors.danger}"
    rounded: "{rounded.md}"
    padding: "8px 12px"
  field:
    backgroundColor: "{colors.paper}"
    textColor: "{colors.ink}"
    rounded: "{rounded.lg}"
    height: "40px"
    padding: "10px 12px"
---

# Design System: Machinist

Tokens live in `internal/controlplane/web/src/styles.css`; primitives live in
`internal/controlplane/web/src/components/ui/`. Light values are the default keys;
`-dark` / `night-*` keys are the `.dark` theme values for the same roles.

## Overview

**Creative North Star: "The Factory Floor Logbook"**

The control plane borrows its look from [machinist.sh](https://machinist.sh/): cool
paper and graphite ink, one steel-teal accent, hairline borders, and almost no
decoration. It reads like an engineer's logbook for a running plant. You open it to
find out what happened and what needs you.

Outcome first. Run state is the loudest element on any screen, and chrome is the
quietest. Density is medium-high: operators scan lists of runs, so the system relies
on tabular numerals, monospace identifiers, and truncation with a `title` fallback
instead of wrapping. The system is expected to evolve; change this file first, then
the code.

**Key Characteristics:**
- Cool off-white paper / near-black night, never pure white or black.
- A single accent (steel teal) for actions and in-progress work.
- Colour encodes state only.
- Flat surfaces with hairline borders; depth is a whisper.
- Light and dark themes are both first-class.

## Colors

A cool, low-chroma neutral ground with one teal accent and three muted state hues.

### Primary
- **Steel Teal** (#1d6683 / dark #66a9c2): buttons, focus rings, links, and anything
  *currently in progress* (running task, running factory, busy lane).

### Neutral
- **Paper** (#f3f4f1 / night #151819): page background.
- **Paper Surface** (#fafbf8 / #1c2021): cards and panels.
- **Paper Muted** (#e4e7e4 / #252a2b): neutral badge fill, hover wash, code blocks.
- **Ink** (#1a1d1e / #edf1ef): primary text.
- **Ink Muted** (#626a6b / #9ba4a4): secondary text, labels, idle state.
- **Hairline** (#c7cdca / #373e3e): every border and divider.

### State
- **Moss** (#326b51 / #78ad91), success: finished well, worker connected.
- **Ochre** (#8a651b / #d0a75c), warning: draining or needs a human.
- **Brick** (#a54237 / #df8075), danger: failed, timed out, factory paused, destructive actions.

### Named Rules
**The State-Only Rule.** Moss, Ochre and Brick appear only as state. Tints use
fixed steps: border `/25`, fill `/10`, faint fill `/5`. No raw hex or Tailwind
palette colours in components.

**The Tone Map Rule.** Every state chip goes through `Badge tone=…`, using this mapping
(see `task-display.jsx`, `factory.jsx`, `factory-controls.js`):

| Domain state | Tone |
|---|---|
| task running; factory running; lane busy | `active` |
| task succeeded / resolved; worker connected | `success` |
| factory draining | `warning` |
| task failed / timed_out; factory paused | `danger` |
| task queued / cancelled; lane idle / offline / disabled; worker disconnected | `neutral` |

## Typography

**Body Font:** Manrope Variable (with ui-sans-serif, system-ui)
**Label/Mono Font:** ui-monospace stack

**Character:** a single humanist-geometric sans keeps the tool calm. Mono marks
anything a machine produced.

### Hierarchy
- **Headline** (2.25rem / 2.5rem): `PageHeading` title, one per view.
- **Title** (1.375rem / 1.75rem): section headings inside a view.
- **Body** (0.875rem / 1.25rem): default UI text and tables.
- **Label** (600, 0.75rem / 1rem, capitalised): badges, field labels, metadata.
- **Mono** (0.8125rem): run IDs, SHAs, paths, durations, code.

### Named Rules
**The Machine Text Rule.** Machine-generated identifiers use `font-mono`; counts use
`tabular-nums`.

## Layout

Sidebar plus content on desktop; on mobile, a horizontally scrollable bottom
navigation bar padded for `safe-area-inset-bottom`. Content is capped at
`max-w-[1500px]` (settings use 1200px). Spacing follows Tailwind's 4px rhythm,
and cards pad `p-4` / `sm:p-5`. Wide tables scroll inside their container.

## Elevation & Depth

Mostly flat. Hairline borders and tonal steps (paper → surface) separate planes.
`.crafted-card` adds a faint lift (`0 1px 0` ink 6% + `0 10px 30px` ink 3%). The
sidebar uses an inset 1px edge. Focus is the only strong depth cue: a 3px primary
ring at 14%.

### Named Rules
**The Whisper Rule.** Shadows stay at or below 6% ink. A shadow that you notice is too strong.

## Shapes

Small, engineered corners: cards 4px, alerts 6px, buttons and fields 8px. Badges and
status dots are fully round. Nav items are square (0 radius) and get a background wash
on hover.

## Components

### Buttons
- **Shape:** 8px radius, height 36px (32px `sm`, 36px square `icon`).
- **Primary:** teal fill, near-white text; hover `primary/90`.
- **Outline / Ghost:** hairline border or no border; hover washes to Paper Muted.
- **Focus:** `ring-2 ring/40`. Destructive actions use an outline button with a danger tint.

### Chips (Badge)
- **Style:** full-round, 1px tinted border, `/10` fill, label type.
- **Tones:** `neutral`, `surface`, `active`, `success`, `warning`, `danger`. `dot` adds a
  current-colour status dot for live state.

### Cards / Containers
- **Corner Style:** 4px. **Background:** Paper Surface. **Border:** Hairline.
- **Shadow Strategy:** `.crafted-card` whisper (see Elevation).

### Inputs / Fields
- **Style:** `.field-control`, with Paper background, Hairline stroke, 8px radius, and 40px minimum height.
- **Focus:** border shifts to primary 60% plus a 3px primary/14% ring.

### Alert
The single error banner (`role="alert"`) uses Brick text, a `/35` border, a `/10` fill and
a 6px radius. `inline` renders only the one-line text form next to a control.

### Navigation
Muted text at 0.8125rem; hover washes Paper Muted at 65%; the active item switches to Ink
without a fill. Mobile moves navigation to a bottom bar.

### PageHeading / QuietState
Every top-level view opens with `PageHeading`. Empty and loading states use
`QuietState`, never a bare spinner or a blank card.

## Operational surfaces

- **Brand:** retain the original “oh my machinist” logo assets, with the light
  and dark variants switched by the theme. Do not replace the logo with a new mark.
- **Tasks board:** four columns—Queued, Running, Needs you, Done. Human-blocked
  work stays visible without enabling Show succeeded. A compact summary reports
  current Running/Needs you counts and outcomes from the last 24 hours. Active
  models come from running executions, never invented configured defaults.
- **Run page:** transcript-first, with a 320px result/details sidebar at desktop
  widths and stacked content on mobile. Ticket and PR links remain visible near
  the title; secondary delivery metadata expands on request. Attempts, activity,
  instructions, artifacts and execution history remain accessible.
- **Tickets:** built-in is the default. Draft creation, editing, comments,
  approval, status changes and confirmed draft deletion share existing form and
  button primitives. Plane/Linear views show imported work and link to their
  source tracker; they do not pretend to be external ticket editors.
- **Capacity:** one panel joins the concurrent-job limit with worker lanes.
  The limit is a dispatch ceiling; lanes are the actual execution capacity.
  The sidebar links to this panel rather than repeating its stepper.
- **Safety:** Stop picking up new jobs drains; Stop all now confirms before
  cancelling running work. Internal ticket links stay in this tab; external
  HTTP(S) ticket/PR links open with `noopener noreferrer`.


## Do's and Don'ts

### Do:
- **Do** map every status through `Badge tone` using the Tone Map table.
- **Do** use `Alert` for errors instead of hand-rolled danger divs.
- **Do** define every new token in both themes in `styles.css` and update this file in the same PR.

### Don't:
- **Don't** colour in-progress work Moss. Running means `active`, not success.
- **Don't** add a second accent hue or decorative gradients.
- **Don't** use shadows heavier than the Whisper Rule allows.
