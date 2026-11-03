#!/bin/sh
# The release a fresh clone installs: the chart's appVersion, and the image
# in the manifests generated from it (deploy/). The documentation site takes
# its version from the release tag instead (hack/mkdocs_macros.py), so
# nothing else in the repository names a version.
#
# .github/workflows/release.yml runs this after a release and commits what
# it changes, so a release needs no version bump by hand.
#
#   hack/stamp-version.sh v0.1.9
set -eu
cd "$(dirname "$0")/.."

VERSION=${1:?usage: hack/stamp-version.sh vX.Y.Z}
case $VERSION in
v[0-9]*) ;;
*)
	echo "hack/stamp-version.sh: $VERSION is not a release tag (vX.Y.Z)" >&2
	exit 1
	;;
esac

sed -i.bak "s/^appVersion:.*/appVersion: $VERSION/" charts/gcr/Chart.yaml
rm -f charts/gcr/Chart.yaml.bak
# image.tag defaults to the chart's appVersion, so this picks it up.
hack/manifests.sh

grep -n '^appVersion:' charts/gcr/Chart.yaml
grep -hn 'image: ghcr.io/makhov/gcr:' deploy/gcr.yaml deploy/gcr-operator.yaml
