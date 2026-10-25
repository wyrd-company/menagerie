#!/usr/bin/env bash
# Acceptance run of the built Menagerie image: the integrated run's image
# topology, with a real Wyrwood agent.
#
#   acceptance.sh up <scratch>       start an owned daemon, build the image and the suites
#   acceptance.sh suite <scratch>    run TestImageWyrwood on that daemon
#   acceptance.sh fixture <scratch>  start TestAcceptanceFixture and wait until it is ready
#   acceptance.sh test <scratch>     run the acceptance Playwright project against it
#   acceptance.sh down <scratch>     stop the fixture and remove the owned daemon
#
# Everything runs on one scripts/isolated-docker.sh daemon: the image, its
# devcontainers, Pocket ID, a private registry and the Wyrwood agent. The
# suites run in that daemon's runner, with Wyrwood's controller and an
# OpenBao dev server. The daemon publishes the site, Pocket ID and the
# fixture's control server on this host's 127.0.0.1, at the ports below, so
# the browser here uses the URLs the services use. WYRWOOD_VERSION and
# OPENBAO_VERSION name the released executables, which up downloads into
# <scratch>/bin and checks against their releases' checksums. Nothing here
# reads a credential; the suites generate every key, token and password.
# Evidence (screenshots, videos, traces, logs) is written under <scratch>.
set -euo pipefail
cmd=$1
scratch=$(realpath -m "$2")
repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
isolated=$repo/scripts/isolated-docker.sh
state=$scratch/daemon
port=${MENAGERIE_ACCEPTANCE_PORT:-18640}
control=${MENAGERIE_ACCEPTANCE_CONTROL_PORT:-18641}
pocket=${MENAGERIE_ACCEPTANCE_POCKETID_PORT:-18642}
info=/menagerie-work/acceptance-info.json
image=menagerie:test
# Test-only bound on the fixture's start, a second apart: it was ready in
# under a minute on the shared host, most of it Wyrwood and Pocket ID.
ready_attempts=${MENAGERIE_ACCEPTANCE_READY_ATTEMPTS:-600}

# fetch <url base> <archive> <member>: download a release archive into
# <scratch>/bin, check it against the release's checksums and extract one
# executable.
fetch() {
  local base=$1 archive=$2 member=$3 dir
  dir=$(mktemp -d)
  curl -fsSL -o "$dir/$archive" "$base/$archive"
  curl -fsSL -o "$dir/checksums.txt" "$base/checksums.txt"
  (cd "$dir" && grep " $archive\$" checksums.txt | sha256sum -c -) >>"$scratch/logs/binaries.txt"
  tar -xzf "$dir/$archive" -C "$scratch/bin" "$member"
  rm -rf "$dir"
}

suite_env() {
  printf '%s\n' -c "$scratch/bin/wyrwood" -c "$scratch/bin/bao" \
    -e "MENAGERIE_IMAGE=$image" -e MENAGERIE_WYRWOOD=/menagerie-work/bin/wyrwood \
    -e MENAGERIE_BAO=/menagerie-work/bin/bao
}

