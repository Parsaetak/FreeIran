```# FreeIran — Engineering Worklog

**Project:** FreeIran  
**Repository:** https://github.com/Parsaetak/FreeIran  
**Architect / Project Originator:** Parsa Tak / SHEYTAN  
**Primary platforms:** Windows + Android  
**Primary implementation language:** Go  
**Current phase:** v0.1 engine foundation  
**Last known project date:** 2026-08-27  
**Purpose of this file:** Portable handoff document for any AI coding agent continuing FreeIran without relying on previous chat history.

---

## 1. Project Mission

FreeIran is intended to be a lightweight, free, open-source VPN/proxy client for environments with restricted or unreliable Internet connectivity, including Iran.

The central idea is:

```text
Public configuration sources
        ↓
Fetch
        ↓
Parse
        ↓
Normalize
        ↓
Deduplicate
        ↓
Test
        ↓
Working configurations → Active pool/database
        ↓
Failed configurations → Archive
        ↓
Protocol core
        ↓
Windows / Android client

The application is not intended to operate a permanent proprietary server network. Its main resource is a continuously maintained set of publicly published configurations.

The engine must remain platform-neutral. Windows and Android should use the same Go engine wherever practical, with platform-specific adapters only where the operating system requires them.

2. Core Architectural Principles
2.1 Small shared engine

Keep the core engine compact and composable.

Avoid introducing backend infrastructure, accounts, telemetry systems, databases requiring native servers, or other large dependencies unless there is a demonstrated need.

2.2 Mature protocol cores

FreeIran should not reimplement established network protocols.

The planned execution architecture is:

FreeIran universal Config
        ↓
Core Registry
        ↓
Xray / sing-box / WireGuard / platform backend

Current intended protocol strategy:

Xray-core: main backend for V2Ray/Xray ecosystem.
sing-box: secondary backend where it provides useful protocol/transport compatibility.
WireGuard: specialized backend/platform integration.

Exact backend versions must be pinned in implementation/build files, not hard-coded into general documentation.

2.3 Deterministic identity

Configuration identity is based on normalized meaningful connection parameters.

The current model uses SHA-256 over a canonical field sequence.

Runtime fields are intentionally excluded from identity.

Current fingerprint-excluded fields include:

ID
Name
Source
Working
LatencyMS
TestedAt

Meaningful protocol/transport/security fields are included.

For WireGuard, current meaningful fingerprint fields include:

PublicKey
AllowedIPs
DNS
MTU
PersistentKeepalive

The private key is intentionally NOT part of the fingerprint.

2.4 Validate before execution

Downloaded configuration data is untrusted input.

Before a configuration reaches a protocol engine:

parse
→ normalize
→ validate
→ deduplicate
→ execute/test

Do not execute arbitrary downloaded scripts or automatically trust downloaded certificates/configuration side effects.

Never put passwords, UUIDs, keys, or full configuration URLs into ordinary diagnostic logs.

2.5 Test before activation

A syntactically valid configuration is not automatically a working configuration.

The planned quality ladder is:

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
3. Current Repository Structure

The current repository contains:

FreeIran/
├── engine/
│   ├── config/
│   ├── core/
│   ├── database/
│   ├── parser/
│   ├── pool/
│   ├── source/
│   ├── tester/
│   ├── archive/        # added during current development
│   └── engine.go
├── .gitignore
├── LICENSE
├── README.md
└── go.mod

The repository is currently public.

4. Development History
4.1 Configuration model

The universal configuration layer was built first.

Current protocol constants:

vless
vmess
trojan
shadowsocks
hysteria
hysteria2
tuic
wireguard
socks
http
unknown

Current config.Config includes:

ID
Type
Name
Address
Port

UUID
Username
Password
Method

Network
Path
Host
Service

Security
ServerName
FingerprintProfile
PublicKey
ShortID

PrivateKey
AllowedIPs
DNS
MTU
PersistentKeepalive

Source

Working
LatencyMS
TestedAt

