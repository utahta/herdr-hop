#!/bin/sh
# install_test.sh: integration tests for install.sh. Run from the repository
# root: sh scripts/install_test.sh
#
# Builds a fixture plugin root (a minimal Go module so the `go build`
# fallback is fast), serves release assets from a local HTTP server via the
# HOP_INSTALL_BASE_URL test hook, and checks every row of the failure table
# in the design: environment problems fall back to a source build, while an
# invalid signature or a checksum mismatch aborts without falling back.
# Assets are signed with a throwaway test key; the fixture's
# release/signing-key.pub holds the test public key, never the production
# one.
set -eu

repo=$(pwd)
[ -f "$repo/install.sh" ] || {
	echo "run from the repository root" >&2
	exit 2
}

work=$(mktemp -d)
server_pid=""
cleanup() {
	[ -n "$server_pid" ] && kill "$server_pid" 2>/dev/null
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

pass=0
fail=0
ok() {
	echo "ok: $1"
	pass=$((pass + 1))
}
bad() {
	echo "FAIL: $1" >&2
	fail=$((fail + 1))
}

# --- fixture plugin root ---------------------------------------------------
version="9.9.9"
fixture="$work/fixture"
mkdir -p "$fixture/release"
cp "$repo/install.sh" "$fixture/"
cp "$repo/release/probe.txt" "$repo/release/probe.sig" "$repo/release/probe-key.pub" "$fixture/release/"
cat >"$fixture/herdr-plugin.toml" <<EOF
id = "utahta.hop"
version = "$version"
EOF
cat >"$fixture/go.mod" <<EOF
module hopfixture

go 1.21
EOF
cat >"$fixture/main.go" <<'EOF'
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("herdr-hop fixture")
	}
}
EOF

# Test signing key; its public half goes into the fixture plugin root.
ssh-keygen -t ed25519 -N '' -C test -f "$work/test_key" -q
printf 'herdr-hop-release %s\n' "$(cut -d' ' -f1,2 <"$work/test_key.pub")" >"$fixture/release/signing-key.pub"

# --- release assets --------------------------------------------------------
os=$(uname -s | tr 'A-Z' 'a-z')
arch=$(uname -m)
case "$arch" in arm64 | aarch64) arch=arm64 ;; x86_64) arch=amd64 ;; esac
asset="herdr-hop_v${version}_${os}_${arch}.tar.gz"

make_release() { # make_release <dir> <binary-source-dir>
	dir=$1
	mkdir -p "$dir"
	(cd "$2" && go build -o "$dir/herdr-hop" .)
	tar -czf "$dir/$asset" -C "$dir" herdr-hop
	rm "$dir/herdr-hop"
	(cd "$dir" && if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$asset" >checksums.txt
	else
		shasum -a 256 "$asset" >checksums.txt
	fi)
	(cd "$dir" && ssh-keygen -Y sign -f "$work/test_key" -n herdr-hop-release checksums.txt 2>/dev/null)
}

serve="$work/serve"
mkdir -p "$serve"

# good: a working release
make_release "$serve/good" "$fixture"

# badsum: the archive is tampered with after checksums.txt was signed
make_release "$serve/badsum" "$fixture"
printf 'x' >>"$serve/badsum/$asset"

# badsig: checksums.txt is modified after signing
make_release "$serve/badsig" "$fixture"
printf '# tampered\n' >>"$serve/badsig/checksums.txt"

# badbin: a correctly signed release whose binary fails --version
badbin_src="$work/badbin_src"
mkdir -p "$badbin_src"
cp "$fixture/go.mod" "$badbin_src/"
cat >"$badbin_src/main.go" <<'EOF'
package main

import "os"

func main() { os.Exit(1) }
EOF
make_release "$serve/badbin" "$badbin_src"

# empty: no assets at all
mkdir -p "$serve/empty"

