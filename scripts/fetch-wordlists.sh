#!/bin/sh
# Downloads the built-in dictionaries (popular open-source word lists) into $1 (default ./wordlists).
# Every list is tried from several mirrors, because any single host can be unreachable from a
# build environment.
set -eu
DEST="${1:-./wordlists}"
mkdir -p "$DEST"

fetch() {
  name="$1"; shift
  for url in "$@"; do
    echo "fetching $name from $url"
    if wget -q -T 30 -O "$DEST/$name.txt" "$url" && [ -s "$DEST/$name.txt" ]; then
      return 0
    fi
    echo "  failed, trying the next mirror" >&2
  done
  echo "could not download $name from any mirror" >&2
  exit 1
}

# https://github.com/first20hours/google-10000-english
fetch google-10000-english \
  "https://raw.githubusercontent.com/first20hours/google-10000-english/master/google-10000-english-no-swears.txt" \
  "https://cdn.jsdelivr.net/gh/first20hours/google-10000-english@master/google-10000-english-no-swears.txt" \
  "https://fastly.jsdelivr.net/gh/first20hours/google-10000-english@master/google-10000-english-no-swears.txt"
# https://github.com/dwyl/english-words
fetch english-words-alpha \
  "https://raw.githubusercontent.com/dwyl/english-words/master/words_alpha.txt" \
  "https://cdn.jsdelivr.net/gh/dwyl/english-words@master/words_alpha.txt" \
  "https://fastly.jsdelivr.net/gh/dwyl/english-words@master/words_alpha.txt"