Normalization trims and canonicalizes strings and removes duplicate empty/network-list entries.

Validation is protocol-aware.

Important current validation behaviour:

VLESS requires UUID.
VMess requires UUID.
Trojan requires password.
Shadowsocks requires method + password.
Hysteria/Hysteria2 requires password or UUID.
TUIC requires UUID + password.
WireGuard currently requires public key.
SOCKS/HTTP require only the base address/port requirements.

Endpoint() produces a normalized host:port value.

Configuration tests

The config package has extensive tests covering:

deterministic fingerprint
identical configurations
runtime-field exclusion
meaningful-field changes
normalization
ID generation
protocol collision avoidance
WireGuard fingerprint behaviour
WireGuard normalization
supported protocol validation
invalid base configuration
missing credentials
endpoint formatting

The user reported the complete config test suite passing.

5. Parser

The parser converts public external configuration formats into universal config.Config objects.

Supported parser families currently tested:

VLESS
VMess
Trojan
Shadowsocks
Hysteria
Hysteria2
hy2 alias
TUIC
SOCKS5
SOCKS4
HTTP
WireGuard

The parser also supports:

multiple configurations in one input
base64 subscription content
normalization
deduplication
skipping invalid entries when valid entries exist
failure when nothing is valid
rejection of unsupported schemes

WireGuard parsing was added after earlier parser failures and is now passing.

Parser testing status

The user reported:

go test -v ./engine/parser
PASS

All currently implemented parser tests passed, including:

TestParseVLESS
TestParseVMess
TestParseTrojan
TestParseShadowsocks
TestParseMultipleConfigurations
TestParseBase64Subscription
TestParseDeduplicatesConfigurations
TestParseRejectsUnsupportedScheme
TestParseRejectsEmptyInput
TestParseSkipsInvalidEntriesWhenValidEntriesExist
TestParseFailsWhenNothingIsValid
TestParseNormalizesConfigurations
TestParseHysteria2
TestParseHysteria2Alias
TestParseTUIC
TestParseSOCKS
TestParseHTTPProxy
TestParseSOCKS4
TestParseProtocolMix
TestParseWireGuardConfig
TestParseWireGuardIPv6Endpoint
TestParseWireGuardRejectsMissingEndpoint
6. Source Collector

The source layer downloads and parses remote configuration sources.

Current source.Source model:

ID
Name
URL
Enabled

Current fetcher behaviour includes:

HTTP GET
context cancellation
timeout
response status validation
maximum response size
user-agent
content-type capture
safe defaults

Current collector behaviour:

source
→ fetch
→ parser
→ Collection

CollectAll() deliberately keeps successful sources even if another source fails.

MergeConfigurations():

normalizes
validates
fingerprints
deduplicates
discards invalid configurations
Source tests

User reported all source tests passing, including:

TestCollect
TestCollectAllKeepsSuccessfulSources
TestMergeConfigurationsDeduplicates
TestMergeConfigurationsRejectsInvalidConfigurations
TestDefaultSources
TestFetch
TestFetchRejectsDisabledSource
TestFetchRejectsEmptyURL
TestFetchRejectsHTTPError
TestFetchRejectsOversizedResponse

The first intended external source is:

NiREvil/vless
https://github.com/NiREvil/vless

This is treated as an external public source, not code embedded permanently in the parser.

Future sources should be added through source configuration/adapters.

7. Tester

The tester layer abstracts actual configuration probing.

Current abstraction:

type Probe interface {
    Supports(config.Type) bool
    Test(context.Context, config.Config) (Result, error)
}

Current tester result contains:

Working
Latency
TestedAt
LastError

Tester.Test():

normalizes the configuration;
validates it;
checks probe support;
calls the probe;
converts probe errors into a non-working result;
normalizes latency;
ensures a test timestamp exists.

ApplyResult() writes runtime state back to config.Config.

TestAndApply() combines probing + runtime state update.

The tester remains independent of the concrete protocol core.

Tester testing status

User reported all tester tests passing:

