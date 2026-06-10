<!--
PR title = the squash commit message (this repo is squash-only). Make it a valid
Conventional Commit:

    <type>[(scope)][!]: <description>

  types: feat · fix · refactor · test · docs · ci · build · chore · perf · style
  breaking change: add ! before the colon — e.g. feat(api)!: remove WithMount
  keep it one line, no trailing period
-->

<!--
Why does this change exist? A few sentences of plain prose — the problem it solves,
what it improves, why now. The diff already shows the WHAT; this is the WHY. No
tables, no file-by-file lists. Replace this comment with your prose (HTML comments
do not render in the PR).
-->


## Checklist

- [ ] Changed the public API, package list, CLI surface, on-disk layout, or dependencies → `ARCHITECTURE.md` updated in this PR
- [ ] Made a new, hard-to-reverse design decision → added an ADR under `docs/adr/` (next sequential number)
- [ ] Breaking change (`!` in the title) → the description spells out what callers must change
