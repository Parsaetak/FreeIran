# FreeIran

A lightweight, free, open-source VPN application for Android and Windows, built around a shared cross-platform engine for discovering, testing, maintaining, and running publicly available proxy/VPN configurations.

**Project:** FreeIran  
**Architect:** Parsa Tak / SHEYTAN  
**Repository:** https://github.com/Parsaetak/FreeIran  
**Status:** Early development

---

## Vision

FreeIran is designed around one simple idea:

> Continuously find publicly available configurations, test them, keep the ones that work, archive the ones that fail, remove duplicates, and make the working pool immediately usable from a lightweight client.

The application is intended for environments where ordinary Internet connectivity can be heavily restricted or unreliable, including Iran.

FreeIran is not intended to operate a permanent server network. Its primary resource is a continuously maintained collection of publicly published configurations.

---

## Core Architecture

```text
                         FREEIRAN
                            │
                    Shared Go Engine
                            │
             ┌──────────────┼──────────────┐
             │              │              │
           Xray          sing-box       WireGuard
             │              │              │
             └──────────────┼──────────────┘
                            │
                     Config Manager
                            │
                    Public Data Sources
                            │
                       NiREvil/vless
                            │
                  ┌─────────┴─────────┐
                  │                   │
               WORKING              FAILED
                  │                   │
                  ▼                   ▼
             Active DB             Archive
                  │
          ┌───────┴────────┐
          │                │
       Android           Windows
```

The engine is shared between platforms.

Android and Windows are thin platform layers around the same core functionality wherever practical.

---

## Main Engine Loop

The engine periodically refreshes its configuration sources.

The default maintenance interval is **one hour**.

```text
FETCH
  ↓
PARSE
  ↓
NORMALIZE
  ↓
DEDUPLICATE
  ↓
TEST
  ↓
┌───────────────┴───────────────┐
│                               │
WORKING                         FAILED
│                               │
▼                               ▼
ACTIVE DATABASE                ARCHIVE
```

Working configurations remain available to the application.

Failed configurations are removed from the active pool and retained in compressed historical storage.

A configuration that becomes functional again can return to the active pool during a later test cycle.

---

## Configuration Lifecycle

Every configuration passes through a simple lifecycle:

```text
Discovered
    ↓
Parsed
    ↓
Normalized
    ↓
Deduplicated
    ↓
Tested
    ├── Working → Active
    └── Failed  → Archive
```

Configurations are identified using deterministic fingerprints so the same configuration received from multiple sources is stored only once.

---

## Active Database

The active database contains only configurations currently considered usable.

Conceptually:

```text
active.db

├── VLESS
├── VMess
├── Trojan
├── Shadowsocks
├── Hysteria
├── Hysteria2
├── TUIC
├── WireGuard
└── other supported configurations
```

The exact supported protocols depend on the capabilities of the integrated protocol cores.

---

## Archive

The archive preserves configurations that are no longer active.

Its purposes are:

- prevent repeatedly downloading identical dead configurations
- preserve historical data
- allow failed configurations to be retested later
- reduce the size of the active database
- provide useful data for future reliability analysis

Archives should be compressed.

Duplicate configurations should never accumulate indefinitely.

---

## Configuration Sources

The first source is:

**NiREvil/vless**

https://github.com/NiREvil/vless

The source is treated as an external public data source rather than as part of the application's permanent codebase.

This allows the source to update independently from the application.

Additional public sources can be added later through source adapters.

Only openly published configuration/subscription data should be ingested.

---

## Protocol Engine Strategy

FreeIran does not implement every network protocol itself.

Instead, established protocol cores provide the actual networking implementations.

### Primary engine

**Xray-core**

Used for the Xray/V2Ray ecosystem and compatible configurations.

### Secondary engine

**sing-box**

Used where its protocol and transport support provides better compatibility.

### Specialized engine

**WireGuard**

Used for WireGuard configurations through the appropriate platform implementation.

The FreeIran engine decides which backend is required for a normalized configuration.

---

## Universal Configuration Model

External formats are converted into an internal representation.

```text
Public configuration
        ↓
Format parser
        ↓
Universal configuration
        ↓
Protocol engine
```

The application should therefore not depend directly on a single subscription format.

Potential input formats include:

- VLESS URLs
- VMess URLs
- Trojan URLs
- Shadowsocks URLs
- subscription lists
- V2Ray JSON
- Sing-box JSON
- Clash-compatible configurations
- other supported public configuration formats

Transport and security parameters are preserved when supported by the selected engine.

Examples include:

- TLS
- REALITY
- WebSocket
- gRPC
- HTTP/2
- XHTTP
- QUIC
- other supported transports

---

## Testing

A configuration is not considered working merely because it can be parsed.

The tester should progressively verify:

```text
Parse
  ↓
Core startup
  ↓
Endpoint reachability
  ↓
Protocol handshake
  ↓
Tunnel establishment
  ↓
Connectivity test
  ↓
Latency measurement
```

The tester records the result and timestamp.

Example:

```json
{
  "status": "working",
  "latency_ms": 143,
  "tested_at": "2026-08-25T00:00:00Z"
}
```

Testing should be deterministic and lightweight.

AI is not required for basic configuration validation.

---

## Deduplication

Duplicate removal happens before expensive testing.

A configuration fingerprint is generated from its normalized connection parameters.

Conceptually:

```text
fingerprint =
SHA-256(
    protocol +
    endpoint +
    port +
    identity +
    transport +
    security +
    transport_parameters
)
```

