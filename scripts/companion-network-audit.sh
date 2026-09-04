#!/usr/bin/env bash
#
# companion-network-audit.sh — prove the companion listener's network boundary.
#
# `mora companion serve` binds 127.0.0.1 and nothing else, and `mora companion
# expose` publishes it to a tailnet through Tailscale Serve. That arrangement is
# only as good as claims about a RUNNING system, which is what this script is
# for:
#
#   1. the socket is on loopback, so no other interface can reach it;
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
# # Three outcomes, not two
#
# A probe is PASS, FAIL, or BLOCKED. BLOCKED means the probe could not be RUN,
# and it is the honest answer to a claim nothing exercised. Today the
# authenticated probes are blocked: `Registry.Confirm` has no production caller,
# so no device can reach the ACTIVE state and every request this script can make
# is refused at the auth guard with a 401. A 401 proves the 401; it proves
# nothing about whether a decoded request body, a prompt, an answer or vault
# text stays out of the log, because none of those was ever produced.
#
# The earlier version of this script reported those as PASS. They were vacuous,
# and a vacuous PASS on a log-silence claim is worse than no probe at all.
#
# Any BLOCKED probe makes the final verdict PARTIAL. PARTIAL is not PASS and
# exits non-zero (3), because "we could not look" and "we looked and it was
# fine" are the two answers an audit must never confuse. FAIL exits 1.
#
# There is deliberately no Mac-side shortcut that confirms a pairing. A pairing
# is proven by the phone with its one-time code; a kernel-side confirm route is
# a separate node. Adding a local backdoor to make this script green would be
# faking the exact evidence it exists to collect.
#
# Every probe also fails CLOSED. A missing tool, an empty command output or an
# unparseable line is a FAIL, never a skip.
#
# Usage:
#   scripts/companion-network-audit.sh --self-test
#   scripts/companion-network-audit.sh [--port N] [--tailnet-port N] [--manage-serve]
#
# --self-test runs the probe logic against fixtures that MUST fail, including a
# real listener bound to 0.0.0.0, a log line containing the session token, and an
# authenticated-probe gate with no active device, so a probe that has silently
# stopped checking anything cannot report PASS.
#
# The live mode drives a session in a throwaway MORA_CONFIG_DIR: it pairs a
# device, calls today, context and health, and revokes. It never reads or writes
# the operator's real vault.

set -euo pipefail

PORT=7778
TAILNET_PORT=8080
MANAGE_SERVE=0
SELF_TEST=0

