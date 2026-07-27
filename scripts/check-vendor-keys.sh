#!/usr/bin/env bash
#
# check-vendor-keys.sh — CI guard against the "silently dropped vendor API
# key" bug class found in Sprint 15 (2026-07-23): cmd/langstream/main.go's
# init() unconditionally registers one real vendor backend per pipeline leg
# (ASR/MT/TTS), each of whose constructor reads its own <VENDOR>_API_KEY
# env var (see e.g. pkg/asr/deepgram.go's NewDeepgramRecognizer). Those keys
# only ever reach a real container if docker-compose.yml's `environment:`
# block explicitly passes them through — nothing enforced the two stayed in
# sync, so GEMINI_API_KEY and ELEVENLABS_API_KEY were added as vendor
# backends without ever being added to docker-compose.yml, and a real
# deployment setting either env var would have it quietly vanish before
# reaching the container (no error, no log line — just a backend that
# always fails to construct as if the operator never set the key at all).
#
# This script closes that gap in both directions:
#   1. A vendor backend is registered in main.go's init() (i.e. its
#      constructor reads some XXX_API_KEY) but docker-compose.yml's
#      environment: block doesn't pass that key through.
#   2. docker-compose.yml passes through an XXX_API_KEY that no
#      constructor reachable from main.go's init() actually reads anymore
#      (a stale leftover from a removed/renamed vendor backend).
#
# It does NOT parse Go with a real parser — main.go's init() body and each
# vendor constructor's body are read as gofmt'd text and scanned with
# grep/awk for the `pkg.NewXxx()` call pattern and the
# `os.Getenv("XXX_API_KEY")` pattern respectively. This is pragmatic, not
# bulletproof: it assumes gofmt's standard "top-level function/brace at
# column 0" layout (true throughout this repo; `make fmt`/CI's gofmt-check
# step enforce it) and that each vendor constructor reads its key directly
# via a literal os.Getenv("XXX_API_KEY") call (true for all six vendors
# today: see pkg/asr/deepgram.go, pkg/asr/sarvam.go,
# pkg/translate/gpt4o.go, pkg/translate/gemini.go, pkg/tts/cartesia.go,
# pkg/tts/elevenlabs.go). If either assumption stops holding for some
# future backend, the script fails loudly (exit 2) rather than silently
# passing, and says why.
#
# Usage: scripts/check-vendor-keys.sh
# Exit codes: 0 = in sync, 1 = mismatch found (see stderr), 2 = the script
# itself couldn't parse something it expected to find (repo layout changed
# out from under it).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MAIN_GO="$REPO_ROOT/cmd/langstream/main.go"
COMPOSE_FILE="$REPO_ROOT/docker-compose.yml"

fail_parse() {
	echo "check-vendor-keys: $1" >&2
	exit 2
}

[[ -f "$MAIN_GO" ]] || fail_parse "cannot find $MAIN_GO"
[[ -f "$COMPOSE_FILE" ]] || fail_parse "cannot find $COMPOSE_FILE"

# --- Step 1: extract the body of main.go's init() function ---------------
init_body="$(awk '
	/^func init\(\) \{/ { flag=1 }
	flag { print }
	/^\}/ { if (flag) exit }
' "$MAIN_GO")"

[[ -n "$init_body" ]] || fail_parse "could not locate 'func init() {' in $MAIN_GO -- vendor backend registration may have moved; update this script's assumptions"

# --- Step 2: find pkg.NewXxx() vendor-constructor calls inside init() -----
calls="$(grep -oE '[a-zA-Z_][a-zA-Z0-9_]*\.New[A-Za-z0-9]+\(\)' <<<"$init_body" | sort -u || true)"

[[ -n "$calls" ]] || fail_parse "found init() in $MAIN_GO but no vendor constructor calls (pkg.NewXxx()) inside it -- registration pattern changed, nothing to check"

# --- Step 3: for each constructor, find the XXX_API_KEY it reads ----------
declare -A main_keys=()   # env var names required per main.go's init()

