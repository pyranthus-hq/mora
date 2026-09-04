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

# local_endpoints_from parses `ifconfig` output into (interface, address) PAIRS,
# one per line, tab separated.
#
# It emits PAIRS, and it deduplicates by the pair rather than by the address.
# Two earlier bugs lived in the difference. Deduplicating addresses alone hid an
# address that is present on two interfaces, and stripping the IPv6 scope id
# turned `fe80::1%en0` into `fe80::1`, which is not routable without a zone: curl
# then failed for the WRONG reason, the failure was flattened to "unreachable",
# and the probe recorded a refusal it never observed. A link-local address keeps
# its zone here and carries it all the way into the URL.
#
# lo0's own link-local (fe80::1%lo0) is deliberately KEPT. The listener binds the
# literal 127.0.0.1, so a connection to any other address — including another
# address on the loopback interface — must be refused, and that is worth probing.
local_endpoints_from() {
  awk '
    /^[a-zA-Z][a-zA-Z0-9_.]*:/ { iface = substr($1, 1, length($1) - 1); next }
    $1 == "inet" && $2 != "127.0.0.1" { print iface "\t" $2 }
    $1 == "inet6" {
      split($2, a, "%")
      if (a[1] != "::1") print iface "\t" $2
    }
  ' | awk '!seen[$0]++'
}

local_endpoints() { ifconfig 2>/dev/null | local_endpoints_from; }

# endpoint_url builds the URL for one (interface, address) pair.
#
# A zoned IPv6 literal carries its zone into the URL with the percent sign
# percent-encoded as %25, which is what RFC 6874 requires and what curl parses.
# Passing the bare `%` produces a URL curl rejects, and dropping the zone
# produces one it cannot route — both of which would look like a refusal.
endpoint_url() {
  local addr=$1 port=$2
  case "$addr" in
    *%*) printf 'http://[%s]:%s/v1/companion/health\n' "${addr%%%*}%25${addr#*%}" "$port" ;;
    *:*) printf 'http://[%s]:%s/v1/companion/health\n' "$addr" "$port" ;;
    *)   printf 'http://%s:%s/v1/companion/health\n' "$addr" "$port" ;;
  esac
}

# classify_endpoint_result names what actually happened to one probe.
#
# There are exactly three answers and only ONE of them is the good one:
#
#   responded  the address answered HTTP at all. The port is reachable off
#              loopback. This is the defect the probe exists to find, and the
#              status code does not matter: a 401 from a LAN address is as bad
#              as a 200.
#   refused    the kernel refused the connection (curl exit 7 AND a refusal in
#              curl's own message). This is the only outcome that proves nothing
#              is listening there.
#   error      anything else: a timeout (28), an unresolvable or unroutable host
#              (6, or 7 with "No route to host"), a TLS failure, a bad URL. The
#              previous version mapped every one of these to the same "-" as a
#              refusal, so a probe that never reached the address counted as
#              proof that the address was closed.
#
# $1 curl exit status, $2 http_code, $3 curl stderr
classify_endpoint_result() {
  local status=$1 code=$2 err=$3
  case "$code" in
    ''|000) ;;
    *[!0-9]*) printf 'error\n'; return ;;
    *) printf 'responded\n'; return ;;
  esac
  if [ "$status" = "7" ]; then
    # Exit 7 covers BOTH "the kernel refused me" and "I could not get there",
    # and only the first proves the port is closed. curl does not separate them
    # by status, so the message is matched, in both directions and explicitly:
    # an unreachable wording wins, a refusal wording passes, and an exit 7 whose
    # wording is neither is an error rather than a guess. macOS curl says
    # "Couldn't connect to server" where Linux curl says "Connection refused";
    # both are the same ECONNREFUSED.
    case "$err" in
      *[Uu]nreachable*|*"No route to host"*|*"Network is down"*)
        printf 'error\n'; return ;;
      *[Rr]efused*|*"Couldn't connect to server"*|*"Could not connect to server"*)
        printf 'refused\n'; return ;;
    esac
  fi
  printf 'error\n'
}