case "$cmd" in
  up)
    : "${WYRWOOD_VERSION:?set WYRWOOD_VERSION}" "${OPENBAO_VERSION:?set OPENBAO_VERSION}"
    mkdir -p "$scratch/logs" "$scratch/bin" "$scratch/evidence"
    [ ! -s "$state/run" ] || { echo "$state already holds a run; run down first" >&2; exit 2; }
    git -C "$repo" rev-parse HEAD >"$scratch/source-head.txt"
    git -C "$repo" status --porcelain >"$scratch/source-status.txt"
    fetch "https://github.com/wyrd-company/wyrwood/releases/download/$WYRWOOD_VERSION" \
      "wyrwood_${WYRWOOD_VERSION}_linux_x86_64.tar.gz" wyrwood
    fetch "https://github.com/openbao/openbao/releases/download/v$OPENBAO_VERSION" \
      "openbao_${OPENBAO_VERSION}_linux_amd64.tar.gz" bao
    od -An -N8 -tx1 /dev/urandom | tr -d ' \n' >"$scratch/owner"
    DIND_PUBLISH="$port $control $pocket" "$isolated" up "$state" "$(cat "$scratch/owner")" | tee "$scratch/logs/daemon.txt"
    "$isolated" build "$state" -t "$image" >"$scratch/logs/image-build.log" 2>&1 ||
      { tail -30 "$scratch/logs/image-build.log"; exit 1; }
    (cd "$repo" && CGO_ENABLED=0 go test -c -tags 'pocketid browser image acceptance' \
      -o "$state/acceptance.test" ./backend/cmd/menagerie/)
    echo "image $image built at $(cat "$scratch/source-head.txt")"
    ;;
  suite)
    mapfile -t env < <(suite_env)
    "$isolated" run "$state" "${env[@]}" "$state/acceptance.test" \
      -test.run '^TestImageWyrwood$' -test.v -test.count=1 -test.timeout 20m 2>&1 | tee "$scratch/logs/suite.log"
    ;;
  fixture)
    mapfile -t env < <(suite_env)
    [ ! -s "$scratch/fixture.pid" ] || { echo "$scratch already runs a fixture" >&2; exit 2; }
    # A previous fixture's description must not read as this one's.
    rm -f "$scratch/info.json"
    "$isolated" shell "$state" "rm -f $info"
    setsid "$isolated" run "$state" "${env[@]}" -e "MENAGERIE_BROWSER_INFO=$info" \
      -e "MENAGERIE_BROWSER_PORT=$port" -e "MENAGERIE_BROWSER_CONTROL_PORT=$control" \
      -e "MENAGERIE_POCKETID_PORT=$pocket" "$state/acceptance.test" \
      -test.run '^TestAcceptanceFixture$' -test.v -test.count=1 -test.timeout 0 \
      </dev/null >"$scratch/logs/fixture.log" 2>&1 &
    echo $! >"$scratch/fixture.pid"
    for ((i = 0; i < ready_attempts; i++)); do
      if "$isolated" shell "$state" "cat $info" >"$scratch/info.json" 2>/dev/null && [ -s "$scratch/info.json" ]; then
        cat "$scratch/info.json"; echo
        exit 0
      fi
      kill -0 "$(cat "$scratch/fixture.pid")" 2>/dev/null || { tail -40 "$scratch/logs/fixture.log"; exit 1; }
      sleep 1
    done
    echo "the fixture was not ready after $ready_attempts s" >&2
    exit 1
    ;;
  test)
    MENAGERIE_BROWSER_INFO="$scratch/info.json" MENAGERIE_EVIDENCE="$scratch/evidence" \
      pnpm --dir "$repo/web" exec playwright test -c playwright.acceptance.config.ts 2>&1 | tee "$scratch/logs/playwright.log"
    ;;
  down)
    # Only the run this scratch folder saved: its control server is shut
    # down after it answers with the saved root, and the daemon is removed
    # by the saved owner token.
    if [ -s "$scratch/info.json" ]; then
      saved_control=$(node -p 'require(process.argv[1]).control' "$scratch/info.json")
      saved_root=$(node -p 'require(process.argv[1]).root' "$scratch/info.json")
      served_root=$(curl -fsS "$saved_control/identity" 2>/dev/null |
        node -e 'let s = ""; process.stdin.on("data", (d) => (s += d)).on("end", () => { try { process.stdout.write(JSON.parse(s).root ?? ""); } catch {} });' || true)
      if [ -n "$served_root" ] && [ "$served_root" = "$saved_root" ]; then
        curl -fsS -X POST "$saved_control/shutdown" >/dev/null || true
      else
        echo "$saved_control does not serve this run ($saved_root); it was not shut down" >&2
      fi
    fi
    if [ -s "$scratch/fixture.pid" ]; then
      pid=$(cat "$scratch/fixture.pid")
      # The fixture ends once its control server is shut down; down then
      # removes the runner it ran in either way.
      for ((i = 0; i < 60; i++)); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 1
      done
      tail -3 "$scratch/logs/fixture.log" 2>/dev/null || true
      rm -f "$scratch/fixture.pid"
    fi
    if [ -s "$scratch/owner" ]; then
      "$isolated" down "$state" "$(cat "$scratch/owner")"
    fi
    ;;
  *) echo "usage: $0 up|suite|fixture|test|down <scratch>" >&2; exit 2 ;;
esac
