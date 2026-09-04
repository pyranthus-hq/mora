#!/usr/bin/env bash
#
# companion-network-audit.sh — prove the companion listener's network boundary.
#
# `mora companion serve` binds 127.0.0.1 and nothing else, and `mora companion
# expose` publishes it to a tailnet through Tailscale Serve. That arrangement is
# only as good as three claims, and this script exists because all three are
# claims about a running system rather than about source code:
#
#   1. the socket is on loopback, so the LAN cannot reach it at all;
#   2. the ONLY way in is the one Serve mapping the operator created, and
#      Funnel — which would publish it to the public internet — is off;
#   3. the listener writes no credential, request, prompt, answer or vault text
#      to its log, so publishing it does not turn its terminal into a leak.
#
# It also RECORDS a property that is not fixed and must not be pretended away:
# behind Serve the client IP does not survive the proxy. The listener sees
# 127.0.0.1 for every phone, so a per-IP throttle would collapse into a single
# bucket shared by every device. That is the N04 finding; probe E states it with
# evidence rather than leaving it to be rediscovered.
#
# Every probe FAILS CLOSED. A missing tool, an empty command output or an
# unparseable line is a FAIL, never a skip, because "we could not look" and
# "we looked and it was fine" are the two answers an audit must never confuse.
#
# Usage:
#   scripts/companion-network-audit.sh --self-test
#   scripts/companion-network-audit.sh [--port N] [--tailnet-port N] [--manage-serve]
#
# --self-test runs the probe logic against fixtures that MUST fail, including a
# real listener bound to 0.0.0.0 and a log line containing the session token, so
# a probe that has silently stopped checking anything cannot report PASS.
#
# The live mode drives a complete session in a throwaway MORA_CONFIG_DIR: it
# pairs a device, calls today, context and health, and revokes. It never reads or
# writes the operator's real vault.

set -euo pipefail

PORT=7778
TAILNET_PORT=8080
MANAGE_SERVE=0
SELF_TEST=0

PASSES=0
FAILURES=0
SERVE_STARTED=0
LISTENER_PID=""
WORKDIR=""

# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------

# redact rewrites every value that identifies this machine or its tailnet. The
# audit output is meant to be pasted into a pull request, and a node name, a
# tailnet name or a LAN address is a fact about someone's network.
redact() {
  local text=$1
  if [ -n "${TAILNET_SUFFIX:-}" ]; then
    text=${text//${TAILNET_SUFFIX}/TAILNET.ts.net}
  fi
  if [ -n "${NODE_FQDN:-}" ]; then
    text=${text//${NODE_FQDN}/NODE.TAILNET.ts.net}
  fi
  if [ -n "${NODE_SHORT:-}" ]; then
    text=${text//${NODE_SHORT}/NODE}
  fi
  if [ -n "${LAN_IP:-}" ]; then
    text=${text//${LAN_IP}/LAN.IP.REDACTED}
  fi
  if [ -n "${TAILNET_IP:-}" ]; then
    text=${text//${TAILNET_IP}/TAILNET.IP.REDACTED}
  fi
  printf '%s\n' "$text"
}

note() { redact "    $*"; }

pass() {
  PASSES=$((PASSES + 1))
  redact "PASS  $*"
}

fail() {
  FAILURES=$((FAILURES + 1))
  redact "FAIL  $*"
}

die() {
  redact "FATAL $*" >&2
  exit 2
}

# ---------------------------------------------------------------------------
# The probe logic
#
# Each probe is a pure function over text so --self-test can feed it a fixture.
# The live mode gathers the text; the logic is the same code in both modes,
# which is the only way a self-test proves anything about the real run.
# ---------------------------------------------------------------------------

# probe_bind: every listening socket on the port must be the literal loopback
# address. An empty listing fails: no socket means nothing was audited.
#
# $1 lsof output, $2 port
probe_bind() {
  local listing=$1 port=$2 line addr bad=0 seen=0
  if [ -z "${listing//[[:space:]]/}" ]; then
    return 1
  fi
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    case "$line" in COMMAND*) continue ;; esac
    # The address is the NAME column: the last field before the optional
    # "(LISTEN)" state, e.g. "127.0.0.1:7778".
    addr=$(printf '%s\n' "$line" | awk '{for (i=NF;i>0;i--) if ($i ~ /:/) {print $i; exit}}')
    [ -z "$addr" ] && { bad=1; continue; }
    seen=$((seen + 1))
    case "$addr" in
      "127.0.0.1:$port") ;;
      *) bad=1 ;;
    esac
  done <<EOF
$listing
EOF
  [ "$seen" -gt 0 ] || return 1
  [ "$bad" -eq 0 ] || return 1
  return 0
}

# probe_serve: `tailscale serve status` must describe exactly one proxy target,
# it must be this listener, and no line may enable Funnel.
#
# $1 serve status output, $2 port
probe_serve() {
  local status=$1 port=$2 proxies mine funnel
  if [ -z "${status//[[:space:]]/}" ]; then
    return 1
  fi
  case "$status" in *"No serve config"*) return 1 ;; esac
  proxies=$(printf '%s\n' "$status" | grep -c 'proxy http://' || true)
  mine=$(printf '%s\n' "$status" | grep -c "proxy http://127\.0\.0\.1:${port}\$" || true)
  # Funnel appears in this output as "(Funnel on)" beside the mapping. Any
  # mention at all is a failure: this listener is a tailnet service, and the
  # audit refuses to reason about a partial match.
  funnel=$(printf '%s\n' "$status" | grep -ci 'funnel' || true)
  [ "$proxies" -eq 1 ] || return 1
  [ "$mine" -eq 1 ] || return 1
  [ "$funnel" -eq 0 ] || return 1
  return 0
}

# probe_log: the log must contain none of the session's secrets or payloads.
#
# $1 log text, remaining args the exact strings used in the session.
probe_log() {
  local log=$1 marker
  shift
  for marker in "$@"; do
    [ -z "$marker" ] && return 1
    case "$log" in
      *"$marker"*) return 1 ;;
    esac
  done
  return 0
}