TestTesterSuccessfulProbe
TestTesterRejectsInvalidConfiguration
TestTesterRejectsUnsupportedProtocol
TestTesterPropagatesProbeFailure
TestTesterPropagatesProbeResultError
TestApplyResult
TestTestAndApply
TestTestAndApplyNilConfig
8. Core Registry

The core layer establishes the execution boundary between normalized configs and protocol implementations.

Current interfaces:

type Core interface {
    Type() config.Type
    Start(context.Context, config.Config) (Instance, error)
}

type Instance interface {
    Endpoint() string
    Close() error
}

The registry maps:

config.Type → Core

Current registry responsibilities:

register core
replace a core for same protocol
lookup
determine support
enumerate registered protocols
start an instance
validate config before start
reject missing/invalid core results
Core testing status

User reported all core tests passing:

TestNewRegistry
TestRegisterAndGet
TestRegisterReplacesExistingCore
TestRegisterRejectsNilCore
TestRegisterRejectsUnknownProtocol
TestSupports
TestStart
TestStartRejectsInvalidConfiguration
TestStartRejectsUnsupportedProtocol
TestStartPropagatesCoreError
TestStartRejectsNilInstance
TestInstanceClose

The registry is still an abstraction. Real protocol cores have not yet been integrated.

9. Database

The active local database was implemented as:

engine/database/database.go
engine/database/database_test.go

Purpose:

persist current configuration state

Current features:

in-memory map
deterministic IDs
Add
Upsert
Get
Has
Remove
List
Count
Clear
Load
Save
JSON persistence
database versioning
atomic temporary-file replacement
private-file mode intent
configuration validation on load

Storage format is versioned:

{
  "version": 1,
  "entries": {
    "<id>": {
      "config": {},
      "added": "...",
      "updated": "..."
    }
  }
}

The database is deliberately separate from the pool.

Database test status

The user reported all database tests passing:

TestNewDatabase
TestNewRejectsInvalidPath
TestAddAndGet
TestAddRejectsDuplicate
TestUpsertReplacesExistingConfiguration
TestRemove
TestRemoveMissing
TestHas
TestListIsDeterministic
TestClear
TestSaveAndLoad
TestLoadMissingFileCreatesEmptyDatabase
TestLoadRejectsCorruptDatabase
TestSaveCreatesDatabaseFile
TestNilConfiguration
Important Windows lesson

Do not assume Unix permission bits exposed through:

os.Stat(...).Mode().Perm()

behave identically on Windows.

Security tests must be platform-aware.

10. Pool

The pool was added as the in-memory state layer.

Current package:

engine/pool/pool.go
engine/pool/pool_test.go

It is a state store, not a worker queue.

Current responsibilities:

Add
Upsert
Get
Remove
Has
MarkTested
IsTested
IsWorking
LastError
List
Working
Tested
Failed
Count
WorkingCount
TestedCount

State is separated into:

configs
working
tested
lastErrors

The implementation uses sync.RWMutex.

List() and filtered lists are deterministic by fingerprint.

Remove() clears associated runtime/testing state.

Pool test status

User reported:

go test -v ./engine/pool
PASS

All current pool tests passed, including:

TestNew
TestAddAndGet
TestAddRejectsDuplicate
TestAddRejectsNilConfiguration
TestUpsert
TestUpsertReplacesExistingConfiguration
TestUpsertRejectsNilConfiguration
TestRemove
TestRemoveMissing
TestHas
TestMarkTestedWorking
TestMarkTestedFailed
TestMarkTestedRejectsMissingConfiguration
TestMarkTestedClearsPreviousError
TestListIsDeterministic
TestWorkingTestedAndFailed
TestRemoveClearsRuntimeState
TestNilPool
11. Engine

Current engine file:

engine/engine.go

Current Engine contains:

Collector
Tester
Registry
Database

New() accepts:

collector
tester
registry

and supplies a default collector when nil.

SetDatabase() attaches a database without changing the original constructor API.

NewWithDatabase():

creates the database;
loads existing state;
creates the engine;
attaches the database.

