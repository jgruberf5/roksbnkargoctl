#!/bin/sh
# roksbnkargoctl installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/jgruberf5/roksbnkargoctl/main/install.sh | sh
#
# Downloads the release archive for this OS/arch and BNK version, verifies its
# SHA256 against the release's checksums file, extracts the binary, and hands off
# to the binary's own `roksbnkargoctl self install --force`, which copies it onto
# PATH. The temp directory holding the archive and the extracted binary is
# removed on exit, leaving only the installed copy.
#
# Each binary installs one BNK release, and the archives are named for it:
#   roksbnkargoctl_<version>_bnk-<BNK version>_<os>_<arch>.tar.gz
#
# Options (environment):
#   VERSION=vX.Y.Z                install that release (or pass it as the first
#                                 argument: `sh -s -- v0.5.0`); default: latest
#   BNK_VERSION=2.4.0             the BNK release the binary installs (default 2.4.0)
#   ROKSBNKARGOCTL_INSTALL_ARGS   passed to `self install` (e.g. "--dir $HOME/bin")
#   GITHUB_TOKEN                  authenticates the GitHub API call (rate limit)
#
# The checksum is mandatory: a release without a checksums file, or one that does
# not list the archive, is refused. Every roksbnkargoctl release is cut by
# goreleaser, which always publishes one, so its absence means something is wrong.
set -eu

REPO="jgruberf5/roksbnkargoctl"
BIN="roksbnkargoctl"
BNK="${BNK_VERSION:-2.4.0}"
INSTALL_ARGS="${ROKSBNKARGOCTL_INSTALL_ARGS:-}"
# The GitHub API base. Overridable only so the installer can be tested against a
# local server; the download URLs come from the API's answer.
API="${ROKSBNKARGOCTL_GITHUB_API:-https://api.github.com}"

die() { echo "$BIN: $*" >&2; exit 1; }

# ---- OS / arch (goreleaser naming) -----------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  *) die "unsupported OS '$os' (Linux/macOS only; on Windows use install.ps1)" ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) die "unsupported architecture '$arch'" ;;
esac
case "$BNK" in
  *[!0-9.]*|'') die "BNK_VERSION '$BNK' is not a version like 2.4.0" ;;
esac

# ---- temp dir, removed on exit ----------------------------------------------
tmp=$(mktemp -d 2>/dev/null || mktemp -d -t "$BIN")
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM

# ---- resolve the release ----------------------------------------------------
ver="${1:-${VERSION:-}}"
if [ -n "$ver" ]; then
  case "$ver" in v*) ;; *) ver="v$ver" ;; esac
  url="$API/repos/$REPO/releases/tags/$ver"
else
  url="$API/repos/$REPO/releases/latest"
fi
set -- -fsSL -H "Accept: application/vnd.github+json"
if [ -n "${GITHUB_TOKEN:-}" ]; then
  set -- "$@" -H "Authorization: Bearer $GITHUB_TOKEN"
fi
curl "$@" "$url" -o "$tmp/release.json" \
  || die "could not read ${ver:-the latest release} of $REPO from the GitHub API ($url)"

# The release JSON, one key per line: splitting on , { and [ puts every key at
# the start of a line whatever the key order or formatting, while a key quoted
# inside a string (the release notes) is escaped as \" and cannot match.
# shellcheck disable=SC2020 # three characters, each to a newline
tr ',{[' '\n\n\n' < "$tmp/release.json" > "$tmp/fields"
tag=$(sed -n 's/^[[:space:]]*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*$/\1/p' "$tmp/fields" | head -1)
[ -n "$tag" ] || die "could not resolve a release version"
verNoV="${tag#v}"
sed -n 's/^[[:space:]]*"browser_download_url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*$/\1/p' "$tmp/fields" > "$tmp/urls"

asset="${BIN}_${verNoV}_bnk-${BNK}_${os}_${arch}.tar.gz"
sums="${BIN}_${verNoV}_checksums.txt"
asset_url=""
sums_url=""
while IFS= read -r u; do
  case "$u" in
    */"$asset") asset_url=$u ;;
    */"$sums") sums_url=$u ;;
  esac
done < "$tmp/urls"

if [ -z "$asset_url" ]; then
  have=$(sed -n "s|.*/${BIN}_[^_/]*_bnk-\([^_/]*\)_[^_/]*_[^_/]*\.[a-z.]*\$|\1|p" "$tmp/urls" | sort -u | tr '\n' ' ')
  if [ -n "$have" ]; then
    echo "$BIN: release $tag has no $asset" >&2
    echo "  it has archives for BNK: $have" >&2
  else
    echo "$BIN: release $tag has no $asset, and no BNK archives at all" >&2
  fi
  echo "  set BNK_VERSION to one of those, or VERSION to a release that has BNK $BNK" >&2
  exit 1
fi
[ -n "$sums_url" ] || die "release $tag has no $sums; refusing to install without checksum verification"

# ---- download + verify --------------------------------------------------------
echo "Downloading $BIN $tag for BNK $BNK ($os/$arch)..."
curl -fsSL "$asset_url" -o "$tmp/$asset" || die "downloading $asset_url failed"
curl -fsSL "$sums_url" -o "$tmp/$sums" || die "downloading $sums_url failed"

want=$(awk -v a="$asset" '$2 == a { print $1; exit }' "$tmp/$sums")
[ -n "$want" ] || die "$sums does not list $asset; refusing to install an unverified archive"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
else
  die "neither sha256sum nor shasum is available to verify $asset"
fi
want=$(echo "$want" | tr '[:upper:]' '[:lower:]')
[ "$want" = "$got" ] || die "checksum mismatch for $asset (want $want, got $got)"
echo "Checksum OK."

# ---- extract + hand off to the binary's own installer ------------------------
mkdir "$tmp/x"
tar -xzf "$tmp/$asset" -C "$tmp/x" "$BIN" || die "extracting $BIN from $asset failed"
[ -x "$tmp/x/$BIN" ] || die "$BIN not found in $asset"

echo "Installing via '$BIN self install'..."
# shellcheck disable=SC2086 # INSTALL_ARGS is split into arguments on purpose
"$tmp/x/$BIN" self install --force $INSTALL_ARGS

echo "Done. Run '$BIN version' to confirm."
