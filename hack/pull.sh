#!/bin/sh
# Pre-pull a toolchain image, surviving a registry that is only intermittently
# reachable.
#
# Everything here builds and tests inside a container, so the first thing every
# target does is reach Docker Hub. A shared runner reaches it over a shared
# egress address: the token endpoint times out under load, and the anonymous
# pull allowance is spent by whoever else is on that address. Either one fails
# `docker run` outright — the daemon does not retry — and takes the whole job
# with it, for a pull that would have worked a second later.
#
# So: skip the network entirely when the image is already local, retry with a
# widening pause, and fall back to Google's pull-through cache of Docker Hub,
# which is a separate address range and needs no token at all. The mirrored
# image is retagged to the name it was asked for, so nothing downstream has to
# know which of the two it came from.
set -eu

mirror=${DOCKER_MIRROR:-mirror.gcr.io}
attempts=${PULL_ATTEMPTS:-3}

for image in "$@"; do
  if docker image inspect "$image" >/dev/null 2>&1; then
    continue
  fi

  n=1
  while ! docker pull "$image"; do
    if [ "$n" -lt "$attempts" ]; then
      sleep $((n * 5))
      n=$((n + 1))
      continue
    fi

    # Official images live under library/ in the cache; anything namespaced
    # already carries its namespace. A private registry in the name is not
    # ours to mirror, so that one is simply a failure.
    case $image in
      *.*/* | *:*/*) echo "pull: $image is not on Docker Hub, giving up" >&2; exit 1 ;;
      */*)           mirrored="$mirror/$image" ;;
      *)             mirrored="$mirror/library/$image" ;;
    esac

    echo "pull: $image failed $attempts times, falling back to $mirrored" >&2
    docker pull "$mirrored"
    docker tag "$mirrored" "$image"
    break
  done
done
