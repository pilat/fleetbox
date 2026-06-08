# coding-style.md

Mandatory style guide for fleetbox. Two layers:

- **Part A — Code Style** (file/function/expression level): naming, error wrapping,
  declaration order, testing.
- **Part B — Architecture Style** (package/module level): layering, the public API
  contract, concurrency, error architecture.

When these rules conflict with existing code, existing code is tech debt — fix it or
document the exception in `docs/adr/`.

Architecture facts (which package does what, how the system fits together) live in
`ARCHITECTURE.md`, not here. This file is **prescriptive**, `ARCHITECTURE.md` is
**descriptive**.

The machine-checkable subset of these rules is enforced by `.golangci.yml`
(`make lint`). A rule being absent from the linter does not make it optional.

---

# Part A — Code Style

## A.1 Package boundaries

One package = one responsibility. No `package main` outside of `cmd/`. The current
package layout is in `ARCHITECTURE.md §5`.

## A.2 Interfaces and constructors

Interfaces exist **where substitution exists** — not everywhere. fleetbox has exactly
one substitution point: the hypervisor backend.

```go
// internal/backend — the contract
type Backend interface {
    Create(cfg Config) (VM, error)
    NestedVirtSupported() bool
}

// internal/backend/vz — an implementation
var _ backend.Backend = (*Backend)(nil)

func New() *Backend { return &Backend{} }
```

Rules:

- A type that implements an interface declares it with a compile-time check:
  `var _ Iface = (*impl)(nil)`.
- Constructors are named `New` (or `NewXxx` when a package has several types) and are
  real constructors — they return a ready-to-use value, never a half-initialized one.
- Plain data holders and single-implementation services stay **concrete types**
  (`store.Store`, `sshkey.Manager`). Do not introduce an interface until a second
  implementation or a test seam genuinely needs it (see YAGNI, A.9).

*Why not interface-first everywhere:* a test-fixture library has few substitution
points, and interfaces without substitution are noise. An interface earns its place by
having (or realistically expecting) more than one implementation.

## A.3 Naming conventions

| What | Pattern | Example |
|------|---------|---------|
| Interface | Role noun | `Backend`, `VM` |
| Implementation struct | Short, lowercase if unexported | `vz.Backend`, `runnerState` |
| Constructor | `New` / `NewXxx` | `store.New()`, `sshkey.NewManager()` |
| Constants | CamelCase, grouped in `const (...)` | `StateRunning`, `defaultImage` |
| Functional options | `WithXxx` | `WithCPUs`, `WithMemoryGB` |
| Test files | `*_test.go` next to code | `store_test.go` |

Receivers are one letter (`v *VM`, `s *Store`, `m *Manager`) — never `this`/`self`.

## A.4 File organisation

Strict declaration order in every `.go` file (top to bottom):

```text
const      -> All constants
var        -> All package-level variables (incl. var _ Iface checks)
type       -> All type declarations
func New() -> Constructor(s)
Func()     -> Exported functions/methods
func()     -> Unexported functions/methods
```

Rules:

1. Compile-time interface checks go right after the implementation type (or in the
   package-level `var` block).
2. Exported functions come before unexported.
3. Keep declarations grouped and predictable — no constants scattered between
   functions.
4. Build tags (`//go:build darwin && arm64`) are the first line of the file, before the
   package doc comment.

## A.5 Size guidance

No per-file, per-function, or per-package line limits. Long files and long functions are
not inherently bad — deeply nested functions are. If a function is long but flat
(sequential steps, early returns), it is fine. Split by responsibility, not by line
count.

## A.6 Error handling

```go
func (v *VM) Destroy(ctx context.Context) error {
    _ = v.backend.Stop(ctx) // best-effort: we are deleting the VM anyway

    if err := v.store.Delete(v.name); err != nil {
        return fmt.Errorf("delete vm files: %w", err)
    }

    return nil
}
```

