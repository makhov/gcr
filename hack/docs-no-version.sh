#!/bin/sh
# No page names the current release: the site's version comes from the
# release tag through mkdocs.yml's macros ({{{ gcr_version }}},
# hack/mkdocs_macros.py), so a version written into a page goes stale with
# the next release. Run by .github/workflows/docs.yml.
set -eu
cd "$(dirname "$0")/.."

version=${GCR_VERSION:-$(git tag --list 'v[0-9]*' --sort=-version:refname | head -1)}
[ -n "$version" ] || {
	echo "hack/docs-no-version.sh: no release tag (vX.Y.Z); set GCR_VERSION" >&2
	exit 1
}

hits=$(grep -rn -F "$version" --include='*.md' docs || true)
[ -z "$hits" ] || {
	printf '%s\n' "$hits" >&2
	echo >&2
	echo "use {{{ gcr_version }}}, which is $version today, rather than writing it in" >&2
	exit 1
}
echo "no page names $version"
