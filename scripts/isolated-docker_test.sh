#!/usr/bin/env bash
# isolated-docker.sh down removes only objects that carry its run's label.
# Foreign objects named exactly like a run's daemon, runner and volume, but
# without the label, must survive down; this test owns and removes them.
set -euo pipefail

script=$(dirname "$0")/isolated-docker.sh
state=$(mktemp -d)
run="menagerie-test-foreign$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
DOCKER_CONFIG=$(mktemp -d)
export DOCKER_CONFIG
printf '{}\n' >"$DOCKER_CONFIG/config.json"
cleanup() {
  docker rm -f "$run-dind" "$run-runner" >/dev/null 2>&1 || true
  docker volume rm "$run-work" >/dev/null 2>&1 || true
  rm -rf "$state" "$DOCKER_CONFIG"
}
trap cleanup EXIT

image=${FOREIGN_IMAGE:-busybox:1.37@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e}
docker volume create "$run-work" >/dev/null
docker create --name "$run-dind" --label menagerie-test=isolated-docker "$image" true >/dev/null
docker create --name "$run-runner" "$image" true >/dev/null
echo "$run" >"$state/run"

"$script" down "$state"
survivors=0
for kind in "container $run-dind" "container $run-runner" "volume $run-work"; do
  if docker "${kind%% *}" inspect "${kind#* }" >/dev/null 2>&1; then
    survivors=$((survivors + 1))
  else
    echo "FAIL down removed foreign ${kind}" >&2
  fi
done
[ "$survivors" -eq 3 ] || exit 1
[ ! -e "$state/run" ] || { echo "FAIL down kept the state" >&2; exit 1; }
echo "ok   down kept 3 foreign objects named like its run's"