Rules:

- **Always wrap errors** crossing a function boundary: `fmt.Errorf("context: %w", err)`.
  Add actionable context (VM name, file path, address). Enforced by `wrapcheck`.
- **Explicit discards only.** An error that is intentionally ignored is assigned to
  `_` (`_ = f.Close()`), never silently dropped by calling a function bare. The `_ =`
  is the documentation that the discard is deliberate. Discards are acceptable only in
  best-effort cleanup paths (closing things on the way out, removing temp files after a
  failure).
- **Sentinel errors** only where a caller branches via `errors.Is`. fleetbox currently
  has none; declare them in the package that produces them when needed.
- **No nested error handling (staircase anti-pattern).** For branching IO logic:
  1. Extract branches into flat methods returning `(*T, error)`. Each wraps errors with
     `fmt.Errorf("context: %w", err)` and lets them bubble up — including sentinel
     errors.
  2. Dispatch via IIFE for 2–3 branches:
     `result, err := func() (*T, error) { if cond { return s.branchA(ctx) } return s.branchB(ctx) }()`.
  3. Catch sentinel errors **once** at the caller, not inside each branch.
  4. Flat error checks: `errors.Is(err, ...)` first (specific), then `if err != nil`
     (general). NEVER nest `if err != nil { if errors.Is(err, ...) { } }`.
- Use `errors.New` for fixed messages, `fmt.Errorf` only when formatting or wrapping
  (enforced by `perfsprint`).

## A.7 Testing

Plain stdlib `testing` — no assertion libraries, no mocking frameworks. A test-fixture
library should not drag test dependencies into its consumers' go.sum.

```go
func TestSafeName(t *testing.T) {
    tests := []struct {
        input string
        want  string
    }{
        {"TestSimple", "testsimple"},
        {"TestWith/Subtest", "testwith-subtest"},
    }

    for _, tt := range tests {
        t.Run(tt.input, func(t *testing.T) {
            if got := safeName(tt.input); got != tt.want {
                t.Errorf("safeName(%q) = %q, want %q", tt.input, got, tt.want)
            }
        })
    }
}
```

Rules:

- Table-driven tests where there are multiple cases of the same shape.
- `t.TempDir()` for filesystem tests — never write outside it.
- Unit tests (`make test`) must not boot VMs, hit the network, or require entitlements.
- VM-booting tests live in `fleetboxtest` behind `make test-vm` and must skip
  gracefully on unsupported hardware (`skipIfUnsupported`) and in `-short` mode.

## A.8 JSON

snake_case tags, `omitempty` where absence is meaningful (enforced by `tagliatelle`):

```go
type VM struct {
    Name      string    `json:"name"`
    MemoryMB  int       `json:"memory_mb"`
    CreatedAt time.Time `json:"created_at"`
}
```

## A.9 Core principles

**Fail fast.** Do not hide impossible states with defensive no-op branches. Fail loudly
when invariants are violated.

**No arrow problem.** Use early returns and continue statements to keep logic flat.

**YAGNI over DRY.** Do not extract helpers, wrapper types, or intermediate abstractions
to reduce repetition unless the abstraction eliminates actual complexity (not just
lines). Three similar `if` blocks are better than one helper that adds an indirection
layer. Extract only when the duplication hides a bug-prone invariant or when the shared
logic will genuinely evolve as one unit.

**Single-use helpers are the same smell.** Do not extract a private method that is
called from exactly one place unless it genuinely simplifies the caller's control flow.

**Minimal exports.** Keep internals private by default. In `internal/` packages, export
only what sibling consumers need. In the root package, every export is public API
forever (well, until v1) — be stingy.

**Comments.**

- All comments and commit messages in English.
- **Godoc on every exported symbol of the public packages** (`fleetbox`,
  `fleetboxtest`) — this is a library; `go doc` output is the user interface.
