# ADR: Distribute the CLI as a macOS Homebrew Cask

**Date:** 2026-06-10
**Status:** Accepted

## Context

fleetbox already ships two release channels (`helper-v*` for the signed macOS helper,
`v*` for the CLI). The `v*` channel builds the `fleetbox` CLI with GoReleaser — a pure-Go,
**unsigned** binary (the ADR-0017/0020 sever keeps user binaries free of codesign; only
the helper is signed) for `darwin/arm64` and `linux/{amd64,arm64}`, published to GitHub
Releases. We want a one-line install for it, like the sibling `devbox` project, which
publishes to a `homebrew-<name>` tap via GoReleaser.

Two constraints shape how:

- GoReleaser **deprecated `brews`** (its Homebrew *formula* generator) — soft since v2.10,
  hard since v2.16, where `goreleaser check` now fails on it. Its guidance is that
  pre-built binaries should ship as Homebrew **casks**. Casks are **macOS-only**;
  Linuxbrew has no cask support.
- A downloaded, unsigned binary is quarantined by Gatekeeper, which then refuses to run
  it ("… is damaged and cannot be opened").

## Decision

Distribute the `fleetbox` CLI as a **Homebrew cask** to the `pilat/homebrew-fleetbox` tap,
generated and pushed by GoReleaser on every `v*` tag (`homebrew_casks` block, authed by
the `CICD_HOMEBREW_GITHUB_TOKEN` PAT secret).

- macOS install: `brew tap pilat/fleetbox && brew install --cask fleetbox`.
- Because the cask is macOS-only, the supported **Linux** install is
  `go install github.com/pilat/fleetbox/cmd/fleetbox@latest` — we do **not** ship a
  Linuxbrew package.
- A post-install hook strips the quarantine bit (`xattr -dr com.apple.quarantine`) so the
  unsigned binary runs without the Gatekeeper block.
- Add a `version` subcommand with `-ldflags -X main.{version,commit,date}` so
  brew-installed users can report what they have.

The cask carries the CLI binary. The signed helper and the cloud-hypervisor VMM/firmware
still auto-download (checksum-pinned) on first boot, so nothing in this channel needs
codesigning.

**Amendment (2026-06-11, ADR-0022):** the cask now *also* installs shell completions, not
only the binary. Once the CLI moved onto cobra, the `homebrew_casks` block gained
`generate_completions_from_executable` (`args: [completion]`, `shell_parameter_format: cobra`,
`shells: [bash, zsh, fish]`); on `brew install` the cask runs `fleetbox completion <shell>`
and installs the output. The quarantine-strip postflight and caveats are unchanged. So the
earlier "the cask carries only the binary" framing is superseded — it carries the binary plus
generated completions. See ADR-0022.

## Alternatives Considered

- **GoReleaser `brews` (Homebrew formula), as devbox does.** Works on both macOS and
  Linuxbrew today, but is deprecated — `goreleaser check` fails on it in v2.16 and it will
  be removed. Shipping a brand-new repo on a deprecated config, inheriting devbox's debt,
  isn't worth the Linuxbrew coverage: that audience is niche, already has `go install`, and
  needs `/dev/kvm` + `CAP_NET_ADMIN` to run anything anyway.
- **Codesign + notarize the CLI** to avoid the quarantine hook. Apple charges a yearly fee
  and it reintroduces signing to the user-binary build that ADR-0017 deliberately kept
  signing-free. The quarantine-strip hook is GoReleaser's standard escape hatch and costs
  nothing.
- **homebrew-core instead of a tap.** Gets `brew install fleetbox` with no tap step, but
  requires meeting Homebrew's notability bar and passing an external review — not worth it
  pre-1.0.

## Consequences

- Good: a one-line macOS install; the cask stays in sync automatically on every release;
  the unsigned-binary build stays unsigned.
- Bad: no Homebrew install on Linux — Linux users must use `go install`. The quarantine
  hook is a workaround for the unsigned binary; if the CLI is ever signed and notarized,
  the hook should be removed.
- Prerequisites for the first `v*` release after this lands: the `pilat/homebrew-fleetbox`
  tap repo must exist, and `CICD_HOMEBREW_GITHUB_TOKEN` (a PAT with write access to the
  tap) must be set as a secret on `pilat/fleetbox`. Without either, the cask-push step of
  the release fails.
