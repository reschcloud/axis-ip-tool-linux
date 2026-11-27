# ADR 0001: One shared, GUI-independent device core

- Status: Accepted
- Date: 2026-11-27

## Context

The project ships a GTK4/libadwaita GUI and a CLI. Discovery, VAPIX communication, the device
model, monitoring and batch planning must behave identically in both. GUI bindings are expensive
to build and must not leak into the CLI.

## Decision

All device-specific logic lives in `internal/axis`. It has no GTK dependency and is imported by
`cmd/ip-tool-for-axis` and by the cgo-free `cmd/ip-tool-for-axis-cli`. Parsing, registry merges,
monitoring, VAPIX calls, firmware handling and batch planning belong there. The CLI stays
`CGO_ENABLED=0`; the GUI is a thin presentation layer over the same types.

## Consequences

- New device features are implemented and tested once in `internal/axis`.
- The CLI remains usable on systems without GTK and is part of the same test suite.
- GUI-only features (dialogs, live view widgets, styling) must not be required for core behavior.
