#!/usr/bin/env bash
# Docker smoke tests for a built reproxy image.
#
# Pins three product contracts, in order:
#   1. version banner — `docker run <image> --version` must print the
#      expected version exactly (the image must not lie about its version);
#   2. round-trip — a real request through the proxy to a public https
#      upstream returns 200 with the expected body marker, and a host
#      outside --allowlist is 403'd naming the allowlist;
#   3. compose — the local-build variant of docker-compose.yml (image:
#      rewritten to build: .) starts a serving proxy that gates too.
#
# Usage:
#   scripts/docker-smoke.sh <image> <expected-version>
#
# Relocated from .github/workflows/docker-publish.yml (its three former
# "Smoke -" steps) so the repo — not a CI template — owns the product
# contracts these checks pin; CI and the README's local-build verification
# run this same file. Ports 18080 (round-trip container) and 8080 (compose
# service) are hardcoded: no knobs.

set -euo pipefail

usage() {
  echo "usage: $0 <image> <expected-version>" >&2
  echo "  <image>             single image reference, e.g. reproxy:local" >&2
  echo "  <expected-version>  exact string 'docker run <image> --version' must print" >&2
}

if [[ "$#" -ne 2 ]]; then
  usage
  exit 2
fi

image="$1"
expected="$2"

# Run from the repo root (this script's parent directory), not the caller's
# cwd: the compose check reads docker-compose.yml and writes
# docker-compose.local.yml NEXT TO the checkout — docker compose resolves
# relative paths (build: .) against the first -f file's directory, so the
# rewritten file must sit next to the real Dockerfile it builds. Anchoring
# on the script's own location makes that true from any invocation cwd.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${script_dir}/.."

# ---- Shared cleanup: every container and temp file this script starts is
# removed on exit, success or failure. (In the workflow these were three
# per-step EXIT traps; one script gets one trap over the accumulated state.)
# `docker rm -f` on a never-started or already-removed container is
# shielded by `|| true`, so the unconditional rm is safe at any point.
compose_file=""
cleanup() {
  docker rm -f reproxy-smoke >/dev/null 2>&1 || true
  if [[ -n "${compose_file}" ]]; then
    docker compose -f "${compose_file}" down -v >/dev/null 2>&1 || true
    rm -f "${compose_file}"
  fi
  rm -f /tmp/body.html /tmp/gated.json /tmp/compose-gated.json
}
trap cleanup EXIT

# ---- Check 1: version banner --------------------------------------------
# The image's version must not lie: `docker run <image> --version` must
# print the expected version exactly.
echo "check 1/3: version banner"
echo "running: ${image} --version"
actual="$(docker run --rm "${image}" --version)"
echo "expected: ${expected}"
echo "actual:   ${actual}"
if [[ "${actual}" != "${expected}" ]]; then
  echo "check 1/3 (version banner) FAILED: image version '${actual}' does not match expected '${expected}'"
  exit 1
fi
echo "check 1/3: version banner OK"

# ---- Check 2: request round-trip through the proxy ----------------------
# The upstream must be a real PUBLIC https host: SSRF layer L3
# (ssrf.go forbiddenIPv4Prefixes) unconditionally refuses
# loopback/RFC1918/link-local targets — --allowlist gates L2 only,
# and --dangerous-allow-all keeps the forbidden-IP list. A local
# test upstream is therefore impossible by design (the product's
# own no-SSRF guarantee); example.com is the stable public stand-in.
# The https handshake also exercises the distroless CA roots in the
# same request — a scratch image would fail exactly here.
echo "check 2/3: request round-trip"
docker run --rm --detach --name reproxy-smoke \
  -p 18080:8080 \
  "${image}" \
  --allowlist example.com

# Wait for the proxy to accept connections (bounded retries; the
# if-condition context shields failed probes from the default
# bash -e so the loop can retry).
ready=""
for _ in $(seq 1 30); do
  if curl -s -o /dev/null --max-time 2 http://127.0.0.1:18080/; then
    ready=1
    break
  fi
  sleep 1
done
if [[ -z "${ready}" ]]; then
  echo "check 2/3 (round-trip) FAILED: proxy did not become ready on 127.0.0.1:18080"
  docker logs reproxy-smoke || true
  exit 1
fi

