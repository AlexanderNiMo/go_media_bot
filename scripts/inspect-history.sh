#!/usr/bin/env bash
#
# Show what a recorded history actually contains.
#
# This exists because the obvious way to review a history does not work. The
# Temporal CLI writes payloads as base64-encoded JSON, so grepping the file for
# a title, a path or a tracker name finds nothing — the values are there, just
# not in a form you can read. Reviewing a history before committing it means
# decoding it first, and that is what this does.
#
# Usage:
#
#   ./scripts/inspect-history.sh internal/request/testdata/histories/*.json
#   ./scripts/inspect-history.sh --values-only <file>
#
# What it prints:
#
#   - every payload that crossed the workflow boundary, decoded: the input, the
#     arguments to and results from every activity
#   - a summary of the distinct strings inside them, which is the list to read
#     before deciding whether a history is safe to commit
#
# What matters for replay is the *sequence* of commands the workflow issued, not
# the values. So if a decoded payload names something you would rather not
# publish, the fix is to scrub the value rather than to drop the history — the
# corpus is only useful if it still replays.

set -euo pipefail

if [ $# -eq 0 ]; then
    echo "usage: $0 <history.json> [...]" >&2
    exit 2
fi

exec python3 - "$@" <<'PY'
import base64
import glob
import json
import re
import sys

# Payloads are {"metadata": {...}, "data": "<base64>"}. The walk finds every
# one, wherever it sits in the event structure.
def payloads(node, out):
    if isinstance(node, dict):
        data = node.get("data")
        if isinstance(data, str):
            try:
                out.append(base64.b64decode(data).decode("utf-8", "replace"))
            except Exception:
                pass
        for v in node.values():
            payloads(v, out)
    elif isinstance(node, list):
        for v in node:
            payloads(v, out)


def summary(values):
    """Distinct human-readable strings, with the plumbing filtered out."""
    strings = set()
    for v in values:
        for m in re.findall(r'"([^"]{3,})"', v):
            if re.fullmatch(r"[0-9a-fA-F-]{16,}", m):   # run and activity ids
                continue
            if re.fullmatch(r"[\d.:+-]+", m):          # numbers and timestamps
                continue
            if m.startswith(("go.temporal", "github.com", "time.")):
                continue
            strings.add(m)
    return sorted(strings)


files = []
for arg in sys.argv[1:]:
    files.extend(sorted(glob.glob(arg)) or [arg])

for path in files:
    try:
        doc = json.load(open(path, encoding="utf-8"))
    except Exception as err:
        print(f"{path}: cannot read: {err}", file=sys.stderr)
        continue

    values = []
    payloads(doc, values)

    print(f"\n=== {path} ===")
    print(f"{len(values)} payloads, {len(set(values))} distinct\n")

    for v in dict.fromkeys(values):
        print("  " + (v[:200] + ("…" if len(v) > 200 else "")))

    print("\n  distinct strings — read these before committing:")
    for s in summary(values):
        print(f"    {s}")
PY
