#!/bin/sh
# Creates the Temporal namespace the application expects.
#
# Why a fresh alpine image with the official installer rather than
# temporalio/admin-tools:1.32.1: admin-tools' bundled CLI hits a gRPC
# deadline against the matching server image pair (TCP works, the gRPC
# call never completes). The CLI binary fetched by
# https://temporal.download/cli.sh works. Both call the same wire protocol;
# whatever the regression is in admin-tools:1.32.1, it is not on this
# container's side.
set -eu

curl -sSf https://temporal.download/cli.sh | sh

# The installer drops the binary in ~/.temporalio/bin and prints the PATH
# line; sourcing it is more reliable than hard-coding the path in case
# upstream changes it again.
. "${HOME}/.temporalio/pathrc" \
  || . "${HOME}/.temporalio/bin/pathrc" \
  || true

# The Temporal frontend takes a moment after the healthcheck flips from
# "TCP port open" to "actually serving gRPC". Poll until it answers, then
# create the namespace if it does not already exist.
until temporal operator cluster health --address "$TEMPORAL_ADDRESS" >/dev/null 2>&1; do
  sleep 2
done

if temporal operator namespace describe -n "$DEFAULT_NAMESPACE" \
     --address "$TEMPORAL_ADDRESS" >/dev/null 2>&1; then
  echo "namespace $DEFAULT_NAMESPACE already exists"
else
  # Retention applies to CLOSED executions only. Workflows stay open for
  # up to two years, so it does not touch them; ContinueAsNew is what
  # keeps their history bounded, not this.
  temporal operator namespace create -n "$DEFAULT_NAMESPACE" \
    --retention 7d --address "$TEMPORAL_ADDRESS"
  echo "namespace $DEFAULT_NAMESPACE created"
fi