# --- HTTP server (test hook only; production path is HTTPS-only) -----------
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')
(cd "$serve" && exec python3 -m http.server "$port" --bind 127.0.0.1 >/dev/null 2>&1) &
server_pid=$!
i=0
until curl -fs -o /dev/null "http://127.0.0.1:$port/good/checksums.txt"; do
	i=$((i + 1))
	[ "$i" -lt 50 ] || {
		echo "server did not start" >&2
		exit 1
	}
	sleep 0.1
done

# --- helpers ---------------------------------------------------------------
new_root() { # fresh copy of the fixture plugin root
	root="$work/root.$1"
	rm -rf "$root"
	cp -R "$fixture" "$root"
}

run_install() { # run_install <case> [extra env as k=v ...]; sets status/out
	case_dir=$1
	shift
	out_file="$work/out"
	status=0
	(cd "$root" && env HOP_INSTALL_BASE_URL="http://127.0.0.1:$port/$case_dir" "$@" sh install.sh) \
		>"$out_file" 2>&1 || status=$?
	out=$(cat "$out_file")
}

has() { printf '%s\n' "$out" | grep -q "$1"; }

# A PATH with only the listed tools, for simulating missing commands.
make_path() { # make_path <dir> <tool...>
	pdir=$1
	shift
	mkdir -p "$pdir"
	for t in "$@"; do
		p=$(command -v "$t" 2>/dev/null) || continue
		ln -s "$p" "$pdir/$t" 2>/dev/null || true
	done
}
base_tools="sh dash uname sed head grep awk curl tar gzip cat mktemp mv chmod rm env dirname basename go"

# --- cases -----------------------------------------------------------------

new_root success
run_install good
if [ "$status" -eq 0 ] && has "installed herdr-hop v$version" && [ -x "$root/herdr-hop" ] &&
	! has "built herdr-hop from source"; then
	ok "success: binary installed"
else
	bad "success: status=$status out: $out"
fi

new_root badsum
run_install badsum
if [ "$status" -ne 0 ] && has "sha256 mismatch" && ! has "built herdr-hop from source" &&
	! [ -e "$root/herdr-hop" ]; then
	ok "checksum mismatch: hard error, no fallback"
else
	bad "badsum: status=$status out: $out"
fi

new_root badsig
run_install badsig
if [ "$status" -ne 0 ] && has "signature verification of checksums.txt failed" &&
	! has "built herdr-hop from source" && ! [ -e "$root/herdr-hop" ]; then
	ok "invalid signature: hard error, no fallback"
else
	bad "badsig: status=$status out: $out"
fi

# ssh-keygen exists but cannot verify (old OpenSSH without -Y): the probe
# must catch it and fall back, before any download is attempted.
new_root oldssh
shim="$work/shim-ssh"
mkdir -p "$shim"
printf '#!/bin/sh\nexit 1\n' >"$shim/ssh-keygen"
chmod +x "$shim/ssh-keygen"
run_install good PATH="$shim:$PATH"
if [ "$status" -eq 0 ] && has "ssh-keygen cannot verify" && has "built herdr-hop from source" &&
	[ -x "$root/herdr-hop" ]; then
	ok "unusable ssh-keygen: probe detects it, source fallback"
else
	bad "oldssh: status=$status out: $out"
fi

# no shasum/sha256sum anywhere on PATH: fall back.
new_root nosum
sumpath="$work/path-nosum"
make_path "$sumpath" $base_tools ssh-keygen
run_install good PATH="$sumpath" CGO_ENABLED=0
if [ "$status" -eq 0 ] && has "neither shasum nor sha256sum" && has "built herdr-hop from source"; then
	ok "missing checksum tool: source fallback"
else
	bad "nosum: status=$status out: $out"
fi

new_root noassets
run_install empty
if [ "$status" -eq 0 ] && has "could not download checksums.txt" && has "built herdr-hop from source"; then
	ok "missing release assets: source fallback"
else
	bad "noassets: status=$status out: $out"
fi

