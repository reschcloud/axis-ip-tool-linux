# ADR 0004: VAPIX compatibility fallbacks, factory state and credentials

- Status: Accepted
- Date: 2026-11-27

## Context

Axis firmware generations differ in their APIs, authentication behavior and factory-state
responses. Device-info endpoints sometimes answer anonymously even with wrong credentials.

## Decision

- Verify credentials only with `Client.CheckLogin` (`pwdgrp.cgi?action=get`, admin required).
  Anonymous device-info answers must not be treated as proof of valid credentials.
- Prefer the JSON APIs. Keep fallbacks to `param.cgi`, `date.cgi` and legacy optics APIs for older
  firmware. Always send the `"context"` field in JSON requests; some firmware rejects requests
  without it with "Invalid POST data".
- Detect factory state through `systemready.cgi`. A heuristic `pwdgrp.cgi` 401 means
  `ErrSetupUnknown`, not "a user exists". Distinguish `ErrNoUser` from `ErrAuth` before mapping
  errors to "wrong password".
- Support creating the first administrator, and change an existing password only after login via
  `pwdgrp.cgi action=update`.
- Keep credentials in memory only. Warn before sending passwords over HTTP. Store the optional
  My Axis session cookie with mode 0600; never log passwords.

## Consequences

- Old and new firmware are handled by one client without firmware-version branching in callers.
- Status messages distinguish factory state, missing login and rejected credentials.
- A password change requires a successful login and updates the stored credentials when changing
  the logged-in user.
