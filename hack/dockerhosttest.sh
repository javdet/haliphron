#!/bin/sh
# The Docker host controller's tests: its own unit tests, then the contract
# suite that drives it against FakeBackend and an in-memory Docker daemon.
# Neither needs a daemon, a cluster or a database.
set -e
cd /w/dockerhost
unformatted=$(gofmt -l .)
[ -z "$unformatted" ] || { echo "gofmt: $unformatted"; exit 1; }
go vet ./...
go test -count=1 -race ./...
cd /w/test/dockerhost
unformatted=$(gofmt -l .)
[ -z "$unformatted" ] || { echo "gofmt: $unformatted"; exit 1; }
go vet ./...
go test -count=1 -race ./...