# One real request THROUGH the proxy to the public upstream.
status="$(curl -s -o /tmp/body.html -w '%{http_code}' --max-time 30 \
  'http://127.0.0.1:18080/https/example.com/')"
echo "proxied status: ${status}"
if [[ "${status}" != "200" ]]; then
  echo "check 2/3 (round-trip) FAILED: round-trip through the proxy returned ${status}, want 200"
  docker logs reproxy-smoke || true
  exit 1
fi
if ! grep -q "Example Domain" /tmp/body.html; then
  echo "check 2/3 (round-trip) FAILED: round-trip body does not contain the expected upstream marker"
  exit 1
fi
echo "round-trip OK: 200 + upstream body marker verified"

# Allowlist gate check (deterministic — no external dependency):
# a host NOT in --allowlist must 403 naming the allowlist, before
# any DNS work (product guarantee L2).
gated="$(curl -s -o /tmp/gated.json -w '%{http_code}' --max-time 5 \
  'http://127.0.0.1:18080/https/other.example.org/')"
echo "gated request status: ${gated}"
if [[ "${gated}" != "403" ]]; then
  echo "check 2/3 (round-trip) FAILED: non-allowlisted host was not refused (got ${gated})"
  exit 1
fi
if ! grep -q "not in the allowlist" /tmp/gated.json; then
  echo "check 2/3 (round-trip) FAILED: 403 body does not name the allowlist gate"
  exit 1
fi
echo "check 2/3: round-trip + allowlist gate OK"

# Remove the round-trip container before the compose check. In the workflow
# the step's EXIT trap did this at the step boundary; this script has one
# trap at script end, so remove it here to keep the compose check running
# under the same conditions (round-trip container gone, only the compose
# service will hold a port). Not suppressed with `|| true` — that shielding
# belongs to a trap that must not mask the real exit code; here a failed rm
# on a container that must exist is a failure worth stopping for (the EXIT
# trap retries the removal anyway).
docker rm -f reproxy-smoke >/dev/null

# ---- Check 3: compose local-build variant ------------------------------
# The committed docker-compose.yml pins the published image; the
# PRD's local-build variant replaces image: with build:. Exercise
# exactly that variant here: build from the checkout, up, one
# gated request (deterministic 403 through the L2 gate), down.
# The rewritten file is written NEXT TO the checkout (not /tmp):
# docker compose resolves relative paths (build: .) against the
# first -f file's directory, so a /tmp copy would look for
# /tmp/Dockerfile and fail.
echo "check 3/3: compose local-build variant"
sed -E 's|^([[:space:]]*)image:[[:space:]]*ghcr\.io/killbus/reproxy:v[0-9.]+.*$|\1build: .|' \
  docker-compose.yml > docker-compose.local.yml
if ! grep -q "build: \." docker-compose.local.yml; then
  echo "check 3/3 (compose) FAILED: local-build rewrite of docker-compose.yml failed"
  cat docker-compose.local.yml
  exit 1
fi
compose_file="docker-compose.local.yml"

docker compose -f docker-compose.local.yml up -d --build

# Wait for the compose service to accept connections. The compose
# file maps 8080:8080 (not the 18080 the round-trip smoke above
# used — that container was removed at the end of check 2; this
# check probes the compose-published port).
ready=""
for _ in $(seq 1 30); do
  if curl -s -o /dev/null --max-time 2 http://127.0.0.1:8080/; then
    ready=1
    break
  fi
  sleep 1
done
if [[ -z "${ready}" ]]; then
  echo "check 3/3 (compose) FAILED: compose service did not become ready"
  docker compose -f docker-compose.local.yml logs || true
  exit 1
fi

# A request to a host outside the allowlist (api.example.com is
# the only allowlisted entry) must 403 — deterministic, no
# network dependency; proves the compose service is OUR proxy
# and the gate is in force.
gated="$(curl -s -o /tmp/compose-gated.json -w '%{http_code}' --max-time 5 \
  'http://127.0.0.1:8080/https/example.com/')"
echo "compose gated request status: ${gated}"
if [[ "${gated}" != "403" ]]; then
  echo "check 3/3 (compose) FAILED: compose service did not gate an un-allowlisted host (got ${gated})"
  docker compose -f docker-compose.local.yml logs || true
  exit 1
fi
echo "compose OK: service up, gated 403 verified"
echo "check 3/3: compose OK"
