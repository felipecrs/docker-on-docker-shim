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

# The socket of the Docker under test, which DOCKER_HOST names with rootless Docker.
host_socket="${DOCKER_HOST:-unix:///var/run/docker.sock}"
host_socket="${host_socket#unix://}"
readonly host_socket
# Options for every container that uses the socket, such as --userns=host with userns-remap.
read -r -a run_args <<<"${TEST_DOCKER_RUN_ARGS:-}"
readonly run_args

readonly docker_args=(docker run --rm "${run_args[@]}" --env DOND_DEBUG --volume "${host_socket}:/var/run/docker.sock")

# Find fixtures directory
script_path=$(realpath "$0")
script_dir=$(dirname "${script_path}")
repo_dir="$(realpath "${script_dir}/..")"
readonly repo_dir
readonly fixtures_dir="${repo_dir}/tests/fixtures"
unset script_path script_dir

temp_dir="$(mktemp -d)"
readonly temp_dir
volume_name=""
readonly daemon_container=dond-test-daemon proxy_container=dond-test-proxy
trap 'rm -rf "${temp_dir}"; docker rm -f "${daemon_container}" "${proxy_container}" >/dev/null 2>&1 || true; docker image rm -f dond-test-image dond-test-varrun dond-test-s6 >/dev/null 2>&1 || true; docker rm -f buildx_buildkit_dond-test-buildx0 >/dev/null 2>&1 || true; docker volume rm -f buildx_buildkit_dond-test-buildx0_state >/dev/null 2>&1 || true; if [[ -n "${volume_name}" ]]; then docker volume rm -f "${volume_name}" >/dev/null; fi' EXIT

# The daemon knows no container with this ID, as with a sidecar daemon. dond reads the ID of
# its container from the resolv.conf that Docker mounts.
mkdir "${temp_dir}/dondtestunknownid"
cp /etc/resolv.conf "${temp_dir}/dondtestunknownid/resolv.conf"

# this avoids messing with the test output during the tests itself
echo "Pulling docker images used in tests"
docker pull -q busybox
docker pull -q nginx:alpine
for docker_version in "${docker_versions[@]}"; do
  docker pull -q "docker:${docker_version}"
done

# Starts the container of the daemon tests: run_daemon_container <socket volume> <docker run arguments...>
run_daemon_container() {
  docker run --detach --name "${daemon_container}" "${run_args[@]}" --env DOND_DEBUG \
    --volume "$1" --volume "${fixtures_dir}:/wd" "${@:2}" >/dev/null
}

# Runs a command as root in the container of the daemon tests.
in_daemon_container() {
  docker exec --user 0 "${daemon_container}" "$@"
}

# Starts dond daemon in the container of the daemon tests, and prints its start line.
start_daemon() {
  in_daemon_container sh -c 'rm -f /tmp/dond.log'
  docker exec --user 0 --detach "${daemon_container}" sh -c 'exec dond daemon 2>/tmp/dond.log'
  for _ in $(seq 100); do
    if docker exec "${daemon_container}" grep -q "serving at" /tmp/dond.log 2>/dev/null; then
      break
    fi
    sleep 0.2
  done
  in_daemon_container sed 's/^/    /' /tmp/dond.log
  in_daemon_container grep -q "serving at" /tmp/dond.log
}

# Stops dond daemon with the given signal, and waits until it exited.
stop_daemon() {
  in_daemon_container pkill "-${1}" -x dond
  for _ in $(seq 50); do
    if ! docker exec "${daemon_container}" pgrep -x dond >/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  return 1
}

# Waits until the log of the container of the daemon tests has the given number of start lines.
wait_for_start_lines() {
  local count
  for _ in $(seq 100); do
    count="$(docker logs "${daemon_container}" 2>&1 | grep -c "serving at" || true)"
    if [[ "${count}" -ge "$1" ]]; then
      docker logs "${daemon_container}" 2>&1 | grep "serving at" | tail -n 1 | sed 's/^/    /'
      return 0
    fi
    sleep 0.2
  done
  docker logs "${daemon_container}"
  return 1
}

# Checks the daemon's proxy at /var/run/docker.sock: with the real docker CLI and no DOCKER_HOST, and
# with the wrapper in front of it, which must not map the paths a second time.
check_daemon() {
  in_daemon_container docker.orig run --rm --volume /test/only-inside-container:/only-inside-container busybox \
    grep -q "^test$" /only-inside-container
  in_daemon_container docker.orig run --rm --volume /wd:/wd busybox \
    grep -q "^test$" /wd/testfile
  in_daemon_container docker run --rm --volume /test/only-inside-container:/only-inside-container busybox \
    grep -q "^test$" /only-inside-container
}

