#!/usr/bin/env bash
#
# The Contributor License Agreement gate for elk-work/ark.
#
# This replaces contributor-assistant/github-action@v2.6.1 (elk-work/ark#50).
# That action declares `runs.using: node20`, its upstream repository is
# archived, and the runner was already force-shimming it onto Node 24 — so
# there was nothing to bump to, and the check would have stopped running the
# day the shim went away.
#
# Driven by .github/workflows/cla.yml, which has two entry points:
#
#   pull_request_target (opened, synchronize)
#       Work out who has commits in the pull request, check each of them
#       against signatures/cla.json on `main` and against the allowlist, and
#       publish the verdict as the `cla-assistant` commit status on the pull
#       request's head commit.
#
#   issue_comment (created)
#       A comment whose whole body is the signing phrase records that
#       commenter's signature into signatures/cla.json on `main`; a comment
#       of `recheck` records nothing. Either way the verdict is recomputed
#       and the status republished.
#
# The verdict lives in a commit status rather than in this job's own success
# or failure because a status can be rewritten later. An `issue_comment` run
# is not attached to the pull request's head commit and cannot change the
# conclusion of the earlier `pull_request_target` job — which is exactly why
# the action this replaces re-ran entire workflow runs, and needed
# `actions: write` to do it. A status needs no such thing. So an unsigned
# pull request leaves this job green and the `cla-assistant` status red: the
# status is the gate, and it is the thing that can go green when somebody
# signs.
#
# --- Security -------------------------------------------------------------
#
# `pull_request_target` runs with a token that can write to this repository
# while the code under review belongs to a stranger. So:
#
#   * Nothing from the pull request's head is fetched, checked out, built or
#     executed, here or in the workflow. The workflow checks out `main` by
#     name; this script reads the head *SHA* as data, to hang a status on.
#   * Every value that starts life in the webhook payload arrives through the
#     environment, never by `${{ }}` interpolation into a shell command, and
#     is quoted at every use. Comment bodies, logins and git author names are
#     contributor-controlled.
#   * The path written to, the branch written on and the shape of the record
#     are constants below. No input reaches any of them, and the only webhook
#     string that reaches a commit message is a login that has had to match
#     the shape GitHub actually mints.
#   * Every JSON body sent to the API is built by `jq --arg`/`--argjson`, so
#     a hostile value stays data even if it were to get this far.
#   * The gate fails closed. The first status written is `pending`, `success`
#     is written on exactly one path — the one that has proved every
#     contributor signed — and any unexpected exit trips a trap that writes
#     `failure`.
#
# --- Testing --------------------------------------------------------------
#
# Everything above the "I/O" banner is pure: no network, no state. That is
# what scripts/cla-test.sh drives, and it is as close to end to end as this
# gets without an outside contributor's pull request to hand.

