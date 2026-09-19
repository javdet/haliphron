#!/bin/sh
# The backend's own tests, then the contract tests against FakeController and a
# real PostgreSQL. The second half needs the database for the same reason the
# store tests do: the lease's SKIP LOCKED, the guard trigger and the generated
# rank live there and nowhere else, and the rules under test are the ones that
# only fire when two things happen at once.
set -e
cd /w/backend && go test -count=1 -race ./...
cd /w/test/backend && go test -count=1 -race ./...