for docker_version in "${docker_versions[@]}"; do
  echo "Testing with docker version: ${docker_version}"

  image_id="$(docker build --target test --build-arg "DOCKER_VERSION=${docker_version}" --quiet .)"
  docker tag "${image_id}" dond-test-image

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

  # A volume, since the root of a child under userns-remap cannot write in a directory of the runner.
  echo "A read-only mount of the container stays read-only, and a read-write one writable"
  volume_name=$(docker volume create)
  "${docker_args[@]}" --volume "${volume_name}:/ro:ro" --volume "${volume_name}:/rw" "${image_id}" sh -c '
    docker run --rm --volume /ro:/x busybox sh -c "touch /x/f 2>&1 | grep -q \"Read-only file system\"" &&
    docker run --rm --mount type=bind,source=/ro,target=/x busybox sh -c "touch /x/f 2>&1 | grep -q \"Read-only file system\"" &&
    docker run --rm --volume /rw:/x busybox touch /x/f &&
    docker run --rm --mount type=bind,source=/rw,target=/x busybox touch /x/f'
  docker volume rm -f "${volume_name}" >/dev/null
  volume_name=""

  # The wrapper needs a writable /tmp for its socket. --mount fails instead of creating /test on the host.
  echo "A container with a read-only root"
  "${docker_args[@]}" --read-only --tmpfs /tmp "${image_id}" \
    docker run --rm --mount type=bind,source=/test,target=/x busybox sh -c 'touch /x/f 2>&1 | grep -q "Read-only file system"'

  echo "With named volume"
  volume_name=$(docker volume create --opt type=none --opt device="${fixtures_dir}" --opt o=bind)
  # this confirms the volume works in the parent container without dond
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

  echo "dond wrap, with dond as docker inside, which is a wrapper inside a wrapper"
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

  # Each docker command is its own wrapper, so the builder must outlive the socket that created it.
  echo "Buildx builders created behind the wrapper keep working in later runs"
  "${docker_args[@]}" "${image_id}" sh -c '
    if ! docker.orig buildx version >/dev/null 2>&1; then
      echo "Skipped: no Buildx plugin"
      exit 0
    fi
    set -e
    builder=dond-test-buildx node=buildx_buildkit_dond-test-buildx0
    docker buildx create --name "${builder}" --driver docker-container >/dev/null
    # inspect exits 0 when it cannot reach the builder.
    docker buildx inspect --bootstrap "${builder}" >/tmp/inspect 2>&1 || true
    grep -Eq "^Endpoint:[[:space:]]+default$" /tmp/inspect && grep -Eq "^Status:[[:space:]]+running$" /tmp/inspect ||
      { cat /tmp/inspect; exit 1; }
    printf "%s\n" "FROM busybox" "RUN echo built" | docker buildx build --builder "${builder}" --quiet --output type=cacheonly - >/dev/null
    docker buildx rm "${builder}"
    [ -z "$(docker.orig ps -aq --filter "name=${node}")$(docker.orig volume ls -q --filter "name=${node}")" ]
    echo "  create --use in a script, through dond as docker inside dond wrap"
    dond wrap sh -c "docker buildx create --name ${builder} --driver docker-container --use >/dev/null"
    docker buildx inspect | grep -Eq "^Name:[[:space:]]+${builder}$"
    docker buildx rm "${builder}"'

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

  echo "Daemon: an unprepared image started the usual way"
  run_daemon_container "${host_socket}:/var/run/docker.sock" "${image_id}" sleep 3600
  start_daemon
  check_daemon
  echo "Daemon: the proxy's socket has the owner, group and mode of the real one"
  in_daemon_container sh -c 'real="$(stat -L -c %u:%g:%a /var/run/docker-host.sock)" proxy="$(stat -c %u:%g:%a /var/run/docker.sock)"
    [ "${real}" = "${proxy}" ] || { echo "real socket ${real}, proxy socket ${proxy}"; ls -ln /run; exit 1; }'
  echo "Daemon: a child container that mounts the socket gets the real one"
  in_daemon_container docker.orig run --rm "${run_args[@]}" --volume /var/run/docker.sock:/var/run/docker.sock "${image_id}" \
    docker.orig version >/dev/null
  echo "Daemon: a clean stop leaves docker working"
  stop_daemon TERM
  in_daemon_container sh -c '[ -L /var/run/docker.sock ] && docker.orig version >/dev/null'
  echo "Daemon: restart after a clean stop"
  docker restart --time 0 "${daemon_container}" >/dev/null
  start_daemon
  check_daemon
  echo "Daemon: started again after a kill"
  stop_daemon KILL
  start_daemon
  check_daemon
  echo "Daemon: restart after a kill"
  stop_daemon KILL
  docker restart --time 0 "${daemon_container}" >/dev/null
  start_daemon
  check_daemon
  docker rm --force "${daemon_container}" >/dev/null

  echo "Daemon: with a tmpfs on /run, and restart after a clean stop"
  run_daemon_container "${host_socket}:/var/run/docker.sock" --tmpfs /run "${image_id}" sleep 3600
  start_daemon
  check_daemon
  stop_daemon TERM
  docker restart --time 0 "${daemon_container}" >/dev/null
  start_daemon
  check_daemon
  docker rm --force "${daemon_container}" >/dev/null

  echo "Daemon: a main process that is not root"
  run_daemon_container "${host_socket}:/var/run/docker.sock" --user 1000:1000 "${image_id}" sleep 3600
  start_daemon
  check_daemon
  docker rm --force "${daemon_container}" >/dev/null

  echo "Daemon: the socket mounted at /var/run/docker-host.sock, as by docker-outside-of-docker"
  run_daemon_container "${host_socket}:/var/run/docker-host.sock" "${image_id}" sleep 3600
  start_daemon
  check_daemon
  docker rm --force "${daemon_container}" >/dev/null

  # A temporary directory stands for the host's /run: never mount the real one here.
  echo "Daemon: refuses to write in a directory of the host"
  mkdir "${temp_dir}/hostrun"
  echo "a file that belongs to the host" >"${temp_dir}/hostrun/docker.sock"
  run_daemon_container "${host_socket}:/var/run/docker.sock" --volume "${temp_dir}/hostrun:/run" "${image_id}" sleep 3600
  exit_code=0
  docker exec --user 0 "${daemon_container}" dond daemon 2>"${temp_dir}/hostrun.log" || exit_code=$?
  sed 's/^/    /' "${temp_dir}/hostrun.log"
  [[ "${exit_code}" -eq 1 ]]
  grep -q "refusing to write there" "${temp_dir}/hostrun.log"
  host_files="$(ls -A "${temp_dir}/hostrun")"
  [[ "${host_files}" == "docker.sock" ]]
  grep -qx "a file that belongs to the host" "${temp_dir}/hostrun/docker.sock"
  in_daemon_container docker.orig version >/dev/null
  docker rm --force "${daemon_container}" >/dev/null
  rm -r "${temp_dir}/hostrun"

  echo "Daemon: an image where /var/run is a directory, not a symlink to /run, and restart after a clean stop"
  printf '%s\n' "FROM dond-test-image" "RUN rm /var/run && mkdir /var/run" |
    docker build --quiet --tag dond-test-varrun - >/dev/null
  run_daemon_container "${host_socket}:/var/run/docker.sock" dond-test-varrun sleep 3600
  start_daemon
  check_daemon
  stop_daemon TERM
  docker restart --time 0 "${daemon_container}" >/dev/null
  start_daemon
  check_daemon
  in_daemon_container sh -c '[ -d /var/run ] && [ ! -L /var/run ] && [ -S /var/run/docker-host.sock ]'
  docker rm --force "${daemon_container}" >/dev/null

  # A capability the helper does not have makes /proc/<pid>/root refuse it.
  echo "Daemon: the route through the container's root on the host"
  run_daemon_container "${host_socket}:/var/run/docker.sock" --cap-add SYS_PTRACE "${image_id}" sleep 3600
  start_daemon
  check_daemon
  docker rm --force "${daemon_container}" >/dev/null

  echo "Daemon: the side socket when the mount cannot be moved"
  run_daemon_container "${host_socket}:/var/run/docker.sock" --cap-add SYS_PTRACE --tmpfs /run "${image_id}" sleep 3600
  start_daemon
  in_daemon_container env DOCKER_HOST=unix:///run/dond.sock docker.orig run --rm \
    --volume /test/only-inside-container:/only-inside-container busybox grep -q "^test$" /only-inside-container
  in_daemon_container docker.orig version >/dev/null
  stop_daemon TERM
  in_daemon_container sh -c '[ ! -e /run/dond.sock ] && [ ! -L /var/run/docker.sock ]'
  docker rm --force "${daemon_container}" >/dev/null

  echo "Daemon: a socket proxy in front of Docker that refuses to create containers"
  mkdir -p "${temp_dir}/proxy"
  printf '%s\n' 'user root;' 'events {}' 'http {' '  server {' '    listen unix:/proxy/docker.sock;' \
    '    location ~ ^(/v[0-9.]+)?/containers/create$ { return 403 "{\"message\": \"refused by the socket proxy\"}"; }' \
    '    location / { proxy_pass http://unix:/var/run/docker.sock; proxy_http_version 1.1; proxy_buffering off; }' \
    '  }' '}' >"${temp_dir}/nginx.conf"
  docker run --detach --name "${proxy_container}" "${run_args[@]}" --volume "${host_socket}:/var/run/docker.sock" \
    --volume "${temp_dir}/proxy:/proxy" --volume "${temp_dir}/nginx.conf:/etc/nginx/nginx.conf:ro" nginx:alpine >/dev/null
  for _ in $(seq 50); do
    if [[ -S "${temp_dir}/proxy/docker.sock" ]]; then
      break
    fi
    sleep 0.2
  done
  [[ -S "${temp_dir}/proxy/docker.sock" ]]
  run_daemon_container "${temp_dir}/proxy/docker.sock:/var/run/docker.sock" "${image_id}" sleep 3600
  start_daemon
  in_daemon_container grep -q "refused by the socket proxy" /tmp/dond.log
  in_daemon_container env DOCKER_HOST=unix:///run/dond.sock docker.orig version >/dev/null
  stop_daemon TERM
  in_daemon_container sh -c '[ ! -e /run/dond.sock ] && docker.orig version >/dev/null'
  docker rm --force "${daemon_container}" "${proxy_container}" >/dev/null

  # The helper cannot start dond from the image, and runs mv instead.
  echo "Daemon: the official image, with dond mounted from outside"
  docker run --rm --entrypoint cat "${image_id}" /usr/local/bin/docker >"${temp_dir}/dond"
  chmod +x "${temp_dir}/dond"
  run_daemon_container "${host_socket}:/var/run/docker.sock" --volume "${temp_dir}/dond:/usr/local/bin/dond:ro" \
    --entrypoint sleep "docker:${docker_version}" 3600
  start_daemon
  # Only exists in the container: --mount fails instead of creating it on the host.
  in_daemon_container docker run --rm --mount type=bind,source=/etc/alpine-release,target=/r busybox test -s /r
  docker rm --force "${daemon_container}" >/dev/null

  echo "Daemon: the s6-overlay example of the README, and restart after docker stop"
  if [[ " ${run_args[*]} " == *" --userns=host "* ]]; then
    # The files of the image belong to the remapped root, and s6-overlay refuses such a /run.
    echo "Skipped: s6-overlay does not start with --userns=host under userns-remap"
    continue
  fi
  # The Dockerfile of the README, with dond and the docker CLI of this build.
  mkdir -p "${temp_dir}/s6"
  sed -n '/^### With s6-overlay$/,/^```$/p' "${repo_dir}/README.md" | sed '1,/^```dockerfile$/d; $d' |
    sed 's|^COPY --from=ghcr.io/felipecrs/dond:[^ ]*-bin /dond |COPY --from=dond-test-image /usr/local/bin/docker |' \
      >"${temp_dir}/s6.Dockerfile"
  grep -q "^COPY --from=dond-test-image /usr/local/bin/docker /usr/local/bin/dond$" "${temp_dir}/s6.Dockerfile"
  echo "COPY --from=dond-test-image /usr/local/bin/docker.orig /usr/local/bin/docker" >>"${temp_dir}/s6.Dockerfile"
  # Again on a failure, as the download of s6-overlay from GitHub times out now and then.
  for attempt in 1 2 3; do
    if docker build --quiet --tag dond-test-s6 --file "${temp_dir}/s6.Dockerfile" "${temp_dir}/s6" >/dev/null; then
      break
    elif [[ "${attempt}" == 3 ]]; then
      exit 1
    fi
  done
  run_daemon_container "${host_socket}:/var/run/docker.sock" dond-test-s6
  wait_for_start_lines 1
  in_daemon_container docker run --rm --mount type=bind,source=/etc/s6-overlay/s6-rc.d/dond/type,target=/t busybox \
    grep -q "^longrun$" /t
  in_daemon_container docker run --rm --mount type=bind,source=/wd,target=/wd busybox grep -q "^test$" /wd/testfile
  docker stop "${daemon_container}" >/dev/null
  docker start "${daemon_container}" >/dev/null
  wait_for_start_lines 2
  docker logs "${daemon_container}" 2>&1 | grep "serving at" | tail -n 1 | grep -q "(symlink)"
  in_daemon_container docker run --rm --mount type=bind,source=/wd,target=/wd busybox grep -q "^test$" /wd/testfile
  docker rm --force "${daemon_container}" >/dev/null
done
