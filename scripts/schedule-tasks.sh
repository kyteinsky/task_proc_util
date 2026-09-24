#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Nextcloud contributors
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Schedule a batch of Task Processing tasks across several task types in one go,
# to give the supervisor something to scale against.
#
# Every task in a run is tagged with the same appId and customId, so
# task-stats.sh can follow exactly this batch and ignore unrelated tasks.

set -euo pipefail

NC_URL="${NC_URL:-http://localhost:8080}"
NC_USER="${NC_USER:-admin}"
NC_PASS="${NC_PASS:-admin}"
APP_ID="${APP_ID:-task_proc_util_bench}"

# type:count pairs used when none are given on the command line.
DEFAULT_SPEC=(
	"core:text2text=6"
	"core:text2text:summary=3"
	"core:text2text:chat=9"
	"core:text2text:topics=2"
)

batch_id=""
delay=0
dry_run=0
declare -a spec=()

usage() {
	cat <<'EOF'
Usage: schedule-tasks.sh [options] [TYPE=COUNT ...]

Schedules tasks across task types and prints the batch id to follow.

Options:
  -b, --batch ID     Batch id (customId) to tag tasks with. Default: generated.
  -d, --delay SECS   Sleep between requests. Use to stay under the OCS rate
                     limit of 20 scheduled tasks per 120s per user.
  -l, --list         List the task types this server offers, then exit.
  -n, --dry-run      Print what would be scheduled without calling the API.
  -h, --help         This text.

Environment:
  NC_URL   Base URL         (default http://localhost:8080)
  NC_USER  Username         (default admin)
  NC_PASS  Password         (default admin)
  APP_ID   appId to tag as  (default task_proc_util_bench)

Examples:
  # Default mix: 20 tasks over 4 types
  ./scripts/schedule-tasks.sh

  # Custom mix
  ./scripts/schedule-tasks.sh core:text2text=50 core:text2text:chat=10

  # Large batch, paced under the rate limit
  ./scripts/schedule-tasks.sh --delay 6 core:text2text=100
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
		-b | --batch)
			batch_id="$2"
			shift 2
			;;
		-d | --delay)
			delay="$2"
			shift 2
			;;
		-n | --dry-run)
			dry_run=1
			shift
			;;
		-l | --list)
			list_types=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		-*)
			echo "unknown option: $1" >&2
			usage >&2
			exit 2
			;;
		*)
			spec+=("$1")
			shift
			;;
	esac
done

for bin in curl jq; do
	command -v "$bin" >/dev/null || { echo "$bin is required" >&2; exit 1; }
done

api() {
	local method="$1" path="$2"
	shift 2
	curl -sS -X "$method" \
		-u "$NC_USER:$NC_PASS" \
		-H "OCS-APIRequest: true" \
		-H "Content-Type: application/json" \
		"$NC_URL/ocs/v2.php/taskprocessing$path" "$@"
}

if [ "${list_types:-0}" = 1 ]; then
	api GET "/tasktypes?format=json" | jq -r '.ocs.data.types | keys[]'
	exit 0
fi

[ ${#spec[@]} -gt 0 ] || spec=("${DEFAULT_SPEC[@]}")
[ -n "$batch_id" ] || batch_id="batch-$(date +%Y%m%d-%H%M%S)-$$"

# Input payload per task type. Task types not listed here fall back to the
# single-field {"input": ...} shape that the core text2text types use.
payload_for() {
	case "$1" in
		core:text2text:chat)
			printf '{"system_prompt":"You are a helpful assistant.","input":%s,"history":[]}' "$2"
			;;
		core:text2text:translate)
			printf '{"input":%s,"origin_language":"en","target_language":"de"}' "$2"
			;;
		*)
			printf '{"input":%s}' "$2"
			;;
	esac
}

total=0
for entry in "${spec[@]}"; do
	case "$entry" in
		*=*) ;;
		*)
			echo "expected TYPE=COUNT, got: $entry" >&2
			exit 2
			;;
	esac
	count="${entry##*=}"
	case "$count" in
		'' | *[!0-9]*)
			echo "count must be a number in: $entry" >&2
			exit 2
			;;
	esac
	total=$((total + count))
done

echo "Batch:  $batch_id"
echo "App id: $APP_ID"
echo "Target: $total task(s) across ${#spec[@]} type(s)"
echo

if [ "$dry_run" = 1 ]; then
	for entry in "${spec[@]}"; do
		echo "  would schedule ${entry##*=} x ${entry%=*}"
	done
	exit 0
fi

scheduled=0
failed=0

for entry in "${spec[@]}"; do
	type="${entry%=*}"
	count="${entry##*=}"

	for i in $(seq 1 "$count"); do
		text=$(jq -Rn --arg t "$type" --arg n "$i" '"benchmark task \($n) for \($t)"')
		body=$(jq -cn \
			--argjson input "$(payload_for "$type" "$text")" \
			--arg type "$type" \
			--arg appId "$APP_ID" \
			--arg customId "$batch_id" \
			'{input: $input, type: $type, appId: $appId, customId: $customId}')

		# The schedule endpoint is rate limited to 20 requests / 120s per user.
		# Back off and retry rather than silently dropping tasks from the batch.
		attempt=0
		while :; do
			response=$(api POST "/schedule?format=json" --data-raw "$body" -w '\n%{http_code}')
			code="${response##*$'\n'}"
			payload="${response%$'\n'*}"

			if [ "$code" = "429" ] && [ "$attempt" -lt 10 ]; then
				attempt=$((attempt + 1))
				echo "  rate limited, waiting 30s (retry $attempt/10)..." >&2
				sleep 30
				continue
			fi
			break
		done

		id=$(printf '%s' "$payload" | jq -r '.ocs.data.task.id // empty' 2>/dev/null || true)
		if [ -n "$id" ]; then
			scheduled=$((scheduled + 1))
			printf '  + %-28s id=%s\n' "$type" "$id"
		else
			failed=$((failed + 1))
			msg=$(printf '%s' "$payload" | jq -r '.ocs.data.message // .ocs.meta.message // "unknown error"' 2>/dev/null || echo "unparseable response")
			printf '  ! %-28s HTTP %s: %s\n' "$type" "$code" "$msg" >&2
		fi

		[ "$delay" = 0 ] || sleep "$delay"
	done
done

echo
echo "Scheduled $scheduled/$total task(s)${failed:+, $failed failed}"
echo
echo "Follow with:"
echo "  ./scripts/task-stats.sh --batch $batch_id --watch"
