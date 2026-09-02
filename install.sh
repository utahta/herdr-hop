#!/bin/sh
# install.sh: install the herdr-hop binary into the plugin root (the current
# directory). Run by herdr's [[build]] step on `herdr plugin install`.
#
# Preferred path: download the prebuilt binary for this platform from GitHub
# Releases, verify the OpenSSH signature on checksums.txt against the
# committed public key (release/signing-key.pub), verify the archive's
# sha256 against the verified checksums.txt, and extract.
#
# Environment problems (unsupported platform, no downloader, missing
# verification tools, download failures, a broken extracted binary) fall
# back to `go build`. Failures that suggest tampering — an invalid
# signature, or a checksum mismatch against the verified checksums.txt —
# abort without falling back.
#
# HOP_INSTALL_BASE_URL overrides the download base URL for integration
# tests only; it relaxes the HTTPS-only restriction so a local HTTP server
# can serve the assets. The default GitHub path always enforces HTTPS.
set -eu

REPO="utahta/herdr-hop"
SIGNER_ID="herdr-hop-release"
PROBE_ID="herdr-hop-probe"

say() { echo "install.sh: $*"; }

fallback() {
	say "$*"
	if command -v go >/dev/null 2>&1; then
		say "falling back to source build: go build -o herdr-hop ."
		go build -o herdr-hop .
		say "built herdr-hop from source"
		exit 0
	fi
	say "error: no prebuilt binary could be installed and Go is not available."
	say "Install Go 1.26+ (https://go.dev/dl/) or check that your platform is supported (macOS/Linux, amd64/arm64)."
	exit 1
}

hard_error() {
	say "error: $*"
	say "aborting: this may indicate tampered or corrupted release assets. Not falling back to a source build."
	exit 1
}

# 1. Platform.
os=$(uname -s)
case "$os" in
Darwin) os=darwin ;;
Linux) os=linux ;;
*) fallback "unsupported OS: $os" ;;
esac
arch=$(uname -m)
case "$arch" in
arm64 | aarch64) arch=arm64 ;;
x86_64) arch=amd64 ;;
*) fallback "unsupported architecture: $arch" ;;
esac

# 2. Version from the manifest. It decides the release tag to download
# from, and is only ever used in the URL and asset name below, so anything
# outside [A-Za-z0-9.] takes the fallback path instead.
version=$(sed -n 's/^version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' herdr-plugin.toml | head -n 1)
case "$version" in
'' | *[!A-Za-z0-9.]*) fallback "cannot determine a usable version from herdr-plugin.toml" ;;
esac

# 3. Verification tools. ssh-keygen capability is decided by verifying a
# fixed probe signature shipped with the repository, not by parsing
# diagnostics (which vary across OpenSSH versions): if the probe verifies,
# the tool works and any later verification failure means a bad signature.
if ! command -v ssh-keygen >/dev/null 2>&1 ||
	! ssh-keygen -Y verify -f release/probe-key.pub -I "$PROBE_ID" -n "$PROBE_ID" \
		-s release/probe.sig <release/probe.txt >/dev/null 2>&1; then
	fallback "ssh-keygen cannot verify signatures on this system"
fi
if command -v shasum >/dev/null 2>&1; then
	sumtool="shasum -a 256"
elif command -v sha256sum >/dev/null 2>&1; then
	sumtool="sha256sum"
else
	fallback "neither shasum nor sha256sum is available"
fi

# 4. Download checksums.txt and its signature.
base_url="https://github.com/$REPO/releases/download/v$version"
if [ -n "${HOP_INSTALL_BASE_URL:-}" ]; then
	base_url="$HOP_INSTALL_BASE_URL"
fi

fetch() { # fetch <url> <outfile>
	if command -v curl >/dev/null 2>&1; then
		if [ -n "${HOP_INSTALL_BASE_URL:-}" ]; then
			curl -fsSL --retry 2 --max-time 120 -o "$2" "$1"
		else
			curl -fsSL --proto '=https' --tlsv1.2 --retry 2 --max-time 120 -o "$2" "$1"
		fi
	elif command -v wget >/dev/null 2>&1; then
		wget -q -T 120 -t 3 -O "$2" "$1"
	else
		return 1
	fi
}

tmp=$(mktemp -d)
# INT/TERM must exit explicitly: a trap that only cleans up would let the
# shell resume after the interrupted command, drive its failure into
# fallback, and report a successful install that nobody asked to finish.
# exit from a signal trap still runs the EXIT trap, which removes $tmp.
trap 'rm -rf "$tmp"' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

asset="herdr-hop_v${version}_${os}_${arch}.tar.gz"
say "downloading checksums for v$version"
fetch "$base_url/checksums.txt" "$tmp/checksums.txt" ||
	fallback "could not download checksums.txt (offline, no curl/wget, or no release for v$version)"
fetch "$base_url/checksums.txt.sig" "$tmp/checksums.txt.sig" ||
	fallback "could not download checksums.txt.sig"

# 5. Verify the signature. The probe above proved ssh-keygen works, so a
# failure here means the checksums file does not match its signature.
say "verifying signature of checksums.txt"
if ! ssh-keygen -Y verify -f release/signing-key.pub -I "$SIGNER_ID" -n "$SIGNER_ID" \
	-s "$tmp/checksums.txt.sig" <"$tmp/checksums.txt" >/dev/null 2>&1; then
	hard_error "signature verification of checksums.txt failed"
fi

# 6. Download the archive and verify its sha256 against the verified list.
awk -v a="$asset" '$2 == a' "$tmp/checksums.txt" >"$tmp/checksum.line"
if ! [ -s "$tmp/checksum.line" ]; then
	fallback "release v$version has no asset for $os/$arch ($asset)"
fi
say "downloading $asset"
fetch "$base_url/$asset" "$tmp/$asset" || fallback "could not download $asset"
say "verifying sha256 of $asset"
if ! (cd "$tmp" && $sumtool -c checksum.line >/dev/null 2>&1); then
	hard_error "sha256 mismatch for $asset against the verified checksums.txt"
fi

# 7. Extract and place the binary, then make sure it actually runs here.
say "extracting"
if ! tar -xzf "$tmp/$asset" -C "$tmp" herdr-hop || ! [ -f "$tmp/herdr-hop" ]; then
	fallback "could not extract herdr-hop from $asset"
fi
mv "$tmp/herdr-hop" ./herdr-hop
chmod +x ./herdr-hop
if ! ./herdr-hop --version; then
	rm -f ./herdr-hop
	fallback "the installed binary failed to run on this system"
fi
say "installed herdr-hop v$version ($os/$arch)"
