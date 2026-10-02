#!/bin/sh
# Downloads the built-in dictionaries (popular open-source word lists) into $1 (default ./wordlists).
set -eu
DEST="${1:-./wordlists}"
mkdir -p "$DEST"

fetch() {
  name="$1"; url="$2"
  echo "fetching $name"
  wget -q -O "$DEST/$name.txt" "$url"
  [ -s "$DEST/$name.txt" ] || { echo "empty download: $name" >&2; exit 1; }
}

# https://github.com/first20hours/google-10000-english
fetch google-10000-english \
  "https://raw.githubusercontent.com/first20hours/google-10000-english/master/google-10000-english-no-swears.txt"
# https://github.com/dwyl/english-words
fetch english-words-alpha \
  "https://raw.githubusercontent.com/dwyl/english-words/master/words_alpha.txt"
