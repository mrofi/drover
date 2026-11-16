#!/usr/bin/env bash
# Boot hook: start drover in the background (a hook that does not return blocks the boot until its timeout).
drover run -c "$HOME/drover.yaml" &