# ---------------------------------------------------------------------------
# Live session
# ---------------------------------------------------------------------------

# Invoked through `trap` in run_live, which shellcheck does not follow.
# shellcheck disable=SC2329
cleanup() {
  local status=$?
  if [ -n "$LISTENER_PID" ] && kill -0 "$LISTENER_PID" 2>/dev/null; then
    kill "$LISTENER_PID" 2>/dev/null || true
    wait "$LISTENER_PID" 2>/dev/null || true
  fi
  if [ "$SERVE_STARTED" -eq 1 ]; then
    tailscale serve "--http=${TAILNET_PORT}" off >/dev/null 2>&1 || true
  fi
  if [ -n "$WORKDIR" ] && [ -d "$WORKDIR" ]; then
    rm -rf "$WORKDIR"
  fi
  exit "$status"
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is not installed; the audit cannot prove anything without it"
}

# start_listener runs `mora companion serve` with its log captured, and waits for
# the banner. $1 is the --allow-host value, empty for the loopback-only control.
start_listener() {
  local allow=$1 deadline
  : >"$WORKDIR/listener.log"
  if [ -n "$allow" ]; then
    "$WORKDIR/mora" companion serve --port "$PORT" --allow-host "$allow" \
      >>"$WORKDIR/listener.log" 2>&1 &
  else
    "$WORKDIR/mora" companion serve --port "$PORT" \
      >>"$WORKDIR/listener.log" 2>&1 &
  fi
  LISTENER_PID=$!
  deadline=$((SECONDS + 15))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if grep -q 'listening on' "$WORKDIR/listener.log" 2>/dev/null; then
      return 0
    fi
    if ! kill -0 "$LISTENER_PID" 2>/dev/null; then
      redact "$(cat "$WORKDIR/listener.log")" >&2
      die "the listener exited before it was listening"
    fi
    sleep 0.2
  done
  die "the listener never printed its banner"
}

stop_listener() {
  if [ -n "$LISTENER_PID" ]; then
    kill "$LISTENER_PID" 2>/dev/null || true
    wait "$LISTENER_PID" 2>/dev/null || true
    LISTENER_PID=""
  fi
}

# http_status prints the response status for one request, or "-" when the
# connection itself failed. A refused connection is a legitimate outcome the
# LAN probe depends on, so it is reported rather than treated as an error.
http_status() {
  local url=$1 status
  status=$(curl -s -o /dev/null -m 6 -w '%{http_code}' \
    -H "Authorization: Bearer ${SESSION_TOKEN}" "$url" 2>/dev/null) || status="-"
  [ -z "$status" ] && status="-"
  printf '%s\n' "$status"
}

