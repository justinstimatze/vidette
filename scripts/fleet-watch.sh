#!/usr/bin/env bash
# Run a vidette audit over the fleet and raise a desktop notification if any
# repo is failing. Intended for a scheduler (systemd user timer / cron).
#
# Repo-relative and machine-path-free so it stays safe if this repo goes public.
# State (the latest report) lands in $XDG_STATE_HOME/vidette, not the repo.
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

bin="$repo_dir/vidette"
[ -x "$bin" ] || go build -o "$bin" .

state_dir="${XDG_STATE_HOME:-$HOME/.local/state}/vidette"
mkdir -p "$state_dir"
report="$state_dir/last-audit.md"

"$bin" audit >"$report" 2>&1 || true

# Summary line looks like: **N repos** · ✗ X fail · ⚠ Y warn · ✓ Z ok
fails="$(grep -m1 -oE '✗ [0-9]+ fail' "$report" | grep -oE '[0-9]+' || echo 0)"

if [ "${fails:-0}" -gt 0 ]; then
	# The alarm table rows, stripped of markdown pipes, for the notification body.
	body="$(grep -E '^\| ✗' "$report" | sed -E 's/\|//g; s/^[[:space:]]+//; s/[[:space:]]+/ /g' | head -12)"
	if command -v notify-send >/dev/null 2>&1; then
		notify-send -u critical "vidette: $fails fleet failure(s)" "$body"
	fi
	echo "vidette fleet watch: $fails failing — report at $report"
	exit 1
fi

echo "vidette fleet watch: clean — $(grep -m1 -oE '✗ [0-9]+ fail · ⚠ [0-9]+ warn · ✓ [0-9]+ ok' "$report")"
