#!/usr/bin/env sh

set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <password>" >&2
  exit 1
fi

password=$1

if command -v sha256sum >/dev/null 2>&1; then
  printf '%s' "$password" | sha256sum | awk '{print $1}'
  exit 0
fi

if command -v shasum >/dev/null 2>&1; then
  printf '%s' "$password" | shasum -a 256 | awk '{print $1}'
  exit 0
fi

if command -v openssl >/dev/null 2>&1; then
  printf '%s' "$password" | openssl dgst -sha256 -r | awk '{print $1}'
  exit 0
fi

echo "no SHA-256 tool found (need sha256sum, shasum, or openssl)" >&2
exit 1