- Exported symbols of `internal/` packages get doc comments too (they are API for the
  rest of the module).
- Comments explain **why**, not **what**. No comment when the code is self-explanatory.

## A.10 Anti-patterns (code level)

```go
// BAD — bare propagation, no context
return err

// GOOD
return fmt.Errorf("parse config: %w", err)
```

```go
// BAD — silently dropped error
f.Close()

// GOOD — explicit, documented discard
_ = f.Close()
```

```go
// BAD — interface for a type with one implementation and no test seam
type Storer interface { /* 18 methods mirroring Store */ }

// GOOD — concrete type until substitution is needed
type Store struct { baseDir string }
```

## A.11 Pre-commit checklist

- [ ] `make test` passes
- [ ] `make lint` is clean (golangci-lint with the project config — includes gofumpt
      formatting and the depguard vz-isolation rule)
- [ ] No `package main` outside `cmd/`
- [ ] New exported symbols have doc comments
- [ ] Errors wrapped with context; discards explicit
- [ ] Declaration order respected (A.4)
- [ ] If the package list, public API, CLI surface, or on-disk layout changed:
      `ARCHITECTURE.md` updated in the same commit (see §8 there)
- [ ] If a design decision was made: ADR added to `docs/adr/`

---

# Part B — Architecture Style

Structural rules for new code. The descriptive counterpart (what exists today) is
`ARCHITECTURE.md`.

## B.1 Layering

Three layers, imports flow downward only:

```
consumers:   cmd/fleetbox   internal/runner   fleetboxtest    (wrap the public API)
public API:  fleetbox (root package)                          (orchestrates internals)
internal:    backend, backend/vz, image, seed,
             store, dhcp, sshkey                               (single-purpose building blocks)
```

`internal/runner` sits in the **consumer** layer despite its import path: it drives
the public API on behalf of the CLI. It lives under `internal/` only because it is not
part of the public contract.

Rules:

- **B.1.1** — `cmd/fleetbox`, `internal/runner`, and `fleetboxtest` consume the
  **public API**. `cmd/fleetbox` and `internal/runner` may additionally import
  `internal/store` for CLI-mode process management (the documented exceptions in
  `ARCHITECTURE.md §5.3` / §5.11); none of the consumers may import the backend,
  image, seed, dhcp, or sshkey packages directly.
- **B.1.2** — Building-block packages do not import each other, with exactly one
  allowed edge: `backend/vz → backend` (implementation → contract). Adding another
  edge requires an ADR.
- **B.1.3** — Nothing imports `cmd/`. Building-block packages never import the root
  package or `runner` (no upward imports).

## B.2 The public API

- **B.2.1 — Backend-neutral, forever.** No type from the vendored vz fork
  (`third_party/vz`, or any future hypervisor SDK) appears in an exported signature.
  Enforced by depguard; violations are bugs (ADR-0002).
- **B.2.2 — Functional options.** VM configuration is expressed as `Option` funcs
  (`WithCPUs`, `WithMemoryGB`). Adding a knob = adding a `WithXxx` — never a config
  struct parameter, never a yaml file.
- **B.2.3 — Idempotency.** `Start` on an existing VM boots it from its stored config;
  callers must not need to check existence first. (Target state: `Start` also detects
  already-running VMs — today that guard lives only in the CLI runner; see the known
  gap in `ARCHITECTURE.md §5.1`.)
- **B.2.4 — Context first.** Every operation that can block takes `ctx context.Context`
  as its first parameter and honors cancellation.
- **B.2.5 — testing.TB integration lives in `fleetboxtest`**, not in the root package.
  The root package must stay importable by non-test code without pulling `testing`.

## B.3 Backend rules

- **B.3.1** — One backend per platform, selected at compile time via build-tagged
  `backend_<GOOS>_<GOARCH>.go` files. Never at runtime, never by config.
