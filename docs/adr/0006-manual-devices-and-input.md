# ADR 0006: Manual devices, hostnames/URLs/ranges and their monitoring semantics

- Status: Accepted
- Date: 2026-11-27

## Context

Not every camera is discoverable (other subnets, routed/VPN networks, hostnames, non-standard
ports). Users want to add them in the same way and see the same live information.

## Decision

- Accept addresses (IPv4/IPv6, link-local with zone), hostnames, complete `http(s)://` URLs,
  CIDR prefixes, ranges (`start-end`, `start..end`, last-octet shorthand) and last-octet
  wildcards. Separate multiple entries with commas, semicolons and/or whitespace.
- Validate and expand the whole input atomically; cap expansions at 1024 addresses; reject
  host-bit CIDRs, loopback/broadcast literals, malformed URLs and URL userinfo.
- Resolve hostnames in the background; show the resolved IPv4/IPv6 addresses and outgoing
  interface. Ping IPv4 first and fall back to IPv6; show the address used in the Ping tooltip.
- Treat manual targets exactly like discovered targets for enrichment, expiry, route/status,
  login and actions. Keep the hostname as the identity until a serial number is learned.
- Run parsing/expansion off the GTK thread.

## Consequences

- Manual cameras have the same monitoring and status semantics as discovered ones.
- DNS failures are visible ("Hostname not resolved"); reachable non-VAPIX targets show
  "No VAPIX response" instead of a misleading login prompt.
- Large ranges are bounded and do not freeze the UI.
