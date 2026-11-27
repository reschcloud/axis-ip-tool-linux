# ADR 0008: English artifacts, unofficial-Axis branding and AGPL

- Status: Accepted
- Date: 2026-11-27

## Context

The project is an independent tool modelled on the AXIS IP Utility. Contributors and users are
international, while the maintainer may communicate in German. It uses Axis trademarks and public
APIs.

## Decision

- UI text, code comments, documentation, commit messages and ADRs are in English. Conversations
  with the maintainer may be in German.
- The app is presented as an unofficial tool and explicitly states that it is not a product of
  Axis Communications AB. Do not ship Axis logos or other proprietary Axis assets.
- License the project under AGPL-3.0-or-later. Third-party code/approaches are credited; the
  firmware-update approach was inspired by Cacsjep/oadm (Apache-2.0) without copying code.
- Keep the app ID, repository name and package names stable within a release series; rename only
  with an explicit migration note.

## Consequences

- Contributions and releases are understandable to an international audience.
- Legal/branding review for Flathub can point to the disclaimer and the AGPL license.
- The user-facing name remains "IP Tool for AXIS (Linux)".
