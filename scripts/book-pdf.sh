#!/usr/bin/env bash
# Builds the book as a PDF and an HTML archive into dist/.
#
#   scripts/book-pdf.sh [version]
#
# Uses roksbnkctl's book toolchain image (mdbook + mdbook-pandoc + pandoc +
# XeLaTeX), pinned by digest. Needs Docker.
set -euo pipefail

version="${1:-dev}"
image="ghcr.io/jgruberf5/roksbnkctl-tools-mdbook@sha256:9d7f9fd7d39e64b6794e062bba54d56aa4b310f8484a5803567f645d03d156c4"
root="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cp -r "$root/book/src" "$work/src"
cat "$root/book/book.toml" "$root/book/pdf/pandoc.toml" > "$work/book.toml"

docker run --rm -u "$(id -u):$(id -g)" -e HOME=/tmp -v "$work:/book" -w /book "$image" build 2>&1 | tee "$work/build.log"

# A glyph the font lacks is printed as nothing; refuse to ship that.
if grep -q "Missing character" "$work/build.log"; then
	grep "Missing character" "$work/build.log" | sort | uniq -c >&2
	echo "book-pdf: the PDF would be missing characters" >&2
	exit 1
fi

mkdir -p "$root/dist"
cp "$work/book/pandoc/pdf/roksbnkargoctl-book.pdf" "$root/dist/roksbnkargoctl-book-${version}.pdf"
tar -czf "$root/dist/roksbnkargoctl-book-${version}-html.tar.gz" -C "$work/book" --transform "s,^html,roksbnkargoctl-book-${version}," html
ls -la "$root/dist"/roksbnkargoctl-book-"${version}"*
