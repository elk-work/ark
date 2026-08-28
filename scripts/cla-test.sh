#!/usr/bin/env bash
#
# Unit tests for the CLA gate's decision logic (scripts/cla.sh).
#
# What this covers: is a comment body the signing phrase, is a login on the
# allowlist, who has commits in a pull request, and who of those still has to
# sign. Plus a handful of assertions about .github/workflows/cla.yml itself,
# guarding the properties that are easy to undo by accident — one `run:`, no
# `actions: write`, a checkout pinned to `main`.
#
# What this does not cover, and cannot: the run against GitHub. A real
# end-to-end exercise needs an outside contributor's pull request, and there
# is no way to manufacture one. Everything below the "I/O" banner in cla.sh —
# the API calls, the signature commit, the status write — is unverified by
# this file.
#
# Run: bash scripts/cla-test.sh

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/.." && pwd)
workflow=$repo/.github/workflows/cla.yml

# shellcheck source=scripts/cla.sh
. "$here/cla.sh"

checks=0
failures=0

ok() {
	checks=$((checks + 1))
	printf 'ok   %s\n' "$1"
}

bad() {
	checks=$((checks + 1))
	failures=$((failures + 1))
	printf 'FAIL %s\n' "$1"
}

expect_eq() {
	local name=$1 want=$2 got=$3
	if [ "$want" = "$got" ]; then
		ok "$name"
	else
		bad "$name"
		printf '       want: %s\n' "$want" | sed 's/^/       /'
		printf '       got:  %s\n' "$got" | sed 's/^/       /'
	fi
}

expect_contains() {
	local name=$1 haystack=$2 needle=$3
	case $haystack in
	*"$needle"*) ok "$name" ;;
	*) bad "$name (missing: $needle)" ;;
	esac
}

expect_not_contains() {
	local name=$1 haystack=$2 needle=$3
	case $haystack in
	*"$needle"*) bad "$name (unexpectedly present: $needle)" ;;
	*) ok "$name" ;;
	esac
}

expect_yes() {
	local name=$1
	shift
	if "$@"; then ok "$name"; else bad "$name"; fi
}

expect_no() {
	local name=$1
	shift
	if "$@"; then bad "$name"; else ok "$name"; fi
}

# ---------------------------------------------------------------------------
# Fixtures. The allowlist and the signature file are read from the repository
# rather than restated, so this tests what actually ships.
# ---------------------------------------------------------------------------

allowlist=$(sed -n 's/^[[:space:]]*CLA_ALLOWLIST:[[:space:]]*//p' "$workflow" | head -1)
signatures=$(cat "$repo/signatures/cla.json")
phrase=$CLA_SIGN_PHRASE

expect_yes "allowlist was found in the workflow" test -n "$allowlist"
expect_contains "allowlist still covers ijroth" "$allowlist" "ijroth"
json_ok() { jq -e . "$1" >/dev/null 2>&1; }
expect_yes "signature file parses" json_ok "$repo/signatures/cla.json"

unlinked_committer=$(printf 'Unlinked Person\t')

# A pull request's commits, as GET /pulls/{n}/commits returns them: an author
# committed by web-flow, the same author twice, a commit whose author is not
# linked to a GitHub account, one of this gate's own signature commits, and a
# commit whose author is unresolved but whose committer is not.
commits='[
  {"author":{"login":"tylercdmx","id":261029081},
   "committer":{"login":"web-flow","id":19864447},
   "commit":{"author":{"name":"Tyler"}}},
  {"author":{"login":"tylercdmx","id":261029081},
   "committer":{"login":"tylercdmx","id":261029081},
   "commit":{"author":{"name":"Tyler"}}},
  {"author":null,"committer":null,
   "commit":{"author":{"name":"Unlinked Person"}}},
  {"author":{"login":"github-actions[bot]","id":41898282},
   "committer":{"login":"github-actions[bot]","id":41898282},
   "commit":{"author":{"name":"github-actions[bot]"}}},
  {"author":null,"committer":{"login":"ijroth","id":100},
   "commit":{"author":{"name":"Somebody Else"}}}
]'

# ---------------------------------------------------------------------------
# The signing phrase
# ---------------------------------------------------------------------------

