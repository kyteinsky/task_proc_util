#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Nextcloud contributors
# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Show live Task Processing queue stats while the supervisor's workers drain it.
#
# Breaks tasks down by status and by task type, so you can watch the scheduler's
# fairness (every type with pending work gets a worker) and throughput.

set -euo pipefail

NC_URL="${NC_URL:-http://localhost:8080}"
NC_USER="${NC_USER:-admin}"
NC_PASS="${NC_PASS:-admin}"
APP_ID="${APP_ID:-task_proc_util_bench}"

batch_id=""
watch=0
interval=2
json_out=0
all_apps=0

usage() {
	cat <<'EOF'
Usage: task-stats.sh [options]

Reports Task Processing tasks by status and type.

Options:
  -b, --batch ID      Only tasks tagged with this customId (from schedule-tasks.sh).
  -a, --all           All of the user's tasks, not just those of APP_ID.
  -w, --watch         Refresh until no task is left scheduled or running.
  -i, --interval N    Seconds between refreshes with --watch. Default 2.
  -j, --json          Emit one JSON stats object and exit (for scripting).
  -h, --help          This text.

Environment:
  NC_URL   Base URL         (default http://localhost:8080)
  NC_USER  Username         (default admin)
  NC_PASS  Password         (default password)
  APP_ID   appId to filter  (default task_proc_util_bench)

Examples:
  ./scripts/task-stats.sh --watch
  ./scripts/task-stats.sh --batch batch-20260923-120000-4242 --watch
  ./scripts/task-stats.sh --json | jq .
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
		-b | --batch)
			batch_id="$2"
			shift 2
			;;
		-i | --interval)
			interval="$2"
			shift 2
			;;
		-w | --watch)
			watch=1
			shift
			;;
		-j | --json)
			json_out=1
			shift
			;;
		-a | --all)
			all_apps=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			echo "unknown option: $1" >&2
			usage >&2
			exit 2
			;;
	esac
done

for bin in curl jq; do
	command -v "$bin" >/dev/null || { echo "$bin is required" >&2; exit 1; }
done

# Fetch the batch in a single request rather than polling each task id.
fetch_tasks() {
	local path
	if [ "$all_apps" = 1 ]; then
		path="/tasks?format=json"
	else
		path="/tasks/app/$APP_ID?format=json"
	fi
	if [ -n "$batch_id" ]; then
		path="$path&customId=$batch_id"
	fi

	curl -sS \
		-u "$NC_USER:$NC_PASS" \
		-H "OCS-APIRequest: true" \
		"$NC_URL/ocs/v2.php/taskprocessing$path" \
		| jq '.ocs.data.tasks // []'
}

# Collapse the task list into counts by status, counts by type/status, and
# duration stats over the tasks that actually finished.
stats_of() {
	jq '
		def dur: select(.endedAt and .startedAt and .endedAt > 0 and .startedAt > 0)
			| .endedAt - .startedAt;
		(map(dur) | sort) as $d
		| {
			total: length,
			byStatus: (
				group_by(.status)
				| map({key: .[0].status, value: length})
				| from_entries
			),
			byType: (
				group_by(.type)
				| map({
					type: .[0].type,
					total: length,
					scheduled: (map(select(.status == "STATUS_SCHEDULED")) | length),
					running:   (map(select(.status == "STATUS_RUNNING"))   | length),
					successful:(map(select(.status == "STATUS_SUCCESSFUL"))| length),
					failed:    (map(select(.status == "STATUS_FAILED"))    | length),
					# Cancelled and unknown are terminal too; without them a
					# batch containing either never reaches 100%.
					other: (map(select(
						.status != "STATUS_SCHEDULED" and
						.status != "STATUS_RUNNING" and
						.status != "STATUS_SUCCESSFUL" and
						.status != "STATUS_FAILED"
					)) | length)
				})
				| sort_by(-.total)
			),
			durations: (
				if ($d | length) > 0 then {
					count: ($d | length),
					min: $d[0],
					median: $d[($d | length / 2 | floor)],
					max: $d[-1],
					mean: (($d | add) / ($d | length) * 100 | round / 100)
				} else null end
			),
			span: (
				(map(select(.startedAt and .startedAt > 0) | .startedAt) | min) as $s
				| (map(select(.endedAt and .endedAt > 0) | .endedAt) | max) as $e
				| if $s and $e then ($e - $s) else null end
			),
			# Mean number of tasks in flight over the observed window: total
			# busy time divided by wall clock. Unlike the instantaneous RUN
			# column this does not miss work that started and finished between
			# two polls, so it is the honest measure of achieved parallelism.
			concurrency: (
				(map(dur) | add) as $busy
				| (map(select(.startedAt and .startedAt > 0) | .startedAt) | min) as $s
				| (map(select(.endedAt and .endedAt > 0) | .endedAt) | max) as $e
				| if $busy and $s and $e and ($e - $s) > 0
				  then (($busy / ($e - $s)) * 100 | round) / 100
				  else null end
			)
		}
	'
}

