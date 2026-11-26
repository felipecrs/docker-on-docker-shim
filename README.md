# docker-on-docker-shim

A shim that remaps volume mounts so they work when running docker on docker.

## Why?

Have you found yourself trying to run docker on docker, but the volume mounts don't work?

This is because when you run docker on docker (i.e. reusing the docker daemon from the host within a container - `--volume /var/run/docker.sock:...`) and you execute a `docker run` command, the volume mounts (with `--volume` or `--mount`) will be interpreted by the docker daemon as paths from the host, not from the container.

Well, this dilema is very common to me, and that's why I created this shim.

## How it works

The shim is a small Docker API proxy. It runs in one of two modes:

- **Wrapper** (the default): the proxy lives for the duration of one command. It needs no supervisor and suits one-off commands, CI jobs and images that run one main command.
- **Daemon** (`dond daemon`): the proxy keeps running and serves at `/var/run/docker.sock` in the container. Every client in the container is covered, including clients that open the socket path by themselves and processes started with `docker exec`, with no wrapper and no `DOCKER_HOST`. It suits containers that already supervise services, with s6-overlay or systemd for example. See [Daemon mode](#daemon-mode).

As a wrapper:

1. It finds the endpoint the command would use (`-H`, `DOCKER_HOST`, the docker context, or `/var/run/docker.sock`).
2. It listens on a private socket and starts the real command with `DOCKER_HOST` pointing at that socket.
3. It forwards every request unchanged, except the creation of a container, where it maps the source of each bind mount to the host: through the mounts of the container the shim runs in, or else through that container's root directory on the host.
4. When the command exits, the shim exits with the same exit code. Nothing keeps running.

Because it works on the Docker API and not on the command line, it covers `docker run`, `docker create`, Docker Compose, Buildx, every CLI plugin, and other clients that use `DOCKER_HOST`.

## Use cases

Although this shim can be used in many cases, some of the most common ones are:

- [devcontainers](https://containers.dev/) with [docker-outside-of-docker](https://github.com/devcontainers/features/tree/master/src/docker-outside-of-docker#readme)
- StackOverflow: [Docker in Docker cannot mount volume](https://stackoverflow.com/questions/31381322)
- Tests that use [Testcontainers](https://testcontainers.com/) in a container

## Highlights

- Tested with all versions of docker from `18.03` to `latest`.
- One static binary for Linux (`amd64` and `arm64`), with no dependencies.
- Rename the docker cli to `docker.orig`, place this shim where the docker cli was, and continue calling `docker` as usual.
- Or install this shim as the `dond` command and call `dond` instead of `docker`.
- Or run any other command behind it with `dond wrap <command>`, or with a symlink named after that command.
- Or run `dond daemon` to serve the proxy at `/var/run/docker.sock` for every client in the container.

## Try it yourself

In the following example, the `/usr/local/bin/dind` file is only available within the container. See how the shim makes mounting it work:

```bash
# This does not work
$ docker run --rm -v /var/run/docker.sock:/var/run/docker.sock docker:latest \
    docker run --rm -v /usr/local/bin/dind:/dind alpine test -f /dind

# A non-zero exit code indicates that it did not work
$ echo $?
1

# This works
$ docker run --rm -v /var/run/docker.sock:/var/run/docker.sock ghcr.io/felipecrs/dond-shim:latest \
    docker run --rm -v /usr/local/bin/dind:/dind alpine test -f /dind

# A zero exit code indicates that it worked
$ echo $?
0
```

## Installation

The shim is a single binary at `/dond` in the `ghcr.io/felipecrs/dond-shim-bin` image. You have a few choices on how to install it:

### As `docker`

This allows you to continue calling `docker` as usual.

```dockerfile
FROM docker:latest

# Rename the docker cli to docker.orig
ARG DOCKER_PATH="/usr/local/bin/docker"
RUN mv -f "${DOCKER_PATH}" "${DOCKER_PATH}.orig"

# Install dond-shim at the same path as the original docker cli
COPY --from=ghcr.io/felipecrs/dond-shim-bin:0.10.0 /dond "${DOCKER_PATH}"
```

### As `dond`

So that you can call `dond` instead of calling `docker`.

```dockerfile
FROM docker:latest

# Install dond-shim to /usr/local/bin/dond
COPY --from=ghcr.io/felipecrs/dond-shim-bin:0.10.0 /dond /usr/local/bin/dond
```

## Docker Compose

Docker Compose works, since it uses the same `DOCKER_HOST` as the docker cli that starts it:

```bash
docker compose up
```

Relative paths in the compose file work too, also for files that only exist in the container.

For the standalone `docker-compose`, install the shim as `docker-compose` the same way as `docker`: rename the real one to `docker-compose.orig`, or put a symlink to the shim named `docker-compose` before it on `PATH`.

## Other commands: `dond wrap` and symlinks

`dond wrap <command> [args...]` runs any command behind the proxy. This covers clients that talk to the Docker API on their own, such as Testcontainers:

```bash
dond wrap mvn test
dond wrap ./gradlew test
```

The shim also behaves this way under any other name: a symlink to it, or a copy of it, named `mvn` runs the real `mvn` behind the proxy. The real command is `mvn.orig` on `PATH`, or else the next `mvn` on `PATH` that is not the shim:

```dockerfile
RUN ln -s /usr/local/bin/dond /usr/local/sbin/mvn
```

Testcontainers mounts the socket that `DOCKER_HOST` names into its Ryuk container. The shim maps it to the real socket.

## Daemon mode

`dond daemon` serves the proxy at `/var/run/docker.sock` inside the container until it receives `SIGTERM` or `SIGINT`. The container is started the usual way, with `-v /var/run/docker.sock:/var/run/docker.sock` and no extra permission, and the image needs nothing more than the binary. The daemon must run as root, but the main process of the container does not have to.

```bash
dond daemon
```

### How it takes the place of the socket

The socket of the host is a bind mount at `/var/run/docker.sock`, which a container with default permissions cannot unmount, remove or rename. The kernel refuses the rename only in the mount namespace that has the mount, so the daemon asks Docker to run a short helper container that shares its PID namespace and renames `/proc/<daemon pid>/root/run/docker.sock` to `/run/docker-host.sock`. The mount follows the rename. The helper is made from the container's own image, with the shim as entrypoint (or `mv`, when the image does not have the shim at the same path), with no network and no privilege, and is removed when done. It takes about half a second.

Every route ends in the same layout:

- `/var/run/docker-host.sock`: the real socket, where the [docker-outside-of-docker](https://github.com/devcontainers/features/tree/main/src/docker-outside-of-docker#readme) feature of devcontainers mounts it too.
- `/var/run/docker.sock`: the proxy's socket while the daemon runs, with the owner, group and mode of the real one. Otherwise a symlink to `docker-host.sock`, so that docker keeps working without the proxy.

At start, in this order:

1. `/var/run/docker.sock` is a symlink to `docker-host.sock`, a socket left by a daemon that was killed, or missing, and `docker-host.sock` is the real socket: the proxy takes the place at once, with no API call. This is the state after a clean stop, with docker-outside-of-docker, and with an image that ships the symlink.
2. `/var/run/docker.sock` is the mount: the helper renames it through `/proc/<pid>/root`.
3. That is refused, for example because the container has a capability the helper does not have, such as `--cap-add SYS_PTRACE`: the helper renames it through the container's root directory on the host instead. This does not work when `/run` is a tmpfs.
4. Neither works: the daemon prints why, and serves at `/run/dond.sock` instead. Clients must then set `DOCKER_HOST=unix:///run/dond.sock`.

On a clean stop the daemon puts the symlink back. After a kill, the dead socket stays at `/var/run/docker.sock`, and docker in the container fails until the daemon is started again, which recovers, also after a restart of the container.

A wrapper inside a container that runs the daemon sees that its upstream is a dond proxy, from a response header, and leaves the paths to the daemon, so they are not mapped twice.

To skip the helper from the first start, ship the symlink in the image. Docker follows it when it mounts the socket:

```dockerfile
RUN ln -s docker-host.sock /run/docker.sock
```

### With s6-overlay

```dockerfile
FROM ubuntu:latest

ARG S6_OVERLAY_VERSION="3.2.1.0"
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz /tmp
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-x86_64.tar.xz /tmp
RUN apt-get update && apt-get install -y xz-utils && \
  tar -C / -Jxpf /tmp/s6-overlay-noarch.tar.xz && \
  tar -C / -Jxpf /tmp/s6-overlay-x86_64.tar.xz

COPY --from=ghcr.io/felipecrs/dond-shim-bin:0.10.0 /dond /usr/local/bin/dond

# A longrun service that runs dond daemon
RUN mkdir -p /etc/s6-overlay/s6-rc.d/dond /etc/s6-overlay/s6-rc.d/user/contents.d && \
  echo longrun >/etc/s6-overlay/s6-rc.d/dond/type && \
  printf '#!/bin/sh\nexec dond daemon\n' >/etc/s6-overlay/s6-rc.d/dond/run && \
  chmod +x /etc/s6-overlay/s6-rc.d/dond/run && \
  touch /etc/s6-overlay/s6-rc.d/user/contents.d/dond

ENTRYPOINT ["/init"]
```

s6 stops the daemon with `SIGTERM`, which puts the symlink back. Clients that start before the daemon serves reach the real socket, without the mapping.

### Limits of the daemon mode

- Docker must accept `--pid=container:<id>` from the client, or else bind mounts of the container's root directory: a socket proxy in front of Docker that refuses both leads to the side socket.
- The daemon serves `/var/run/docker.sock` only; it ignores `DOCKER_HOST`.
- A socket proxy already at `/var/run/docker.sock`, such as the one docker-outside-of-docker starts with `socat` for a non-root user, is left alone: the daemon refuses to start.
- Not tested: SELinux, rootless Docker, and `userns-remap`.

## Environment variables

- `DOND_SHIM_DOCKER_PATH`: the docker cli to run, instead of `docker.orig` or the next `docker` on `PATH`.
- `DOND_SHIM_SKIP=true`: run the command without the proxy.
- `DOND_SHIM_DEBUG=true`: print the shim version, the endpoint it forwards to, and each mapped mount to stderr.
- `DOND_SHIM_PROXY`: set by the shim for the command it runs, so that a shim started by that command does not start a second proxy. Do not set it yourself.

`DOND_SHIM_PRINT_COMMAND` was removed, because the shim no longer changes the command line.

## Limits

- A client that ignores `DOCKER_HOST` and opens `/var/run/docker.sock` by itself is not covered by the wrapper. The [daemon mode](#daemon-mode) covers it.
- These run without the proxy, as if the shim were not installed: the `--context` flag, TLS (`--tls*` flags, `DOCKER_TLS_VERIFY`, `DOCKER_TLS`, or a context with TLS material), and endpoints other than `unix://` and `tcp://`, such as `ssh://`.
- When the daemon does not know the container the shim runs in, for example a daemon in a sidecar container, the shim changes nothing.
- The supported storage drivers are `overlay2` and `overlayfs` (the containerd image store). With another one, mounting a path that is not under one of the container's mounts fails with an error that names the driver.
- As a wrapper, one extra process lives for the duration of each command.
