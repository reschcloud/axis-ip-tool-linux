# ADR 0003: GTK thread model, live cell updates and login gating

- Status: Accepted
- Date: 2026-11-27

## Context

GTK is single-threaded. Network, DNS, HTTP and ICMP work can block the UI and race with device
updates. Users also need a clear, non-destructive way to authenticate before privileged actions.

## Decision

- Run all blocking work in goroutines and marshal results back with `ui()`/`glib.IdleAdd`.
  Reuse `async()` and `withLogin()` in `actions.go`.
- Keep the list model stable. Bind labels per device key in `window.cells` and re-render them
  for routine updates; do not replace model items or the selection on every ping.
- Use the `listCell` interface for ColumnView factories (`*gtk.ColumnViewCell` is what gotk4
  delivers, not `*gtk.ListItem`). Compare widgets by native pointer.
- When a user exists but no valid login is known, actions show a lock and open the login dialog;
  after a successful login the originally requested action continues. The web interface and live
  view are exempt (browser login / anonymous viewers), and live view tries anonymous access first.
- Never prompt for the same login twice in parallel; keep a per-device in-flight guard.

## Consequences

- The UI stays responsive and selection/filter state survives background updates.
- Authentication is centralized, testable and consistent across all actions.
- Destructive actions still require explicit confirmation even after login.