render() {
	local stats="$1"
	local total done_n sched run ok fail other

	total=$(printf '%s' "$stats" | jq -r '.total')
	sched=$(printf '%s' "$stats" | jq -r '.byStatus.STATUS_SCHEDULED // 0')
	run=$(printf '%s' "$stats" | jq -r '.byStatus.STATUS_RUNNING // 0')
	ok=$(printf '%s' "$stats" | jq -r '.byStatus.STATUS_SUCCESSFUL // 0')
	fail=$(printf '%s' "$stats" | jq -r '.byStatus.STATUS_FAILED // 0')
	other=$(printf '%s' "$stats" | jq -r '[.byType[].other] | add // 0')
	# Anything not scheduled or running has reached a terminal state.
	done_n=$((total - sched - run))

	if [ "$total" -eq 0 ]; then
		echo "No tasks found for appId=$APP_ID${batch_id:+ batch=$batch_id}."
		echo "Schedule some with ./scripts/schedule-tasks.sh"
		return
	fi

	printf 'Tasks: %d total   scheduled %d   running %d   successful %d   failed %d' \
		"$total" "$sched" "$run" "$ok" "$fail"
	[ "$other" -eq 0 ] && printf '\n' || printf '   other %d\n' "$other"

	# Progress bar over completed tasks.
	local width=40 filled
	filled=$((done_n * width / total))
	printf '['
	printf '%*s' "$filled" '' | tr ' ' '#'
	printf '%*s' $((width - filled)) '' | tr ' ' '.'
	printf '] %d/%d (%d%%)\n\n' "$done_n" "$total" $((done_n * 100 / total))

	printf '%-32s %6s %6s %6s %6s %6s\n' "TASK TYPE" "TOTAL" "SCHED" "RUN" "OK" "FAIL"
	printf '%s\n' "----------------------------------------------------------------------"
	printf '%s' "$stats" | jq -r '
		.byType[]
		| [.type, .total, .scheduled, .running, .successful, .failed]
		| @tsv
	' | while IFS=$'\t' read -r type tot sc rn okc fc; do
		printf '%-32s %6s %6s %6s %6s %6s\n' "$type" "$tot" "$sc" "$rn" "$okc" "$fc"
	done
	printf '%s\n' "----------------------------------------------------------------------"
	printf '%-32s %6s %6s %6s %6s %6s\n' "TOTAL" "$total" "$sched" "$run" "$ok" "$fail"

	local have_dur
	have_dur=$(printf '%s' "$stats" | jq -r '.durations != null')
	if [ "$have_dur" = "true" ]; then
		echo
		printf '%s' "$stats" | jq -r '
			"Duration (s) over \(.durations.count) finished: " +
			"min \(.durations.min)  median \(.durations.median)  " +
			"mean \(.durations.mean)  max \(.durations.max)"
		'
		printf '%s' "$stats" | jq -r '
			if .span and .span > 0 and .durations.count > 0
			then "Wall clock: \(.span)s for \(.durations.count) task(s) " +
			     "→ \((.durations.count / .span * 100 | round) / 100) task/s"
			else empty end
		'
		# RUN above is a single instant and routinely undercounts: a task that
		# starts and finishes between two polls is never seen as running.
		printf '%s' "$stats" | jq -r '
			if .concurrency
			then "Achieved concurrency: \(.concurrency) task(s) in flight on average " +
			     "(RUN column is a point sample and undercounts)"
			else empty end
		'
	fi
}

if [ "$json_out" = 1 ]; then
	fetch_tasks | stats_of
	exit 0
fi

if [ "$watch" = 0 ]; then
	render "$(fetch_tasks | stats_of)"
	exit 0
fi

start=$(date +%s)
while :; do
	stats=$(fetch_tasks | stats_of)
	now=$(date +%s)

	printf '\033[H\033[2J'
	echo "Task Processing stats — $NC_URL"
	echo "appId=$APP_ID${batch_id:+  batch=$batch_id}  elapsed=$((now - start))s"
	echo
	render "$stats"

	pending=$(printf '%s' "$stats" | jq -r '
		(.byStatus.STATUS_SCHEDULED // 0) + (.byStatus.STATUS_RUNNING // 0)
	')
	total=$(printf '%s' "$stats" | jq -r '.total')

	if [ "$total" -gt 0 ] && [ "$pending" -eq 0 ]; then
		echo
		echo "Queue drained in $((now - start))s."
		exit 0
	fi

	sleep "$interval"
done