expect_yes "phrase: exact" cla_is_signature "$phrase"
expect_yes "phrase: surrounded by whitespace" cla_is_signature "  $phrase
"
expect_yes "phrase: lower case" cla_is_signature "i have read the cla document and i hereby sign the cla"
expect_yes "phrase: upper case" cla_is_signature "I HAVE READ THE CLA DOCUMENT AND I HEREBY SIGN THE CLA"
expect_no "phrase: prefixed with other text" cla_is_signature "please sign: $phrase"
expect_no "phrase: followed by other text" cla_is_signature "$phrase, I suppose"
expect_no "phrase: quoted inside a sentence" cla_is_signature "you have to comment \"$phrase\" here"
expect_no "phrase: empty" cla_is_signature ""
expect_no "phrase: recheck is not a signature" cla_is_signature "recheck"

expect_yes "recheck: exact" cla_is_recheck "recheck"
expect_yes "recheck: whitespace and case" cla_is_recheck "  RECHECK
"
expect_no "recheck: a longer word" cla_is_recheck "rechecking"
expect_no "recheck: in a sentence" cla_is_recheck "please recheck this"
expect_no "recheck: empty" cla_is_recheck ""
expect_no "recheck: the phrase is not a recheck" cla_is_recheck "$phrase"

# ---------------------------------------------------------------------------
# The allowlist
# ---------------------------------------------------------------------------

expect_eq "glob: brackets are literal" '^dependabot\[bot\]$' "$(cla_glob_to_regex 'dependabot[bot]')"
expect_eq "glob: star is the wildcard" '^.*\[bot\]$' "$(cla_glob_to_regex '*[bot]')"
expect_eq "glob: dots are literal" '^a\.b$' "$(cla_glob_to_regex 'a.b')"

expect_yes "allow: ijroth" cla_allowlisted "ijroth" "$allowlist"
expect_yes "allow: case does not matter" cla_allowlisted "IJRoth" "$allowlist"
expect_yes "allow: dependabot[bot]" cla_allowlisted "dependabot[bot]" "$allowlist"
# The regression the bracket escaping exists for: read as a character class,
# `dependabot[bot]` matches this and not the bot.
expect_no "allow: dependabotb is not dependabot[bot]" cla_allowlisted "dependabotb" "$allowlist"
expect_no "allow: dependabot is not dependabot[bot]" cla_allowlisted "dependabot" "$allowlist"
expect_no "allow: an outside contributor" cla_allowlisted "tylercdmx" "$allowlist"
expect_no "allow: the empty login" cla_allowlisted "" "$allowlist"
expect_no "allow: nobody, on an empty allowlist" cla_allowlisted "ijroth" ""
expect_no "allow: no substring match" cla_allowlisted "ijroth-impostor" "$allowlist"
expect_yes "allow: a wildcard entry" cla_allowlisted "renovate[bot]" "someone,*[bot]"
expect_no "allow: a wildcard entry does not over-match" cla_allowlisted "renovatebot" "someone,*[bot]"
expect_yes "allow: entries may be spaced" cla_allowlisted "someone" " someone , other "

# ---------------------------------------------------------------------------
# Who has commits in the pull request
# ---------------------------------------------------------------------------

# Built with printf rather than written out: several of these lines end in a
# tab — an unlinked author has no id — and a trailing tab in a source file is
# invisible and the first thing an editor strips.
want_committers=$(printf 'Unlinked Person\t\nijroth\t100\ntylercdmx\t261029081')
expect_eq "committers: author preferred, bot dropped, deduped" \
	"$want_committers" "$(cla_committers "$commits")"

expect_eq "signed ids come from the shipped signature file" \
	"261029081" "$(cla_signed_ids "$signatures")"

# ---------------------------------------------------------------------------
# Who still has to sign
# ---------------------------------------------------------------------------

# The compatibility guarantee: the person already in signatures/cla.json is
# never asked again. If the record format ever stops being understood, this is
# the assertion that says so.
expect_eq "outstanding: an existing signatory is not asked again" \
	"" "$(cla_outstanding "tylercdmx	261029081" "$(cla_signed_ids "$signatures")" "$allowlist")"

