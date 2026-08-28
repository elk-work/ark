#!/usr/bin/env bash
#
# Ark recovery drill — restore a repository from its stored copy.
#
# Exercises the recovery path documented in docs/self-hosting.md ("Restore
# from a stored copy"): a repository's `repos/<id>.db` is lost, corrupted, or
# rolled back, and the service is stood back up from the object alone.
#
# The drill is the artifact. It is meant to be re-run — before a storage
# change, after a server change that touches internal/server/repodb, or on a
# schedule — and to fail loudly rather than to be read.
#
#   scripts/restore-drill.sh --mode local
#   scripts/restore-drill.sh --mode gcs --bucket <scratch-bucket>
#
# `--mode local` rehearses every step against a DATA_DIR instance and needs
# nothing but a Go toolchain. It validates the script; it does not drill GCS,
# where the object layout, the generation compare-and-swap and the soft-delete
# window actually live.
#
# `--mode gcs` is the drill. It needs Application Default Credentials
# (`gcloud auth application-default login`) because the Go GCS client reads
# them, and gcloud for the operator-side object surgery a restore is made of.
#
# NEVER point --bucket at a production bucket. The drill deletes, truncates
# and overwrites `repos/<id>.db` on purpose.
#
#   --mode local|gcs   required
#   --bucket NAME      scratch bucket; required for --mode gcs
#   --project ID       GCP project, if not your gcloud default
#   --work DIR         where to put the log, snapshots and clients (default: mktemp)
#   --port N           port for the drill's ark-server (default 8099)
#   --ark PATH         use this ark binary instead of building one
#   --ark-server PATH  use this ark-server binary instead of building one
#   --keep             leave the drill's objects in the bucket after a pass
#
# Exit 0 = every assertion held. Exit 1 = an assertion failed; read the
# FAIL lines and the drill log. Exit 2 = the drill could not run.

set -euo pipefail

MODE=""
BUCKET=""
PROJECT="${CLOUDSDK_CORE_PROJECT:-}"
WORK=""
KEEP=0
PORT="${ARK_DRILL_PORT:-8099}"
ARK_BIN=""
SERVER_BIN=""

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
	sed -n '2,38p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
	exit 2
}

while [ $# -gt 0 ]; do
	case "$1" in
	--mode) MODE="$2"; shift 2 ;;
	--bucket) BUCKET="$2"; shift 2 ;;
	--project) PROJECT="$2"; shift 2 ;;
	--work) WORK="$2"; shift 2 ;;
	--port) PORT="$2"; shift 2 ;;
	--ark) ARK_BIN="$2"; shift 2 ;;
	--ark-server) SERVER_BIN="$2"; shift 2 ;;
	--keep) KEEP=1; shift ;;
	-h | --help) usage ;;
	*) echo "unknown argument: $1" >&2; usage ;;
	esac
done

case "$MODE" in
local) ;;
gcs)
	[ -n "$BUCKET" ] || { echo "--mode gcs requires --bucket" >&2; exit 2; }
	case "$BUCKET" in
	*ark-artifacts* | elkproject-*)
		echo "refusing: --bucket $BUCKET looks like the production bucket" >&2
		exit 2
		;;
	esac
	;;
*) echo "--mode must be 'local' or 'gcs'" >&2; usage ;;
esac

for tool in git jq curl sqlite3; do
	command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 2; }
done
if [ "$MODE" = gcs ]; then
	command -v gcloud >/dev/null || { echo "missing required tool: gcloud" >&2; exit 2; }
	if [ -n "$PROJECT" ]; then export CLOUDSDK_CORE_PROJECT="$PROJECT"; fi
	if ! gcloud auth application-default print-access-token >/dev/null 2>&1; then
		echo "Application Default Credentials are not configured." >&2
		echo "The Go GCS client needs them. Run: gcloud auth application-default login" >&2
		exit 2
	fi
fi

if [ -z "$WORK" ]; then
	WORK="$(mktemp -d "${TMPDIR:-/tmp}/ark-restore-drill.XXXXXX")"
fi
mkdir -p "$WORK"
LOG="$WORK/drill.log"
: >"$LOG"

DATA_DIR="$WORK/server-data"
CACHE_DIR="$WORK/server-cache"
SNAP="$WORK/snapshots"
mkdir -p "$DATA_DIR" "$CACHE_DIR" "$SNAP"

TOKEN="drill-$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"
BASE_URL="http://127.0.0.1:$PORT"

PASS=0
FAIL=0
FAILED_LABELS=()
DRILL_START=$(date +%s)

# ---------------------------------------------------------------- reporting

say() { printf '%s\n' "$*" | tee -a "$LOG"; }
phase() {
	say ""
	say "══ $* ══"
}
step() { say "   · $*"; }
note() { say "   ~ $*"; }

