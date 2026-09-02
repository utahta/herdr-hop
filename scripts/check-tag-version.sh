#!/bin/sh
# check-tag-version.sh <tag>: fail unless <tag> is exactly "v" + the version
# in herdr-plugin.toml. Run by the release workflow before anything is
# published, so a mismatched tag never produces a release that install.sh
# (which derives the download tag from the manifest version) cannot find.
set -eu

if [ $# -ne 1 ]; then
	echo "usage: $0 <tag>" >&2
	exit 2
fi
tag=$1

version=$(sed -n 's/^version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' herdr-plugin.toml | head -n 1)
if [ -z "$version" ]; then
	echo "error: could not read version from herdr-plugin.toml" >&2
	exit 1
fi
if [ "$tag" != "v$version" ]; then
	echo "error: tag $tag does not match herdr-plugin.toml version $version (expected v$version)" >&2
	exit 1
fi
echo "tag $tag matches manifest version $version"