CLA_SIGNATURES_PATH=${CLA_SIGNATURES_PATH:-signatures/cla.json}
CLA_BRANCH=${CLA_BRANCH:-main}
CLA_STATUS_CONTEXT=${CLA_STATUS_CONTEXT:-cla-assistant}
CLA_DOCUMENT_URL=${CLA_DOCUMENT_URL:-https://github.com/elk-work/ark/blob/main/CLA.md}
CLA_SIGN_PHRASE=${CLA_SIGN_PHRASE:-I have read the CLA Document and I hereby sign the CLA}
CLA_ALLOWLIST=${CLA_ALLOWLIST:-}
CLA_TARGET_URL=${CLA_TARGET_URL:-}

# Invisible in the rendered comment, and the only thing that identifies the
# one comment this gate owns. Matching on prose, the way the action this
# replaces did, breaks the moment the prose is reworded.
CLA_COMMENT_MARKER='<!-- ark-cla-assistant -->'

# github-actions[bot]. Its commits are this gate's own signature commits, and
# it is in no position to sign a CLA. Dropped for the reason upstream dropped
# it.
CLA_ACTIONS_BOT_ID=41898282

# At most this many names in the "still to sign" list, so a pull request with
# a long history cannot produce a comment nobody can read.
CLA_MAX_LISTED=20

# ---------------------------------------------------------------------------
# Decision logic — pure, and unit-tested by scripts/cla-test.sh
# ---------------------------------------------------------------------------

cla_trim() {
	local s=$1
	s=${s#"${s%%[![:space:]]*}"}
	s=${s%"${s##*[![:space:]]}"}
	printf '%s' "$s"
}

cla_lower() {
	printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

# Is this comment body the signing phrase?
#
# Surrounding whitespace and letter case are forgiven — a comment box supplies
# the first and a person supplies the second — and nothing else is. A comment
# that merely *contains* the phrase is not a signature: the action this
# replaces wrapped its match in `.*` at both ends, so quoting the instructions
# back at the bot signed the agreement.
cla_is_signature() {
	local body want
	body=$(cla_lower "$(cla_trim "$1")")
	want=$(cla_lower "$CLA_SIGN_PHRASE")
	[ -n "$want" ] && [ "$body" = "$want" ]
}

cla_is_recheck() {
	local body
	body=$(cla_lower "$(cla_trim "$1")")
	[ "$body" = "recheck" ]
}

# Turn an allowlist entry into an anchored ERE in which `*` is the only
# wildcard and every other character is literal.
#
# This is not fussiness. `dependabot[bot]` is on this repository's allowlist,
# and both bash pattern matching and a naive regex read `[bot]` as a character
# class — so the entry would match `dependabotb` and would not match
# `dependabot[bot]`. Wrong in both directions, on a security control.
cla_glob_to_regex() {
	local pattern=$1 out='' i=0 ch
	while [ "$i" -lt "${#pattern}" ]; do
		ch=${pattern:$i:1}
		case $ch in
		'*') out="$out.*" ;;
		'.' | '^' | '$' | '+' | '?' | '(' | ')' | '[' | ']' | '{' | '}' | '|' | \\)
			out="$out\\$ch"
			;;
		*) out="$out$ch" ;;
		esac
		i=$((i + 1))
	done
	printf '^%s$' "$out"
}

# Is this login exempt from signing?
#
# Compared case-insensitively: GitHub logins are unique without regard to
# case, so no impostor can be minted that differs from an allowlisted name
# only in case, and a maintainer who writes `IJRoth` in the workflow gets what
# they meant rather than a silently dead entry.
cla_allowlisted() {
	local login=$1 csv=${2-} rest pattern re lc_login lc_pattern
	[ -n "$login" ] || return 1
	lc_login=$(cla_lower "$login")
	rest=$csv
	while [ -n "$rest" ]; do
		case $rest in
		*,*)
			pattern=${rest%%,*}
			rest=${rest#*,}
			;;
		*)
			pattern=$rest
			rest=''
			;;
		esac
		pattern=$(cla_trim "$pattern")
		if [ -z "$pattern" ]; then
			continue
		fi
		lc_pattern=$(cla_lower "$pattern")
		case $lc_pattern in
		*'*'*)
			re=$(cla_glob_to_regex "$lc_pattern")
			if [[ $lc_login =~ $re ]]; then
				return 0
			fi
			;;
		*)
			if [ "$lc_login" = "$lc_pattern" ]; then
				return 0
			fi
			;;
		esac
	done
	return 1
}

