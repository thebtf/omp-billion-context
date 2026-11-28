---
name: Billion Context
description: A fold journal for operators of a CPA installation.
colors:
  accent: "#17665e"
  accent-2: "#104c46"
  on-accent: "#ffffff"
  shell: "#18574f"
  plane: "#f3f6f3"
  plane-2: "#e7eeea"
  plane-3: "#dfe4e7"
  ink: "#203a36"
  ink-2: "#445e58"
  ink-3: "#526962"
  rule: "#cbd7d0"
  rule-strong: "#91a79c"
  ring: "#19766a"
  ok: "#1c6a4c"
  bad: "#a2272b"
  accent-dark: "#7dc8af"
  accent-2-dark: "#a7dfcb"
  on-accent-dark: "#10231b"
  shell-dark: "#194c44"
  plane-dark: "#141e1c"
  plane-2-dark: "#1d2c27"
  plane-3-dark: "#232b30"
  ink-dark: "#e4eee9"
  ink-2-dark: "#b4c6bd"
  ink-3-dark: "#a2b6ab"
  rule-dark: "#354a40"
  rule-strong-dark: "#647c6e"
  ring-dark: "#a5ddc4"
  ok-dark: "#6cc59c"
  bad-dark: "#e8908c"
typography:
  body:
    fontFamily: 'ui-sans-serif, system-ui, -apple-system, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif'
    fontSize: '.9375rem'
    fontWeight: 400
    lineHeight: 1.55
  journal-title:
    fontSize: 'clamp(1.75rem, 3.2vw, 2.8rem)'
    lineHeight: 1.12
    letterSpacing: '-.035em'
  savings:
    fontSize: 'clamp(2.8rem, 5vw, 4.3rem)'
    lineHeight: 1.15
    letterSpacing: '-.035em'
  model:
    fontFamily: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace'
rounded:
  control: '2px'
spacing:
  control-gap: '.5rem'
  receipt-inset: '1.4rem'
components:
  button:
    backgroundColor: '{colors.plane}'
    rounded: '{rounded.control}'
  button-primary:
    backgroundColor: '{colors.accent}'
    rounded: '{rounded.control}'
  button-small:
    backgroundColor: '{colors.plane}'
    rounded: '{rounded.control}'
  search:
    backgroundColor: '{colors.plane}'
    rounded: '{rounded.control}'
  nav:
    textColor: '{colors.ink-2}'
  badge:
    textColor: '{colors.ink-2}'
    padding: '.05rem .35rem'
---

# Design System: Billion Context

## Overview

**Creative North Star: "Журнал свёрток"**

The fold journal puts conversations, provider readings and fold receipts on one ruled surface. A large estimate answers how much context was saved; a separate reading answers what the provider actually reported.

**Key Characteristics:**
- A dark teal masthead in both schemes
- Tabular numerals and labeled readings
- Conversation rows paired with a dated receipt

## Colors

Teal marks the active navigation underline, selected language, links and provider percentage. In dark mode the accent becomes mint, with dark ink on filled selections. The masthead retains white lettering. Success and error colors accompany text; muted ink carries timestamps and reading labels.

**The Separate Readings Rule.** Keep estimated savings separate from provider-reported cache usage.

## Typography

Body copy uses the sans stack and tabular numerals. Model identifiers use monospace. At desktop width the savings estimate is the largest number, followed by the fold receipt (2.3rem) and provider percentage (1.45rem). Reading labels and event times share the smaller .8rem size.

## Layout

The centered app caps at 100rem. Desktop gutters follow clamp(1rem, 3.4vw, 3.5rem). The masthead reserves max(20rem, 22%) at right for the host panel. The metrics split 1:1.25; below them the conversation list and receipt split 2.25:1, with an 18rem minimum for the receipt.

At 59.999rem and below, controls wrap below the brand with 4.6rem of clearance above them. Metrics and receipt stack, the receipt divider moves to its top, and the overview hides the model column. The estimate becomes 2.7rem. At 45rem the session table hides model and protocol columns and shows row metadata; at 34rem it also hides the updated column.

## Elevation & Depth

Journal sections sit on the page plane. A stronger rule separates regions; lighter rules separate rows. Floating help uses the theme shadow. The host overlay is external chrome.

## Shapes

Buttons, segmented language controls, badges and search fields have 2px corners. Tables and metric bands use straight edges.

## Components

- **Masthead and navigation.** Language is a pressed segment. The active route has a 2px underline. Keyboard focus uses a 2px ring with a 2px offset.
- **Forms and notices.** Neutral buttons and search fields use a strong border. Primary actions use a filled accent. Notices use the secondary plane and wrap their text and action.
- **Provider cache.** Keep the percentage beside its heading and the individual readings beneath it. A disclosure opens the fold/replay comparison.
- **Conversation rows.** Under the title, non-legacy sessions show the last recorded event in secondary ink (.81rem/1.45), or explicitly state that no attempts were recorded. Adjacent numeric headers and cells have extra leading padding (1.1rem) to distinguish block counts from savings.
- **Fold receipt.** A labeled outcome precedes the large estimate. Ruled timestamp/event pairs lead to the session detail link.

## Do's and Don'ts

- Do name the denominator beside the provider percentage: input tokens for calls reporting both counts. Show read tokens, write tokens and reporting coverage separately.
- Do keep language and lock controls clear of the host overlay.
- Don’t turn an absent reading into zero.
- Don’t give the host overlay’s rounded card treatment to journal sections.
