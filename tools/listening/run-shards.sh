#!/usr/bin/env bash
# Run the listenbatch generator across N parallel shards against one corpus
# root. Every shard is an independent process bounded by the same RSS ceiling;
# blind IDs are content-derived so shards never collide, and re-running this
# script after an interruption only fills unfinished items.
#
# Usage: tools/listening/run-shards.sh <corpus-root> <item-count> <shard-count> [seed]
set -euo pipefail

ROOT=${1:?corpus root required}
COUNT=${2:?item count required}
SHARDS=${3:?shard count required}
SEED=${4:-20261112}
RSS_LIMIT=${RSS_LIMIT_MIB:-256}

REPO_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
BIN=${REPO_ROOT}/var/bin/listenbatch

export CGO_ENABLED=0
( cd "$REPO_ROOT" && go build -buildvcs=false -o "$BIN" ./cmd/listenbatch )

"$BIN" init -root "$ROOT" -count "$COUNT" -seed "$SEED" -rss-limit-mib "$RSS_LIMIT"

pids=()
for (( s=0; s<SHARDS; s++ )); do
  "$BIN" generate -root "$ROOT" -count "$COUNT" -seed "$SEED" \
      -shard "$s" -shards "$SHARDS" -rss-limit-mib "$RSS_LIMIT" \
      >"$ROOT/logs/shard-$s.log" 2>&1 &
  pids+=("$!")
done

status=0
for pid in "${pids[@]}"; do
  if ! wait "$pid"; then
    status=1
  fi
done

"$BIN" scan -root "$ROOT" -count "$COUNT" -seed "$SEED"
exit "$status"