# curl_probe runs one request and prints "<exit> <http_code> <stderr>".
#
# The stub seam exists so --self-test can drive the LAN probe with a timeout and
# with a 200 without needing a machine that produces either.
curl_probe() {
  local url=$1 iface=${2:-} out status
  if [ -n "${AUDIT_CURL_STUB:-}" ]; then
    "$AUDIT_CURL_STUB" "$url" "$iface"
    return
  fi
  # -S keeps curl's own message on stderr under -s; without it the message is
  # empty and the refusal cannot be told from an unreachable address at all.
  # --connect-timeout bounds an address that simply never answers, which is what
  # a link-local on an idle tunnel does.
  local args=(-sS -o /dev/null -m 6 --connect-timeout 2 -w '%{http_code}')
  if [ -n "$iface" ]; then
    args+=(--interface "$iface")
  fi
  out=$(curl "${args[@]}" -H "Authorization: Bearer ${SESSION_TOKEN:-}" "$url" 2>"$TMP_CURL_ERR") && status=0 || status=$?
  printf '%s %s %s\n' "$status" "${out:-000}" "$(tr '\n' ' ' <"$TMP_CURL_ERR")"
}

# probe_lan_endpoints drives every (interface, address) pair and reports.
#
# It distinguishes the three things that can happen, because collapsing them is
# exactly the defect this probe had:
#
#   refused    the kernel refused the connection. The port is closed there, and
#              this is the only outcome that PROVES it.
#   responded  the address answered HTTP at all. The boundary is violated, at
#              any status code. This is a FAILURE.
#   unreachable the connect never completed — a timeout, no route, an
#              unreachable network. It proves NOTHING in either direction, so it
#              is neither a pass nor a violation: the endpoint is BLOCKED.
#
# On a Mac with AirDrop or a VPN up the unreachable set is not empty and cannot
# be made empty: the fe80:: link-locals on awdl0 and on the utun interfaces
# never answer, and a tailnet IPv6 is blackholed by tailscaled rather than
# refused. Calling those a failure would make the audit permanently red for a
# reason that has nothing to do with the listener; calling them a refusal was
# the original bug. They are named, with interface, address, zone and curl exit,
# and they make the RUN partial.
#
# It prints one line per interesting endpoint plus a PROBED/REFUSED/BLOCKED
# tally, and it fails CLOSED: zero endpoints is a failure.
#
# Exit: 0 every endpoint refused; 1 at least one responded; 3 none responded but
# some were unreachable.
#
# Endpoints arrive on stdin as "interface<TAB>address" lines.
probe_lan_endpoints() {
  local port=$1 iface addr url zone verdict result status code err
  local seen=0 refused=0 responded=0 unreachable=0
  while IFS=$'\t' read -r iface addr; do
    [ -z "$addr" ] && continue
    seen=$((seen + 1))
    url=$(endpoint_url "$addr" "$port")
    zone=""
    # A link-local address needs an interface to leave the host at all, so curl
    # is told which one rather than being left to guess.
    case "$addr" in
      fe80:*|FE80:*) zone=${addr#*%}; [ "$zone" = "$addr" ] && zone=$iface ;;
    esac
    result=$(curl_probe "$url" "$zone")
    status=${result%% *}
    result=${result#* }
    code=${result%% *}
    err=${result#* }
    verdict=$(classify_endpoint_result "$status" "$code" "$err")
    case "$verdict" in
      refused) refused=$((refused + 1)) ;;
      responded)
        responded=$((responded + 1))
        printf 'REACHABLE %s %s answered HTTP %s\n' "$iface" "$addr" "$code"
        ;;
      *)
        unreachable=$((unreachable + 1))
        printf 'UNREACHABLE %s %s zone=%s curl exit %s (%s); no answer either way, so nothing is proved here\n' \
          "$iface" "$addr" "${zone:-none}" "$status" "${err:-no message}"
        ;;
    esac
  done
  if [ "$seen" -eq 0 ]; then
    printf 'UNREACHABLE no non-loopback address was found on any interface\n'
    return 1
  fi
  printf 'PROBED %s REFUSED %s RESPONDED %s UNREACHABLE %s\n' \
    "$seen" "$refused" "$responded" "$unreachable"
  [ "$responded" -eq 0 ] || return 1
  [ "$unreachable" -eq 0 ] || return 3
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
  TMP_CURL_ERR="$WORKDIR/curl.err"
  trap cleanup EXIT INT TERM

  NODE_FQDN=$(tailscale status --json | json_query "print(d['Self']['DNSName'].rstrip('.'))" '.Self.DNSName | rtrimstr(".")')
  [ -n "$NODE_FQDN" ] || die "tailscale reported no node name; is it logged in?"
  NODE_SHORT=${NODE_FQDN%%.*}
  TAILNET_SUFFIX=${NODE_FQDN#*.}
  TAILNET_IP=$(tailscale ip -4 2>/dev/null | head -1)
  [ -n "$TAILNET_IP" ] || die "tailscale reported no IPv4 address"
  LOCAL_ENDPOINTS=$(local_endpoints)
  # redact() folds every local address away, so a pasted report names interfaces
  # and outcomes but never an address on this machine.
  LOCAL_ADDRS=$(printf '%s\n' "$LOCAL_ENDPOINTS" | awk -F'\t' '{ split($2, a, "%"); print a[1] }' | tr '\n' ' ')
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

  local lan_report lan_rc=0 line tally
  lan_report=$(printf '%s\n' "$LOCAL_ENDPOINTS" | probe_lan_endpoints "$PORT") || lan_rc=$?
  tally=$(printf '%s\n' "$lan_report" | awk '$1 == "PROBED" { print }')
  case "$lan_rc" in
    0)
      pass "C2 every non-loopback endpoint on every interface EXPLICITLY refused the connection (${tally})"
      ;;
    3)
      # Not a violation and not a proof. An address that never answers proves
      # nothing in either direction, so the claim stays open rather than being
      # counted either way.
      blocked "C2 some non-loopback endpoints could not be reached at all; none answered (${tally})"
      note "An unreachable endpoint is not a refusal. On a Mac with AirDrop or a VPN up this set is"
      note "never empty: the fe80:: link-locals on awdl0 and the utun interfaces never answer, and a"
      note "tailnet IPv6 is blackholed by tailscaled rather than refused."
      while IFS= read -r line; do
        case "$line" in UNREACHABLE*) note "$line" ;; esac
      done <<EOF