expect_eq "outstanding: an allowlisted committer is not asked" \
	"" "$(cla_outstanding "ijroth	12345" "$(cla_signed_ids "$signatures")" "$allowlist")"

expect_eq "outstanding: a new contributor is asked" \
	"newperson	999" "$(cla_outstanding "newperson	999" "$(cla_signed_ids "$signatures")" "$allowlist")"

expect_eq "outstanding: an unlinked author always blocks" \
	"$unlinked_committer" "$(cla_outstanding "$unlinked_committer" "$(cla_signed_ids "$signatures")" "$allowlist")"

# The allowlist names GitHub accounts. Where a commit's author is linked to no
# account, the name in hand is the git `user.name` the committer typed, and
# `git config user.name ijroth` must not be a way past the gate.
impostor=$(printf 'ijroth\t')
expect_eq "outstanding: an unlinked author cannot borrow an allowlisted name" \
	"$impostor" "$(cla_outstanding "$impostor" "$(cla_signed_ids "$signatures")" "$allowlist")"
impostor=$(printf 'Claude\t')
expect_eq "outstanding: nor an allowlisted bot's name" \
	"$impostor" "$(cla_outstanding "$impostor" "$(cla_signed_ids "$signatures")" "$allowlist")"

# A signed id is matched whole. 26102908 is 261029081 with the last digit
# removed, and must not be read as signed.
expect_eq "outstanding: ids are matched whole, not by prefix" \
	"someone	26102908" "$(cla_outstanding "someone	26102908" "$(cla_signed_ids "$signatures")" "$allowlist")"

expect_eq "outstanding: a bracketed bot login is allowlisted" \
	"" "$(cla_outstanding "dependabot[bot]	49699333" "$(cla_signed_ids "$signatures")" "$allowlist")"

mixed='ijroth	100
tylercdmx	261029081
newperson	999'
expect_eq "outstanding: only the new contributor, out of three" \
	"newperson	999" "$(cla_outstanding "$mixed" "$(cla_signed_ids "$signatures")" "$allowlist")"

expect_eq "outstanding: nobody at all" \
	"" "$(cla_outstanding "" "$(cla_signed_ids "$signatures")" "$allowlist")"

expect_yes "id list: present" cla_id_in_list "7" "3 7 11"
expect_no "id list: absent" cla_id_in_list "1" "3 7 11"
expect_no "id list: not a prefix match" cla_id_in_list "1" "11 111"

# ---------------------------------------------------------------------------
# Rendering a contributor-controlled name
# ---------------------------------------------------------------------------

expect_eq "sanitize: markup and mentions are neutralised" \
	'Bad ?Name??http???x? ?ijroth' "$(cla_sanitize_display 'Bad [Name](http://x) @ijroth')"
truncated=$(cla_sanitize_display 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx')
expect_eq "sanitize: truncated to 40 characters" 40 "${#truncated}"

signed_body=$(cla_comment_body "")
expect_contains "comment: all-signed carries the marker" "$signed_body" "$CLA_COMMENT_MARKER"
expect_contains "comment: all-signed says so" "$signed_body" "All contributors have signed"

unsigned_body=$(cla_comment_body "newperson	999")
expect_contains "comment: unsigned carries the marker" "$unsigned_body" "$CLA_COMMENT_MARKER"
expect_contains "comment: unsigned quotes the phrase" "$unsigned_body" "$phrase"
expect_contains "comment: unsigned names who must sign" "$unsigned_body" "@newperson"
expect_contains "comment: unsigned links the agreement" "$unsigned_body" "$CLA_DOCUMENT_URL"

# A git author name is free text chosen by the contributor. It must not reach
# the comment as a mention or as markup.
hostile_body=$(cla_comment_body "$(printf 'Evil @ijroth [x](http://y)\t')")
expect_not_contains "comment: an unlinked name cannot mention anyone" "$hostile_body" "@ijroth"
expect_not_contains "comment: an unlinked name cannot inject a link" "$hostile_body" "(http://y)"

# ---------------------------------------------------------------------------
# The driver, against a fake `gh`
#
# This exercises cla_main and everything below the "I/O" banner — the order of
# the API calls, the bodies sent, the status transitions, the retry-free happy
# paths — without touching GitHub. It is not an end-to-end test: the fake
# answers the way the API is documented to, not the way it will.
# ---------------------------------------------------------------------------

fake=$(mktemp -d)
trap 'rm -rf "$fake"' EXIT
mkdir -p "$fake/bin" "$fake/state"

cat >"$fake/bin/gh" <<'FAKE_GH'
#!/usr/bin/env bash
# A stand-in for `gh api` covering exactly the calls scripts/cla.sh makes.
set -u
state=$FAKE_GH_STATE
shift # "api"

method=GET jqfilter='' input='' url=''
while [ $# -gt 0 ]; do
	case $1 in
	-X) method=$2; shift 2 ;;
	-H) shift 2 ;;
	--paginate) shift ;;
	--jq) jqfilter=$2; shift 2 ;;
	--input) input=$2; shift 2 ;;
	*) url=$1; shift ;;
	esac