ok() {
	PASS=$((PASS + 1))
	say "   ✓ $1"
}
bad() {
	FAIL=$((FAIL + 1))
	FAILED_LABELS[${#FAILED_LABELS[@]}]="$1"
	say "   ✗ FAIL $1"
	if [ $# -gt 1 ]; then say "       $2"; fi
	return 0
}

assert_eq() { # label expected actual
	if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1" "expected [$2] got [$3]"; fi
}
assert_ne() { # label unexpected actual
	if [ "$2" != "$3" ]; then ok "$1 ($3 ≠ $2)"; else bad "$1" "expected anything but [$2]"; fi
}
assert_contains() { # label needle haystack
	case "$3" in
	*"$2"*) ok "$1" ;;
	*) bad "$1" "expected [$3] to contain [$2]" ;;
	esac
}
assert_ge() { # label floor actual
	if [ "$3" -ge "$2" ] 2>/dev/null; then ok "$1 ($3 ≥ $2)"; else bad "$1" "expected ≥ $2 got $3"; fi
}
assert_lt() { # label ceiling actual
	if [ "$3" -lt "$2" ] 2>/dev/null; then ok "$1 ($3 < $2)"; else bad "$1" "expected < $2 got $3"; fi
}

# ---------------------------------------------------------------- portable

case "$(uname -s)" in
Darwin | *BSD)
	file_size() { stat -f '%z' "$1"; }
	file_mtime_ns() { stat -f '%Fm' "$1" | tr -d '.'; }
	;;
*)
	file_size() { stat -c '%s' "$1"; }
	file_mtime_ns() { date -r "$1" '+%s%N'; }
	;;
esac

if command -v shasum >/dev/null; then
	sha_short() { shasum | cut -c1-12; }
else
	sha_short() { sha256sum | cut -c1-12; }
fi

# ------------------------------------------------------------------ storage
#
# Six operations are all a restore is made of. Implemented twice so the
# rehearsal and the drill run the same script.

OBJ_PATH="" # set once the repository ID is known

storage_label() {
	if [ "$MODE" = gcs ]; then echo "gs://$BUCKET/$OBJ_PATH"; else echo "$DATA_DIR/repos/$(basename "$OBJ_PATH")"; fi
}

st_exists() {
	if [ "$MODE" = gcs ]; then
		gcloud storage objects describe "gs://$BUCKET/$OBJ_PATH" >/dev/null 2>&1
	else
		[ -f "$DATA_DIR/repos/$(basename "$OBJ_PATH")" ]
	fi
}

st_generation() {
	if [ "$MODE" = gcs ]; then
		gcloud storage objects describe "gs://$BUCKET/$OBJ_PATH" --format='value(generation)' 2>/dev/null
	else
		# LocalBackend uses the file's mtime in nanoseconds as its generation.
		file_mtime_ns "$DATA_DIR/repos/$(basename "$OBJ_PATH")" 2>/dev/null
	fi
}

st_download() { # dest
	if [ "$MODE" = gcs ]; then
		gcloud storage cp "gs://$BUCKET/$OBJ_PATH" "$1" >>"$LOG" 2>&1
	else
		cp "$DATA_DIR/repos/$(basename "$OBJ_PATH")" "$1"
	fi
}

st_upload() { # src — unconditional overwrite, the operator's restore
	if [ "$MODE" = gcs ]; then
		gcloud storage cp "$1" "gs://$BUCKET/$OBJ_PATH" >>"$LOG" 2>&1
	else
		cp "$1" "$DATA_DIR/repos/$(basename "$OBJ_PATH")"
	fi
}

st_delete() {
	if [ "$MODE" = gcs ]; then
		gcloud storage rm "gs://$BUCKET/$OBJ_PATH" >>"$LOG" 2>&1
	else
		rm -f "$DATA_DIR/repos/$(basename "$OBJ_PATH")"
	fi
}

st_soft_deleted_generations() {
	[ "$MODE" = gcs ] || return 0
	gcloud storage ls --soft-deleted "gs://$BUCKET/$OBJ_PATH" 2>/dev/null |
		sed -n 's/.*#\([0-9][0-9]*\)$/\1/p'
}

st_restore_generation() { # generation — GCS soft-delete restore
	# `gcloud storage restore` will not overwrite a live object synchronously:
	# --allow-overwrite is an --async-only flag. So clear the live object
	# first. That is safe — deleting it soft-deletes it too, so the thing
	# being replaced stays recoverable for the rest of the retention window.
	# Since elk-work/ark#66 a client syncing at the lost repository no longer
	# leaves a live object here, but an operator restoring for real may find
	# one, so the clear stays.
	if st_exists; then
		gcloud storage rm "gs://$BUCKET/$OBJ_PATH" >>"$LOG" 2>&1
	fi
	gcloud storage restore "gs://$BUCKET/$OBJ_PATH#$1" >>"$LOG" 2>&1
}

st_blob_count() {
	if [ "$MODE" = gcs ]; then
		gcloud storage ls -r "gs://$BUCKET/sha256/**" 2>/dev/null | grep -c 'sha256/' || true
	else
		find "$DATA_DIR/blobs" -type f 2>/dev/null | wc -l | tr -d ' '
	fi
}

# ------------------------------------------------------------------- server

SERVER_PID=""