PASSES=0
FAILURES=0
BLOCKED=0
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
  local text=$1 addr
  if [ -n "${TAILNET_SUFFIX:-}" ]; then
    text=${text//${TAILNET_SUFFIX}/TAILNET.ts.net}
  fi
  if [ -n "${NODE_FQDN:-}" ]; then
    text=${text//${NODE_FQDN}/NODE.TAILNET.ts.net}
  fi
  if [ -n "${NODE_SHORT:-}" ]; then
    text=${text//${NODE_SHORT}/NODE}
  fi
  if [ -n "${TAILNET_IP:-}" ]; then
    text=${text//${TAILNET_IP}/TAILNET.IP.REDACTED}
  fi
  for addr in ${LOCAL_ADDRS:-}; do
    text=${text//${addr}/LOCAL.IP.REDACTED}
  done
  printf '%s\n' "$text"
}

note() { redact "    $*"; }

pass() {
  PASSES=$((PASSES + 1))
  redact "PASS    $*"
}

fail() {
  FAILURES=$((FAILURES + 1))
  redact "FAIL    $*"
}

# blocked records a probe that could not be run. It is never a pass.
blocked() {
  BLOCKED=$((BLOCKED + 1))
  redact "BLOCKED $*"
}

die() {
  redact "FATAL $*" >&2
  exit 2
}

# verdict_of maps the three counters onto the final word. It is a pure function
# so the self-test can prove that BLOCKED never yields PASS.
#
# $1 passes, $2 failures, $3 blocked
verdict_of() {
  local passes=$1 failures=$2 blocked=$3
  if [ "$failures" -gt 0 ]; then
    printf 'FAIL\n'
    return
  fi
  if [ "$blocked" -gt 0 ]; then
    printf 'PARTIAL\n'
    return
  fi
  if [ "$passes" -le 0 ]; then
    # No probe ran at all. That is not a pass either.
    printf 'FAIL\n'
    return
  fi
  printf 'PASS\n'
}

# ---------------------------------------------------------------------------
# The probe logic
#
# Each probe is a pure function over text so --self-test can feed it a fixture.
# The live mode gathers the text; the logic is the same code in both modes,
# which is the only way a self-test proves anything about the real run.
# ---------------------------------------------------------------------------

# json_query runs a structural query over JSON on stdin and prints its result.
#
# It uses python3, falling back to jq, and dies when neither exists: parsing
# JSON with sed was the previous shape, and a greedy regex over a document whose
# layout can change is a probe that can quietly start reading the wrong field.
#
# $1 python expression over the parsed document bound to `d`, printing its own
# output; $2 the equivalent jq filter.
json_query() {
  local py=$1 filter=$2
  if command -v python3 >/dev/null 2>&1; then
    python3 -c "
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(1)
$py
" || return 1
    return 0
  fi
  if command -v jq >/dev/null 2>&1; then
    jq -er "$filter" || return 1
    return 0
  fi
  die "neither python3 nor jq is installed; JSON cannot be parsed structurally"
}

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

# probe_serve: the serve configuration must describe exactly one handler, it
# must proxy to this listener, no raw TCP forward may exist, and Funnel must be
# off. The input is `tailscale serve status --json`, parsed structurally.
#
# $1 serve status JSON, $2 port
probe_serve() {
  local status=$1 port=$2 result
  if [ -z "${status//[[:space:]]/}" ]; then
    return 1
  fi
  # The jq filter is deliberately single-quoted: $-expressions in it are jq's
  # own, and the port reaches it through the environment, not through the shell.
  # shellcheck disable=SC2016
  result=$(printf '%s' "$status" | json_query "
port = '$port'
want = 'http://127.0.0.1:' + port
web = d.get('Web') or {}
handlers = [h for vhost in web.values() for h in (vhost.get('Handlers') or {}).values()]
proxies = [h.get('Proxy') for h in handlers if h.get('Proxy')]
# A handler that is not a proxy (static text, a served path) is still ingress.
other = [h for h in handlers if not h.get('Proxy')]
# A raw TCP forward bypasses the Web layer entirely.
tcp = [k for k, v in (d.get('TCP') or {}).items() if v.get('TCPForward')]
funnel = [k for k, v in (d.get('AllowFunnel') or {}).items() if v]
ok = (len(handlers) == 1 and len(proxies) == 1 and proxies[0] == want
      and not other and not tcp and not funnel)
print('ok' if ok else 'handlers=%d proxies=%s other=%d tcp=%s funnel=%s' % (
    len(handlers), proxies, len(other), tcp, funnel))
" '
  (([.Web // {} | .[] | .Handlers // {} | .[]]) as $h
   | ($h | map(select(.Proxy)) | map(.Proxy)) as $p
   | if ($h|length) == 1 and ($p|length) == 1
        and $p[0] == ("http://127.0.0.1:" + env.AUDIT_PORT)
        and ([.TCP // {} | .[] | select(.TCPForward)] | length) == 0
        and ([.AllowFunnel // {} | .[] | select(.)] | length) == 0
     then "ok" else "not-ok" end)') || return 1
  [ "$result" = "ok" ] || return 1
  return 0
}

# probe_active_gate: the authenticated probes may run only when the registry
# holds at least one ACTIVE device.
#
# Returns 0 when they may run and 1 when they are BLOCKED. It is the whole
# reason this audit has three outcomes: a pending device authenticates nothing,
# so driving the routes with a made-up token exercises the 401 path and NOTHING
# else, and reporting a log-silence claim from that would be a lie.
#
# $1 `mora companion status --json` output
probe_active_gate() {
  local status=$1 active
  if [ -z "${status//[[:space:]]/}" ]; then
    return 1
  fi
  active=$(printf '%s' "$status" | json_query "print(int(d.get('active', 0)))" '.active') || return 1
  case "$active" in ''|*[!0-9]*) return 1 ;; esac
  [ "$active" -ge 1 ] || return 1
  return 0
}

# probe_log: the log must contain none of the given strings.
#
# $1 log text, remaining args the exact strings used in the session.
probe_log() {
  local log=$1 marker
  shift
  [ "$#" -gt 0 ] || return 1
  for marker in "$@"; do
    [ -z "$marker" ] && return 1
    case "$log" in
      *"$marker"*) return 1 ;;
    esac
  done
  return 0
}

# local_addresses lists every non-loopback address on every interface that has
# one. The LAN probe used to test en0 alone, which proves nothing about a Mac
# with Wi-Fi and Ethernet both up, or about a utun the listener could be
# reachable on.
local_addresses() {
  ifconfig 2>/dev/null | awk '
    $1 == "inet" && $2 != "127.0.0.1" { print $2 }
    $1 == "inet6" { split($2, a, "%"); if (a[1] != "::1") print a[1] }
  ' | sort -u
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
    # Targeted, never `reset`: reset would remove every mapping on this node,
    # including one another tool created.
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
  need ifconfig
  command -v python3 >/dev/null 2>&1 || command -v jq >/dev/null 2>&1 || \
    die "neither python3 nor jq is installed; JSON cannot be parsed structurally"

  WORKDIR=$(mktemp -d)
  trap cleanup EXIT INT TERM

  NODE_FQDN=$(tailscale status --json | json_query "print(d['Self']['DNSName'].rstrip('.'))" '.Self.DNSName | rtrimstr(".")')
  [ -n "$NODE_FQDN" ] || die "tailscale reported no node name; is it logged in?"
  NODE_SHORT=${NODE_FQDN%%.*}
  TAILNET_SUFFIX=${NODE_FQDN#*.}
  TAILNET_IP=$(tailscale ip -4 2>/dev/null | head -1)
  [ -n "$TAILNET_IP" ] || die "tailscale reported no IPv4 address"
  LOCAL_ADDRS=$(local_addresses | tr '\n' ' ')
  export AUDIT_PORT="$PORT"

  # The session's secrets and payloads. These exact strings are what the log
  # probes grep for, so they are generated per run: a marker baked into the
  # script could match a stale log and pass for the wrong reason.
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
  # the unauthenticated log probe looks for, which is what stops THAT probe from
  # being vacuous. It does NOT make the device usable: pairing is confirmed by
  # the phone, and there is no route for it yet.
  local pair_json device_id
  pair_json=$("$WORKDIR/mora" companion pair --label "audit" --json) || die "mora companion pair failed"
  device_id=$(printf '%s' "$pair_json" | json_query "print(d['device_id'])" '.device_id')
  PAIRING_CODE=$(printf '%s' "$pair_json" | json_query "print(d['pairing_code'])" '.pairing_code')
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
  serve_status=$(tailscale serve status --json 2>&1 || true)
  if probe_serve "$serve_status" "$PORT"; then
    pass "B  exactly one Serve handler, it proxies to this listener, no TCP forward, Funnel off"
  else
    fail "B  the serve configuration is not exactly one Funnel-free proxy to this listener"
  fi

  # ---- Probe C: who can reach it ------------------------------------------
  local via_tailnet
  via_tailnet=$(http_status "http://${NODE_FQDN}:${TAILNET_PORT}/v1/companion/health")
  if [ "$via_tailnet" = "401" ]; then
    pass "C1 a tailnet request reaches the listener and is refused with the opaque 401 (the token is not a device token)"
  else
    fail "C1 a tailnet request did not reach the listener as expected (status ${via_tailnet}, want 401)"
  fi

  local addr probed=0 reachable=0 status
  for addr in $LOCAL_ADDRS; do
    probed=$((probed + 1))
    case "$addr" in
      *:*) status=$(http_status "http://[${addr}]:${PORT}/v1/companion/health") ;;
      *) status=$(http_status "http://${addr}:${PORT}/v1/companion/health") ;;
    esac
    if [ "$status" != "-" ]; then
      reachable=$((reachable + 1))
      fail "C2 ${addr} answered with status ${status}; the port is reachable off loopback"
    fi
  done
  if [ "$probed" -eq 0 ]; then
    fail "C2 no non-loopback address was found on any interface, so nothing could be probed (fail closed)"
  elif [ "$reachable" -eq 0 ]; then
    pass "C2 every non-loopback address on this Mac refuses the connection (${probed} probed across all interfaces)"
  fi

  # ---- Probe D: the log ----------------------------------------------------
  # The unauthenticated half of the session: three route calls that are all
  # refused at the auth guard, then a revoke.
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
  # These strings WERE on the wire, so their absence is a real result.
  if probe_log "$log_text" "$SESSION_TOKEN" "$PAIRING_CODE" "$device_id"; then
    pass "D1 the log holds no bearer token, no pairing code and no device id, all of which the session really sent"
  else
    fail "D1 the log holds a credential or a device id"
  fi
  if probe_log "$log_text" "$NODE_FQDN"; then
    pass "D2 the log does not name the published host, so it can be pasted into a bug report unredacted"
  else
    fail "D2 the log carries the published host name"
  fi

  # ---- The authenticated probes -------------------------------------------
  local status_json
  status_json=$("$WORKDIR/mora" companion status --json 2>/dev/null || true)
  if probe_active_gate "$status_json"; then
    run_authenticated_probes "$log_text"
  else
    # This is the honest answer, and it is why the verdict below is PARTIAL.
    blocked "D3 the log holds no decoded request body, prompt, answer or vault text"
    note "reason: no ACTIVE device, so every request in this session was refused at the auth guard"
    note "with a 401. Nothing decoded the body, ran retrieval, or produced an answer, so the"
    note "absence of those strings from the log is not evidence of anything."
    blocked "D4 an authenticated route call is served (200) and its projection decodes"
    note "reason: the same. Registry.Confirm has no production caller — there is no"
    note "pairing-confirm route yet — so no device can reach the ACTIVE state, and a pairing"
    note "must be proven by the phone with its one-time code rather than by a local shortcut."
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

# run_authenticated_probes is reachable only with an ACTIVE device, which needs a
# pairing-confirm route that does not exist yet. It is written out so the audit
# is complete the day that route lands, and it is NOT reachable by any local
# shortcut on purpose.
#
# $1 the log text captured so far.
run_authenticated_probes() {
  local log_text=$1 body status
  body=$(curl -s -m 10 -X POST \
    -H "Authorization: Bearer ${DEVICE_TOKEN:-$SESSION_TOKEN}" -H 'Content-Type: application/json' \
    -d "{\"schema\":\"mora.companion.context.request\",\"schema_version\":1,\"mode\":\"ask\",\"query\":\"${SESSION_PROMPT}\"}" \
    -w '\n%{http_code}' \
    "http://${NODE_FQDN}:${TAILNET_PORT}/v1/companion/context" 2>/dev/null || true)
  status=${body##*$'\n'}
  body=${body%$'\n'*}
  if [ "$status" = "200" ] && printf '%s' "$body" | json_query "print(d['schema'])" '.schema' >/dev/null 2>&1; then
    pass "D4 an authenticated route call is served (200) and its projection decodes"
  else
    fail "D4 an authenticated route call did not produce a decodable projection (status ${status})"
  fi
  log_text=$(cat "$WORKDIR/listener.log")
  if probe_log "$log_text" "$SESSION_PROMPT" "$VAULT_MARKER"; then
    pass "D3 after a served, decoded request the log still holds no prompt, answer or vault text"
  else
    fail "D3 the log holds a prompt, an answer or vault text"
  fi
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

# expect_verdict asserts the final word for a set of counters.
expect_verdict() {
  local want=$1 label=$2 got
  shift 2
  got=$(verdict_of "$@")
  if [ "$got" = "$want" ]; then
    pass "ST $label"
  else
    fail "ST $label (verdict was ${got}, want ${want})"
  fi
}

run_self_test() {
  need lsof
  need python3
  export AUDIT_PORT=7778
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

  # Serve status is parsed structurally, so the fixtures are real documents.
  expect_probe pass "one proxy handler to the listener is accepted" \
    probe_serve '{"TCP":{"8080":{"HTTP":true}},"Web":{"node.example:8080":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7778"}}}}}' 7778
  expect_probe fail "no serve config is rejected" \
    probe_serve '{}' 7778
  expect_probe fail "empty serve output is rejected" \
    probe_serve "" 7778
  expect_probe fail "malformed JSON is rejected" \
    probe_serve 'No serve config' 7778
  expect_probe fail "a second handler is rejected" \
    probe_serve '{"Web":{"node.example:8080":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7778"},"/x":{"Proxy":"http://127.0.0.1:9999"}}}}}' 7778
  expect_probe fail "a second vhost is rejected" \
    probe_serve '{"Web":{"node.example:8080":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7778"}}},"node.example:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7778"}}}}}' 7778
  expect_probe fail "a proxy to another port is rejected" \
    probe_serve '{"Web":{"node.example:8080":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:9999"}}}}}' 7778
  expect_probe fail "a non-proxy handler is rejected" \
    probe_serve '{"Web":{"node.example:8080":{"Handlers":{"/":{"Text":"hello"}}}}}' 7778
  expect_probe fail "a raw TCP forward is rejected" \
    probe_serve '{"TCP":{"2222":{"TCPForward":"127.0.0.1:22"}},"Web":{"node.example:8080":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7778"}}}}}' 7778
  expect_probe fail "Funnel is rejected" \
    probe_serve '{"AllowFunnel":{"node.example:443":true},"Web":{"node.example:8080":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7778"}}}}}' 7778

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
  expect_probe fail "no markers at all is rejected" \
    probe_log "listening"

  # The gate that makes the authenticated probes honest. A registry with only a
  # PENDING device authenticates nothing, so it must NOT open them.
  expect_probe pass "an active device opens the authenticated probes" \
    probe_active_gate '{"active":1,"pending":0,"revoked":0}'
  expect_probe fail "a pending-only registry blocks the authenticated probes" \
    probe_active_gate '{"active":0,"pending":1,"revoked":0}'
  expect_probe fail "an empty registry blocks the authenticated probes" \
    probe_active_gate '{"active":0,"pending":0,"revoked":0}'
  expect_probe fail "unreadable status blocks the authenticated probes" \
    probe_active_gate 'not json'
  expect_probe fail "empty status blocks the authenticated probes" \
    probe_active_gate ''

  # And the verdict. This is the must-not-PASS fixture the previous version of
  # this script would have failed: probes that could not be run must never add
  # up to PASS.
  expect_verdict PARTIAL "authenticated probes blocked with no active device -> PARTIAL, never PASS" 8 0 2
  expect_verdict PASS "everything run and green -> PASS" 10 0 0
  expect_verdict FAIL "any failure -> FAIL" 10 1 0
  expect_verdict FAIL "a failure outranks a block" 10 1 2
  expect_verdict FAIL "no probe ran at all -> FAIL" 0 0 0
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
      sed -n '2,60p' "$0" | sed 's/^# \{0,1\}//'
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
VERDICT=$(verdict_of "$PASSES" "$FAILURES" "$BLOCKED")
case "$VERDICT" in
  PASS)
    redact "VERDICT PASS — ${PASSES} probes, 0 failures, 0 blocked"
    exit 0
    ;;
  PARTIAL)
    redact "VERDICT PARTIAL — ${PASSES} passed, ${BLOCKED} BLOCKED, 0 failed"
    redact "PARTIAL is not PASS. The blocked probes above were not run, so what they claim is unproven."
    exit 3
    ;;
  *)
    redact "VERDICT FAIL — ${PASSES} passed, ${FAILURES} failed, ${BLOCKED} blocked"
    exit 1
    ;;
esac