# One `login<TAB>id` line per person with commits in this pull request, from
# the payload of GET /repos/{owner}/{repo}/pulls/{n}/commits. `id` is empty
# when the commit's author is not linked to any GitHub account.
#
# A commit's GitHub *author* is preferred over its committer, so a
# maintainer's rebase is not read as authorship. Only committers are asked to
# sign: somebody who opens a pull request containing nobody's work but another
# person's has contributed nothing to license, and the person whose work it is
# is on this list either way.
cla_committers() {
	printf '%s' "$1" | jq -r --argjson botid "$CLA_ACTIONS_BOT_ID" '
		[ .[]
		  | (.author // .committer) as $user
		  | if $user == null
		    then { login: (.commit.author.name // .commit.committer.name // ""), id: null }
		    else { login: ($user.login // ""), id: $user.id }
		    end
		]
		| map(select(.login != ""))
		| map(select(.id != $botid))
		| unique_by(.login)
		| .[]
		| [ .login, (.id // "" | tostring) ]
		| @tsv
	'
}

# The GitHub user ids already in signatures/cla.json, one per line.
cla_signed_ids() {
	printf '%s' "$1" | jq -r '
		[ .signedContributors // [] | .[] | .id | select(. != null) ] | .[] | tostring
	'
}

cla_id_in_list() {
	local needle=$1 item
	for item in ${2-}; do
		if [ "$item" = "$needle" ]; then
			return 0
		fi
	done
	return 1
}

# The people who still have to sign, as `login<TAB>id` lines: every committer
# who is neither allowlisted nor already recorded. A committer with no GitHub
# account has no id, can never be matched against the file, and so always
# blocks — which is the honest answer, and the comment says how to fix it.
cla_outstanding() {
	local committer_lines=$1 signed_ids=$2 allowlist=$3
	local login id
	printf '%s\n' "$committer_lines" | while IFS=$'\t' read -r login id; do
		if [ -z "$login" ]; then
			continue
		fi
		# The allowlist names GitHub accounts, so it is only applied to a
		# committer who resolved to one. Where there is no account the
		# `login` is the commit's git `user.name` — free text the committer
		# chose — and honouring the allowlist there would let anyone through
		# with `git config user.name Claude` and an address linked to no
		# account. The action this replaces compares the same fallback name
		# against its allowlist and has the same hole.
		if [ -n "$id" ] && cla_allowlisted "$login" "$allowlist"; then
			continue
		fi
		if [ -n "$id" ] && cla_id_in_list "$id" "$signed_ids"; then
			continue
		fi
		printf '%s\t%s\n' "$login" "$id"
	done
}

# A commit author with no GitHub account contributes a free-text git
# `user.name`, which is contributor-controlled and about to be rendered into a
# Markdown comment. Keep it to characters that cannot become markup, a link or
# an @ mention, and keep it short.
cla_sanitize_display() {
	local s
	s=$(printf '%s' "$1" | LC_ALL=C tr -c 'A-Za-z0-9 ._-' '?')
	printf '%s' "${s:0:40}"
}

# The body of the one comment this gate owns.
cla_comment_body() {
	local outstanding=$1
	local text lines='' login id shown=0 unlinked=0 display

	if [ -z "$outstanding" ]; then
		printf '%s\n\nAll contributors have signed the CLA.\n' "$CLA_COMMENT_MARKER"
		return 0
	fi

	while IFS=$'\t' read -r login id; do
		if [ -z "$login" ] || [ "$shown" -ge "$CLA_MAX_LISTED" ]; then
			continue
		fi
		shown=$((shown + 1))
		if [ -n "$id" ]; then
			lines="$lines- @$login"$'\n'
		else
			unlinked=1
			display=$(cla_sanitize_display "$login")
			lines="$lines- \`$display\` — these commits are not linked to a GitHub account."$'\n'
		fi
	done <<CLA_OUTSTANDING
$outstanding
CLA_OUTSTANDING

	text="$CLA_COMMENT_MARKER"$'\n\n'
	text="${text}Thank you for the contribution. Before it can be merged, everyone with"$'\n'
	text="${text}commits in this pull request has to sign the"$'\n'
	text="${text}[Contributor License Agreement]($CLA_DOCUMENT_URL)."$'\n\n'
	text="${text}To sign it, post a comment on this pull request whose entire body is:"$'\n\n'
	text="${text}"'```'$'\n'"$CLA_SIGN_PHRASE"$'\n''```'$'\n\n'
	text="${text}Still to sign:"$'\n\n'"$lines"$'\n'
	if [ "$unlinked" -eq 1 ]; then
		text="${text}An author with no linked GitHub account can never satisfy this check."$'\n'
		text="${text}[Add the address you committed with to your account](https://docs.github.com/account-and-profile/setting-up-and-managing-your-personal-account-on-github/managing-email-preferences/adding-an-email-address-to-your-github-account),"$'\n'
		text="${text}then comment \`recheck\`."$'\n\n'
	fi
	text="${text}Comment \`recheck\` to run this check again."$'\n'
	printf '%s' "$text"
}

# ---------------------------------------------------------------------------
# I/O — everything below here talks to GitHub
# ---------------------------------------------------------------------------

cla_log() { printf 'cla: %s\n' "$*" >&2; }

cla_fail() {
	printf 'cla: %s\n' "$*" >&2
	exit 1
}

cla_need() {
	command -v "$1" >/dev/null 2>&1 || cla_fail "$1 is not on PATH"
}

# Set once the verdict has been published, so the exit trap can tell an
# orderly finish from a crash.
CLA_DONE=0
CLA_HEAD_SHA=${CLA_HEAD_SHA:-}
CLA_SIG_CONTENT=''
CLA_SIG_SHA=''
CLA_TMP=''

cla_set_status() {
	local state=$1 description=$2
	description=$(printf '%s' "$description" | cut -c1-140)
	jq -n \
		--arg state "$state" \
		--arg context "$CLA_STATUS_CONTEXT" \
		--arg description "$description" \
		--arg target_url "$CLA_TARGET_URL" '
		{ state: $state, context: $context, description: $description }
		+ (if $target_url == "" then {} else { target_url: $target_url } end)
	' | gh api -X POST "repos/$GH_REPO/statuses/$CLA_HEAD_SHA" --input - >/dev/null
}

cla_on_exit() {
	local code=$?
	# Fail closed. A crash anywhere after the head commit is known leaves the
	# status red rather than pending; a crash before that leaves no status at
	# all, which is still not a pass.
	if [ "$code" -ne 0 ] && [ "$CLA_DONE" -eq 0 ] && [ -n "$CLA_HEAD_SHA" ]; then
		cla_log "errored; leaving the $CLA_STATUS_CONTEXT status red"
		cla_set_status failure "CLA check errored — see the workflow run" || true
	fi
	if [ -n "$CLA_TMP" ]; then
		rm -rf "$CLA_TMP"
	fi
	exit "$code"
}

# Read signatures/cla.json from $CLA_BRANCH through the API rather than from
# the checkout, so a run that started before another run's signature commit
# still sees it. Sets CLA_SIG_CONTENT and CLA_SIG_SHA.
cla_read_signatures() {
	CLA_SIG_CONTENT=''
	CLA_SIG_SHA=''
	if gh api "repos/$GH_REPO/contents/$CLA_SIGNATURES_PATH?ref=$CLA_BRANCH" \
		>"$CLA_TMP/sig.json" 2>"$CLA_TMP/sig.err"; then
		CLA_SIG_SHA=$(jq -r '.sha // ""' "$CLA_TMP/sig.json")
		CLA_SIG_CONTENT=$(jq -r '.content | gsub("\n"; "") | @base64d' "$CLA_TMP/sig.json")
		# A file that will not parse is not an empty file. Stopping here keeps
		# a mangled commit from quietly un-signing everyone who has signed.
		printf '%s' "$CLA_SIG_CONTENT" | jq -e 'type == "object"' >/dev/null 2>&1 ||
			cla_fail "$CLA_BRANCH:$CLA_SIGNATURES_PATH is not a JSON object"
		return 0
	fi
	if grep -q 'HTTP 404' "$CLA_TMP/sig.err"; then
		cla_log "$CLA_BRANCH:$CLA_SIGNATURES_PATH does not exist yet; starting one"
		CLA_SIG_CONTENT='{"signedContributors":[]}'
		return 0
	fi
	cla_fail "could not read $CLA_BRANCH:$CLA_SIGNATURES_PATH — $(cat "$CLA_TMP/sig.err")"
}

# Append one signature to signatures/cla.json on $CLA_BRANCH and print the
# resulting file content.
#
# Printing it — rather than letting the caller re-read the branch — is
# deliberate: the contents API is not promised to serve a write straight back,
# and a stale read here would turn a signature that landed into a red check
# seconds after somebody signed.
cla_append_signature() {
	local pr=$1 login=$2 uid=$3 cid=$4 created=$5 repo_id=$6
	local attempt updated b64 message

	message="chore: $login has signed the CLA (#$pr)"

	for attempt in 1 2 3 4 5; do
		cla_read_signatures

		if printf '%s' "$CLA_SIG_CONTENT" | jq -e --argjson id "$uid" '
			[ .signedContributors // [] | .[] | .id ] | index($id) != null
		' >/dev/null; then
			cla_log "$login is already recorded; nothing to commit"
			printf '%s' "$CLA_SIG_CONTENT"
			return 0
		fi

		# Field names, field order and two-space indentation match what the
		# retired action wrote, because signatures/cla.json is a record that
		# has to keep working: nobody who has already signed may be asked
		# again.
		updated=$(printf '%s' "$CLA_SIG_CONTENT" | jq --indent 2 \
			--arg name "$login" \
			--argjson id "$uid" \
			--argjson comment_id "$cid" \
			--arg created_at "$created" \
			--argjson repoId "$repo_id" \
			--argjson pullRequestNo "$pr" '
			.signedContributors = ((.signedContributors // []) + [{
				name: $name,
				id: $id,
				comment_id: $comment_id,
				created_at: $created_at,
				repoId: $repoId,
				pullRequestNo: $pullRequestNo
			}])
		')

		b64=$(printf '%s' "$updated" | base64 | tr -d '\n')

		if jq -n \
			--arg message "$message" \
			--arg content "$b64" \
			--arg branch "$CLA_BRANCH" \
			--arg sha "$CLA_SIG_SHA" '
			{ message: $message, content: $content, branch: $branch }
			+ (if $sha == "" then {} else { sha: $sha } end)
		' | gh api -X PUT "repos/$GH_REPO/contents/$CLA_SIGNATURES_PATH" \
			--input - >/dev/null 2>"$CLA_TMP/put.err"; then
			cla_log "recorded $login in $CLA_BRANCH:$CLA_SIGNATURES_PATH"
			printf '%s' "$updated"
			return 0
		fi

		# Two runs signing at once collide on the blob SHA. Re-read and retry
		# rather than lose one of them.
		cla_log "signature commit attempt $attempt failed: $(cat "$CLA_TMP/put.err")"
		sleep $((attempt * 2))
	done

	cla_fail "could not record the signature in $CLA_BRANCH:$CLA_SIGNATURES_PATH"
}

cla_find_comment() {
	gh api --paginate "repos/$GH_REPO/issues/$1/comments?per_page=100" |
		jq -s 'add // []' |
		jq -r --arg marker "$CLA_COMMENT_MARKER" '
			[ .[] | select(.body != null and (.body | contains($marker))) | .id ] | first // ""
		'
}

# Create or update the one comment this gate owns. A comment is only ever
# created when something is outstanding, so a pull request that was fine all
# along never acquires one.
cla_upsert_comment() {
	local pr=$1 outstanding=$2
	local body existing
	body=$(cla_comment_body "$outstanding")
	existing=$(cla_find_comment "$pr")
	if [ -n "$existing" ]; then
		jq -n --arg body "$body" '{ body: $body }' |
			gh api -X PATCH "repos/$GH_REPO/issues/comments/$existing" --input - >/dev/null
	elif [ -n "$outstanding" ]; then
		jq -n --arg body "$body" '{ body: $body }' |
			gh api -X POST "repos/$GH_REPO/issues/$pr/comments" --input - >/dev/null
	fi
}

# Record the commenter's signature, if it is theirs to give. Prints the
# signature file content the caller should judge against.
cla_sign() {
	local pr=$1 outstanding=$2 signatures=$3
	local login=${CLA_COMMENT_USER_LOGIN:-}
	local uid=${CLA_COMMENT_USER_ID:-}
	local cid=${CLA_COMMENT_ID:-}
	local created=${CLA_COMMENT_CREATED_AT:-}
	local repo_id=${CLA_REPO_ID:-}
	local needle

	# Validated, not merely quoted. `login` is the one webhook string that
	# reaches a commit message, and the three numbers are what the record is
	# keyed by. A value that is not the shape GitHub mints is a bug or an
	# attack, and either way this stops rather than guesses.
	local login_re='^[A-Za-z0-9-]{1,39}(\[bot\])?$'
	local num_re='^[0-9]+$'
	local ts_re='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'
	[[ $login =~ $login_re ]] || cla_fail "comment author login is malformed"
	[[ $uid =~ $num_re ]] || cla_fail "comment author id is malformed"
	[[ $cid =~ $num_re ]] || cla_fail "comment id is malformed"
	[[ $created =~ $ts_re ]] || cla_fail "comment timestamp is malformed"
	[[ $repo_id =~ $num_re ]] || cla_fail "repository id is malformed"

	# You sign for yourself, on a pull request you contributed to. Without
	# this, anybody could add a row to the file from any open pull request.
	# `grep -F` because a bot login such as `dependabot[bot]` is a character
	# class to every regex engine there is.
	needle=$(printf '%s\t%s' "$login" "$uid")
	if ! printf '%s\n' "$outstanding" | grep -Fqx -- "$needle"; then
		cla_log "$login is not an outstanding committer on #$pr; recording nothing"
		printf '%s' "$signatures"
		return 0
	fi

	cla_append_signature "$pr" "$login" "$uid" "$cid" "$created" "$repo_id"
}

cla_main() {
	cla_need gh
	cla_need jq

	local event=${CLA_EVENT:-}
	case $event in
	pull_request_target | issue_comment) ;;
	*) cla_fail "unsupported event '$event'" ;;
	esac

	[ -n "${GH_REPO:-}" ] || cla_fail "GH_REPO is not set"

	local signing=0
	if [ "$event" = issue_comment ]; then
		# `issue_comment` fires for ordinary issues too, and an issue has no
		# CLA and must not be commented on. The workflow guards this as well;
		# so does this, because the workflow is not the only possible caller.
		if [ -z "${CLA_ISSUE_PR_URL:-}" ]; then
			cla_log "comment is on an issue, not a pull request; nothing to do"
			return 0
		fi
		if cla_is_signature "${CLA_COMMENT_BODY:-}"; then
			signing=1
		elif cla_is_recheck "${CLA_COMMENT_BODY:-}"; then
			signing=0
		else
			cla_log "comment is neither the signing phrase nor 'recheck'; nothing to do"
			return 0
		fi
	fi

	local pr=${CLA_PR_NUMBER:-}
	local num_re='^[0-9]+$'
	[[ $pr =~ $num_re ]] || cla_fail "pull request number is missing or malformed"

	CLA_TMP=$(mktemp -d)
	trap cla_on_exit EXIT

	if [ "$event" = issue_comment ]; then
		CLA_HEAD_SHA=$(gh api "repos/$GH_REPO/pulls/$pr" --jq '.head.sha')
	fi
	local sha_re='^[0-9a-f]{40}$'
	[[ $CLA_HEAD_SHA =~ $sha_re ]] || cla_fail "head SHA is missing or malformed"

	# Nothing is written as passing before the work is done.
	cla_set_status pending "Checking the CLA"

	local commits committers signatures signed_ids outstanding count
	commits=$(gh api --paginate "repos/$GH_REPO/pulls/$pr/commits?per_page=100" | jq -s 'add // []')
	committers=$(cla_committers "$commits")

	cla_read_signatures
	signatures=$CLA_SIG_CONTENT
	signed_ids=$(cla_signed_ids "$signatures")
	outstanding=$(cla_outstanding "$committers" "$signed_ids" "$CLA_ALLOWLIST")

	if [ "$signing" -eq 1 ]; then
		signatures=$(cla_sign "$pr" "$outstanding" "$signatures")
		signed_ids=$(cla_signed_ids "$signatures")
		outstanding=$(cla_outstanding "$committers" "$signed_ids" "$CLA_ALLOWLIST")
	fi

	# Guidance, not the gate: a pull request that is locked, or an API blip on
	# the comment endpoints, must not turn a signed contributor's check red.
	# The verdict below is computed from the signature file and nothing else.
	cla_upsert_comment "$pr" "$outstanding" ||
		cla_log "could not create or update the pull request comment"

	if [ -z "$outstanding" ]; then
		cla_set_status success "All contributors have signed the CLA"
		CLA_DONE=1
		cla_log "all contributors have signed"
		return 0
	fi

	count=$(printf '%s\n' "$outstanding" | grep -c . || true)
	cla_set_status failure "$count contributor(s) have yet to sign the CLA"
	CLA_DONE=1
	cla_log "still to sign:"
	printf '%s\n' "$outstanding" >&2
	return 0
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	set -euo pipefail
	cla_main "$@"
fi