server_start() {
	local env_common=(
		"ARK_API_TOKEN=$TOKEN"
		"CACHE_DIR=$CACHE_DIR"
		"PORT=$PORT"
	)
	if [ "$MODE" = gcs ]; then
		env "${env_common[@]}" "GCS_BUCKET=$BUCKET" "$SERVER_BIN" >>"$WORK/server.log" 2>&1 &
	else
		env "${env_common[@]}" "DATA_DIR=$DATA_DIR" "BASE_URL=$BASE_URL" "$SERVER_BIN" >>"$WORK/server.log" 2>&1 &
	fi
	SERVER_PID=$!
	local i
	for i in $(seq 1 60); do
		if curl -fsS "$BASE_URL/health" >/dev/null 2>&1; then return 0; fi
		sleep 0.25
	done
	echo "ark-server did not become healthy; see $WORK/server.log" >&2
	exit 2
}

server_stop() {
	[ -n "$SERVER_PID" ] || return 0
	kill "$SERVER_PID" 2>/dev/null || true
	wait "$SERVER_PID" 2>/dev/null || true
	SERVER_PID=""
}

server_restart() {
	server_stop
	server_start
}

cleanup() {
	server_stop
	note "work directory left at $WORK (drill log, snapshots, server log)"
}
trap cleanup EXIT

# Removes what this run put in the bucket. Only ever called after a passing
# drill: on a failure the objects are the evidence.
drill_objects_remove() {
	[ "$MODE" = gcs ] || return 0
	[ -n "$OBJ_PATH" ] || return 0
	if st_exists; then gcloud storage rm "gs://$BUCKET/$OBJ_PATH" >>"$LOG" 2>&1 || true; fi
}

# ---------------------------------------------------------------- API calls
#
# Raw HTTP, deliberately. "Restore from the object alone, with no client
# involved" is only demonstrated if the verification is not a client either.

api_status() { # path json-body -> http status
	curl -s -o "$WORK/last-response.json" -w '%{http_code}' \
		-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
		-d "$2" "$BASE_URL$1"
}

api_pull() { # after_revision -> http status; body in $WORK/last-response.json
	api_status /v1/sync/pull "{\"repository_id\":\"$REPO_ID\",\"after_revision\":$1}"
}

api_last() { cat "$WORK/last-response.json"; }

# ------------------------------------------------------------------ clients

client_dir() { echo "$WORK/clients/$1"; }

new_git_repo() { # dir
	mkdir -p "$1"
	git -C "$1" init -q -b main
	git -C "$1" config user.name "Restore Drill"
	git -C "$1" config user.email "drill@example.invalid"
	echo "drill" >"$1/README.md"
	git -C "$1" add README.md
	git -C "$1" commit -q -m "drill fixture"
}

ark_in() { # name args...
	local name="$1"
	shift
	ARK_TOKEN="$TOKEN" "$ARK_BIN" -C "$(client_dir "$name")" "$@"
}

# Runs `ark sync --json`, capturing exit code and result. Never aborts.
SYNC_RC=0
SYNC_JSON=""
client_sync() { # name
	SYNC_RC=0
	SYNC_JSON="$(ark_in "$1" sync --json 2>>"$LOG")" || SYNC_RC=$?
	# A sync that fails outright prints nothing on stdout. Keep SYNC_JSON valid
	# so every later `sync_field` reads "null" rather than the empty string,
	# which would turn one real failure into a cascade of confusing ones.
	case "$SYNC_JSON" in "") SYNC_JSON='{}' ;; esac
	printf 'sync[%s] rc=%d %s\n' "$1" "$SYNC_RC" "$SYNC_JSON" >>"$LOG"
}

sync_field() { echo "$SYNC_JSON" | jq -r "$1"; }

status_field() { # name jq-path
	ark_in "$1" status --json 2>>"$LOG" | jq -r "$2"
}

client_snapshot() { # name snapfile
	tar -C "$(client_dir "$1")" -cf "$2" .ark
}

client_from_snapshot() { # newname snapfile
	local dir
	dir="$(client_dir "$1")"
	rm -rf "$dir"
	new_git_repo "$dir"
	tar -C "$dir" -xf "$2"
}

task_count() { # name
	ark_in "$1" task list -s all --json 2>>"$LOG" | jq 'length'
}
thread_count() { ark_in "$1" thread list --json 2>>"$LOG" | jq 'length'; }
run_count() { ark_in "$1" run list --json 2>>"$LOG" | jq 'length'; }
artifact_count() { ark_in "$1" artifact list --json 2>>"$LOG" | jq 'length'; }

# --------------------------------------------------------------- inspection

# fingerprint DBFILE -> "<rows> <sha1-of-type/id/revision-triples>"
# Computed from the stored SQLite file directly. Nothing here goes through
# ark-server or a client, so a matching fingerprint is evidence about the
# object, not about anybody's replica.
fingerprint() {
	local rows sum
	rows="$(sqlite3 "$1" 'SELECT count(*) FROM records' 2>/dev/null || echo ERR)"
	sum="$(sqlite3 "$1" 'SELECT record_type || "/" || record_id || "/" || server_revision FROM records ORDER BY record_type, record_id' 2>/dev/null | sha_short)"
	echo "$rows $sum"
}

db_revision() { sqlite3 "$1" 'SELECT revision FROM meta WHERE id = 1' 2>/dev/null || echo ERR; }
db_breakdown() { sqlite3 "$1" 'SELECT record_type, count(*) FROM records GROUP BY 1 ORDER BY 1' 2>/dev/null || true; }

