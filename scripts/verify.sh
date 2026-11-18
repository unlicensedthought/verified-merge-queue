#!/usr/bin/env bash
# Install/build/test gate for this repo. The last stdout line is the verdict.
set -u
cd "$(dirname "$0")/.."
gates=0
fail() { echo "VERIFY {\"verdict\":\"fail\",\"failed\":\"$1\"}"; exit 1; }
step() { # step <name> <cmd...>
  local name=$1; shift
  echo "== $name" >&2
  "$@" >&2 || fail "$name"
  gates=$((gates + 1))
}
command -v go >/dev/null 2>&1 || fail "install"
command -v git >/dev/null 2>&1 || fail "install"
gates=$((gates + 1))
step deps go mod download
step build go build -o /dev/null ./...
step vet go vet ./...
step fmt test -z "$(gofmt -l .)"
step test go test -count=1 ./...
echo "VERIFY {\"verdict\":\"pass\",\"gates\":$gates}"
