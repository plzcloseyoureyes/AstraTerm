#!/bin/sh
# Prints the CHANGELOG.md section of one release, for the GitHub release notes (.github/workflows/release.yml):
#
#   scripts/release/changelog-notes.sh v1.2.3 > notes.md
#
# The section is the one headed "## [1.2.3]" (Keep a Changelog); it fails when the section is missing or empty, so a
# tag cannot be released without its changelog entry. A pre-release tag (v1.2.3-rc.1) without its own section uses
# "## [Unreleased]": a release candidate is what is about to be released.
set -eu
version=${1:?usage: changelog-notes.sh <tag, e.g. v1.2.3>}
version=${version#v}
file=${2:-CHANGELOG.md}
section() {
	awk -v v="$1" '
		/^## \[/ { if (found) exit; if (index($0, "## [" v "]") == 1) { found = 1; next } }
		found && /^\[[^]]+\]: / { exit }
		found { print }
	' "$file"
}
notes=$(section "$version")
case "$version" in
*-*) [ -n "$(printf '%s' "$notes" | tr -d '[:space:]')" ] || notes=$(section Unreleased) ;;
esac
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
	echo "changelog-notes: no \"## [$version]\" section in $file" >&2
	exit 1
fi
printf '%s\n' "$notes"
