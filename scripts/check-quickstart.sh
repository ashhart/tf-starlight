#!/usr/bin/env bash
# check-quickstart.sh: runs README.md's Compose quick start as written, then proves what it started answers.
# Needs Docker with Compose, curl and Go; STARLIGHT_ENGINE names a TensorFold engine for the enrolled node.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
compose="$root/deploy/compose"
cd "$root"
: "${STARLIGHT_ENGINE:?STARLIGHT_ENGINE must name a TensorFold engine binary}"
if [ -e "$compose/secrets" ] || [ -e "$compose/.env" ]; then
  echo "$compose already holds a quick start's secrets or .env; remove them first" >&2
  exit 1
fi

# block N prints the Nth bash block under the quick start's heading, exactly as a reader copies it.
block() {
  awk -v want="$1" '
    /^## / { on = ($0 == "## Quick start with Docker Compose") }
    on && /^```bash$/ { if (++n == want) { code = 1 } ; next }
    code && /^```$/ { exit }
    code { print }' "$root/README.md"
}

work="$(mktemp -d)"
node_pid=""
cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    echo "::group::why it failed"
    (cd "$compose" && docker compose ps -a && docker compose logs --no-color --tail 100) || true
    [ -e "$work/node.log" ] && tail -n 40 "$work/node.log"
    echo "::endgroup::"
  fi
  [ -z "$node_pid" ] || kill "$node_pid" 2>/dev/null || true
  (cd "$compose" && docker compose down -v --remove-orphans) > /dev/null 2>&1 || true
  rm -rf "$compose/secrets" "$compose/.env" "$work"
  exit "$status"
}
trap cleanup EXIT

start="$(block 1)"
for step in ./bootstrap.sh "docker compose build" "starlight init" "docker compose up -d"; do
  grep -qF "$step" <<< "$start" || { echo "the README's quick start no longer runs $step; update this check" >&2; exit 1; }
done
echo "Running the README's quick start:"
echo "$start"
bash -eu -c "$start" 2>&1 | tee "$work/start.log"

base="http://localhost:8443"
# answers WHAT STATUS PATTERN URL [curl arguments]: the URL must answer with STATUS and a body or redirect matching PATTERN.
answers() {
  local what="$1" want="$2" pattern="$3" url="$4"
  shift 4
  local got
  got="$(curl -sS -o "$work/body" -w '%{http_code} %{redirect_url}' "$@" "$url")"
  { echo "$got"; cat "$work/body"; } > "$work/seen"
  if [ "${got%% *}" != "$want" ] || ! grep -qE "$pattern" "$work/seen"; then
    echo "$what: wanted $want matching $pattern from $url, got $got:" >&2
    head -c 2000 "$work/body" >&2
    exit 1
  fi
  echo "ok: $what answers $want"
}

for _ in $(seq 1 60); do
  curl -fsS -o /dev/null "$base/healthz" 2> /dev/null && break
  sleep 2
done
answers "liveness" 200 '^ok$' "$base/healthz"
answers "readiness" 200 '^ready$' "$base/readyz"
answers "the root" 303 "$base/console" "$base/"
answers "the console" 303 "$base/auth/login" "$base/console"
answers "the sign-in page" 200 'Example Corp' "$base/auth/login"
setup="$(grep -oE "$base/auth/setup\?code=[A-Za-z0-9_]+" "$work/start.log" | head -n 1)"
[ -n "$setup" ] || { echo "starlight init printed no setup link" >&2; exit 1; }
answers "the setup link" 200 'Set up single sign-on' "$setup"
answers "discovery" 200 '"org_name":"Example Corp"' "$base/.well-known/tensorfold-starlight"
answers "the models API, without a key" 401 'invalid_api_key' "$base/v1/models"
answers "the chat API, with a bad key" 401 'invalid_api_key' "$base/v1/chat/completions" \
  -H 'Authorization: Bearer slr_not_a_key' -H 'Content-Type: application/json' -d '{"model":"none","messages":[]}'

cd "$compose"
docker compose exec -T starlight starlight doctor
echo "ok: the doctor passes"

# The README's next steps: Starlight prints an enrolment command, and a machine runs it as printed.
printed="$(bash -eu -c "$(block 2)" | tail -n 1)"
case "$printed" in
  "starlight-node enroll "*) ;;
  *) echo "Starlight printed no enrolment command: $printed" >&2; exit 1 ;;
esac
(cd "$root" && go build -o "$work/bin/starlight-node" ./cmd/starlight-node)
export PATH="$work/bin:$PATH" STARLIGHT_NODE_STATE_DIR="$work/node" STARLIGHT_NODE_ENGINE="$STARLIGHT_ENGINE"
bash -eu -c "$printed"
starlight-node run > "$work/node.log" 2>&1 &
node_pid=$!
for _ in $(seq 1 30); do
  report="$(docker compose exec -T starlight starlight doctor)"
  grep -q '1 online' <<< "$report" && break
  sleep 2
done
echo "$report"
grep -q '1 online' <<< "$report" || { echo "the enrolled node never came online" >&2; exit 1; }
echo "ok: the node enrolled with the printed command and came online"

verified="$(docker compose exec -T starlight starlight audit verify)"
echo "$verified"
grep -qE '^verified [1-9]' <<< "$verified" || { echo "the audit chain holds no events" >&2; exit 1; }
echo "ok: the audit chain verifies"
