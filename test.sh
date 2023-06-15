#!/bin/bash

set -euo pipefail

if [[ "${DEBUG:-false}" == true ]]; then
  set -x
  export DOND_SHIM_DEBUG=true
fi

# Set docker versions from args or use defaults
if [[ $# -eq 0 ]]; then
  docker_versions=("18.09" "19.03" "20.10" "23" "24" latest)
else
  docker_versions=("$@")
fi

for docker_version in "${docker_versions[@]}"; do
  echo "Testing with docker version: ${docker_version}"

  image_id="$(docker build --target test --build-arg "DOCKER_VERSION=${docker_version}" --quiet .)"

  # shellcheck disable=SC2312
  docker_args=(docker run --rm --env DOND_SHIM_DEBUG --entrypoint= --volume /var/run/docker.sock:/var/run/docker.sock)

  echo "Do not change global options or after the image"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${PWD}:/wd" "${image_id}" \
    docker --host test run --volume /wd:/wd alpine --volume /wd:/wd |
    grep --quiet "^docker.orig --host test run --volume ${PWD}:/wd alpine --volume /wd:/wd$"

  echo "Same but for container run"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${PWD}:/wd" "${image_id}" \
    docker --host test container run --volume /wd:/wd alpine --volume /wd:/wd |
    grep --quiet "^docker.orig --host test container run --volume ${PWD}:/wd alpine --volume /wd:/wd$"

  echo "Same but for create"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${PWD}:/wd" "${image_id}" \
    docker --host test create --volume /wd:/wd alpine --volume /wd:/wd |
    grep --quiet "^docker.orig --host test create --volume ${PWD}:/wd alpine --volume /wd:/wd$"

  echo "Same but container create"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${PWD}:/wd" "${image_id}" \
    docker --host test container create --volume /wd:/wd alpine --volume /wd:/wd |
    grep --quiet "^docker.orig --host test container create --volume ${PWD}:/wd alpine --volume /wd:/wd$"

  echo "Do not do anything for other commands"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${PWD}:/wd" "${image_id}" \
    docker --host test whatever --volume /wd:/wd alpine --volume /wd:/wd |
    grep --quiet "^docker.orig --host test whatever --volume /wd:/wd alpine --volume /wd:/wd$"

  echo "Check if docker on docker is working"
  "${docker_args[@]}" "${image_id}" \
    docker version >/dev/null

  echo "Check if mounting an volume from the container gets fixed"
  "${docker_args[@]}" "${image_id}" \
    docker run --rm --volume /test/only-inside-container:/only-inside-container ubuntu:latest grep "^test$" /only-inside-container >/dev/null

  echo "Same but with equals sign"
  "${docker_args[@]}" "${image_id}" \
    docker run --rm --volume=/test/only-inside-container:/only-inside-container ubuntu:latest grep "^test$" /only-inside-container >/dev/null

  echo "Check if mounting a volume which is already a volume gets fixed"
  "${docker_args[@]}" --volume "${PWD}:/wd" "${image_id}" \
    docker run --rm --volume /wd:/wd ubuntu:latest grep "^test$" /wd/testfile >/dev/null

  echo "Same as above but for a file within the volume"
  "${docker_args[@]}" --volume "${PWD}:/wd" "${image_id}" \
    docker run --rm --volume /wd/testfile:/wd/testfile ubuntu:latest grep "^test$" /wd/testfile >/dev/null

  echo "Check if mounting a volume which contains another volume adds all proper volumes"
  "${docker_args[@]}" --volume "${PWD}/testfile:/test/testfile" "${image_id}" \
    docker run --rm --volume /test:/wd ubuntu:latest grep "^test$" /wd/testfile >/dev/null

  echo "Same as above but for multiple files under different volumes"
  "${docker_args[@]}" --volume "${PWD}/testfile:/test/testfile" --volume "${PWD}/testfile:/test/testfile2" "${image_id}" \
    docker run --rm --volume /test:/wd ubuntu:latest bash -c 'grep "^test$" /wd/testfile && grep "^test$" /wd/testfile2 && grep "^test$" /wd/only-inside-container' >/dev/null

done
