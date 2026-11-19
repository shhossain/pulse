#!/bin/sh
# Installs the pulse CLI from a GitHub release, verified against SHA256SUMS.
#
#   curl -fsSL https://raw.githubusercontent.com/shhossain/pulse/main/install.sh | sh
#
# Linux (amd64, arm64) and macOS. Settings, as environment variables:
#   PULSE_VERSION       a release tag such as v0.2.0 (default: the latest release)
#   PULSE_INSTALL_DIR   where to put pulse (default: /usr/local/bin when writable or
#                       running as root, else ~/.local/bin)
#
# It installs one file and changes nothing else: no shell profile edits, no services.
set -eu

REPO="shhossain/pulse"

say() { printf '%s\n' "$*" >&2; }
die() { say "pulse install: $*"; exit 1; }

# fetch <url> <file>
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q --https-only -O "$2" "$1"
	else
		die "needs curl or wget"
	fi
}

# latest_tag: the tag the releases/latest page redirects to.
latest_tag() {
	if command -v curl >/dev/null 2>&1; then
		url=$(curl -fsSLI --proto '=https' --tlsv1.2 -o /dev/null -w '%{url_effective}' "https://github.com/$REPO/releases/latest")
	else
		url=$(wget -q --https-only --max-redirect=5 -S -O /dev/null "https://github.com/$REPO/releases/latest" 2>&1 |
			sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1 | tr -d '\r')
	fi
	tag=${url##*/tag/}
	[ -n "$tag" ] && [ "$tag" != "$url" ] || die "couldn't find the latest release (set PULSE_VERSION=vX.Y.Z)"
	printf '%s' "$tag"
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "needs sha256sum or shasum to verify the download"
	fi
}

main() {
	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported system $(uname -s) (Linux and macOS only)" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported CPU $(uname -m) (amd64 and arm64 only)" ;;
	esac
	[ "$os" = darwin ] && arch=universal

	tag=${PULSE_VERSION:-$(latest_tag)}
	case "$tag" in v*) ;; *) tag="v$tag" ;; esac
	version=${tag#v}
	file="pulse_${version}_${os}_${arch}.tar.gz"
	base="https://github.com/$REPO/releases/download/$tag"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM
	say "Downloading pulse $tag ($os/$arch)…"
	fetch "$base/$file" "$tmp/$file" || die "couldn't download $base/$file"
	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "couldn't download the checksums"

	want=$(awk -v f="$file" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "$file isn't listed in SHA256SUMS"
	got=$(sha256 "$tmp/$file")
	[ "$got" = "$want" ] || die "checksum mismatch for $file (expected $want, got $got); not installing"

	tar -xzf "$tmp/$file" -C "$tmp" pulse
	[ -f "$tmp/pulse" ] || die "the archive has no pulse binary"
	chmod 0755 "$tmp/pulse"

	dir=${PULSE_INSTALL_DIR:-}
	if [ -z "$dir" ]; then
		if [ "$(id -u)" = 0 ] || [ -w /usr/local/bin ]; then
			dir=/usr/local/bin
		else
			dir="$HOME/.local/bin"
		fi
	fi
	mkdir -p "$dir"
	# Replace atomically: a job may be running the old binary right now.
	cp "$tmp/pulse" "$dir/.pulse.new.$$"
	mv -f "$dir/.pulse.new.$$" "$dir/pulse"

	say "Installed $("$dir/pulse" version 2>/dev/null | head -n 1 || echo "pulse $tag") to $dir/pulse"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "Note: $dir isn't on your PATH. Add it, e.g.: export PATH=\"$dir:\$PATH\"" ;;
	esac
}

main "$@"
