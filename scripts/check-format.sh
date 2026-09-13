#!/bin/sh
set -eu
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  printf 'Go files require gofmt:\n%s\n' "$unformatted"
  exit 1
fi
