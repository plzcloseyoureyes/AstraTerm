## What and why

<!-- What does this change do, and which problem does it solve? Link the issue: "Fixes #123". -->

## How it was tested

<!-- Commands you ran and what you checked by hand (browser, OS, run mode). -->

## Checklist

- [ ] `make check` passes (gofmt, go vet, Go tests, typecheck, oxlint + lint-ui, frontend tests)
- [ ] Tests added or updated next to the changed code
- [ ] Contracts respected: [SPEC.md](../blob/main/docs/SPEC.md) (deviations documented in §9) and [UX.md](../blob/main/docs/UX.md)
- [ ] No new dependencies (or discussed first; `make notices` rerun and `THIRD_PARTY_NOTICES.md` committed)
- [ ] Outbound connections go through `internal/netguard`; secrets are never logged, returned or put in URLs
- [ ] User-visible changes noted under `## [Unreleased]` in [CHANGELOG.md](../blob/main/CHANGELOG.md)
