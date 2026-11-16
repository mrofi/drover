#!/usr/bin/env bash
# Shutdown hook: drover stops its instances on SIGTERM. Give it a moment before the final backup runs.
pkill -TERM -x drover || exit 0
for _ in 1 2 3 4 5 6 7 8; do
  pgrep -x drover >/dev/null || exit 0
  sleep 1
done
