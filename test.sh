#!/usr/bin/env bash

if [[ "${DEBUG:-false}" == true ]]; then
  set -o xtrace
fi

# Exit on any kind of errors
# https://unix.stackexchange.com/questions/23026
set -o errexit
set -o nounset
set -o pipefail
set -o errtrace
set -o functrace
shopt -s inherit_errexit

# Ensure CTRL+C properly aborts the script
trap "exit 130" INT

# Set docker versions from args or use default
if [[ $# -eq 0 ]]; then
  docker_versions=("18.09" "19.03" "20.10" "23" "24" latest)
else
  docker_versions=("$@")
fi
readonly docker_versions

readonly docker_args=(docker run --rm --env DOND_SHIM_DEBUG --volume /var/run/docker.sock:/var/run/docker.sock)

for docker_version in "${docker_versions[@]}"; do
  echo "Testing with docker version: ${docker_version}"

  image_id="$(docker build --target test --build-arg "DOCKER_VERSION=${docker_version}" --quiet .)"

  echo "Do not change global options or after the image"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${PWD}:/wd" "${image_id}" \
    docker --host test run --volume /wd:/wd alpine --volume /wd:/wd |
    grep --quiet "^docker.orig --host test run --volume ${PWD}:/wd alpine --volume /wd:/wd$"

  echo "Same as above, but retaining read only mode"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${PWD}:/wd" "${image_id}" \
    docker --host test run --volume /wd:/wd:ro alpine --volume /wd:/wd |
    grep --quiet "^docker.orig --host test run --volume ${PWD}:/wd:ro alpine --volume /wd:/wd$"

  echo "Same as above but with --mount"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${PWD}:/wd" "${image_id}" \
    docker --host test run --volume /wd:/wd:ro --mount=type=bind,source=/wd,readonly,destination=/wd2 alpine --volume /wd:/wd |
    grep --quiet "^docker.orig --host test run --volume ${PWD}:/wd:ro --mount=type=bind,source=${PWD},readonly,destination=/wd2 alpine --volume /wd:/wd$"

  echo "Same as above (without --mount), but retaining read only mode on auto added volume"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --env DOND_SHIM_MOCK_CONTAINER_ROOT_ON_HOST=/container-root --volume "${PWD}:/wd" --volume "${PWD}/testfile:/test/testfile" "${image_id}" \
    docker --host test run --volume /wd:/wd:ro --volume /test:/test:ro alpine --volume /wd:/wd |
    grep --quiet "^docker.orig --host test run --volume ${PWD}:/wd:ro --volume /container-root/test:/test:ro --volume ${PWD}/testfile:/test/testfile:ro alpine --volume /wd:/wd$"

  echo "Same as above (with --mount src and target, dst), but retaining read only mode on auto added volume"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --env DOND_SHIM_MOCK_CONTAINER_ROOT_ON_HOST=/container-root --volume "${PWD}:/wd" --volume "${PWD}/testfile:/test/testfile" "${image_id}" \
    docker --host test run --mount type=bind,src=/wd,target=/wd,readonly --mount type=bind,source=/test,dst=/test,readonly alpine --mount type=bind,source=/wd,destination=/wd,readonly |
    grep --quiet "^docker.orig --host test run --mount type=bind,src=${PWD},target=/wd,readonly --mount type=bind,source=/container-root/test,dst=/test,readonly --mount type=bind,source=${PWD}/testfile,dst=/test/testfile,readonly alpine --mount type=bind,source=/wd,destination=/wd,readonly$"

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

  echo "With --mount"
  "${docker_args[@]}" --volume "${PWD}/testfile:/test/testfile" "${image_id}" \
    docker run --rm --mount type=bind,source=/test,destination=/wd ubuntu:latest grep "^test$" /wd/testfile >/dev/null

  echo "With --mount shuffling order"
  "${docker_args[@]}" --volume "${PWD}/testfile:/test/testfile" "${image_id}" \
    docker run --rm --mount destination=/wd,source=/test,type=bind ubuntu:latest grep "^test$" /wd/testfile >/dev/null

  echo "Same as above but for multiple files under different volumes"
  "${docker_args[@]}" --volume "${PWD}/testfile:/test/testfile" --volume "${PWD}/testfile:/test/testfile2" "${image_id}" \
    docker run --rm --volume /test:/wd ubuntu:latest bash -c 'grep "^test$" /wd/testfile && grep "^test$" /wd/testfile2 && grep "^test$" /wd/only-inside-container' >/dev/null

  echo "Same test as above but with a read only volume"
  "${docker_args[@]}" --volume "${PWD}/testfile:/test/testfile" --volume "${PWD}/testfile:/test/testfile2" "${image_id}" \
    docker run --rm --volume /test:/wd:ro ubuntu:latest bash -c 'grep "^test$" /wd/testfile && grep "^test$" /wd/testfile2 && grep "^test$" /wd/only-inside-container' >/dev/null

  echo "Same as above but with a volume that matches the parent first"
  "${docker_args[@]}" --volume "${PWD}:/folder" --volume "${PWD}/testfile:/test/testfile" --volume "${PWD}/testfile:/test/testfile2" "${image_id}" \
    docker run --rm --volume /folder:/test --volume /test:/wd:ro ubuntu:latest bash -c 'grep "^test$" /wd/testfile && grep "^test$" /wd/testfile2 && grep "^test$" /wd/only-inside-container' >/dev/null
done
