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

temp_dir="$(mktemp -d)"
readonly temp_dir
volume_name=""
trap 'rm -rf "${temp_dir}"; if [[ -n "${volume_name}" ]]; then docker volume rm -f "${volume_name}" >/dev/null; fi' EXIT

# The daemon knows no container with this ID, as with a sidecar daemon. The shim reads the ID of
# its container from the resolv.conf that Docker mounts.
mkdir "${temp_dir}/dondtestunknownid"
cp /etc/resolv.conf "${temp_dir}/dondtestunknownid/resolv.conf"

# this avoids messing with the test output during the tests itself
echo "Pulling docker images used in tests"
docker pull -q busybox
for docker_version in "${docker_versions[@]}"; do
  docker pull -q "docker:${docker_version}"
done

for docker_version in "${docker_versions[@]}"; do
  echo "Testing with docker version: ${docker_version}"

  image_id="$(docker build --target test --build-arg "DOCKER_VERSION=${docker_version}" --quiet .)"

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
  # this confirms the volume works in the parent container prior to the shim
  "${docker_args[@]}" --volume "${volume_name}:/wd" "${image_id}" \
    grep -q "^test$" /wd/testfile
  "${docker_args[@]}" --volume "${volume_name}:/wd" "${image_id}" \
    docker run --rm --volume /wd:/wd busybox \
    grep -q "^test$" /wd/testfile
  docker volume rm -f "${volume_name}" >/dev/null
  volume_name=""

  echo "With -H on the command line"
  "${docker_args[@]}" "${image_id}" \
    docker -H unix:///var/run/docker.sock run --rm --volume /test/only-inside-container:/only-inside-container busybox \
    grep -q "^test$" /only-inside-container

  echo "Exit code of the container"
  exit_code=0
  "${docker_args[@]}" "${image_id}" \
    docker run --rm busybox sh -c 'exit 7' || exit_code=$?
  [[ "${exit_code}" -eq 7 ]]

  echo "dond wrap, with the docker shim inside, which is a wrapper inside a wrapper"
  "${docker_args[@]}" "${image_id}" \
    dond wrap sh -c 'docker run --rm --volume /test/only-inside-container:/only-inside-container busybox grep -q "^test$" /only-inside-container'

  echo "A symlink with another name"
  "${docker_args[@]}" "${image_id}" sh -c '
    mkdir /tmp/bin && ln -s /usr/local/bin/docker /tmp/bin/env && export PATH="/tmp/bin:${PATH}" &&
    env docker.orig run --rm --volume /test/only-inside-container:/only-inside-container busybox grep -q "^test$" /only-inside-container'

  echo "Compose with a relative path and a path that only exists in the container"
  "${docker_args[@]}" --volume "${fixtures_dir}:/wd" "${image_id}" sh -c '
    if ! docker.orig compose version >/dev/null 2>&1; then
      echo "Skipped: no Compose plugin"
      exit 0
    fi
    printf "%s\n" "services:" "  test:" "    image: busybox" "    volumes:" \
      "      - ./testfile:/testfile" "      - /test/only-inside-container:/only-inside-container" \
      "    command: [\"sh\", \"-c\", \"grep -qx test /testfile && grep -qx test /only-inside-container\"]" \
      >/tmp/compose.yaml
    compose="docker compose --project-name dond-test-compose --project-directory /wd --file /tmp/compose.yaml"
    ${compose} run --rm test
    exit_code=$?
    ${compose} down >/dev/null 2>&1
    exit "${exit_code}"'

  echo "Passes through when the daemon does not know the container"
  "${docker_args[@]}" --volume "${temp_dir}/dondtestunknownid/resolv.conf:/etc/resolv.conf:ro" "${image_id}" sh -c '
    without="$(docker.orig run --rm --volume /etc/os-release:/os-release busybox cat /os-release)" &&
    with="$(docker run --rm --volume /etc/os-release:/os-release busybox cat /os-release)" &&
    [ -n "${with}" ] && [ "${with}" = "${without}" ]'

  echo "SIGTERM sent to the wrapper reaches the child"
  "${docker_args[@]}" "${image_id}" sh -c '
    dond wrap sh -c "trap \"exit 42\" TERM; sleep 30 & wait" &
    sleep 1
    kill -TERM $!
    wait $!
    [ $? -eq 42 ]'
done