run_live() {
  need lsof
  need curl
  need tailscale
  need go

  WORKDIR=$(mktemp -d)
  trap cleanup EXIT INT TERM

  NODE_FQDN=$(tailscale status --json | sed -n 's/.*"DNSName"[[:space:]]*:[[:space:]]*"\([^"]*\)\.".*/\1/p' | head -1)
  [ -n "$NODE_FQDN" ] || die "tailscale reported no node name; is it logged in?"
  NODE_SHORT=${NODE_FQDN%%.*}
  TAILNET_SUFFIX=${NODE_FQDN#*.}
  TAILNET_IP=$(tailscale ip -4 2>/dev/null | head -1)
  [ -n "$TAILNET_IP" ] || die "tailscale reported no IPv4 address"
  LAN_IP=$(ipconfig getifaddr en0 2>/dev/null || true)

  # The session's secrets and payloads. These exact strings are what probe D
  # greps for, so they are generated per run: a marker baked into the script
  # could match a stale log and pass for the wrong reason.
  SESSION_TOKEN="audit-token-$(openssl rand -hex 16)"
  SESSION_PROMPT="AUDITPROMPT$(openssl rand -hex 8)"
  VAULT_MARKER="AUDITVAULT$(openssl rand -hex 8)"

  redact "companion network audit"
  note "node        NODE.TAILNET.ts.net"
  note "listener    http://127.0.0.1:${PORT}"
  note "published   http://NODE.TAILNET.ts.net:${TAILNET_PORT}"
  echo

  go build -o "$WORKDIR/mora" ./cmd/mora || die "could not build mora"

  export MORA_CONFIG_DIR="$WORKDIR/home"
  mkdir -p "$MORA_CONFIG_DIR"
  "$WORKDIR/mora" init >/dev/null || die "mora init failed"
  "$WORKDIR/mora" write --scope global --type fact \
    --title "audit canary" --text "$VAULT_MARKER" >/dev/null || die "mora write failed"

  # Pair. The pairing code is a REAL live secret, and it is one of the strings
  # probe D looks for, which is what stops that probe from being vacuous.
  local pair_json device_id
  pair_json=$("$WORKDIR/mora" companion pair --label "audit" --json) || die "mora companion pair failed"
  device_id=$(printf '%s\n' "$pair_json" | sed -n 's/.*"device_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
  PAIRING_CODE=$(printf '%s\n' "$pair_json" | sed -n 's/.*"pairing_code"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
  [ -n "$device_id" ] && [ -n "$PAIRING_CODE" ] || die "could not read the pairing payload"

  # ---- Probe A: the socket -------------------------------------------------
  start_listener "$NODE_FQDN:$TAILNET_PORT"
  local bind_listing
  bind_listing=$(lsof -nP -a -p "$LISTENER_PID" "-iTCP:$PORT" -sTCP:LISTEN 2>/dev/null || true)
  if probe_bind "$bind_listing" "$PORT"; then
    pass "A  the listener socket is bound to 127.0.0.1:${PORT} and nothing else"
  else
    fail "A  the listener has a socket that is not 127.0.0.1:${PORT}"
    note "$bind_listing"
  fi

  # ---- Probe B: the only ingress ------------------------------------------
  local serve_status
  if [ "$MANAGE_SERVE" -eq 1 ]; then
    serve_status=$(tailscale serve status 2>&1 || true)
    case "$serve_status" in
      *"No serve config"*) ;;
      *) die "--manage-serve refuses to touch an existing serve configuration" ;;
    esac
    tailscale serve --bg "--http=${TAILNET_PORT}" "http://127.0.0.1:${PORT}" >/dev/null \
      || die "tailscale serve failed"
    SERVE_STARTED=1
  fi
  serve_status=$(tailscale serve status 2>&1 || true)
  if probe_serve "$serve_status" "$PORT"; then
    pass "B  exactly one Tailscale Serve mapping, it targets this listener, Funnel is off"
  else
    fail "B  the serve configuration is not exactly one Funnel-free mapping to this listener"
    note "$serve_status"
  fi

  # ---- Probe C: who can reach it ------------------------------------------
  local via_tailnet via_lan
  via_tailnet=$(http_status "http://${NODE_FQDN}:${TAILNET_PORT}/v1/companion/health")
  if [ "$via_tailnet" = "401" ]; then
    pass "C1 a tailnet request reaches the listener (401: the guard passed, the token is not a real device token)"
  else
    fail "C1 a tailnet request did not reach the listener as expected (status ${via_tailnet}, want 401)"
  fi

  if [ -n "$LAN_IP" ]; then
    via_lan=$(http_status "http://${LAN_IP}:${PORT}/v1/companion/health")
    if [ "$via_lan" = "-" ]; then
      pass "C2 a request to this Mac's LAN address is refused at the socket — there is nothing listening there"
    else
      fail "C2 the LAN address answered with status ${via_lan}; the port is reachable off loopback"
    fi
  else
    fail "C2 no LAN address was found, so the LAN could not be probed (fail closed)"
  fi

  # ---- Probe D: the log ----------------------------------------------------
  # A full session first: the three routes, then a revoke.
  curl -s -o /dev/null -m 6 -H "Authorization: Bearer ${SESSION_TOKEN}" \
    "http://${NODE_FQDN}:${TAILNET_PORT}/v1/companion/today" || true
  curl -s -o /dev/null -m 6 -X POST \
    -H "Authorization: Bearer ${SESSION_TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"schema\":\"mora.companion.context.request\",\"schema_version\":1,\"mode\":\"ask\",\"query\":\"${SESSION_PROMPT}\"}" \
    "http://${NODE_FQDN}:${TAILNET_PORT}/v1/companion/context" || true
  curl -s -o /dev/null -m 6 -H "Authorization: Bearer ${SESSION_TOKEN}" \
    "http://${NODE_FQDN}:${TAILNET_PORT}/v1/companion/health" || true
  "$WORKDIR/mora" companion revoke "$device_id" >/dev/null || true

  local log_text
  log_text=$(cat "$WORKDIR/listener.log")
  if probe_log "$log_text" "$SESSION_TOKEN" "$PAIRING_CODE" "$SESSION_PROMPT" "$VAULT_MARKER" "$device_id"; then
    pass "D  the listener log holds no token, pairing code, prompt, answer or vault text"
  else
    fail "D  the listener log holds a session secret or payload"
  fi
  # The published name is not a secret, but a log an operator can paste without
  # redacting is worth more than one they have to remember to scrub.
  if probe_log "$log_text" "$NODE_FQDN"; then
    pass "D2 the listener log does not name the published host either"
  else
    fail "D2 the listener log carries the published host name"
  fi

  # ---- Probe E: the client IP ---------------------------------------------
  # This is a RECORDED LIMITATION, not a solved problem.
  #
  # The proof is the 401 above. The listener admits a proxied request only when
  # the Host matches AND the peer is loopback, so a 401 rather than a 403 is a
  # direct witness that the peer tailscaled dialled from was 127.0.0.1 — the
  # phone's address did not survive the proxy. The control below is the other
  # half: with no --allow-host the same request is refused at the Host guard,
  # which is the N04 finding that made this flag necessary.
  stop_listener
  start_listener ""
  local control
  control=$(http_status "http://${NODE_FQDN}:${TAILNET_PORT}/v1/companion/health")
  if [ "$control" = "403" ]; then
    pass "E1 without --allow-host the same tailnet request is 403 forbidden_host (Serve forwards the client's Host verbatim)"
  else
    fail "E1 the loopback-only control returned ${control}, want 403"
  fi
  if [ "$via_tailnet" = "401" ]; then
    pass "E2 with --allow-host the request passes the guard, which requires a LOOPBACK peer — the client IP does not survive Serve"
    note "N04 stands: a per-IP throttle behind Serve collapses to one bucket for every device."
    note "The real client is carried only in X-Forwarded-For, which is untrusted input; the"
    note "listener reads neither it nor the Tailscale-User-* identity headers."
  else
    fail "E2 could not witness the loopback peer, because the guarded request did not return 401"
  fi
  stop_listener
}

# ---------------------------------------------------------------------------
# Self-test
# ---------------------------------------------------------------------------

# expect_probe asserts a probe's verdict on a fixture. $1 is "pass" or "fail".
expect_probe() {
  local want=$1 label=$2
  shift 2
  if "$@"; then
    if [ "$want" = "pass" ]; then pass "ST $label"; else fail "ST $label (a must-fail fixture was accepted)"; fi
  else
    if [ "$want" = "fail" ]; then pass "ST $label"; else fail "ST $label (a valid fixture was rejected)"; fi
  fi
}

run_self_test() {
  need lsof
  need python3
  redact "companion network audit — self-test"
  echo

  # A REAL listener bound to 0.0.0.0, audited by the real gathering path. A
  # fixture string would only prove the parser; this proves the probe.
  local bad_port=17999 pid listing
  python3 -c "
import socket, sys, time
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(('0.0.0.0', int(sys.argv[1])))
s.listen(1)
time.sleep(30)
" "$bad_port" &
  pid=$!
  sleep 1
  listing=$(lsof -nP -a -p "$pid" "-iTCP:$bad_port" -sTCP:LISTEN 2>/dev/null || true)
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  if [ -z "${listing//[[:space:]]/}" ]; then
    fail "ST could not observe the 0.0.0.0 fixture listener; the bind probe is unproven"
  else
    expect_probe fail "a real listener bound to 0.0.0.0 is rejected" probe_bind "$listing" "$bad_port"
  fi

  expect_probe pass "a loopback-only listing is accepted" \
    probe_bind "mora 1 me 3u IPv4 0x1 0t0 TCP 127.0.0.1:7778 (LISTEN)" 7778
  expect_probe fail "an empty listing is rejected (nothing was audited)" \
    probe_bind "" 7778
  expect_probe fail "a wildcard listing is rejected" \
    probe_bind "mora 1 me 3u IPv4 0x1 0t0 TCP *:7778 (LISTEN)" 7778
  expect_probe fail "a second non-loopback socket is rejected" \
    probe_bind "$(printf 'mora 1 me 3u IPv4 0x1 0t0 TCP 127.0.0.1:7778 (LISTEN)\nmora 1 me 4u IPv4 0x2 0t0 TCP 192.0.2.10:7778 (LISTEN)')" 7778

  expect_probe pass "one mapping to the listener is accepted" \
    probe_serve "$(printf 'http://node.example:8080 (tailnet only)\n|-- / proxy http://127.0.0.1:7778')" 7778
  expect_probe fail "no serve config is rejected" \
    probe_serve "No serve config" 7778
  expect_probe fail "empty serve output is rejected" \
    probe_serve "" 7778
  expect_probe fail "a second mapping is rejected" \
    probe_serve "$(printf '|-- / proxy http://127.0.0.1:7778\n|-- /x proxy http://127.0.0.1:9999')" 7778
  expect_probe fail "a mapping to another port is rejected" \
    probe_serve "|-- / proxy http://127.0.0.1:9999" 7778
  expect_probe fail "Funnel is rejected" \
    probe_serve "$(printf 'https://node.example (Funnel on)\n|-- / proxy http://127.0.0.1:7778')" 7778

  local token="audit-token-deadbeef"
  expect_probe pass "a clean log is accepted" \
    probe_log "$(printf 'mora companion serve listening on http://127.0.0.1:7778/  (loopback only)\n  routes: GET /v1/companion/today')" "$token" "AUDITPROMPT1" "AUDITVAULT1"
  expect_probe fail "a log line containing the token is rejected" \
    probe_log "$(printf 'listening\n  auth Bearer %s\n' "$token")" "$token" "AUDITPROMPT1" "AUDITVAULT1"
  expect_probe fail "a log line containing the prompt is rejected" \
    probe_log "$(printf 'listening\n  query=AUDITPROMPT1\n')" "$token" "AUDITPROMPT1" "AUDITVAULT1"
  expect_probe fail "a log line containing vault text is rejected" \
    probe_log "$(printf 'listening\n  answer: AUDITVAULT1\n')" "$token" "AUDITPROMPT1" "AUDITVAULT1"
  expect_probe fail "an empty marker is rejected (a probe with nothing to look for proves nothing)" \
    probe_log "listening" ""
}

# ---------------------------------------------------------------------------
# Entry
# ---------------------------------------------------------------------------

while [ $# -gt 0 ]; do
  case "$1" in
    --self-test) SELF_TEST=1 ;;
    --manage-serve) MANAGE_SERVE=1 ;;
    --port) shift; PORT=${1:-} ;;
    --tailnet-port) shift; TAILNET_PORT=${1:-} ;;
    -h|--help)
      sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *) die "unknown argument $1" ;;
  esac
  shift
done

case "$PORT" in ''|*[!0-9]*) die "--port must be a number" ;; esac
case "$TAILNET_PORT" in ''|*[!0-9]*) die "--tailnet-port must be a number" ;; esac

if [ "$SELF_TEST" -eq 1 ]; then
  run_self_test
else
  run_live
fi

echo
if [ "$FAILURES" -eq 0 ] && [ "$PASSES" -gt 0 ]; then
  redact "VERDICT PASS — ${PASSES} probes, 0 failures"
  exit 0
fi
redact "VERDICT FAIL — ${PASSES} passed, ${FAILURES} failed"
exit 1
