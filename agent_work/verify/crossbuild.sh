#!/usr/bin/env bash
set -u
root=$(git rev-parse --show-toplevel) || exit 2
cd "$root" || exit 2
fail=0
step() {
  echo "== $*"
  if "$@"; then
    echo "   ok"
  else
    echo "   FAILED"
    fail=1
  fi
}
step env GOOS=linux GOARCH=amd64 go build ./...
step env GOOS=linux GOARCH=amd64 go vet ./...
step go test ./...
for arch in arm64 amd64; do
  step env GOOS=darwin GOARCH=$arch go build ./...
  step env GOOS=darwin GOARCH=$arch go vet ./...
  step env GOOS=darwin GOARCH=$arch go test -count=1 -run '^$' -exec /bin/true ./...
done
step go run ./cmd/course check gofmt
if [ $fail -ne 0 ]; then
  echo "CROSSBUILD FAILED"
  exit 1
fi
echo "CROSSBUILD OK"
