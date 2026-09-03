#!/usr/bin/env bash
#
# generate-ca.example.sh — example CA-bundle generator for mcp-gateway.
#
# mcp-gateway's `ca_bundle` feature is trust-store- and OS-agnostic: the gateway
# only runs this command and injects the resulting bundle path into every
# backend's environment (SSL_CERT_FILE, REQUESTS_CA_BUNDLE, NODE_EXTRA_CA_CERTS).
# ALL knowledge of where trusted certificates come from lives here, in your
# generator — never in the gateway.
#
# Contract
# --------
# The gateway invokes this script with a subcommand and sets CA_BUNDLE_PATH in
# the environment:
#
#   $0 check    -> print "CURRENT" if the existing bundle is still up to date,
#                  or anything else (e.g. "STALE") to request regeneration.
#                  Should be FAST (target < 100ms): it runs on every gateway
#                  start. Prefer a cheap change signal (file mtimes, a store
#                  version, an ETag) over re-reading all certificates.
#
#   $0 bundle   -> (re)write the PEM bundle to "$CA_BUNDLE_PATH". This runs only
#                  when `check` reports non-CURRENT, so it may be slower.
#
# Any non-CURRENT output, a non-zero exit, or a timeout is treated as "stale"
# and triggers `bundle` (fail-safe).
#
# This example gathers roots from the operating system trust store. Adapt the
# source to your environment (a corporate PKI endpoint, a mounted secret, a
# vault, etc.). The "change signal" here is the set of source-file mtimes.
#
set -euo pipefail

: "${CA_BUNDLE_PATH:?CA_BUNDLE_PATH must be set by mcp-gateway}"
STATE_FILE="${CA_BUNDLE_PATH}.state"

# ---------------------------------------------------------------------------
# Configure your trust-store sources here.
#
# Example for macOS keychains (system roots + admin/MDM roots). On Linux you
# might point at /etc/ssl/certs/ca-certificates.crt, or curl a corporate PKI
# bundle, etc.
# ---------------------------------------------------------------------------
SOURCES=(
  "/System/Library/Keychains/SystemRootCertificates.keychain"
  "/Library/Keychains/System.keychain"
)

# Optional: additional roots to include, filtered by an identifying substring.
# Leave EXTRA_SOURCE empty to skip. Example: include only your org's roots from
# the user login keychain so you don't trust arbitrary user-imported certs.
EXTRA_SOURCE=""                 # e.g. "${HOME}/Library/Keychains/login.keychain-db"
EXTRA_MATCH="my-org|internal"   # case-insensitive subject/issuer substrings

# mtime helper (prefers GNU stat if present).
_mtime() {
  if command -v gstat >/dev/null 2>&1; then gstat -c '%Y' "$1" 2>/dev/null || echo 0
  else stat -f '%m' "$1" 2>/dev/null || stat -c '%Y' "$1" 2>/dev/null || echo 0; fi
}

# A cheap signature of the current source state: the mtimes of all source files.
current_signature() {
  local sig=""
  for s in "${SOURCES[@]}" ${EXTRA_SOURCE:+"$EXTRA_SOURCE"}; do
    [ -e "$s" ] && sig="${sig}$(_mtime "$s"):" || sig="${sig}0:"
  done
  printf '%s' "$sig"
}

cmd_check() {
  if [ ! -s "$CA_BUNDLE_PATH" ] || [ ! -f "$STATE_FILE" ]; then
    echo "STALE"; return 0
  fi
  [ "$(current_signature)" = "$(cat "$STATE_FILE" 2>/dev/null || true)" ] \
    && echo "CURRENT" || echo "STALE"
}

cmd_bundle() {
  local tmp; tmp="$(mktemp)"; trap 'rm -f "$tmp"' EXIT

  # macOS keychain export example. Replace with your own source retrieval.
  for s in "${SOURCES[@]}"; do
    security find-certificate -a -p "$s" >> "$tmp" 2>/dev/null || true
  done

  # Optional filtered extra source (slow per-cert path; only runs on change).
  if [ -n "$EXTRA_SOURCE" ] && [ -e "$EXTRA_SOURCE" ]; then
    security find-certificate -a -p "$EXTRA_SOURCE" 2>/dev/null | awk -v pat="$EXTRA_MATCH" '
      BEGIN { RS="-----END CERTIFICATE-----\n" }
      NF {
        block = $0 (RT ? RT : "")
        cmd = "printf %s \"" block "\" | openssl x509 -noout -subject -issuer 2>/dev/null"
        cmd | getline meta; close(cmd)
        if (tolower(meta) ~ tolower(pat)) printf "%s", block
      }' >> "$tmp" || true
  fi

  [ -s "$tmp" ] || { echo "generate-ca: produced empty bundle" >&2; exit 1; }

  mkdir -p "$(dirname "$CA_BUNDLE_PATH")"
  mv "$tmp" "$CA_BUNDLE_PATH"; trap - EXIT
  chmod 0600 "$CA_BUNDLE_PATH"
  current_signature > "$STATE_FILE"
}

case "${1:-}" in
  check)  cmd_check ;;
  bundle) cmd_bundle ;;
  *) echo "usage: $0 {check|bundle}" >&2; exit 2 ;;
esac