# Pulls the object down and fingerprints it, with no client involved.
object_fingerprint() {
	rm -f "$WORK/inspect.db"
	st_download "$WORK/inspect.db" || { echo "ERR ERR"; return 0; }
	fingerprint "$WORK/inspect.db"
}
object_revision() {
	rm -f "$WORK/inspect.db"
	st_download "$WORK/inspect.db" || { echo ERR; return 0; }
	db_revision "$WORK/inspect.db"
}

# ================================================================== phase 0

phase "Phase 0 — build and start"

if [ -z "$ARK_BIN" ] || [ -z "$SERVER_BIN" ]; then
	step "building ark and ark-server from $REPO_ROOT"
	mkdir -p "$WORK/bin"
	(cd "$REPO_ROOT" && go build -o "$WORK/bin/ark" ./cmd/ark) >>"$LOG" 2>&1
	(cd "$REPO_ROOT" && go build -o "$WORK/bin/ark-server" ./cmd/ark-server) >>"$LOG" 2>&1
	ARK_BIN="${ARK_BIN:-$WORK/bin/ark}"
	SERVER_BIN="${SERVER_BIN:-$WORK/bin/ark-server}"
fi
step "ark        $ARK_BIN"
step "ark-server $SERVER_BIN"
step "mode       $MODE"
if [ "$MODE" = gcs ]; then step "bucket     gs://$BUCKET"; else step "data dir   $DATA_DIR"; fi
step "work       $WORK"

server_start
step "server up at $BASE_URL (pid $SERVER_PID)"

# ================================================================== phase 1

phase "Phase 1 — populate a repository and take the baseline"

mkdir -p "$WORK/clients"
new_git_repo "$(client_dir alpha)"
ark_in alpha init --skill=false >>"$LOG" 2>&1
ark_in alpha remote set "$BASE_URL" >>"$LOG" 2>&1
REPO_ID="$(ark_in alpha status --json | jq -r .repository_id)"
OBJ_PATH="repos/$REPO_ID.db"
step "repository $REPO_ID"
step "object     $(storage_label)"

step "writing records"
for i in $(seq 1 20); do
	ark_in alpha task create -t "drill task $i" -b "record $i of the baseline set" >>"$LOG" 2>&1
done
for i in 1 2 3 4; do
	ark_in alpha task comment "$i" -b "comment on task $i" >>"$LOG" 2>&1
done
ark_in alpha task edit 5 --status done >>"$LOG" 2>&1
ark_in alpha task close 6 >>"$LOG" 2>&1
THREAD_ID="$(ark_in alpha thread create -t "drill thread" --task 1 --json | jq -r '.id')"
for i in 1 2 3; do
	ark_in alpha thread message "$THREAD_ID" -r agent -b "thread message $i" >>"$LOG" 2>&1
done
RUN_ID="$(ark_in alpha run start --task 2 --agent-name drill-agent -i "populate the drill repository" --json | jq -r '.id')"
ark_in alpha run finish "$RUN_ID" -r "wrote the baseline record set" --no-review >>"$LOG" 2>&1

client_sync alpha
assert_eq "records push clean" 0 "$SYNC_RC"

# Artifact blobs are the service's other half, and in object-storage mode they
# move over V4 signed URLs — which the Go storage client can only sign with
# service-account, external-account or impersonated credentials. A locally-run
# server holding a *user's* Application Default Credentials cannot sign at all.
# That is a property of who the server is running as, not of the repository
# database this drill is about, so probe for it and carry on either way.
BLOBS_AVAILABLE=1
PROBE_SHA="$(printf 'drill blob probe' | shasum -a 256 2>/dev/null | cut -d' ' -f1)"
probe_code="$(api_status /v1/artifacts/upload-url \
	"{\"repository_id\":\"$REPO_ID\",\"sha256\":\"$PROBE_SHA\",\"size_bytes\":16}")"
if [ "$probe_code" = 200 ]; then
	ok "the service can hand out signed URLs for artifact blobs"
	echo "drill artifact payload $(date -u +%FT%TZ)" >"$WORK/artifact.txt"
	ark_in alpha artifact add "$WORK/artifact.txt" --parent "task:1" >>"$LOG" 2>&1
	client_sync alpha
	assert_eq "artifact sync exits clean" 0 "$SYNC_RC"
else
	BLOBS_AVAILABLE=0
	note "artifact blob transfer is UNAVAILABLE here: upload-url answered $probe_code"
	note "  $(api_last | jq -r '.message // empty' 2>/dev/null || true)"
	note "  Signing V4 URLs needs service-account, external-account or impersonated"
	note "  credentials; a person's ADC has no key to sign with. The repository"
	note "  database — what this drill restores — is unaffected."
fi

R1="$(sync_field .server_revision)"
step "server revision after baseline push: R1=$R1"

if st_exists; then ok "object exists at $OBJ_PATH"; else bad "object exists at $OBJ_PATH"; fi
G1="$(st_generation)"
step "generation G1=$G1"

BASE_F="$(object_fingerprint)"
BASE_ROWS="${BASE_F%% *}"
step "baseline fingerprint: $BASE_F"
say "$(db_breakdown "$WORK/inspect.db" | sed 's/^/       /')"
assert_ge "baseline holds enough records to be observably right or wrong" 30 "$BASE_ROWS"
assert_eq "stored database carries the repository id" "$REPO_ID" \
	"$(sqlite3 "$WORK/inspect.db" 'SELECT repository_id FROM meta WHERE id = 1')"