done

body=''
if [ "$input" = "-" ]; then body=$(cat); fi
printf '%s %s\n' "$method" "$url" >>"$state/calls.log"

if [ -n "${FAKE_GH_FAIL_ON:-}" ]; then
	case $url in
	*"$FAKE_GH_FAIL_ON"*)
		printf 'gh: Internal Server Error (HTTP 500)\n' >&2
		exit 1
		;;
	esac
fi

out=''
case $method:$url in
POST:*/statuses/*)
	printf '%s\n' "$body" | jq -r '.state' >>"$state/statuses.log"
	out='{}'
	;;
GET:*/contents/*)
	if [ -f "$state/cla.json" ]; then
		out=$(jq -n \
			--arg sha "$(cat "$state/blobsha")" \
			--arg content "$(base64 <"$state/cla.json" | tr -d '\n')" \
			'{ sha: $sha, content: $content }')
	else
		printf 'gh: Not Found (HTTP 404)\n' >&2
		exit 1
	fi
	;;
PUT:*/contents/*)
	printf '%s\n' "$body" >"$state/put-body.json"
	printf '%s' "$body" | jq -r '.content' | base64 -d >"$state/cla.json"
	printf 'blob%s' "$RANDOM" >"$state/blobsha"
	out='{}'
	;;
GET:*/pulls/*/commits*)
	out=$(cat "$state/commits.json")
	;;
GET:*/pulls/*)
	out=$(jq -n --arg s "$(cat "$state/headsha")" '{ head: { sha: $s } }')
	;;
GET:*/issues/*/comments*)
	out=$(cat "$state/comments.json")
	;;
POST:*/issues/*/comments*)
	printf '%s\n' "$body" >"$state/comment-body.json"
	printf '%s' "$body" | jq '[ { id: 1, body: .body } ]' >"$state/comments.json"
	out='{}'
	;;
PATCH:*/issues/comments/*)
	printf '%s\n' "$body" >"$state/comment-body.json"
	out='{}'
	;;
*)
	printf 'fake gh: unhandled %s %s\n' "$method" "$url" >&2
	exit 1
	;;
esac

if [ -n "$jqfilter" ]; then
	printf '%s' "$out" | jq -r "$jqfilter"
else
	printf '%s' "$out"
fi
FAKE_GH
chmod +x "$fake/bin/gh"

head_sha=1111111111111111111111111111111111111111

# A pull request with one commit, by whoever is named.
one_commit() {
	jq -n --arg login "$1" --argjson id "$2" \
		'[ { author: { login: $login, id: $id }, committer: null, commit: { author: { name: $login } } } ]'
}

driver_reset() {
	rm -rf "$fake/state"
	mkdir -p "$fake/state"
	cp "$repo/signatures/cla.json" "$fake/state/cla.json"
	printf 'blob0' >"$fake/state/blobsha"
	printf '%s' "$head_sha" >"$fake/state/headsha"
	printf '%s' "$1" >"$fake/state/commits.json"
	printf '[]' >"$fake/state/comments.json"
	: >"$fake/state/calls.log"
	: >"$fake/state/statuses.log"
}

driver_run() {
	env -i \
		PATH="$fake/bin:$PATH" \
		FAKE_GH_STATE="$fake/state" \
		FAKE_GH_FAIL_ON="${FAKE_GH_FAIL_ON:-}" \
		GH_REPO=elk-work/ark \
		CLA_TARGET_URL=https://example.invalid/run/1 \
		CLA_ALLOWLIST="$allowlist" \
		"$@" bash "$repo/scripts/cla.sh" \
		>"$fake/state/out" 2>"$fake/state/err"
}

statuses() { tr '\n' ' ' <"$fake/state/statuses.log" | sed 's/ *$//'; }

# A new contributor opens a pull request.
driver_reset "$(one_commit newperson 999)"
expect_yes "driver: unsigned run succeeds" driver_run \
	CLA_EVENT=pull_request_target CLA_PR_NUMBER=70 CLA_HEAD_SHA="$head_sha"
expect_eq "driver: unsigned goes pending then failure" "pending failure" "$(statuses)"
expect_yes "driver: unsigned asks for a signature" test -f "$fake/state/comment-body.json"
expect_contains "driver: the comment names the contributor" \
	"$(jq -r .body "$fake/state/comment-body.json")" "@newperson"
expect_no "driver: unsigned commits nothing" test -f "$fake/state/put-body.json"

# The contributor already in signatures/cla.json opens one.
driver_reset "$(one_commit tylercdmx 261029081)"
expect_yes "driver: signed run succeeds" driver_run \
	CLA_EVENT=pull_request_target CLA_PR_NUMBER=71 CLA_HEAD_SHA="$head_sha"
expect_eq "driver: signed goes pending then success" "pending success" "$(statuses)"
expect_no "driver: signed acquires no comment" test -f "$fake/state/comment-body.json"

# A maintainer opens one. Allowlisted, never asked.
driver_reset "$(one_commit ijroth 100)"
expect_yes "driver: allowlisted run succeeds" driver_run \
	CLA_EVENT=pull_request_target CLA_PR_NUMBER=72 CLA_HEAD_SHA="$head_sha"
expect_eq "driver: allowlisted goes pending then success" "pending success" "$(statuses)"

# Somebody commits from an address linked to no GitHub account, under an
# allowlisted name. The allowlist is about accounts, so this must not pass.
driver_reset '[{"author":null,"committer":null,"commit":{"author":{"name":"ijroth"}}}]'
expect_yes "driver: impostor run succeeds" driver_run \
	CLA_EVENT=pull_request_target CLA_PR_NUMBER=73 CLA_HEAD_SHA="$head_sha"
expect_eq "driver: an unlinked author cannot borrow an allowlisted name" \
	"pending failure" "$(statuses)"

# The new contributor signs.
driver_reset "$(one_commit newperson 999)"
expect_yes "driver: signing run succeeds" driver_run \
	CLA_EVENT=issue_comment CLA_PR_NUMBER=70 \
	CLA_ISSUE_PR_URL=https://api.github.com/repos/elk-work/ark/pulls/70 \
	CLA_COMMENT_BODY="$phrase" \
	CLA_COMMENT_ID=5000 CLA_COMMENT_USER_LOGIN=newperson CLA_COMMENT_USER_ID=999 \
	CLA_COMMENT_CREATED_AT=2026-08-28T10:00:00Z CLA_REPO_ID=1297714473
expect_eq "driver: signing ends green" "pending success" "$(statuses)"
expect_yes "driver: signing commits the file" test -f "$fake/state/put-body.json"
expect_eq "driver: the commit lands on main" \
	"main" "$(jq -r .branch "$fake/state/put-body.json")"
expect_eq "driver: the commit message names the signer and the pull request" \
	"chore: newperson has signed the CLA (#70)" "$(jq -r .message "$fake/state/put-body.json")"
expect_eq "driver: the existing signatory survives the write" \
	"tylercdmx newperson" \
	"$(jq -r '[.signedContributors[].name] | join(" ")' "$fake/state/cla.json")"
expect_eq "driver: the new record keeps the established shape" \
	"name id comment_id created_at repoId pullRequestNo" \
	"$(jq -r '.signedContributors[1] | keys_unsorted | join(" ")' "$fake/state/cla.json")"
expect_eq "driver: two-space indent is preserved" \
	'  "signedContributors": [' "$(sed -n 2p "$fake/state/cla.json")"
# The file the retired action wrote ends without a newline. Matching it keeps
# the first signature recorded by this implementation from showing up as a
# whole-file diff.
expect_eq "driver: the file still ends without a trailing newline" \
	"}" "$(tail -c 1 "$fake/state/cla.json")"

# Somebody else posts the phrase on a pull request that is not theirs.
driver_reset "$(one_commit newperson 999)"
expect_yes "driver: a bystander's signature run succeeds" driver_run \
	CLA_EVENT=issue_comment CLA_PR_NUMBER=70 \
	CLA_ISSUE_PR_URL=https://api.github.com/repos/elk-work/ark/pulls/70 \
	CLA_COMMENT_BODY="$phrase" \
	CLA_COMMENT_ID=5001 CLA_COMMENT_USER_LOGIN=passerby CLA_COMMENT_USER_ID=1234 \
	CLA_COMMENT_CREATED_AT=2026-08-28T10:00:00Z CLA_REPO_ID=1297714473
expect_no "driver: a bystander cannot add themselves" test -f "$fake/state/put-body.json"
expect_eq "driver: a bystander leaves the check red" "pending failure" "$(statuses)"

# `recheck` re-evaluates and records nothing.
driver_reset "$(one_commit newperson 999)"
expect_yes "driver: recheck run succeeds" driver_run \
	CLA_EVENT=issue_comment CLA_PR_NUMBER=70 \
	CLA_ISSUE_PR_URL=https://api.github.com/repos/elk-work/ark/pulls/70 \
	CLA_COMMENT_BODY=recheck \
	CLA_COMMENT_ID=5002 CLA_COMMENT_USER_LOGIN=newperson CLA_COMMENT_USER_ID=999 \
	CLA_COMMENT_CREATED_AT=2026-08-28T10:00:00Z CLA_REPO_ID=1297714473
expect_no "driver: recheck records no signature" test -f "$fake/state/put-body.json"
expect_eq "driver: recheck still publishes a verdict" "pending failure" "$(statuses)"

# A comment on an ordinary issue must touch nothing at all.
driver_reset "$(one_commit newperson 999)"
expect_yes "driver: an issue comment run succeeds" driver_run \
	CLA_EVENT=issue_comment CLA_PR_NUMBER=70 \
	CLA_COMMENT_BODY="$phrase" \
	CLA_COMMENT_ID=5003 CLA_COMMENT_USER_LOGIN=newperson CLA_COMMENT_USER_ID=999 \
	CLA_COMMENT_CREATED_AT=2026-08-28T10:00:00Z CLA_REPO_ID=1297714473
expect_eq "driver: an issue is never called about" "" "$(cat "$fake/state/calls.log")"

# Fail closed: the deciding step erroring must leave the status red.
driver_reset "$(one_commit newperson 999)"
FAKE_GH_FAIL_ON=/pulls/70/commits
expect_no "driver: an error exits non-zero" driver_run \
	CLA_EVENT=pull_request_target CLA_PR_NUMBER=70 CLA_HEAD_SHA="$head_sha"
expect_eq "driver: an error leaves the status red" "pending failure" "$(statuses)"
FAKE_GH_FAIL_ON=

# ---------------------------------------------------------------------------
# The workflow itself
# ---------------------------------------------------------------------------

runs=$(sed -n 's/^[[:space:]]*run:[[:space:]]*//p' "$workflow")
expect_eq "workflow: exactly one run, and it is the script" "bash scripts/cla.sh" "$runs"

expect_no "workflow: does not ask for actions: write" \
	grep -qE '^[[:space:]]*actions:[[:space:]]*write' "$workflow"
expect_yes "workflow: checks out main by name" \
	grep -qE '^[[:space:]]*ref:[[:space:]]*main[[:space:]]*$' "$workflow"
expect_no "workflow: never names the pull request head ref" \
	grep -q 'head\.ref' "$workflow"
expect_yes "workflow: keeps the cla-assistant status context" \
	grep -q 'cla-assistant' "$workflow"

printf '\n%s checks, %s failures\n' "$checks" "$failures"
[ "$failures" -eq 0 ]