- **B.3.2** — A backend package translates ALL of its SDK's types/errors/states into
  `internal/backend` types at its boundary. SDK types never leak upward.
- **B.3.3** — The `Backend`/`VM` interfaces stay minimal: create, start, stop, state,
  wait. Features that can be built on top (IP discovery, SSH) stay outside the backend.

## B.4 Concurrency

- **B.4.1 — Goroutines are scoped.** A `go func()` appears only where its lifetime is
  bounded by an owner: the serial-console copier (dies with the VM), the runner's
  socket listener (dies with the runner process), per-connection handlers (die with the
  connection). Ad-hoc goroutines in business logic are forbidden.
- **B.4.2 — One mutex per struct.** A struct holding shared state uses a single
  `sync.Mutex` (see `runnerState`). Multi-mutex structs require a doc comment declaring
  lock order.
- **B.4.3 — Polling is acceptable** for waiting on external state (VM boot, DHCP lease,
  SSH readiness) — with a deadline and a ctx check per iteration. This is a test
  fixture library; simplicity beats event plumbing.
- **B.4.4 — Cleanup is deferred.** Every resource acquired in a function is released
  via `defer` in that function, with explicit `_ =` discards on cleanup errors.

## B.5 Error architecture

- **B.5.1 — Propagate by default.** Log-and-continue does not exist here (the library
  has no logger). An error either propagates wrapped, or is explicitly discarded with
  `_ =` in a best-effort cleanup path.
- **B.5.2 — Sentinel errors** are declared in the package that produces them, only when
  a caller branches on them via `errors.Is`. Catch sentinels once at the caller, not
  inside each branch.
- **B.5.3 — Infra errors stay internal.** If callers of the public API ever need to
  branch on error classes (e.g. "image not found" vs "boot failed"), expose sentinel
  errors from the root package — never let internal package errors become part of the
  public contract by accident.

## B.6 Guest contract

- **B.6.1 — Nothing of ours inside the guest.** No agent, no helper binary, no
  host↔guest protocol. The seed ISO (cloud-init user-data: one user, one key, hostname,
  sudo) is the entire provisioning surface (ADR-0005).
- **B.6.2 — No per-distro code paths.** The same seed, the same boot path, the same
  discovery for every image. A distro that needs special handling is a distro we don't
  support.
- **B.6.3 — The smell test:** if a yaml parser or a binary that must be placed inside
  the guest image appears in this repo, the project has failed its premise.

## B.7 API evolution & removals

- **B.7.1 — Pre-1.0 rule: remove in one PR.** When deleting or renaming an exported
  symbol, update every consumer in the same commit. No stubs, no parallel versions, no
  deprecation shims. A grep for the old name returns zero hits after the PR lands.
- **B.7.2 — Deprecation comments are for post-1.0 only.**
- **B.7.3 — Contract changes propagate through the type system.** If an interface
  changes, all implementors are updated in the same PR. Compile errors are the intended
  signal.
- **B.7.4 — `ARCHITECTURE.md` updates are part of the change.** A PR that changes a
  package's purpose, API, dependencies, or invariants updates the corresponding §5
  section in the same PR.

## B.8 Anti-patterns (architecture level)

Patterns from other VM tooling that we explicitly do not import:

- **Port forwarding / hostagent tunnels** — direct IP is the whole point (ADR-0004).
- **Guest agents and host↔guest protocols** — nothing of ours runs in the guest
  (ADR-0005).
- **Yaml/template-driven VM definitions** — flags, defaults, and a dumb alias map
  (ADR-0003).
- **Per-distro provisioning scripts** — one code path for all images.
- **Cluster as an entity** — clusters are a naming convention (`ARCHITECTURE.md §4.2`).
- **Multiple hypervisors per platform** — one backend per platform, compile-time
  selected (ADR-0002).
- **Runtime backend/config selection** — if it can be decided at compile time, it is.
- **GUI, video, audio devices** — this is a headless test fixture.
