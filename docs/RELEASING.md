# Releasing AstraTerm

Releases are cut by pushing a version tag. `.github/workflows/release.yml` then builds the web UI once, the server
archives for every platform (`scripts/release/dist`, exactly what a local `make release` produces), the desktop app
natively on macOS, Windows and Linux runners (`desktop/`, Tauri; [DESKTOP.md](DESKTOP.md)), and creates a **draft**
GitHub release with:

- **Desktop app:** `AstraTerm_<version>_aarch64.dmg` and `_x64.dmg` (macOS), `AstraTerm_<version>_x64-setup.exe` and
  `.msi` (Windows), `.deb`, `.rpm` and `.AppImage` for Linux x64 and ARM64;
- **Server / CLI:** `astraterm_<version>_<os>_<arch>.tar.gz` (macOS, Linux, FreeBSD) and `.zip` (Windows), each with
  the `astraterm` executable, README, LICENSE, CHANGELOG and third-party notices;
- `SHA256SUMS` over every file, an SPDX SBOM per server archive (`*.sbom.json`), and a signed build-provenance
  attestation;
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
   git remote add origin git@github.com:plzcloseyoureyes/astraterm.git
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
   [Unreleased]: https://github.com/plzcloseyoureyes/astraterm/compare/vX.Y.Z...HEAD
   [X.Y.Z]: https://github.com/plzcloseyoureyes/astraterm/releases/tag/vX.Y.Z
   ```
   `scripts/release/changelog-notes.sh vX.Y.Z` prints what will become the release notes.
2. **Refresh the notices and run the checks** (with `web/node_modules` installed):
   ```sh
   make notices                      # commit THIRD_PARTY_NOTICES.md if it changed
   make check
   make release VERSION=vX.Y.Z       # optional local dry run: the exact archives + SHA256SUMS CI will publish
   ```
3. **Commit and merge** the release commit (`release: vX.Y.Z`) to `main` through a pull request; wait for CI.
4. **Tag** the merge commit and push the tag (a signed tag if you have signing set up):
   ```sh
   git switch main && git pull
   git tag -s vX.Y.Z -m "AstraTerm vX.Y.Z"      # or: git tag -a vX.Y.Z -m "AstraTerm vX.Y.Z"
   git push origin vX.Y.Z
   ```
5. **Watch the Release workflow** (Actions tab). It stops early when `LICENSE` is missing, the changelog has no
   section for the version, or `THIRD_PARTY_NOTICES.md` does not match the tagged dependencies.
6. **Review the draft release**: the notes, the desktop installers, 8 server archives + 8 SBOMs and `SHA256SUMS`.
   Install the desktop app on at least one platform and start it. Download one or two archives and
   verify them:
   ```sh
   sha256sum --ignore-missing -c SHA256SUMS          # macOS: shasum -a 256 --ignore-missing -c SHA256SUMS
   gh attestation verify astraterm_X.Y.Z_linux_amd64.tar.gz --repo plzcloseyoureyes/astraterm
   tar -xzf astraterm_X.Y.Z_linux_amd64.tar.gz && ./astraterm_X.Y.Z_linux_amd64/astraterm version
   ```
   `astraterm version` must show `vX.Y.Z`, the tagged commit and its date.
7. **Publish** the draft (Edit → Publish release). Pre-release tags are published as pre-releases.

## Fixing a bad release

- **Before publishing:** delete the draft and the tag (`git push --delete origin vX.Y.Z`, `git tag -d vX.Y.Z`), fix,
  and tag again.
- **After publishing:** never move or reuse a published tag. Release `vX.Y.(Z+1)` with the fix; if the broken release
  is dangerous, mark it as such in its notes (or delete its assets) and point to the new one. Security fixes follow
  [SECURITY.md](../SECURITY.md) (advisory, coordinated disclosure).

## Platforms

The matrix lives in three places that must stay in sync: `PLATFORMS` in the `Makefile`, `defaultPlatforms` in
`scripts/release/dist/main.go` and `platforms` in `scripts/release/notices/main.go`.

Current: darwin/amd64, darwin/arm64, linux/amd64, linux/arm64, linux/arm/7, windows/amd64, windows/arm64,
freebsd/amd64. `make cross-check` compiles every package for all of them. FreeBSD and 32-bit ARMv7 are built but not
yet tested on real systems; smoke-test them on hardware or under QEMU before calling them supported.

Windows executables of the server get their icon, version information and manifest (per-monitor DPI, long paths)
from `packaging/windows/winres.json` with `go tool go-winres` (pinned in `go.mod`).

The desktop app is built per platform on native runners (`desktop` job): macOS Apple silicon and Intel, Windows x64,
Linux x64 and ARM64. Its installers are unsigned for now ([DESKTOP.md → Next steps](DESKTOP.md#next-steps)).

## Tool versions

- go-winres (Windows resources): the `tool` line in `go.mod`.
- Tauri: `desktop/package.json` (CLI) and `desktop/src-tauri/Cargo.toml` (crates, locked in `Cargo.lock`); Rust:
  the stable toolchain on the runners.
- GitHub Actions are pinned to commit SHAs (with the version in a comment); Dependabot proposes updates weekly.
- Node.js for CI: `NODE_VERSION` in the workflows (24); Go: the `go` line of `go.mod`.