RunOnce() currently performs:

context validation
        ↓
CollectAll
        ↓
MergeConfigurations
        ↓
count discovered
        ↓
count unique
        ↓
for every config:
    context check
    optional registry support check
    Tester.TestAndApply
        ↓
sort final configurations
        ↓
Database.Upsert
        ↓
Database.Save
        ↓
CycleResult

Current CycleResult contains:

StartedAt
FinishedAt
Duration

Discovered
Unique
Tested
Working
Failed

Configurations
Important current behaviour

If a registry exists and does not support the configuration protocol:

Tested++
Failed++
Working=false

and the configuration is skipped rather than sent to an incompatible tester.

If no tester exists:

discovery can still occur
but the cycle returns:
"engine tester is not configured"

Source failures do not prevent successful-source configurations from being processed; the returned error is aggregated.

An already-cancelled context is detected before collection.

Engine test status

User reported all engine tests passing:

TestRunOnceWithoutDatabase
TestRunOnceDeduplicatesConfigurations
TestRunOnceHandlesPartialSourceFailure
TestRunOnceRejectsUnsupportedRegistryProtocol
TestRunOncePersistsDatabase
TestNewWithDatabase
TestRunOnceContextCancellation
TestRunOnceNilTester

The user ran:

go test -v ./engine

and all eight tests passed.

12. Archive

The archive package was added after the database.

Files:

engine/archive/archive.go
engine/archive/archive_test.go

Purpose:

keep failed / historical configurations separate from active state

Current archive model:

type Entry struct {
    Config    *config.Config
    Archived  time.Time
    LastError string
}

Current features:

New
Path
Count
Has
Get
Add
Remove
List
Clear
Load
Save
versioned JSON
validation on load
deterministic list
private-file intent
atomic temporary-file replacement

Add() intentionally replaces an existing archived configuration with the newest archive metadata/reason.

Archive testing history

Archive tests were developed and mostly passed.

A previous test named:

TestSaveCreatesPrivateFile

failed on Windows because:

os.Stat(path).Mode().Perm()

reported:

666

even though the implementation explicitly used:

0600

for the temporary file and final Chmod.

This exposed a test/platform semantic issue, not necessarily a persistence-code defect.

The exact archive test state must be considered authoritative from the current local repository, not from old chat output.

13. Full Verified Test Milestones

The user reported these passing checkpoints.

Config
go test -v ./engine/config
PASS
Parser
go test -v ./engine/parser
PASS
Source
go test -v ./engine/source
PASS
Tester
go test -v ./engine/tester
PASS
Pool
go test -v ./engine/pool
PASS
Engine
go test -v ./engine
PASS
Full suite

At one point the complete repository passed:

go test -v ./...

including:

engine/config
engine/core
engine/parser
engine/source
engine/tester

and the engine package.

Because archive was subsequently modified, the full suite should be rerun after the archive issue is conclusively resolved.

14. Known Mistakes / Lessons From Development

This section is critical for future AI agents.

14.1 Never infer APIs from memory

Several avoidable errors occurred when tests were generated against imagined APIs.

Examples:

ProtocolVLESS        ← wrong
Config.Protocol      ← wrong
Config.Identity      ← wrong
Network struct       ← wrong
Pool.New(workerCount) ← wrong
Pool.Submit()        ← wrong
Pool.Close()         ← wrong
Pool.Run()           ← wrong

The actual repository APIs must always be inspected first.

14.2 Current files beat historical logs

A previous test output may refer to an earlier version of a file.

Never assume that because a user pasted:

TestSaveCreatesPrivateFile

that the current repository still has that test.

Always inspect the current GitHub/local file before editing.

14.3 Match types exactly

Examples of previous mistakes:

Get() returned (*Config, bool)

but code treated it as a single value.

Other examples included:

time.Time vs int64
error vs bool

Before writing tests, inspect the function signature.

14.4 Do not over-test

The user prefers efficient development.

Use:

targeted test
→ fix
→ targeted test
→ next subsystem

