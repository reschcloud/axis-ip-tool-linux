# Architecture decision records

These ADRs describe the principles behind IP Tool for AXIS (Linux). They are written in
English, like the rest of the repository. Each record is immutable once accepted; a later
decision supersedes it with a new ADR.

| ADR | Decision |
|---|---|
| [0001](0001-shared-device-core.md) | One shared, GUI-independent device core for GUI and CLI |
| [0002](0002-discovery-and-monitoring.md) | Dual-track discovery, continuous monitoring and the registry model |
| [0003](0003-gui-thread-and-model.md) | GTK thread model, live cell updates and login gating |
| [0004](0004-vapix-compatibility-and-auth.md) | VAPIX compatibility fallbacks, factory state and credential handling |
| [0005](0005-https-and-ipv6.md) | HTTPS policy, IPv6/link-local handling and browser forwarding |
| [0006](0006-manual-devices-and-input.md) | Manual devices, hostnames/URLs/ranges and their monitoring semantics |
| [0007](0007-packaging-release-flathub.md) | Packaging, fixed artifacts, Flatpak/Flathub and release metadata |
| [0008](0008-language-branding-license.md) | English artifacts, unofficial-Axis branding and AGPL licensing |
