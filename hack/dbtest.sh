#!/bin/sh
# Store contract tests. They need a real PostgreSQL for the same reason the CRD
# tests need a real API server: generated columns, domain constraints, partial
# indexes, FOR UPDATE SKIP LOCKED and the guard trigger are behaviour of the
# database and of nothing else.
set -e
cd /w/test/store && go test -count=1 -race ./...
