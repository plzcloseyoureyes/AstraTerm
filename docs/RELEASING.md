# Releasing Termstead

Releases are cut by pushing a version tag. `.github/workflows/release.yml` then builds the web UI once, cross-compiles
every platform with GoReleaser (`.goreleaser.yaml`), and creates a **draft** GitHub release with:

- `termstead_<version>_<os>_<arch>.tar.gz` (macOS, Linux, FreeBSD) and `.zip` (Windows), each with the executable,
  README, LICENSE, CHANGELOG and third-party notices;
- `SHA256SUMS`, an SPDX SBOM per archive (`*.sbom.json`), and a signed build-provenance attestation;
- release notes taken from the version's section in `CHANGELOG.md`.

A person reviews the draft and publishes it. Nothing is published automatically.

Versions follow [Semantic Versioning](https://semver.org/): tags look like `v0.3.0`, `v1.0.0`, or `v1.1.0-rc.1`
(anything with a pre-release suffix is marked as a pre-release).

## First publication (one time)

1. **Choose a license** and add it as `LICENSE` at the repository root (GitHub's "Add file → Create new file →
   LICENSE" offers templates). Then update the *License* section of `README.md`. The release workflow fails without
   a `LICENSE*` file. Check the obligations listed in `THIRD_PARTY_NOTICES.md` ("Obligations beyond keeping the
   notices") against the license you pick.
2. **Replace the placeholders**:
   `grep -rn "OWNER\|TBD.example\|TODO(owner)" README.md SECURITY.md CODE_OF_CONDUCT.md CHANGELOG.md .github docs/RELEASING.md`
   — the GitHub owner/organization, the security and conduct contact addresses.
3. **Create the repository** on GitHub (empty: no README, license or .gitignore), then locally:
   ```sh
   git add -A && git status          # review: no data dirs, binaries, node_modules or secrets
   git commit -m "Initial import"
   git remote add origin git@github.com:OWNER/termstead.git
   git push -u origin main
   ```
4. **Repository settings** (Settings on GitHub):
   - *Code security*: enable private vulnerability reporting, Dependabot alerts and security updates, secret scanning
     with push protection. CodeQL runs from `.github/workflows/codeql.yml` (do not also enable "default setup").
   - *Actions → General*: workflow permissions "Read repository contents" (each workflow asks for what it needs);
     require approval for fork pull request workflows.
   - *Rules*: protect `main` (pull requests, required checks: the CI jobs, no force pushes) and add a tag rule for
     `v*` so only maintainers can create or delete release tags.
   - *Releases*: enable immutable releases if available, so published assets cannot be replaced.
5. Uncomment the badges at the top of `README.md` and add screenshots under `docs/images/`.

## Cutting a release

1. **Update the changelog.** In `CHANGELOG.md`, rename `## [Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, add a fresh
   empty `## [Unreleased]` above it, and update the link references at the bottom:
   ```markdown
   [Unreleased]: https://github.com/OWNER/termstead/compare/vX.Y.Z...HEAD
   [X.Y.Z]: https://github.com/OWNER/termstead/releases/tag/vX.Y.Z
   ```
   `scripts/release/changelog-notes.sh vX.Y.Z` prints what will become the release notes.
2. **Refresh the notices and run the checks** (with `web/node_modules` installed):
   ```sh
   make notices                      # commit THIRD_PARTY_NOTICES.md if it changed
   make check
   make release                      # optional local dry run: dist/ archives + SHA256SUMS
   make snapshot                     # optional: the exact CI pipeline (GoReleaser), nothing is published
   ```
   `make snapshot` needs a git commit and runs GoReleaser through `go run` (GoReleaser v2.18 requires Go ≥ 1.27.1,
   which the `go` command downloads automatically); an installed `goreleaser` (`goreleaser release --snapshot --clean`)
   is faster. SBOMs are produced when `syft` is on the PATH (skipped otherwise).
3. **Commit and merge** the release commit (`release: vX.Y.Z`) to `main` through a pull request; wait for CI.
4. **Tag** the merge commit and push the tag (a signed tag if you have signing set up):
   ```sh
   git switch main && git pull
   git tag -s vX.Y.Z -m "Termstead vX.Y.Z"      # or: git tag -a vX.Y.Z -m "Termstead vX.Y.Z"
   git push origin vX.Y.Z
   ```
5. **Watch the Release workflow** (Actions tab). It stops early when `LICENSE` is missing, the changelog has no
   section for the version, or `THIRD_PARTY_NOTICES.md` does not match the tagged dependencies.
6. **Review the draft release**: the notes, and 7 archives + 7 SBOMs + `SHA256SUMS`. Download one or two archives and
   verify them:
   ```sh
   sha256sum --ignore-missing -c SHA256SUMS          # macOS: shasum -a 256 --ignore-missing -c SHA256SUMS
   gh attestation verify termstead_X.Y.Z_linux_amd64.tar.gz --repo OWNER/termstead
   tar -xzf termstead_X.Y.Z_linux_amd64.tar.gz && ./termstead_X.Y.Z_linux_amd64/termstead version
   ```
   `termstead version` must show `vX.Y.Z`, the tagged commit and its date.
7. **Publish** the draft (Edit → Publish release). Pre-release tags are published as pre-releases.

## Fixing a bad release

- **Before publishing:** delete the draft and the tag (`git push --delete origin vX.Y.Z`, `git tag -d vX.Y.Z`), fix,
  and tag again.
- **After publishing:** never move or reuse a published tag. Release `vX.Y.(Z+1)` with the fix; if the broken release
  is dangerous, mark it as such in its notes (or delete its assets) and point to the new one. Security fixes follow
  [SECURITY.md](../SECURITY.md) (advisory, coordinated disclosure).

## Platforms

The matrix lives in four places that must stay in sync: `PLATFORMS` in the `Makefile`, `defaultPlatforms` in
`scripts/release/dist/main.go`, `platforms` in `scripts/release/notices/main.go` and `builds` in `.goreleaser.yaml`.

Current: darwin/amd64, darwin/arm64, linux/amd64, linux/arm64, windows/amd64, windows/arm64, freebsd/amd64.

32-bit ARMv7 (`linux/arm/7`) is not included yet: `internal/recording/cast.go` (`formatClock`) converts
`time.Minute` to `int`, which overflows on 32-bit platforms. With that fixed (`int(d % time.Hour / time.Minute)`), the
whole tree builds and vets for linux/arm; add the platform in the four places above (GoReleaser: `goarch: arm`,
`goarm: ["7"]`) and smoke-test it on real hardware or under QEMU.

## Tool versions

- GoReleaser: `GORELEASER_VERSION` in the Makefile and in `release.yml` (currently v2.18.2).
- GitHub Actions are pinned to commit SHAs (with the version in a comment); Dependabot proposes updates weekly.
- Node.js for CI: `NODE_VERSION` in the workflows (24); Go: the `go` line of `go.mod`.