Do not repeatedly run:

go test ./...

after every tiny change.

Reserve full-suite testing for integration checkpoints.

14.5 Test contract must match implementation semantics

A cancelled engine cycle sets:

FinishedAt
Duration

so the cancellation test should verify that behaviour rather than assume cancellation means an empty result.

14.6 Windows is a first-class platform

The project target includes Windows.

Do not blindly apply Unix assumptions to Windows file-permission or networking tests.

15. Current Architecture Diagram
                         FREEIRAN
                            │
                     Shared Go Engine
                            │
      ┌─────────────────────┼─────────────────────┐
      │                     │                     │
   Sources               Config               Core Registry
      │                     │                     │
      ▼                     ▼                     ▼
   Fetch                 Parse              Xray / sing-box /
      │                 Normalize              WireGuard
      │                     │                     │
      └──────────────► Deduplicate ◄─────────────┘
                            │
                            ▼
                          Tester
                            │
                    ┌───────┴───────┐
                    │               │
                 Working         Failed
                    │               │
                    ▼               ▼
                  Pool          Archive
                    │
                    ▼
                 Database
                    │
             ┌──────┴──────┐
             │             │
          Android       Windows
16. Important Architectural Correction for the Next Phase

The current architecture has both:

Pool
Database

but they currently duplicate some responsibilities.

The next phase must define their relationship clearly.

Recommended model:

Database = persistent source of durable state
Pool     = fast in-memory operational index/cache
Archive  = persistent historical state

So the engine should eventually become:

Load database
Load archive
        ↓
Build/rebuild pool
        ↓
Discovery
        ↓
Deduplicate against pool/database
        ↓
Test
        ↓
Working:
    pool + database

Failed:
    remove from active pool/database
    add to archive

This should be implemented deliberately rather than allowing the pool and database to diverge.

17. Next Major Development Sequence

The highest-value sequence from the current foundation is:

Phase A — finalize archive semantics

Before moving on:

Resolve Windows/POSIX permission test semantics.
Ensure archive persistence tests are green.
Add archive integration to the engine.
Ensure failed configurations move out of the active store.
Phase B — unify Pool + Database + Archive

Target lifecycle:

NEW
 ↓
DISCOVERED
 ↓
VALID
 ↓
TESTED
 ├── WORKING ──→ ACTIVE
 │                  │
 │                  └── persistence
 │
 └── FAILED ────→ ARCHIVE

Implement explicit transitions rather than relying on scattered map manipulation.

Phase C — scheduler

Add:

engine/scheduler/

Required behaviour:

default interval: 1 hour
immediate first run option
context cancellation
no overlapping cycles
graceful shutdown
configurable interval
deterministic lifecycle
no platform-specific code

Avoid background goroutine leaks.

Phase D — source configuration

Add a clean source registry/configuration layer.

First source:

NiREvil/vless

Do not hard-code large configuration datasets into Go source files.

Phase E — real protocol core adapter

Start with the most important backend.

Likely sequence:

Xray adapter
   ↓
VLESS
VMess
Trojan
Shadowsocks

Then evaluate:

sing-box adapter
Hysteria
Hysteria2
TUIC

WireGuard should use the appropriate platform/runtime implementation rather than being forced through Xray.

Phase F — real testing

Replace mock probes with real protocol-core-backed probes.

Target:

configuration
→ core.Start()
→ running instance
→ network/protocol test
→ latency
→ Close()

Every instance must be cleaned up reliably.

Phase G — client layer

Only after the engine is operational:

android/
windows/

Windows and Android remain thin wrappers over the shared engine.

18. Scheduler Requirements

When implemented, scheduler must satisfy:

start
 ↓
optional immediate run
 ↓
wait interval
 ↓
RunOnce
 ↓
wait interval
 ↓
repeat

Rules:

one cycle at a time
cancellation stops future runs
current run receives context
no leaked tickers/goroutines
configurable interval
default: one hour

A scheduler should never silently launch overlapping engine cycles.

