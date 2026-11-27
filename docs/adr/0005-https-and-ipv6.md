# ADR 0005: HTTPS policy, IPv6/link-local handling and browser forwarding

- Status: Accepted
- Date: 2026-11-27

## Context

Axis cameras use self-signed certificates and often announce only IPv4 via mDNS, while their
link-local IPv6 addresses are useful but not directly usable in browser URLs. Some users require
encrypted connections.

## Decision

- Probe HTTPS with a TLS handshake on port 443 (cached briefly). If it works, use HTTPS; otherwise
  fall back to HTTP on the device's HTTP port. Self-signed certificates are accepted but not
  pinned (explicit trade-off: encryption, not protection against active attackers).
- Offer "HTTPS only" to forbid the HTTP fallback. A manual URL's explicit scheme/port wins.
- Discover IPv6 addresses through mDNS/DNS and the kernel neighbor table (netlink, no root).
  Match neighbor entries by MAC (= serial number). Prefer link-local addresses, then stable
  addresses (EUI-64 when possible).
- For link-local IPv6, open the web UI through a local `127.0.0.1` forwarding; browsers cannot
  handle `fe80::…%iface` URLs. Keep the zone for all API traffic but never put it in a URL.
- Distinguish local and routed addresses per family (IPv4/IPv6) for the Route column and tooltip.

## Consequences

- The app works on modern and older devices without user-side certificate setup.
- IPv6 remains usable without browser-specific zone support.
- Users who enable HTTPS-only get a clear `ErrNoHTTPS` instead of a silent downgrade.