while IFS= read -r call; do
	pkg="${call%%.*}"
	fn="${call#*.}"
	fn="${fn%()}"

	dir="$REPO_ROOT/pkg/$pkg"
	[[ -d "$dir" ]] || fail_parse "package dir $dir (for call $call in main.go's init()) does not exist"

	found_key=""
	for f in "$dir"/*.go; do
		[[ -f "$f" ]] || continue
		case "$f" in *_test.go) continue ;; esac
		grep -q "^func $fn(" "$f" || continue

		body="$(awk -v fn="$fn" '
			$0 ~ "^func " fn "\\(" { flag=1 }
			flag { print }
			/^\}/ { if (flag) exit }
		' "$f")"

		key="$(grep -oE 'os\.Getenv\("[A-Z0-9_]+_API_KEY"\)' <<<"$body" | grep -oE '[A-Z0-9_]+_API_KEY' | head -n1 || true)"
		if [[ -n "$key" ]]; then
			found_key="$key"
			break
		fi
	done

	[[ -n "$found_key" ]] || fail_parse "could not find an *_API_KEY env var read inside $call's constructor (searched $dir/*.go) -- update this script or check that vendor's constructor"

	main_keys["$found_key"]=1
done <<<"$calls"

# --- Step 4: parse docker-compose.yml's environment: block ---------------
compose_block="$(awk '
	/^[[:space:]]*environment:[[:space:]]*$/ { flag=1; next }
	flag && /^[[:space:]]{0,4}[A-Za-z_-]+:/ { exit }
	flag { print }
' "$COMPOSE_FILE")"

[[ -n "$compose_block" ]] || fail_parse "could not find a non-empty 'environment:' block in $COMPOSE_FILE"

declare -A compose_keys=()
while IFS= read -r key; do
	[[ -n "$key" ]] && compose_keys["$key"]=1
done < <(grep -oE '^[[:space:]]*-[[:space:]]*[A-Z0-9_]+_API_KEY=' <<<"$compose_block" | grep -oE '[A-Z0-9_]+_API_KEY' || true)

[[ ${#compose_keys[@]} -gt 0 ]] || fail_parse "found environment: block in $COMPOSE_FILE but no *_API_KEY entries in it"

# --- Step 5: diff both directions -----------------------------------------
missing_in_compose=()
for k in "${!main_keys[@]}"; do
	[[ -n "${compose_keys[$k]+x}" ]] || missing_in_compose+=("$k")
done

stale_in_compose=()
for k in "${!compose_keys[@]}"; do
	[[ -n "${main_keys[$k]+x}" ]] || stale_in_compose+=("$k")
done

status=0

if [[ ${#missing_in_compose[@]} -gt 0 ]]; then
	status=1
	echo "check-vendor-keys: FAIL -- vendor backend(s) registered in cmd/langstream/main.go's init() need the following env var(s), which docker-compose.yml's environment: block does NOT pass through:" >&2
	for k in "${missing_in_compose[@]}"; do
		echo "  - $k" >&2
	done
	echo "  Fix: add '- NAME=\${NAME:-}' for each key above to docker-compose.yml's environment: block (see the six existing entries for the pattern), or a real deployment setting that env var will have it silently vanish before reaching the container." >&2
fi

if [[ ${#stale_in_compose[@]} -gt 0 ]]; then
	status=1
	echo "check-vendor-keys: FAIL -- docker-compose.yml's environment: block passes through the following env var(s), which no vendor constructor reachable from cmd/langstream/main.go's init() reads anymore:" >&2
	for k in "${stale_in_compose[@]}"; do
		echo "  - $k" >&2
	done
	echo "  Fix: remove the stale entry from docker-compose.yml's environment: block, or if a backend genuinely still uses it, check why this script's grep-based scan of pkg/*/*.go didn't find it (renamed constructor / renamed env var / new package?)." >&2
fi

if [[ $status -eq 0 ]]; then
	echo "check-vendor-keys: OK -- ${#main_keys[@]} vendor API key(s) required by cmd/langstream/main.go's init() are all present in docker-compose.yml's environment: block, with no stale entries."
fi

exit $status