19. Configuration Selection Strategy

Once real working configurations exist, selection should initially be deterministic.

A simple initial ranking can be:

working
↓
lowest latency
↓
most recently verified
↓
stable deterministic fingerprint tie-breaker

Do not introduce AI into the basic selection algorithm unless empirical evidence demonstrates a need.

AI may later help with:

source quality ranking
anomaly detection
predicting configuration lifetime
grouping near-duplicates
choosing retest priority

But basic networking correctness should remain deterministic.

20. Data Retention Strategy

The intended storage model is:

ACTIVE
small
fast
working only

ARCHIVE
historical
compressed eventually
failed/obsolete configurations

Do not indefinitely store duplicate dead configurations.

A future archive implementation should consider:

configuration ID
first seen
last seen
last tested
failure reason
failure count
success history
source history

but avoid adding metadata merely because it might be useful later.

21. Source Management Strategy

Public configuration sources are untrusted external inputs.

For every source:

ID
Name
URL
Enabled

Future metadata can include:

last fetch time
last successful fetch
failure count
content hash
configuration count

but only when required by actual features.

The source system should tolerate:

404
500
timeouts
invalid data
oversized responses
malformed configurations
partial source failure

without taking down the entire engine.

22. Protocol Coverage Goal

The project's universal parser currently targets:

VLESS
VMess
Trojan
Shadowsocks
Hysteria
Hysteria2
TUIC
WireGuard
SOCKS
HTTP

This does NOT mean every protocol already has a real runtime core.

Important distinction:

Parser support ≠ runtime support

A protocol may be:

parsed ✅
validated ✅
tested through mock probe ✅
real core ❌

Future agents must preserve this distinction.

23. Android Architecture Target

Android should use the shared Go engine through a suitable native binding/runtime architecture.

Platform-specific responsibilities:

VPN permission
VPN service lifecycle
network routing
foreground/background lifecycle
notifications
connection state UI

Shared engine responsibilities:

config discovery
config parsing
deduplication
testing
storage
scheduler
core selection

The Android app should remain thin.

24. Windows Architecture Target

Windows should use the same shared engine.

Platform-specific responsibilities:

desktop UI
system integration
network adapter/tunnel integration
service/background behaviour
startup
tray/status handling

Shared responsibilities remain in Go.

25. Security Requirements

Never:

execute arbitrary shell commands from downloaded config
execute arbitrary downloaded scripts
automatically install arbitrary certificates
expose credentials in logs
trust external config syntax without validation
allow a malformed config to panic the engine
let one broken source crash the complete collection cycle
store unnecessary telemetry
require a central account for basic operation

Downloaded config is hostile/untrusted input.

26. Privacy Requirements

FreeIran should not require:

central account
browsing history
visited URL logging
unnecessary telemetry

Configuration data should remain local where possible.

Public configuration credentials should be treated as sensitive data even if publicly published.

27. Dependency Philosophy

Prefer:

standard library
+
small focused dependencies
+
mature protocol cores

Avoid dependency proliferation.

Before adding a dependency, ask:

Can standard Go solve this?
Can an existing project component solve it?
Does the dependency materially reduce complexity?
Is it maintained?
Does it work on Windows and Android?
28. Coding Rules for Future AI Agents

These rules are mandatory for reliable continuation.

Rule 1 — inspect first

Before proposing a code change:

inspect current file
inspect related types
inspect related tests
inspect package API

Never guess.

Rule 2 — use the repository as source of truth

Do not rely on:

old snippets
old test output
remembered API
generic implementation patterns

when current source is available.

Rule 3 — exact replacement instructions

When asking the human to edit manually:

file path
block name
exact current block
exact replacement block

Use exact line ranges when they have been verified.

Rule 4 — whole-file replacements are acceptable

When a file is small or heavily changed, prefer:

replace entire file

rather than complicated patch instructions.

Rule 5 — minimal verification

After changing one subsystem:

gofmt
targeted test

After an integration milestone:

go test -v ./...

