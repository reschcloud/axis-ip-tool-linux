# ADR 0002: Dual-track discovery, continuous monitoring and the registry

- Status: Accepted
- Date: 2026-11-27

## Context

Axis devices announce themselves differently depending on firmware and network setup. Results
arrive at different times and can be incomplete. Users expect an accurate online/offline state
without re-scanning manually.

## Decision

- Discover on two tracks: Bonjour/mDNS through the system Avahi service (system D-Bus, no raw
  multicast socket) and SSDP/UPnP M-SEARCH. Every result is enriched through VAPIX.
- Feed all partial updates into `axis.Registry`. A device is keyed by serial number when known,
  otherwise by address (including a non-default port). A later serial number re-keys an
  address-only entry.
- Monitor continuously: ping every 2 s (IPv4 first, IPv6 fallback for hostnames), re-query every
  `CheckInterval` (10 s), and mark devices offline after 10 s without a sign of life (25 s for
  devices that never answer pings).
- Manual devices use the same target/monitor/enrich pipeline as discovered devices.
- Keep status updates stable: routine ping results must not replace model rows or reset selection.

## Consequences

- The GUI and CLI see the same device list and online semantics.
- Avahi and network access are required permissions, including in the Flatpak sandbox.
- Partial/contradictory discovery data is merged instead of creating duplicate rows.
