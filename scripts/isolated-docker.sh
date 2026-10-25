#!/usr/bin/env bash
# A disposable, test-owned Docker daemon for Menagerie's live suites.
#
#   isolated-docker.sh up <state> [owner]                 start the daemon and the runner
#   isolated-docker.sh build <state> [build args...]      docker build the repository on the daemon
#   isolated-docker.sh shell <state> <script>             run a shell script in the runner
#   isolated-docker.sh run <state> [-c FILE]... [-e NAME=VALUE]... <binary> [args...]
#                                                         run a test binary in the runner
#   isolated-docker.sh down <state> [owner]               remove what up created
#
# The daemon is a docker:dind container with its own data root, BuildKit
# and network namespace; nothing here builds on or changes the ambient
# daemon's images. The runner shares the daemon's network namespace, so a
# port a test publishes on 127.0.0.1 is the daemon's loopback too. One named
# volume is mounted at /menagerie-work in both, and TMPDIR points into it, so
# the bind mounts a test asks the daemon for resolve to the same paths. The
# runner image carries glibc, libcurl and git for the template service
# binary, Toha's file transport and the test Git hosts.
#
# <state> is a folder that records the owned container and volume names.
# down removes only objects that carry this run's label, and it is safe to
# repeat. A caller that cleans up unconditionally passes the same [owner]
# token to up and down: down then acts only on the run that up started for
# that token, so a refused up never removes another invocation's run. Anonymous pulls only: DOCKER_CONFIG is an empty file for every
# docker command here.
set -euo pipefail

dind_image=${DIND_IMAGE:-docker:29.9.2-dind@sha256:9e79b879b177fbd72c34376a3dd3d91747da5db49e1f560f76aea59561471977}
runner_image=${RUNNER_IMAGE:-ghcr.io/wyrd-company/devcontainers/base:noble@sha256:866956f9ad07fd0440cdd4e94dc1fbbebfdddbe9f43ade4d29568317e6612e75}
work=/menagerie-work
# Test-only bound on the daemon's start; it listened in under 5 s on the
# shared host and on CI.
ready_attempts=${DIND_READY_ATTEMPTS:-60}

usage() {
  sed -n '2,9p' "$0" >&2
  exit 2
}

[ $# -ge 2 ] || usage
cmd=$1 state=$2
shift 2

DOCKER_CONFIG=$(mktemp -d)
export DOCKER_CONFIG
printf '{}\n' >"$DOCKER_CONFIG/config.json"
trap 'rm -rf "$DOCKER_CONFIG"' EXIT

owned() { # owned container|volume <name>: the object exists and carries this run's label
  local labels=.Labels run
  [ "$1" = container ] && labels=.Config.Labels
  run=$(docker "$1" inspect --format "{{index $labels \"menagerie-test-run\"}}" "$2" 2>/dev/null) || return 1
  [ "$run" = "$(cat "$state/run")" ]
}

dind_exec() { docker exec "$(cat "$state/run")-dind" docker -H tcp://127.0.0.1:2375 "$@"; }

case "$cmd" in
  up)
    mkdir -p "$state"
    if [ -s "$state/run" ]; then
      echo "$state already records run $(cat "$state/run"); run down first" >&2
      exit 1
    fi
    run="menagerie-test-$(od -An -N6 -tx1 /dev/urandom | tr -d ' \n')"
    echo "$run" >"$state/run"
    if [ $# -ge 1 ]; then echo "$1" >"$state/owner"; else rm -f "$state/owner"; fi
    labels=(--label menagerie-test=isolated-docker --label "menagerie-test-run=$run")
    docker volume create "${labels[@]}" "$run-work" >/dev/null
    docker run -d --privileged --name "$run-dind" "${labels[@]}" -e DOCKER_TLS_CERTDIR= \
      -v "$run-work:$work" "$dind_image" \
      dockerd --host tcp://127.0.0.1:2375 --host unix:///var/run/docker.sock >/dev/null
    for ((i = 0; i < ready_attempts; i++)); do
      dind_exec version >/dev/null 2>&1 && break
      sleep 1
    done
    dind_exec version --format 'daemon {{.Server.Version}} api {{.Server.APIVersion}}' || {
      docker logs "$run-dind" 2>&1 | tail -20
      exit 1
    }
    dind_exec info --format 'driver {{.Driver}} {{json .DriverStatus}}'
    docker run -d --name "$run-runner" "${labels[@]}" --network "container:$run-dind" \
      -v "$run-work:$work" --entrypoint sleep "$runner_image" infinity >/dev/null
    docker exec "$run-runner" mkdir -p "$work/tmp" "$work/bin"
    docker inspect "$run-dind" --format 'dind image {{.Config.Image}} {{.Image}}'
    docker inspect "$run-runner" --format 'runner image {{.Config.Image}} {{.Image}}'
    echo "run $run"
    ;;
  build)
    repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
    # The build context is the checkout's tracked and unignored files, as
    # .dockerignore then filters them; build output on disk never enters.
    # A local --output dest under /menagerie-work is visible to the runner.
    (cd "$repo" && git ls-files -z --cached --others --exclude-standard |
      tar --null --no-recursion -T - -cf -) |
      docker exec -i "$(cat "$state/run")-dind" docker build --progress plain "$@" -
    ;;
  shell)
    [ $# -eq 1 ] || usage
    docker exec -w / "$(cat "$state/run")-runner" sh -c "$1"
    ;;
  run)
    run=$(cat "$state/run")
    envs=(-e DOCKER_HOST=tcp://127.0.0.1:2375 -e HOME=/root -e "TMPDIR=$work/tmp")
    while [ $# -gt 0 ]; do
      case "$1" in
        -c) docker cp "$2" "$run-runner:$work/bin/$(basename "$2")" >/dev/null; shift 2 ;;
        -e) envs+=(-e "$2"); shift 2 ;;
        *) break ;;
      esac
    done
    [ $# -ge 1 ] || usage
    bin=$1
    shift
    docker cp "$bin" "$run-runner:$work/bin/$(basename "$bin")" >/dev/null
    docker exec "${envs[@]}" -w / "$run-runner" "$work/bin/$(basename "$bin")" "$@"
    ;;
  down)
    [ -s "$state/run" ] || exit 0
    run=$(cat "$state/run")
    if [ $# -ge 1 ] && [ "$(cat "$state/owner" 2>/dev/null)" != "$1" ]; then
      echo "$state holds run $run of another invocation; leaving it"
      exit 0
    fi
    for name in "$run-runner" "$run-dind"; do
      if owned container "$name"; then docker rm -fv "$name" >/dev/null; fi
    done
    if owned volume "$run-work"; then docker volume rm "$run-work" >/dev/null; fi
    rm -f "$state/run" "$state/owner"
    echo "removed run $run"
    ;;
  *) usage ;;
esac