# unsupported platform: uname says SunOS.
new_root sunos
shim="$work/shim-uname"
mkdir -p "$shim"
printf '#!/bin/sh\necho SunOS\n' >"$shim/uname"
chmod +x "$shim/uname"
run_install good PATH="$shim:$PATH"
if [ "$status" -eq 0 ] && has "unsupported OS" && has "built herdr-hop from source"; then
	ok "unsupported platform: source fallback"
else
	bad "sunos: status=$status out: $out"
fi

# version with characters outside [A-Za-z0-9.]: never used in a URL.
new_root badver
sed 's/^version = .*/version = "9.9.9$(rm -rf x)"/' "$fixture/herdr-plugin.toml" >"$root/herdr-plugin.toml"
run_install good
if [ "$status" -eq 0 ] && has "cannot determine a usable version" && has "built herdr-hop from source"; then
	ok "unusable manifest version: source fallback"
else
	bad "badver: status=$status out: $out"
fi

# a verified, correctly signed binary that does not run: rebuild from source.
new_root badbin
run_install badbin
if [ "$status" -eq 0 ] && has "failed to run" && has "built herdr-hop from source" &&
	[ -x "$root/herdr-hop" ] && "$root/herdr-hop" --version >/dev/null; then
	ok "broken binary: --version check catches it, source fallback"
else
	bad "badbin: status=$status out: $out"
fi

# interrupted install (SIGINT to the process group, as Ctrl+C sends it): the
# installer must stop instead of treating the killed download as a failure
# and continuing into the source-build fallback, and its EXIT trap must
# still remove the temporary directory.
new_root interrupt
shim="$work/shim-slowcurl"
mkdir -p "$shim"
# The shim marks that the installer reached the download (so its traps are
# set and it is inside fetch) before blocking; the driver waits for the
# marker instead of a fixed delay, so a slow start on a loaded CI machine
# cannot make SIGINT arrive before the trap exists (which would kill the
# shell with the default action and exit 254 instead of 130).
printf '#!/bin/sh\ntouch "$INTERRUPT_READY"\nexec sleep 30\n' >"$shim/curl"
chmod +x "$shim/curl"
int_tmp="$work/tmpdir-interrupt"
mkdir -p "$int_tmp"
cat >"$work/interrupt.py" <<'EOF'
import os, signal, subprocess, sys, time

ready = os.environ["INTERRUPT_READY"]
p = subprocess.Popen(["sh", "install.sh"], preexec_fn=os.setpgrp,
                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
deadline = time.time() + 30
while not os.path.exists(ready):
    if p.poll() is not None:  # exited before ever reaching the download
        out, _ = p.communicate()
        sys.stdout.buffer.write(out)
        sys.exit(98)
    if time.time() > deadline:
        os.killpg(p.pid, signal.SIGKILL)
        out, _ = p.communicate()
        sys.stdout.buffer.write(out)
        sys.exit(97)
    time.sleep(0.05)
os.killpg(p.pid, signal.SIGINT)
try:
    out, _ = p.communicate(timeout=25)
except subprocess.TimeoutExpired:
    os.killpg(p.pid, signal.SIGKILL)
    out, _ = p.communicate()
    sys.stdout.buffer.write(out)
    sys.exit(99)
sys.stdout.buffer.write(out)
sys.exit(p.returncode)
EOF
status=0
(cd "$root" && env HOP_INSTALL_BASE_URL="http://127.0.0.1:$port/good" \
	PATH="$shim:$PATH" TMPDIR="$int_tmp" INTERRUPT_READY="$work/interrupt.ready" \
	python3 "$work/interrupt.py") \
	>"$work/out" 2>&1 || status=$?
out=$(cat "$work/out")
if [ "$status" -eq 130 ] && ! has "built herdr-hop from source" &&
	! [ -e "$root/herdr-hop" ] && [ -z "$(ls -A "$int_tmp")" ]; then
	ok "interrupt: SIGINT stops the installer, no fallback, tmp removed"
else
	bad "interrupt: status=$status leftover-tmp='$(ls -A "$int_tmp" 2>/dev/null)' out: $out"
fi

echo
echo "install_test.sh: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
