#!/usr/bin/env bash
# A disposable, test-owned Docker daemon for Menagerie's live suites.
#
#   isolated-docker.sh up <state> [owner]                 start the daemon and the runner
#   isolated-docker.sh build <state> [build args...]      docker build the repository on the daemon
#   isolated-docker.sh shell <state> <script>             run a shell script in the runner
#   isolated-docker.sh run <state> [-c FILE]... [-e NAME=VALUE]... <binary> [args...]
#                                                         run a test binary in the runner
#   isolated-docker.sh proxy <state> <sockguard> <config> start the Docker proxy on the daemon
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
# DIND_REGISTRY_MIRROR, when set, is a Docker Hub mirror for the daemon, so
# that Docker Hub's anonymous pull limit does not end a run; images pinned by
# digest are the same through a mirror.
#
# DIND_PUBLISH, when set, is a space-separated list of ports that up
# publishes from the daemon's network namespace on the caller's 127.0.0.1,
# each at the same number, so a browser here reaches a service that listens
# there on every address.
#
# proxy runs the Docker proxy as Menagerie's deployment runs it, on the
# daemon: <sockguard> is a commit of github.com/wyrd-company/sockguard,
# built on the daemon, or an image reference, pulled; <config> is its
# rendered configuration (scripts/proxy-config.sh). It mounts the work
# volume read-only at its own path, so it resolves bind sources as the
# daemon does, and serves the socket that proxy_socket names; the
# exceptions file folder is proxy_exceptions. A test reaches the proxy
# with DOCKER_HOST=unix://$proxy_socket.
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

proxy_socket=$work/proxy/socket/menagerie.sock
proxy_exceptions=$work/proxy/exceptions

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
    publish=()
    for port in ${DIND_PUBLISH:-}; do
      case "$port" in '' | *[!0-9]*) echo "DIND_PUBLISH holds $port, not a port" >&2; exit 2 ;; esac
      publish+=(-p "127.0.0.1:$port:$port")
    done
    docker run -d --privileged --name "$run-dind" "${labels[@]}" -e DOCKER_TLS_CERTDIR= \
      "${publish[@]}" -v "$run-work:$work" "$dind_image" \
      dockerd --host tcp://127.0.0.1:2375 --host unix:///var/run/docker.sock \
      ${DIND_REGISTRY_MIRROR:+--registry-mirror "$DIND_REGISTRY_MIRROR"} >/dev/null
    for ((i = 0; i < ready_attempts; i++)); do
      dind_exec version >/dev/null 2>&1 && break
      sleep 1
    done
    dind_exec version --format 'daemon {{.Server.Version}} api {{.Server.APIVersion}}' || {
      docker logs "$run-dind" 2>&1 | tail -20
      exit 1
    }
    dind_exec info --format 'driver {{.Driver}} {{json .DriverStatus}} mirrors {{json .RegistryConfig.Mirrors}}'
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
  proxy)
    [ $# -eq 2 ] || usage
    run=$(cat "$state/run")
    source=$1 config=$2
    case "$source" in
      *[!0-9a-f]* | '') image=$source; dind_exec pull -q "$image" >/dev/null ;;
      *)
        [ ${#source} -eq 40 ] || { echo "$source is neither a full commit nor an image reference" >&2; exit 2; }
        image=menagerie-test-sockguard:$source
        dind_exec build -q -t "$image" "https://github.com/wyrd-company/sockguard.git#$source" >/dev/null
        ;;
    esac
    docker exec "$run-runner" mkdir -p "$work/proxy/socket" "$work/proxy/config" "$proxy_exceptions"
    docker cp "$config" "$run-runner:$work/proxy/config/sockguard.yaml" >/dev/null
    # As compose.yml runs it: root in the socket's group, with only the
    # capability to resolve every bind source, no network, and a writable
    # /tmp only, where it spools a copy's archive to inspect it.
    dind_exec run -d --name sockguard --user 0:0 --group-add 2375 --cap-drop ALL --cap-add DAC_READ_SEARCH \
      --read-only --tmpfs /tmp --security-opt no-new-privileges --network none \
      -v /var/run/docker.sock:/var/run/docker.sock -v "$work:$work:ro" \
      -v "$work/proxy/socket:/run/sockguard" \
      -v "$work/proxy/config/sockguard.yaml:/etc/sockguard/sockguard.yaml:ro" \
      -v "$proxy_exceptions:/etc/sockguard/exceptions:ro" "$image" >/dev/null
    for ((i = 0; i < ready_attempts; i++)); do
      docker exec "$run-runner" test -S "$proxy_socket" && break
      sleep 1
    done
    docker exec "$run-runner" test -S "$proxy_socket" || {
      dind_exec logs sockguard 2>&1 | tail -20
      exit 1
    }
    echo "proxy $image on unix://$proxy_socket"
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