$lan_report
EOF
      ;;
    *)
      fail "C2 a non-loopback address answered HTTP; the port is reachable off loopback (${tally})"
      while IFS= read -r line; do
        case "$line" in REACHABLE*|UNREACHABLE*) note "$line" ;; esac
      done <<EOF
$lan_report
EOF
      ;;
  esac

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

# expect_string asserts an exact value. $1 want, $2 label, $3 got.
expect_string() {
  local want=$1 label=$2 got=$3
  if [ "$got" = "$want" ]; then
    pass "ST $label"
  else
    fail "ST $label"
    note "got:  $got"
    note "want: $want"
  fi
}

# expect_count asserts the number of enumerated endpoints.
expect_count() {
  local want=$1 label=$2 endpoints=$3 got
  got=$(printf '%s\n' "$endpoints" | grep -c .)
  expect_string "$want" "$label" "$got"
}

# endpoints_contain reports whether the enumeration holds an exact pair.
# Invoked through expect_probe, which shellcheck does not follow.
# shellcheck disable=SC2329
endpoints_contain() {
  local endpoints=$1 want=$2 line
  while IFS= read -r line; do
    [ "$line" = "$want" ] && return 0
  done <<EOF
$endpoints
EOF
  return 1
}

# lan_probe_with runs the real LAN probe with a stubbed curl.
# $1 stub function name, $2 the endpoint list.
# Invoked through expect_probe, which shellcheck does not follow.
# shellcheck disable=SC2329
lan_probe_with() {
  local stub=$1 endpoints=$2 want=${3:-0} rc=0
  AUDIT_CURL_STUB=$stub
  printf '%s\n' "$endpoints" | probe_lan_endpoints 7778 >/dev/null || rc=$?
  AUDIT_CURL_STUB=""
  [ "$rc" = "$want" ]
}