Do not repeatedly run full-suite tests for trivial edits.

Rule 6 — never paper over a failure

If a test fails:

trace failure
identify actual layer
fix actual layer

Do not weaken tests simply to make them pass.

Rule 7 — platform-aware behaviour

The target is:

Windows
Android

Every filesystem/network/system assumption must be checked against both eventually.

Rule 8 — avoid speculative abstraction

Do not add interfaces, workers, queues, caches, schedulers, agents, or configuration fields merely because they might become useful.

Implement only the next necessary abstraction.

29. Current Known Technical Risks
Risk A — Pool/database duplication

Needs architectural unification before the engine becomes production-ready.

Risk B — Real core lifecycle

Mock probes currently prove orchestration but do not prove real tunnel functionality.

Risk C — WireGuard runtime

Parsing exists, but platform-level execution needs dedicated implementation.

Risk D — Windows vs Unix filesystem semantics

Tests must not confuse Unix permission bits with Windows ACL semantics.

Risk E — Scheduler overlap

Must ensure one cycle at a time.

Risk F — Core cleanup

Real protocol instances must always be closed.

Risk G — configuration credential exposure

Credentials should not appear in logs or accidental debug output.

30. Definition of Done for v0.1 Engine

v0.1 should not be considered complete until:

[ ] Config model stable
[x] Parser stable
[x] Source collector stable
[x] Deduplication stable
[x] Tester abstraction stable
[x] Core registry stable
[x] Database stable
[x] Pool stable
[ ] Archive fully verified cross-platform
[ ] Pool/database/archive unified
[ ] Scheduler
[ ] NiREvil production source configuration
[ ] Real Xray core adapter
[ ] Real probing
[ ] Working → Active lifecycle
[ ] Failed → Archive lifecycle
[ ] Recovery of previously failed configs
[ ] Resource cleanup
[ ] Full integration tests
[ ] Windows build
[ ] Android build/binding proof-of-concept

Do not claim v0.1 is complete before the remaining boxes are addressed.

31. Immediate Next Task

The next implementation task should be:

ARCHIVE INTEGRATION INTO ENGINE

Specifically:

Engine
├── Database
├── Pool
└── Archive

Then:

working configuration
    → pool
    → database

failed configuration
    → remove from active state
    → archive

This is the bridge between the isolated components already built and the actual FreeIran configuration-maintenance loop.

Before editing:

inspect current engine.go;
inspect current pool.go;
inspect current database.go;
inspect current archive.go;
inspect their tests;
design the smallest integration API;
implement;
add targeted integration tests;
run the targeted engine test;
only after the integration checkpoint run go test -v ./....
32. Handoff Protocol for Any New AI Agent

A new AI agent should begin by reading this file and then:

STEP 1
Inspect repository state.

STEP 2
Verify which files listed here still match reality.

STEP 3
Run or inspect the narrowest relevant tests.

STEP 4
Do not regenerate completed components.

STEP 5
Continue from "Immediate Next Task".

STEP 6
Before every code change:
inspect actual current API.

STEP 7
After every meaningful subsystem:
format + targeted test.

STEP 8
At integration milestones:
full suite.

If the repository differs from this worklog, the repository wins.

Update this file whenever architecture or milestone state materially changes.

33. Current Session Context

This project was developed iteratively with manual one-file-at-a-time edits.

The user explicitly prefers:

efficient development
minimal unnecessary testing
exact replacement instructions
current repository inspection before coding
high code quality
strong verification

The user may manually create files and paste complete file contents.

When supplying a replacement, prefer:

File:
engine/example/example.go

Replace entire file with:
<complete block>

When supplying a block patch, show both:

DELETE:
<exact current block>

ADD:
<exact replacement block>

Do not give vague instructions such as:

"change the relevant section"

unless the exact section has been verified.

34. Attribution

FreeIran

Architect / Project Originator:

Parsa Tak / SHEYTAN

Repository:

https://github.com/Parsaetak/FreeIran

This worklog is an engineering continuity document for the project.```
