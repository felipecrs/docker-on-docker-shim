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
  docker_versions=(latest)
else
  docker_versions=("$@")
fi
readonly docker_versions

readonly docker_args=(docker run --rm --env DOND_SHIM_DEBUG --volume /var/run/docker.sock:/var/run/docker.sock)

# Find fixtures directory
script_path=$(realpath "$0")
script_dir=$(dirname "${script_path}")
fixtures_dir="$(realpath "${script_dir}/../tests/fixtures")"
readonly fixtures_dir
unset script_path script_dir

# this avoids messing with the test output during the tests itself
echo "Pulling docker images used in tests"
docker pull -q busybox

for docker_version in "${docker_versions[@]}"; do
  echo "Testing with docker version: ${docker_version}"

  image_id="$(docker build --target test --build-arg "DOCKER_VERSION=${docker_version}" --quiet .)"

  echo "Do not change global options or after the image"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${fixtures_dir}:/wd" "${image_id}" \
    docker --host test run --volume /wd:/wd busybox --volume /wd:/wd |
    grep -q "^docker.orig --host test run --volume ${fixtures_dir}:/wd busybox --volume /wd:/wd$"

  echo "Same as above, but retaining read only mode"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${fixtures_dir}:/wd" "${image_id}" \
    docker --host test run --volume /wd:/wd:ro busybox --volume /wd:/wd |
    grep -q "^docker.orig --host test run --volume ${fixtures_dir}:/wd:ro busybox --volume /wd:/wd$"

  echo "Same as above but with --mount"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${fixtures_dir}:/wd" "${image_id}" \
    docker --host test run --volume /wd:/wd:ro --mount=type=bind,source=/wd,readonly,destination=/wd2 busybox --volume /wd:/wd |
    grep -q "^docker.orig --host test run --volume ${fixtures_dir}:/wd:ro --mount=type=bind,source=${fixtures_dir},readonly,destination=/wd2 busybox --volume /wd:/wd$"

  echo "Same as above (without --mount), but retaining read only mode on auto added volume"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --env DOND_SHIM_MOCK_CONTAINER_ROOT_ON_HOST=/container-root --volume "${fixtures_dir}:/wd" --volume "${fixtures_dir}/testfile:/test/testfile" "${image_id}" \
    docker --host test run --volume /wd:/wd:ro --volume /test:/test:ro busybox --volume /wd:/wd |
    grep -q "^docker.orig --host test run --volume ${fixtures_dir}:/wd:ro --volume /container-root/test:/test:ro --volume ${fixtures_dir}/testfile:/test/testfile:ro busybox --volume /wd:/wd$"

  echo "Same as above but should not auto add mounts which are not bind mounts"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --env DOND_SHIM_MOCK_CONTAINER_ROOT_ON_HOST=/container-root --volume "${fixtures_dir}:/wd" --volume "${fixtures_dir}/testfile:/test/testfile" --mount type=tmpfs,target=/test/tmpfsdir "${image_id}" \
    docker --host test run --volume /wd:/wd:ro --volume /test:/test:ro busybox --volume /wd:/wd |
    grep -q "^docker.orig --host test run --volume ${fixtures_dir}:/wd:ro --volume /container-root/test:/test:ro --volume ${fixtures_dir}/testfile:/test/testfile:ro busybox --volume /wd:/wd$"

  echo "Same as above (with --mount src and target, dst), but retaining read only mode on auto added volume"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --env DOND_SHIM_MOCK_CONTAINER_ROOT_ON_HOST=/container-root --volume "${fixtures_dir}:/wd" --volume "${fixtures_dir}/testfile:/test/testfile" "${image_id}" \
    docker --host test run --mount type=bind,src=/wd,target=/wd,readonly --mount type=bind,source=/test,dst=/test,readonly busybox --mount type=bind,source=/wd,destination=/wd,readonly |
    grep -q "^docker.orig --host test run --mount type=bind,src=${fixtures_dir},target=/wd,readonly --mount type=bind,source=/container-root/test,dst=/test,readonly --mount type=bind,source=${fixtures_dir}/testfile,dst=/test/testfile,readonly busybox --mount type=bind,source=/wd,destination=/wd,readonly$"

  echo "Same but for container run"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${fixtures_dir}:/wd" "${image_id}" \
    docker --host test container run --volume /wd:/wd busybox --volume /wd:/wd |
    grep -q "^docker.orig --host test container run --volume ${fixtures_dir}:/wd busybox --volume /wd:/wd$"

  echo "Same but for create"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${fixtures_dir}:/wd" "${image_id}" \
    docker --host test create --volume /wd:/wd busybox --volume /wd:/wd |
    grep -q "^docker.orig --host test create --volume ${fixtures_dir}:/wd busybox --volume /wd:/wd$"

  echo "Same but container create"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${fixtures_dir}:/wd" "${image_id}" \
    docker --host test container create --volume /wd:/wd busybox --volume /wd:/wd |
    grep -q "^docker.orig --host test container create --volume ${fixtures_dir}:/wd busybox --volume /wd:/wd$"

  echo "Do not do anything for other commands"
  "${docker_args[@]}" --env DOND_SHIM_PRINT_COMMAND=true --volume "${fixtures_dir}:/wd" "${image_id}" \
    docker --host test whatever --volume /wd:/wd busybox --volume /wd:/wd |
    grep -q "^docker.orig --host test whatever --volume /wd:/wd busybox --volume /wd:/wd$"

  echo "Check if docker on docker is working"
  "${docker_args[@]}" "${image_id}" \
    docker version >/dev/null

  echo "Check if mounting a volume from the container gets fixed"
  "${docker_args[@]}" "${image_id}" \
    docker run --rm --volume /test/only-inside-container:/only-inside-container busybox \
    grep -q "^test$" /only-inside-container

  echo "Same but with equals sign"
  "${docker_args[@]}" "${image_id}" \
    docker run --rm --volume=/test/only-inside-container:/only-inside-container busybox \
    grep -q "^test$" /only-inside-container

  echo "Check if mounting a volume which is already a volume gets fixed"
  "${docker_args[@]}" --volume "${fixtures_dir}:/wd" "${image_id}" \
    docker run --rm --volume /wd:/wd busybox \
    grep -q "^test$" /wd/testfile

  echo "Same as above but for a file within the volume"
  "${docker_args[@]}" --volume "${fixtures_dir}:/wd" "${image_id}" \
    docker run --rm --volume /wd/testfile:/wd/testfile busybox \
    grep -q "^test$" /wd/testfile

  echo "Check if mounting a volume which contains another volume adds all proper volumes"
  "${docker_args[@]}" --volume "${fixtures_dir}/testfile:/test/testfile" "${image_id}" \
    docker run --rm --volume /test:/wd busybox \
    grep -q "^test$" /wd/testfile

  echo "With --mount"
  "${docker_args[@]}" --volume "${fixtures_dir}/testfile:/test/testfile" "${image_id}" \
    docker run --rm --mount type=bind,source=/test,destination=/wd busybox \
    grep -q "^test$" /wd/testfile

  echo "With --mount shuffling order"
  "${docker_args[@]}" --volume "${fixtures_dir}/testfile:/test/testfile" "${image_id}" \
    docker run --rm --mount destination=/wd,source=/test,type=bind busybox \
    grep -q "^test$" /wd/testfile

  echo "Same as above but for multiple files under different volumes"
  "${docker_args[@]}" --volume "${fixtures_dir}/testfile:/test/testfile" --volume "${fixtures_dir}/testfile:/test/testfile2" "${image_id}" \
    docker run --rm --volume /test:/wd busybox \
    sh -c 'grep -q "^test$" /wd/testfile && grep -q "^test$" /wd/testfile2 && grep -q "^test$" /wd/only-inside-container'

  echo "Same test as above but with a read only volume"
  "${docker_args[@]}" --volume "${fixtures_dir}/testfile:/test/testfile" --volume "${fixtures_dir}/testfile:/test/testfile2" "${image_id}" \
    docker run --rm --volume /test:/wd:ro busybox \
    sh -c 'grep -q "^test$" /wd/testfile && grep -q "^test$" /wd/testfile2 && grep -q "^test$" /wd/only-inside-container'

  echo "Same as above but with a volume that matches the parent first"
  "${docker_args[@]}" --volume "${fixtures_dir}:/folder" --volume "${fixtures_dir}/testfile:/test/testfile" --volume "${fixtures_dir}/testfile:/test/testfile2" "${image_id}" \
    docker run --rm --volume /folder:/test --volume /test:/wd:ro busybox \
    sh -c 'grep -q "^test$" /wd/testfile && grep -q "^test$" /wd/testfile2 && grep -q "^test$" /wd/only-inside-container'

  echo "With named volume"
  volume_name=$(docker volume create --opt type=none --opt device="${fixtures_dir}" --opt o=bind)
  trap 'docker volume rm -f "${volume_name}" >/dev/null' EXIT
  # this confirms the volume works in the parent container prior to the shim
  "${docker_args[@]}" --volume "${volume_name}:/wd" "${image_id}" \
    grep -q "^test$" /wd/testfile
  "${docker_args[@]}" --volume "${volume_name}:/wd" "${image_id}" \
    docker run --rm --volume /wd:/wd busybox \
    grep -q "^test$" /wd/testfile
  docker volume rm -f "${volume_name}" >/dev/null
done