This prevents the same configuration from being tested repeatedly when it appears in multiple sources.

---

## Storage Philosophy

FreeIran should remain lightweight.

The active dataset should contain only useful configurations.

Historical data belongs in compressed archives.

The application should avoid accumulating:

- duplicate configurations
- obsolete active entries
- unnecessary metadata
- redundant source copies
- large uncompressed datasets

The goal is a small, fast local database.

---

## Cross-Platform Design

The shared engine is written in Go.

```text
                 Go Engine
                     │
          ┌──────────┴──────────┐
          │                     │
       Android                Windows
          │                     │
     VPN adapter          Network adapter
```

The shared engine handles:

- configuration management
- parsing
- normalization
- deduplication
- testing
- scheduling
- storage
- archive management
- protocol-core management

Platform-specific code handles operating-system networking and VPN integration.

---

## Android

Android will use the native VPN facilities of the operating system.

The application should provide:

- connect
- disconnect
- automatic best configuration
- configuration list
- basic connection status
- VPN permission handling
- background maintenance
- minimal resource usage

The UI should remain simple and responsive.

---

## Windows

Windows will use the shared engine with a Windows-specific networking adapter.

The Windows application should provide:

- connect
- disconnect
- automatic best configuration
- configuration list
- system networking integration
- background maintenance
- minimal resource usage

---

## Privacy

FreeIran should follow a privacy-first architecture.

The application should not require a central account or user browsing history.

Configuration discovery and testing should not collect:

- browsing history
- visited URLs
- personal identity
- unnecessary telemetry
- private credentials

Local configuration data should be protected appropriately by each platform.

---

## Security

Downloaded configuration data is untrusted input.

The engine must validate and sanitize configurations before passing them to a protocol core.

The application must never:

- execute arbitrary downloaded scripts
- install arbitrary certificates automatically
- trust downloaded configuration files blindly
- expose configuration credentials in diagnostic logs

Only public configuration data intentionally published by its source should be considered for ingestion.

---

## Development Principles

FreeIran follows several simple engineering rules:

1. **Keep the engine small.**
2. **Prefer existing mature protocol cores.**
3. **Do not duplicate functionality unnecessarily.**
4. **Test configurations before activating them.**
5. **Remove duplicates before testing.**
6. **Keep failed data compressed in the archive.**
7. **Keep the active database small.**
8. **Keep Android and Windows on the same engine.**
9. **Avoid a backend until one is actually necessary.**
10. **Prefer deterministic mechanisms over AI for networking decisions.**

---

## Development Roadmap

### v0.1 — Engine Foundation

- [ ] Shared Go engine
- [ ] Configuration model
- [ ] Deduplication
- [ ] Local database
- [ ] Archive
- [ ] Scheduler
- [ ] NiREvil source
- [ ] Configuration parser
- [ ] Basic tester

### v0.2 — Protocol Engine

- [ ] Xray integration
- [ ] VLESS
- [ ] VMess
- [ ] Trojan
- [ ] Shadowsocks
- [ ] Core lifecycle management

### v0.3 — Clients

- [ ] Android client
- [ ] Windows client
- [ ] Connect/disconnect
- [ ] Automatic working-server selection

### v0.4 — Expanded Compatibility

- [ ] sing-box integration
- [ ] Hysteria
- [ ] Hysteria2
- [ ] TUIC
- [ ] WireGuard
- [ ] Additional configuration formats

### v0.5 — Source Expansion

- [ ] Additional public sources
- [ ] Source update management
- [ ] Improved historical testing
- [ ] Better reliability statistics

### v1.0 — Stable Release

- [ ] Stable Android release
- [ ] Stable Windows release
- [ ] Automated configuration maintenance
- [ ] Robust core management
- [ ] Security review
- [ ] Reproducible builds
- [ ] Documentation

---

## Repository Structure

```text
FreeIran/
│
├── engine/
│   ├── core/
│   ├── config/
│   ├── parser/
│   ├── tester/
│   ├── database/
│   ├── archive/
│   └── scheduler/
│
├── sources/
│   └── nirevil/
│
├── android/
│
├── windows/
│
├── tests/
│
├── scripts/
│
├── go.mod
├── README.md
└── LICENSE
```

The structure is intentionally minimal and can evolve as implementation requirements become clear.

---

## Current Status

FreeIran is in the foundation stage.

The immediate goal is to build the shared engine before building a complex user interface.

The first functional milestone is:

```text
NiREvil source
      ↓
Parse
      ↓
Deduplicate
      ↓
Store
      ↓
Test
      ↓
Working configurations
      ↓
Android / Windows
```

---

## Attribution

**Architect / Project Originator:**  
Parsa Tak / SHEYTAN

**Project:** FreeIran

**Repository:**  
https://github.com/Parsaetak/FreeIran

---

## License

To be finalized during the initial implementation stage.
```

### Why I changed one thing from our earlier plan

I would **not put specific Xray/sing-box version numbers in the README yet**. The current Xray release stream is already changing rapidly, with 26.7.x releases visible upstream, and sing-box is likewise actively releasing. 

We'll pin **tested versions in the engine/build configuration**, where they can be updated safely.

Also, a recent Xray change removed `allowInsecure` in favour of certificate pinning, which is exactly the sort of compatibility change our engine needs to handle centrally rather than freezing into documentation. 

This README is therefore the **stable project contract**, while implementation-specific versions remain in the code.

**Add this as `README.md`.** Then the next file should be the first actual engine file rather than another documentation file.