# The curl stubs. Each prints "<exit> <http_code> <stderr>", the same three
# fields the real curl_probe prints.
# shellcheck disable=SC2329
stub_curl_refused() { printf '7 000 curl: (7) Failed to connect to %s: Connection refused\n' "$1"; }
# shellcheck disable=SC2329
stub_curl_timeout() { printf '28 000 curl: (28) Operation timed out after 6000 milliseconds\n'; }
# shellcheck disable=SC2329
stub_curl_200() { printf '0 200 \n'; }
# shellcheck disable=SC2329
stub_curl_401() { printf '0 401 \n'; }
# stub_curl_unroutable_zone refuses a zoned URL and times out on everything
# else. Before the zone was carried into the URL this shape passed, because a
# curl that could not route was recorded as a refusal.
# stub_curl_unknown_wording is an exit 7 whose message matches neither the
# refusal nor the unreachable vocabulary — the shape a future curl release could
# produce. It must degrade to unreachable, not to a pass.
# shellcheck disable=SC2329
stub_curl_unknown_wording() { printf '7 000 curl: (7) something new nobody has parsed yet\n'; }
# shellcheck disable=SC2329
stub_curl_unroutable_zone() {
  case "$1" in
    *%25*) printf '7 000 curl: (7) Failed to connect: No route to host\n' ;;
    *) printf "7 000 curl: (7) Couldn't connect to server\n" ;;
  esac
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

  # ---- The LAN probe ------------------------------------------------------
  #
  # Enumeration first. The fixture is real `ifconfig` text and carries the two
  # shapes the previous version got wrong: the same address on two interfaces,
  # and a link-local IPv6 with a scope id.
  local ifconfig_fixture endpoints
  ifconfig_fixture=$(cat <<'FIXTURE'
lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384
	inet 127.0.0.1 netmask 0xff000000
	inet6 ::1 prefixlen 128
	inet6 fe80::1%lo0 prefixlen 64 scopeid 0x1
en0: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 192.0.2.10 netmask 0xffffff00 broadcast 192.0.2.255
	inet6 fe80::dead:beef:cafe:1%en0 prefixlen 64 secured scopeid 0xc
	inet6 2001:db8::10 prefixlen 64 autoconf secured
en1: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	inet 192.0.2.10 netmask 0xffffff00 broadcast 192.0.2.255
utun3: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1280
	inet6 fe80::9f4b:1a2b:3c4d:5e6f%utun3 prefixlen 64 scopeid 0x10
FIXTURE
)
  endpoints=$(printf '%s\n' "$ifconfig_fixture" | local_endpoints_from)

  # ::1 and 127.0.0.1 are out; everything else is in, ONCE PER INTERFACE.
  expect_probe fail "the loopback address is not enumerated" \
    endpoints_contain "$endpoints" "lo0	127.0.0.1"
  expect_probe fail "::1 is not enumerated" \
    endpoints_contain "$endpoints" "lo0	::1"
  expect_probe pass "lo0's own link-local is still enumerated" \
    endpoints_contain "$endpoints" "lo0	fe80::1%lo0"
  expect_probe pass "a link-local IPv6 keeps its scope id" \
    endpoints_contain "$endpoints" "en0	fe80::dead:beef:cafe:1%en0"
  expect_probe pass "a tunnel link-local is enumerated too" \
    endpoints_contain "$endpoints" "utun3	fe80::9f4b:1a2b:3c4d:5e6f%utun3"
  expect_probe pass "the same address on a second interface is a second endpoint" \
    endpoints_contain "$endpoints" "en1	192.0.2.10"
  expect_probe pass "and the first one is still there" \
    endpoints_contain "$endpoints" "en0	192.0.2.10"
  expect_count 6 "six endpoints, deduplicated by PAIR and not by address" "$endpoints"

  # The URL a zoned address produces. `%` must reach curl as `%25`, or the URL
  # is rejected and the rejection looks exactly like a refusal.
  expect_string "http://[fe80::dead:beef:cafe:1%25en0]:7778/v1/companion/health" \
    "a zoned IPv6 endpoint is percent-encoded per RFC 6874" \
    "$(endpoint_url 'fe80::dead:beef:cafe:1%en0' 7778)"
  expect_string "http://[2001:db8::10]:7778/v1/companion/health" \
    "a plain IPv6 endpoint is bracketed" "$(endpoint_url '2001:db8::10' 7778)"
  expect_string "http://192.0.2.10:7778/v1/companion/health" \
    "an IPv4 endpoint is bare" "$(endpoint_url '192.0.2.10' 7778)"

  # Classification. Only an explicit refusal is a refusal.
  expect_string refused "curl exit 7 saying Connection refused is a refusal" \
    "$(classify_endpoint_result 7 000 'curl: (7) Failed to connect to 192.0.2.10 port 7778: Connection refused')"
  expect_string refused "curl exit 7 in macOS wording is the same refusal" \
    "$(classify_endpoint_result 7 000 "curl: (7) Failed to connect to 192.0.2.10 port 7778 after 1 ms: Couldn't connect to server")"
  expect_string error "curl exit 7 with no route to host is NOT a refusal" \
    "$(classify_endpoint_result 7 000 'curl: (7) Failed to connect: No route to host')"
  expect_string error "curl exit 7 with an unreachable network is NOT a refusal" \
    "$(classify_endpoint_result 7 000 'curl: (7) Failed to connect: Network is unreachable')"
  expect_string error "curl exit 7 with a host unreachable is NOT a refusal" \
    "$(classify_endpoint_result 7 000 'curl: (7) Failed to connect: Host is unreachable')"
  expect_string error "curl exit 7 with no message at all is NOT a refusal" \
    "$(classify_endpoint_result 7 000 '')"
  expect_string error "a timeout is not a refusal" \
    "$(classify_endpoint_result 28 000 'curl: (28) Operation timed out')"
  expect_string error "an unresolvable host is not a refusal" \
    "$(classify_endpoint_result 6 000 'curl: (6) Could not resolve host')"
  expect_string error "a malformed URL is not a refusal" \
    "$(classify_endpoint_result 3 000 'curl: (3) URL rejected')"
  expect_string responded "any HTTP status from the address is a response" \
    "$(classify_endpoint_result 0 200 '')"
  expect_string responded "a 401 from the address is still a response" \
    "$(classify_endpoint_result 0 401 '')"
  expect_string responded "a 500 from the address is still a response" \
    "$(classify_endpoint_result 0 500 '')"

  # And the probe end to end, driven through the curl seam.
  # Exit 0 = every endpoint refused, 1 = something answered, 3 = something was
  # unreachable and nothing answered. Each is asserted by the code it must
  # produce, so a timeout can neither pass as a refusal nor be mistaken for a
  # violation.
  expect_probe pass "every endpoint explicitly refused -> PASS (exit 0)" \
    lan_probe_with stub_curl_refused "$endpoints" 0
  expect_probe fail "every endpoint refused is NOT reported as unreachable" \
    lan_probe_with stub_curl_refused "$endpoints" 3
  expect_probe pass "a timeout is never a refusal: it BLOCKS (exit 3)" \
    lan_probe_with stub_curl_timeout "$endpoints" 3
  expect_probe fail "a timeout must not pass as a refusal" \
    lan_probe_with stub_curl_timeout "$endpoints" 0
  expect_probe pass "a 200 from a LAN address -> FAIL (exit 1)" \
    lan_probe_with stub_curl_200 "$endpoints" 1
  expect_probe fail "a 200 from a LAN address must not pass" \
    lan_probe_with stub_curl_200 "$endpoints" 0
  expect_probe fail "a 200 from a LAN address must not merely block" \
    lan_probe_with stub_curl_200 "$endpoints" 3
  expect_probe pass "a 401 from a LAN address -> FAIL (exit 1)" \
    lan_probe_with stub_curl_401 "$endpoints" 1
  expect_probe pass "no endpoints at all -> FAIL (nothing was audited)" \
    lan_probe_with stub_curl_refused "" 1
  # The regression this whole section exists for: the zoned link-local used to
  # lose its zone, curl failed to route, and the failure was counted as a
  # refusal. A stub that refuses only the UNZONED urls and cannot route the
  # zoned one passed then; it must not pass now.
  expect_probe fail "an unroutable zoned endpoint is no longer mistaken for a refusal" \
    lan_probe_with stub_curl_unroutable_zone "$endpoints" 0
  expect_probe pass "an unroutable zoned endpoint blocks instead (exit 3)" \
    lan_probe_with stub_curl_unroutable_zone "$endpoints" 3
  # A wording curl might change under us must degrade to unreachable, never to a
  # silent pass.
  expect_probe pass "an exit 7 with an unrecognized message blocks rather than passing" \
    lan_probe_with stub_curl_unknown_wording "$endpoints" 3

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
