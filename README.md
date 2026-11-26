# docker-on-docker-shim

A shim that remaps volume mounts so they work when running docker on docker.

## Why?

Have you found yourself trying to run docker on docker, but the volume mounts don't work?

This is because when you run docker on docker (i.e. reusing the docker daemon from the host within a container - `--volume /var/run/docker.sock:...`) and you execute a `docker run` command, the volume mounts (with `--volume` or `--mount`) will be interpreted by the docker daemon as paths from the host, not from the container.

Well, this dilema is very common to me, and that's why I created this shim.

## How it works

The shim is a wrapper with a small Docker API proxy inside it:

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

## Environment variables

- `DOND_SHIM_DOCKER_PATH`: the docker cli to run, instead of `docker.orig` or the next `docker` on `PATH`.
- `DOND_SHIM_SKIP=true`: run the command without the proxy.
- `DOND_SHIM_DEBUG=true`: print the shim version, the endpoint it forwards to, and each mapped mount to stderr.
- `DOND_SHIM_PROXY`: set by the shim for the command it runs, so that a shim started by that command does not start a second proxy. Do not set it yourself.

`DOND_SHIM_PRINT_COMMAND` was removed, because the shim no longer changes the command line.

## Limits

- A client that ignores `DOCKER_HOST` and opens `/var/run/docker.sock` by itself is not covered.
- These run without the proxy, as if the shim were not installed: the `--context` flag, TLS (`--tls*` flags, `DOCKER_TLS_VERIFY`, `DOCKER_TLS`, or a context with TLS material), and endpoints other than `unix://` and `tcp://`, such as `ssh://`.
- When the daemon does not know the container the shim runs in, for example a daemon in a sidecar container, the shim changes nothing.
- The supported storage drivers are `overlay2` and `overlayfs` (the containerd image store). With another one, mounting a path that is not under one of the container's mounts fails with an error that names the driver.
- One extra process lives for the duration of each command.
