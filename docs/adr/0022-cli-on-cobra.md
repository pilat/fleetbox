# ADR: Rewrite the CLI on cobra; ship completions via the Homebrew cask

**Date:** 2026-06-11
**Status:** Accepted

## Context

The `cmd/fleetbox` CLI was hand-written on the stdlib `flag` package: a manual `switch`
dispatcher in `main()`, a monolithic `usage()` blob, a `parseInterspersed` hack to allow
flags after positionals, and a custom `stringSlice` flag type for `--fixture`. It had no
shell completion, inconsistent help/exit behaviour (bare `fleetbox` exited 1; a subcommand's
`-h` went to stderr with exit 2), and a design audit found several command-surface footguns
where the tool silently did the wrong thing (`ssh web uname -a` dropped the command;
`cp a:/x b:/y` copied inside `a`; `down web` was a no-op on a cluster while `rm web`
over-matched an unrelated `web-prod`).

Two forces drove a framework choice. First, we wanted **shell completion** — both static
(commands/flags) and **dynamic** (live VM names), the headline ergonomic gap for a
test-fixture tool. Second, completion had to be **delivered**: fleetbox already ships a macOS
Homebrew **cask** (ADR-0021), and GoReleaser can install per-shell completion scripts from a
cask by running the built binary at install time — but only if the binary emits standard
per-shell scripts.

## Decision

Rewrite the CLI on **`github.com/spf13/cobra`**, written deliberately clean, and deliver
completions through the existing cask.

- **cobra structure, no globals, no `init()`.** Each command is a `func newXxxCmd()
  *cobra.Command` returning the command; `newRootCmd` in `root.go` assembles the tree. The
  only package-level vars are the ldflags-set `version/commit/date`. Each command's `RunE`
  calls a `runX` helper that opens its own `store.New()`, so store-free commands (`version`,
  `completion`, `--help`) never fail on a store error. A single `cliExit{code, silent}` error
  type carries the process exit code out to `main`, used by ssh/cp exit-code propagation and
  the best-effort bulk `down`/`rm` loops.
- **Backward-compatible surface, plus guardrails.** Same commands, same flags, same human
  output. `-n` stays a long-name-`n`-with-shorthand-`n` int flag (no new `--count`). We *add*
  aliases (`start`/`stop`/`halt`/`shell`/`destroy`/`delete`/…), machine-readable `ls -q` /
  `ls -o json`, dynamic VM-name completion (`ValidArgsFunction`), and behavioural fixes for
  the audited footguns (shared `down`/`rm` target resolution via `store.ClusterName`; `rm`
  confirms any removal; `cp` rejects VM↔VM; `ssh` requires `--` and propagates the child exit
  code). No new *command* surface (no `info`/`status`, no new `ls` columns).
- **Completion via the cask.** Add `generate_completions_from_executable` to the
  `homebrew_casks` block (`args: [completion]`, `shell_parameter_format: cobra`,
  `shells: [bash, zsh, fish]`; cobra adds pwsh for free). The cask runs `fleetbox completion
  <shell>` to install the scripts; the existing quarantine-strip postflight and caveats stay.

## Alternatives Considered

- **kong.** Its completion model is `posener/complete`'s `complete -C` — a single
  `__complete`-style hook, not per-shell scripts. It does not fit GoReleaser's cask
  `generate_completions_from_executable`, which is exactly our delivery mechanism.
- **urfave/cli v3.** Good ergonomics, but its *dynamic* completion is self-admittedly still
  maturing (urfave/cli #1905), and dynamic VM-name completion is the headline feature.
- **charmbracelet/fang.** Prettiest output, but it is a wrapper *over cobra* that pulls heavy
  charm deps while keeping cobra's structure anyway. Deferred, not rejected: the door is open
  to add `fang.Execute(ctx, root)` later for styled output without re-architecting.
- **Keep the hand-rolled `flag` CLI.** Rejected: no path to per-shell completion without
  re-implementing what cobra/GoReleaser already do, and the footgun fixes plus interspersed
  parsing are exactly what a real parser gives for free.

cobra wins because completion is the #1 requirement: GoReleaser's cask completion maps to it
one-to-one (`shell_parameter_format: cobra`), and `ValidArgsFunction` is the most
battle-tested dynamic-completion system in Go (kubectl/gh/docker).

## Consequences

- Good: one consistent help/exit model; static + dynamic completion installed automatically
  by the cask; the audited footguns are fixed; `ls -o json`/`-q` make the tool scriptable.
  The new dependency is `spf13/cobra` (+ `pflag`, `mousetrap`), all pure Go — the darwin
  sever (ADR-0017/0020) is unaffected (`cmd/fleetbox` still links no hypervisor on darwin).
- Bad: a third-party CLI dependency where there was none. Accepted — cobra is the de-facto
  standard and the completion delivery justifies it.
- The `ls -o json` keys are **snake_case** (`memory_mb`/`disk_mb`/`created_at`) to match
  `config.json` and the control protocol — one consistent case across all fleetbox JSON
  (and it satisfies the repo's `tagliatelle` lint rule without an exception).
- Verification caveat: `goreleaser check` only validates config syntax. The completion
  stanza needs GoReleaser ≥ v2.15 and must be proven with a real build
  (`goreleaser release --snapshot --clean`), which renders the cask's
  `generate_completions_from_executable` directive — done for this change on v2.16.0.
- Amends ADR-0021 (the cask now installs completions, not only the binary).