assert_eq "stored revision matches what the server reported" "$R1" "$(db_revision "$WORK/inspect.db")"

BLOBS_AT_BASELINE="$(st_blob_count)"
if [ "$BLOBS_AVAILABLE" = 1 ]; then
	assert_ge "artifact blob stored as its own object, outside repos/" 1 "$BLOBS_AT_BASELINE"
fi

# The operator's out-of-band copy. `gcloud storage cp` out of the bucket is
# the whole backup procedure; there is nothing else to capture.
cp "$WORK/inspect.db" "$SNAP/baseline-R$R1.db"
client_snapshot alpha "$SNAP/alpha-R$R1.tar"
ALPHA_TASKS="$(task_count alpha)"
ALPHA_THREADS="$(thread_count alpha)"
ALPHA_RUNS="$(run_count alpha)"
ALPHA_ARTIFACTS="$(artifact_count alpha)"
step "operator copy at $SNAP/baseline-R$R1.db; client snapshot at $SNAP/alpha-R$R1.tar"
step "client holds $ALPHA_TASKS tasks, $ALPHA_THREADS threads, $ALPHA_RUNS runs, $ALPHA_ARTIFACTS artifacts"

# ================================================================== phase 2

phase "Phase 2 — the working cache is disposable"

server_stop
rm -rf "${CACHE_DIR:?}"/*
server_start
step "wiped CACHE_DIR and restarted the server"

code="$(api_pull 0)"
assert_eq "pull after a cold cache succeeds" 200 "$code"
assert_eq "pull reports the baseline revision" "$R1" "$(api_last | jq -r .server_revision)"
assert_eq "pull returns the whole baseline record set" "$BASE_ROWS" \
	"$(api_last | jq -r '(.records | length) + (.tombstones | length)')"
assert_eq "a read does not rewrite the object" "$G1" "$(st_generation)"

# ================================================================== phase 3

phase "Phase 3 — loss: the object is deleted"

st_delete
if st_exists; then bad "object deleted"; else ok "object deleted"; fi

code="$(api_pull 0)"
assert_eq "pull against an absent repository is 404" 404 "$code"
assert_eq "  and says not_found" not_found "$(api_last | jq -r .code)"

code="$(api_status /v1/sync/push "{\"repository_id\":\"$REPO_ID\",\"client_id\":\"drill\",\"mutations\":[]}")"
assert_eq "push against an absent repository is 404" 404 "$code"

step "now the client's view of an absent repository (elk-work/ark#58's shape)"
client_from_snapshot alpha_absent "$SNAP/alpha-R$R1.tar"
client_sync alpha_absent
assert_eq "sync against an absent repository exits 7 (partial)" 7 "$SYNC_RC"
assert_ne "history-loss detection fired" null "$(sync_field '.history_reset')"
# Zero, and exactly zero: the service holds nothing for this repository and
# now says so rather than standing an empty one up first. It used to be a
# revision or two in, because registration created the repository and upserted
# this checkout's actors into it before the pull ever ran (elk-work/ark#66).
assert_eq "  the service reports holding no history at all" 0 \
	"$(sync_field '.history_reset.server_revision')"
assert_eq "  reported this checkout's revision" "$R1" "$(sync_field '.history_reset.local_revision')"
assert_eq "  nothing was re-pushed" 0 "$(sync_field .pushed)"
assert_eq "status: pending mutations still zero (#59's property)" 0 "$(status_field alpha_absent .pending_mutations)"
assert_eq "status: rejected mutations still zero (#59's property)" 0 "$(status_field alpha_absent .rejected_mutations)"
assert_ne "status: history_reset carried separately" null "$(status_field alpha_absent '.history_reset')"
assert_eq "the client did not lose its own records" "$ALPHA_TASKS" "$(task_count alpha_absent)"

# The finding this phase used to surface, now pinned the other way round
# (elk-work/ark#66). Registration ran on every sync with create=true, so the
# first client to touch an absent repository stood an empty one back up at
# revision 1 — and the 404, the only clean evidence the repository had been
# lost, did not survive contact with a client. The client's cursor now travels
# on the registration and the service refuses to create for a client that has
# already synced.
if st_exists; then
	bad "the object is still absent after a client synced at it" \
		"a syncing client re-created it: $(object_fingerprint)"
else
	ok "the object is still absent after a client synced at it"
fi
code="$(api_pull 0)"
assert_eq "  pull still answers 404 — the diagnosis survived the sync" 404 "$code"
assert_eq "  and still says not_found" not_found "$(api_last | jq -r .code)"
assert_ge "  the service logged the refusal" 1 \
	"$(grep -c 'refused to create a repository' "$WORK/server.log" 2>/dev/null || true)"

# And it is the steady state, not a one-shot: an absent repository stays
# absent however many times a client syncs at it, which is what stops a lost
# repository accruing an empty generation per sync and aging the good one out
# of the bucket's retention window.
client_sync alpha_absent
assert_eq "a second sync also exits 7 (partial)" 7 "$SYNC_RC"
if st_exists; then
	bad "a later sync re-created the object"
else
	ok "a later sync left it absent too"
fi

# ================================================================== phase 4

phase "Phase 4 — restore from the stored copy, with no client involved"

RESTORE_METHOD="out-of-band copy"
RESTORE_START=$(date +%s)
if [ "$MODE" = gcs ]; then
	# Restore from the soft-delete window. Production now also has object
	# versioning with a lifecycle rule (see "Choosing a retention posture"
	# in docs/self-hosting.md), so this is no longer its *only* bucket-side
	# source — but it is the weaker of the two, it is what a self-hoster on
	# the defaults has, and it is the one that has to keep working, so it
	# is the path this drill exercises. Point --bucket at an unversioned
	# scratch bucket to test it as written; restoring a noncurrent version
	# is not covered here.
	SD_GENS="$(st_soft_deleted_generations)"
	step "soft-deleted generations available: $(echo "$SD_GENS" | tr '\n' ' ')"
	if echo "$SD_GENS" | grep -qx "$G1"; then
		ok "the pre-loss generation G1 is in the soft-delete window"
		if st_restore_generation "$G1"; then
			RESTORE_METHOD="soft-delete restore of generation $G1"
		else
			bad "soft-delete restore of generation $G1" "see $LOG"
			st_upload "$SNAP/baseline-R$R1.db"
		fi
	else
		bad "the pre-loss generation G1 is in the soft-delete window" \
			"falling back to the out-of-band copy"
		st_upload "$SNAP/baseline-R$R1.db"
	fi
else
	st_upload "$SNAP/baseline-R$R1.db"
fi
RESTORE_SECS=$(($(date +%s) - RESTORE_START))
step "restore method: $RESTORE_METHOD"
step "restore took ${RESTORE_SECS}s"

G2="$(st_generation)"
assert_ne "the restored object carries a new generation" "$G1" "$G2"

REST_F="$(object_fingerprint)"
assert_eq "the restored object is byte-for-byte the baseline record set" "$BASE_F" "$REST_F"

step "verifying through the running server — not restarted since the loss"
code="$(api_pull 0)"
assert_eq "pull after the restore succeeds without a restart" 200 "$code"
assert_eq "  serves the baseline revision again" "$R1" "$(api_last | jq -r .server_revision)"
assert_eq "  serves the whole baseline record set" "$BASE_ROWS" \
	"$(api_last | jq -r '(.records | length) + (.tombstones | length)')"

# ---- the generation compare-and-swap across a restore -------------------
#
# The server has been running throughout: the object was deleted under it in
# phase 3 and replaced under it here, with no client standing an empty one up
# in between any more (elk-work/ark#66). Whatever it has cached, the object is
# now at G2 with entirely different content. A write here is the
# question the issue asks: does a stale generation wedge the CAS, and worse,
# does the server apply the write on top of its stale cached copy?
step "generation CAS: writing through a server whose cached generation is stale"
ERRORS_BEFORE_CAS="$(grep -c '"level":"ERROR"' "$WORK/server.log" 2>/dev/null || true)"
client_from_snapshot alpha_cas "$SNAP/alpha-R$R1.tar"
ark_in alpha_cas task create -t "post-restore canary" -b "written after the restore" >>"$LOG" 2>&1
client_sync alpha_cas
assert_eq "a write against a stale cached generation succeeds first time" 0 "$SYNC_RC"
assert_ge "  and was applied" 1 "$(sync_field .applied)"
R_CANARY="$(sync_field .server_revision)"
assert_ge "  advancing the revision past the restored one" $((R1 + 1)) "$R_CANARY"

CAS_F="$(object_fingerprint)"
CAS_ROWS="${CAS_F%% *}"
assert_ge "the write landed on the RESTORED file, not the stale cached one" $((BASE_ROWS + 1)) "$CAS_ROWS"
assert_eq "  every baseline record survived the write" "$BASE_ROWS" \
	"$(sqlite3 "$WORK/inspect.db" "SELECT count(*) FROM records WHERE server_revision <= $R1")"
assert_eq "  the canary is there" 1 \
	"$(sqlite3 "$WORK/inspect.db" "SELECT count(*) FROM records WHERE data LIKE '%post-restore canary%'")"
ERRORS_AFTER_CAS="$(grep -c '"level":"ERROR"' "$WORK/server.log" 2>/dev/null || true)"
assert_eq "  the server logged no error doing it" "${ERRORS_BEFORE_CAS:-0}" "${ERRORS_AFTER_CAS:-0}"

# Back to the clean baseline for the convergence checks.
st_upload "$SNAP/baseline-R$R1.db"
G3="$(st_generation)"
assert_eq "re-restored to the baseline" "$BASE_F" "$(object_fingerprint)"

step "convergence of a client that had synced before the loss"
client_from_snapshot alpha_pre "$SNAP/alpha-R$R1.tar"
client_sync alpha_pre
assert_eq "pre-loss client syncs clean" 0 "$SYNC_RC"
assert_eq "  and pushed nothing" 0 "$(sync_field .pushed)"
assert_eq "  and pulled nothing (already converged)" 0 "$(sync_field .pulled_records)"
assert_eq "  no history reset" null "$(sync_field '.history_reset')"
assert_eq "  server revision is the restored one" "$R1" "$(sync_field .server_revision)"
assert_eq "  its records are intact" "$ALPHA_TASKS" "$(task_count alpha_pre)"

step "a fresh client — the case that tests the restore rather than a replica"
new_git_repo "$(client_dir beta)"
ark_in beta init --skill=false --repository "$REPO_ID" >>"$LOG" 2>&1
ark_in beta remote set "$BASE_URL" >>"$LOG" 2>&1
client_sync beta
assert_eq "fresh client syncs clean" 0 "$SYNC_RC"
# At least, not exactly: registering a new checkout mints its own actor record
# before the pull, so a joining client pulls the restored set plus itself.
assert_ge "  pulled the complete restored record set" "$BASE_ROWS" \
	"$(($(sync_field .pulled_records) + $(sync_field .pulled_tombstones)))"
assert_eq "  sees every task the original client had" "$ALPHA_TASKS" "$(task_count beta)"
assert_eq "  sees every thread" "$ALPHA_THREADS" "$(thread_count beta)"
assert_eq "  sees every agent run" "$ALPHA_RUNS" "$(run_count beta)"
assert_eq "  sees every artifact" "$ALPHA_ARTIFACTS" "$(artifact_count beta)"
assert_eq "  no history reset on a joining client" null "$(sync_field '.history_reset')"

assert_eq "artifact blobs were untouched by the repos/ restore" "$BLOBS_AT_BASELINE" "$(st_blob_count)"

# ================================================================== phase 5

phase "Phase 5 — corruption: the object is truncated"

HALF=$(($(file_size "$SNAP/baseline-R$R1.db") / 2))
head -c "$HALF" "$SNAP/baseline-R$R1.db" >"$WORK/truncated.db"
st_upload "$WORK/truncated.db"
step "uploaded a ${HALF}-byte truncation of a $(file_size "$SNAP/baseline-R$R1.db")-byte database"

code="$(api_pull 0)"
assert_eq "pull against a truncated database is 500" 500 "$code"
step "  server answered $code: $(api_last | jq -c . 2>/dev/null || api_last)"
# Not `internal`. A 500 that will still be a 500 after any number of retries is
# a different state from a service having a moment, and until elk-work/ark#65
# the two were the same body — so the only place the truth existed was the
# service's own log line, "apply schema: database disk image is malformed".
assert_eq "  and says repository_corrupt, not internal" repository_corrupt "$(api_last | jq -r .code)"
assert_contains "  and names the repository rather than the verb that failed" \
	"$REPO_ID" "$(api_last | jq -r .message)"

client_from_snapshot alpha_trunc "$SNAP/alpha-R$R1.tar"
client_sync alpha_trunc
# 8, not 6. Exit 6 is offline — the code a retry loop keys on — and this
# condition is permanent until an operator restores the object.
assert_eq "a client sync against a truncated database exits 8, not 6 (offline)" 8 "$SYNC_RC"
assert_eq "  the corrupted object was not overwritten by the failed sync" \
	"$(sha_short <"$WORK/truncated.db")" \
	"$(st_download "$WORK/inspect.db" && sha_short <"$WORK/inspect.db")"

step "restoring from the out-of-band copy"
RESTORE2_START=$(date +%s)
st_upload "$SNAP/baseline-R$R1.db"
RESTORE2_SECS=$(($(date +%s) - RESTORE2_START))
assert_eq "restored object matches the baseline" "$BASE_F" "$(object_fingerprint)"
code="$(api_pull 0)"
assert_eq "pull works again after the corruption restore" 200 "$code"
assert_eq "  at the baseline revision" "$R1" "$(api_last | jq -r .server_revision)"
step "restore took ${RESTORE2_SECS}s"

client_from_snapshot alpha_post_trunc "$SNAP/alpha-R$R1.tar"
client_sync alpha_post_trunc
assert_eq "pre-corruption client converges without re-pushing" 0 "$SYNC_RC"
assert_eq "  pushed nothing" 0 "$(sync_field .pushed)"

step "the other corruption, which is not corruption: a zero-length object"
: >"$WORK/empty.db"
st_upload "$WORK/empty.db"
# SQLite is happy with an empty file — it is a valid empty database, so it
# opens and the schema applies over it. What is missing is the repository row,
# not the bytes, which makes this a repository the service no longer holds
# rather than one it cannot read. Registration answers it as such
# (elk-work/ark#66), and that is where a sync meets it first.
#
# The raw route is what an operator meets, because this runbook has them curl
# the pull route to check the service's copy. It used to answer 500 internal
# here — the one shape of the loss the three routes did not agree on — until
# the check moved into the storage layer (elk-work/ark#85).
code="$(api_pull 0)"
assert_eq "pull against a zero-length object is 404, the same as an absent one" 404 "$code"
step "  server answered $code: $(api_last | jq -c . 2>/dev/null || api_last)"
assert_eq "  and says not_found, not internal" not_found "$(api_last | jq -r .code)"
assert_contains "  and says the object is there and holds nothing" \
	"holds no repository" "$(api_last | jq -r .message)"

client_from_snapshot alpha_zero "$SNAP/alpha-R$R1.tar"
client_sync alpha_zero
# 7, not 8, and deliberately: the client turns registration's refusal into a
# recorded history reset, which is a stronger signal than "the copy is
# unreadable" and the one #59 built for exactly this.
assert_eq "a client sync against a zero-length object exits 7 (history reset)" 7 "$SYNC_RC"
assert_ne "  history-loss detection fired" null "$(sync_field '.history_reset')"
rm -f "$WORK/inspect.db"
st_download "$WORK/inspect.db"
assert_eq "  and the zero-length object was not adopted and rewritten" 0 "$(file_size "$WORK/inspect.db")"

st_upload "$SNAP/baseline-R$R1.db"
assert_eq "restored again after the zero-length corruption" "$BASE_F" "$(object_fingerprint)"

# ================================================================== phase 6

phase "Phase 6 — rollback: restored from an earlier point"

client_from_snapshot alpha2 "$SNAP/alpha-R$R1.tar"
client_sync alpha2
step "writing a second batch"
for i in $(seq 1 5); do
	ark_in alpha2 task create -t "second batch task $i" -b "written after the baseline" >>"$LOG" 2>&1
done
ark_in alpha2 task comment 1 -b "second batch comment" >>"$LOG" 2>&1
client_sync alpha2
assert_eq "second batch pushes clean" 0 "$SYNC_RC"
R2="$(sync_field .server_revision)"
assert_ge "revision advanced past the baseline" $((R1 + 1)) "$R2"
SECOND_F="$(object_fingerprint)"
cp "$WORK/inspect.db" "$SNAP/second-R$R2.db"
client_snapshot alpha2 "$SNAP/alpha-R$R2.tar"
ALPHA2_TASKS="$(task_count alpha2)"
step "R2=$R2, fingerprint $SECOND_F"

step "rolling the object back to the baseline copy"
st_upload "$SNAP/baseline-R$R1.db"
assert_eq "the object is back at the baseline" "$BASE_F" "$(object_fingerprint)"
code="$(api_pull 0)"
assert_eq "  and the service serves the earlier revision" "$R1" "$(api_last | jq -r .server_revision)"

client_from_snapshot alpha2_rb "$SNAP/alpha-R$R2.tar"
client_sync alpha2_rb
assert_eq "a client past the rollback point exits 7" 7 "$SYNC_RC"
assert_ne "  history-loss detection fired" null "$(sync_field '.history_reset')"
assert_eq "  reported service revision" "$R1" "$(sync_field '.history_reset.server_revision')"
assert_eq "  reported this checkout's revision" "$R2" "$(sync_field '.history_reset.local_revision')"
assert_eq "  pending mutations zero (#59's property)" 0 "$(status_field alpha2_rb .pending_mutations)"
assert_eq "  rejected mutations zero (#59's property)" 0 "$(status_field alpha2_rb .rejected_mutations)"
assert_eq "  the client kept its own second-batch records" "$ALPHA2_TASKS" "$(task_count alpha2_rb)"

step "a client that never saw the newer history cannot detect the rollback"
new_git_repo "$(client_dir gamma)"
ark_in gamma init --skill=false --repository "$REPO_ID" >>"$LOG" 2>&1
ark_in gamma remote set "$BASE_URL" >>"$LOG" 2>&1
client_sync gamma
assert_eq "joining client syncs clean against the rolled-back service" 0 "$SYNC_RC"
assert_eq "  no history reset — it has nothing to compare against" null "$(sync_field '.history_reset')"
assert_eq "  and sees only the baseline set" "$ALPHA_TASKS" "$(task_count gamma)"

step "restoring the newer copy and re-syncing the affected client"
st_upload "$SNAP/second-R$R2.db"
assert_eq "the newer copy is back" "$SECOND_F" "$(object_fingerprint)"
client_sync alpha2_rb
assert_eq "the affected client now syncs clean" 0 "$SYNC_RC"
assert_eq "  no NEW reset is reported" null "$(sync_field '.history_reset')"
assert_ne "  but status still carries the recorded reset — it does not self-clear" \
	null "$(status_field alpha2_rb '.history_reset')"

# =================================================================== report

phase "Result"
DRILL_SECS=$(($(date +%s) - DRILL_START))
say "mode              $MODE"
say "repository        $REPO_ID"
say "object            $(storage_label)"
say "baseline          revision $R1, $BASE_ROWS records, generation $G1"
say "restore method    $RESTORE_METHOD"
say "restore duration  ${RESTORE_SECS}s (deletion) / ${RESTORE2_SECS}s (corruption)"
say "drill duration    ${DRILL_SECS}s"
say "assertions        $PASS passed, $FAIL failed"
say "log               $LOG"
if [ "$FAIL" -gt 0 ]; then
	say ""
	say "failed assertions:"
	for l in "${FAILED_LABELS[@]}"; do say "  · $l"; done
	say ""
	say "The drill's objects are left in place — they are the evidence."
	exit 1
fi
if [ "$KEEP" = 0 ]; then
	drill_objects_remove
	say "scratch objects  removed (pass --keep to leave them for inspection)"
else
	say "scratch objects  kept at $(storage_label)"
fi
say ""
say "DRILL PASSED"
