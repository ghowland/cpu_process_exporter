#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

go mod tidy
go vet ./...

go build -o ./process_exporter ./cmd/process_exporter

ls -l ./process_exporter

