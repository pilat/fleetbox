# Architecture Decision Records

This directory holds the **why** behind significant architectural choices in fleetbox.
`ARCHITECTURE.md` describes *what* the system looks like today; ADRs explain *why it
looks that way* — the alternatives that were considered, the constraints in play, the
trade-offs accepted.

The initial set (ADR-0001 … ADR-0007) was extracted from the v0 design spec, which
lives in gitignored local working files (`ai/tasks/`) and does not travel with the
repo. ADRs are how those decisions become part of the repository.

## When to write an ADR

Write one when the decision is:

- **Hard to reverse.** Public API shape, on-disk layout, platform commitments, guest
  contracts.
- **Likely to be questioned later.** "Why didn't we just use X?" If the answer needs
  context that won't be obvious from reading the code in six months, capture it.
- **A deliberate trade-off.** You picked one thing over another for reasons, not by
  default.

Skip the ADR for routine choices that the code itself documents: which helper to call,
naming, internal refactors.

## Filename convention

`NNNN-kebab-case-title.md`, where `NNNN` is a zero-padded sequential number (`0001`,
`0002`, ...). Pick the next free number when adding a new ADR. This is the Fowler /
adr-tools convention — referenceable as "ADR-7", trivially sortable, no collisions even
when multiple ADRs land on the same day.

Once an ADR is accepted, **its number and content are immutable**. To revise a
decision, write a new ADR that supersedes the old one (and link both ways).

## Structure

Use `TEMPLATE.md` as the starting point. Required sections: **Status**, **Context**,
**Decision**, **Alternatives Considered**, **Consequences**. Keep each section short —
an ADR is not a design doc, it's a record of a decision and its reasoning.

## Status values

- **Proposed** — under discussion, not yet committed to code.
- **Accepted** — the decision is in effect; code reflects it.
- **Superseded by ADR-NNNN** — a later ADR replaces this one; link the successor.
- **Rejected** — the decision was considered and explicitly turned down. Useful when
  the same idea keeps resurfacing.
