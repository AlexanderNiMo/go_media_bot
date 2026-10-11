#!/usr/bin/env bash
#
# Capture workflow histories into the replay corpus.
#
# A recorded history is the only thing that can answer "does this code still do
# what it did for an execution that already happened". The test environment
# cannot: it runs a workflow fresh and never replays anything, so it passes
# happily on code that cannot replay.
#
# Usage:
#
#   ./scripts/capture-histories.sh <workflow-id> [<workflow-id> ...]
#
#   # the watcher and a request that reached a terminal state
#   ./scripts/capture-histories.sh download-watcher movie-603
#
# The histories are written to internal/request/testdata/histories/, which is
# committed. Keep the corpus small: one history per decision point that has
# ever existed, not a large sample nothing reads.
#
# Before committing anything captured from a real deployment, read it. A
# history contains every value that crossed the workflow boundary, including
# the arguments to and results from every activity — and those include the
# media titles someone asked for, the indexer and torrent names, and the paths
# the downloads landed in.
#
#   grep -o '"[^"]*"' internal/request/testdata/histories/*.json | sort -u | less
#
# What matters for replay is the *sequence* of commands the workflow issued, not
# the payloads, so a history scrubbed of private values still does its job.

set -euo pipefail

readonly namespace="${TEMPORAL_NAMESPACE:-mediad}"
readonly outdir="internal/request/testdata/histories"
readonly compose_service="${COMPOSE_SERVICE:-temporal-namespace-setup}"

if [ $# -eq 0 ]; then
    echo "usage: $0 <workflow-id> [<workflow-id> ...]" >&2
    echo >&2
    echo "Find candidates with:" >&2
    echo "  temporal workflow list --namespace ${namespace} --query 'ExecutionStatus=\"Completed\"'" >&2
    exit 2
fi

if [ ! -f docker-compose.yml ]; then
    echo "run this from the repository root" >&2
    exit 1
fi

mkdir -p "$outdir"

# The Temporal CLI lives in the admin-tools image, which is the same one the
# namespace setup uses. Reusing it avoids requiring a local install.
run_temporal() {
    docker compose run --rm --no-deps \
        --entrypoint temporal \
        -e TEMPORAL_ADDRESS=temporal:7233 \
        "$compose_service" "$@"
}

for id in "$@"; do
    out="${outdir}/${id}.json"

    echo "capturing ${id} ..."
    if ! run_temporal workflow show \
        --namespace "$namespace" \
        --workflow-id "$id" \
        --output json > "$out"; then
        echo "  failed; removing the partial file" >&2
        rm -f "$out"
        exit 1
    fi

    size=$(wc -c < "$out" | tr -d ' ')
    if [ "$size" -lt 200 ]; then
        echo "  suspiciously small (${size} bytes); is the workflow id right?" >&2
        rm -f "$out"
        exit 1
    fi

    echo "  wrote ${out} (${size} bytes)"
done

echo
echo "captured. Now:"
echo "  1. read the files before committing them — they contain real values"
echo "  2. go test ./internal/request/ -run TestReplay -v"
