#!/bin/bash

set -euxo pipefail

image_id="$(docker build --quiet .)"

# shellcheck disable=SC2312
docker_args=(docker run --rm --user "$(id -u):$(id -g)" --volume /var/run/docker.sock:/var/run/docker.sock)

# Check if docker on docker is working
"${docker_args[@]}" "${image_id}" docker version >/dev/null

# Check if mounting an volume from the container gets fixed
"${docker_args[@]}" "${image_id}" docker run --rm --volume /home/rootless/only-inside-container:/only-inside-container ubuntu:latest grep "^test$" /only-inside-container >/dev/null
"${docker_args[@]}" "${image_id}" docker run --rm --volume=/home/rootless/only-inside-container:/only-inside-container ubuntu:latest grep "^test$" /only-inside-container >/dev/null

# Check if mounting a volume which is already a volume gets fixed
"${docker_args[@]}" --volume "${PWD}:/wd" "${image_id}" docker run --rm --volume /wd:/wd ubuntu:latest grep "^test$" /wd/testfile >/dev/null

# Same as above but for a file within the volume
"${docker_args[@]}" --volume "${PWD}:/wd" "${image_id}" docker run --rm --volume /wd/testfile:/wd/testfile ubuntu:latest grep "^test$" /wd/testfile >/dev/null

# Volumes inside volumes
"${docker_args[@]}" --volume "${PWD}/testfile:/home/rootless/testfile" "${image_id}" docker run --rm --volume /home/rootless:/wd ubuntu:latest grep "^test$" /wd/testfile >/dev/null
"${docker_args[@]}" --volume "${PWD}/testfile:/home/rootless/testfile" --volume "${PWD}/testfile:/home/rootless/testfile2" "${image_id}" docker run --rm --volume /home/rootless:/wd ubuntu:latest bash -c 'grep "^test$" /wd/testfile && grep "^test$" /wd/testfile2 && grep "^test$" /wd/only-inside-container' >/dev/null
