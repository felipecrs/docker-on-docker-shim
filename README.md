# docker-on-docker-shim

A small Docker API proxy that makes bind mounts work in a container that uses the Docker daemon of its host, also known as docker-on-docker or docker-outside-of-docker.

Available for `linux/amd64` and `linux/arm64`. On Windows with Docker in WSL, see [docker-wsl-proxy](https://github.com/felipecrs/docker-wsl-proxy), which is built on the same proxy.

## The problem

In a container started with `-v /var/run/docker.sock:/var/run/docker.sock`, `docker run -v /workspace/app:/app …` sends the path `/workspace/app` to the daemon, and the daemon looks for it on the host. The directory of your container is not there, so Docker creates an empty one on the host and mounts that.

It shows up in:

- [devcontainers](https://containers.dev/) with the [docker-outside-of-docker](https://github.com/devcontainers/features/tree/main/src/docker-outside-of-docker#readme) feature
- CI jobs that run in a container and start other containers with the sources mounted
- Compose files with relative paths in `volumes`
- tests with [Testcontainers](https://testcontainers.com/) that mount files, when the tests run in a container
- Stack Overflow: [Docker in Docker cannot mount volume](https://stackoverflow.com/questions/31381322)

## Try it

`/usr/local/bin/dind` exists only in the `docker` image, not on your host:

```bash
# Fails: the daemon looks for /usr/local/bin/dind on the host
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock docker:latest \
  docker run --rm -v /usr/local/bin/dind:/dind alpine test -f /dind

# Works: the same image, with the shim installed as docker
docker run --rm -v /var/run/docker.sock:/var/run/docker.sock ghcr.io/felipecrs/dond-shim:latest \
  docker run --rm -v /usr/local/bin/dind:/dind alpine test -f /dind
```

## How a path is mapped

The shim forwards every request to the daemon unchanged, except the one that creates a container. There it changes the source of each bind mount (`-v /path:…`, `--mount type=bind`, Compose `volumes`, or the API), after asking Docker about the container it runs in:

1. A path inside a mount of your container, bind or volume, becomes the same place on the host. If your container was started with `-v /home/me/project:/workspace`, then `/workspace/app` becomes `/home/me/project/app`.
2. Any other path is a file of your container itself, and becomes a path inside the root directory of your container on the host, such as `/var/lib/docker/overlay2/<id>/merged/usr/local/bin/dind`.
3. When mounts of your container sit inside the directory you asked for, they are added too, so that the new container sees the same tree as you do.
4. A path that your container can only read, because its mount is read-only or the container was started with `--read-only`, is mounted read-only, even if you did not ask for `:ro`.
5. The proxy's own socket, which some tools mount into a container (Testcontainers does, for Ryuk), becomes the real Docker socket.

Named volumes (`-v name:/path`), `tmpfs` mounts and every other request go through as they are.

## Installation

The shim is one static binary, `dond`:

- in the image `ghcr.io/felipecrs/dond-shim-bin`, at `/dond`, to copy into your own image
- in `dond-shim-linux-amd64.tar.gz` and `dond-shim-linux-arm64.tar.gz` of each [release](https://github.com/felipecrs/docker-on-docker-shim/releases), with a `checksums.txt`. With [mise](https://mise.jdx.dev): `mise use -g github:felipecrs/docker-on-docker-shim`
- installed already in the image `ghcr.io/felipecrs/dond-shim`, which is the [`docker`](https://hub.docker.com/_/docker) image with the shim as `docker`

It runs in one of two ways:

| | Wrapper | `dond daemon` |
|---|---|---|
| What runs | a proxy for each command, gone when the command ends | one proxy for the container, until it is stopped |
| Covers | the command it wraps, and what that command starts | every program in the container, also from `docker exec` |
| Needs | nothing | root, and something that keeps it running |
| Good for | CI jobs, one-off commands, images with one main command | containers that supervise services already, with s6-overlay or systemd |

### Wrapper, as `docker`

Rename the docker cli to `docker.orig` and put the shim in its place. Everyone keeps calling `docker`:

```dockerfile
FROM docker:latest

ARG DOCKER_PATH="/usr/local/bin/docker"
RUN mv -f "${DOCKER_PATH}" "${DOCKER_PATH}.orig"
COPY --from=ghcr.io/felipecrs/dond-shim-bin:0.10.0 /dond "${DOCKER_PATH}"
```

The shim runs `docker.orig` from `PATH`, or else the next `docker` on `PATH` that is not the shim, so a directory that comes earlier on `PATH` works too.

### Wrapper, as `dond`

Leave `docker` alone, and call `dond` where the paths must be mapped:

```dockerfile
COPY --from=ghcr.io/felipecrs/dond-shim-bin:0.10.0 /dond /usr/local/bin/dond
```

```bash
dond run --rm -v "$PWD:/src" alpine ls /src
```

### Wrapper, for other programs

`dond wrap <command>` runs any program behind the proxy, by setting `DOCKER_HOST` for it. This is for programs that talk to Docker themselves, such as Testcontainers:

```bash
dond wrap mvn test
dond wrap ./gradlew test
```

A symlink to the shim, or a copy of it, under another name does the same for the program of that name. The real one is `<name>.orig` on `PATH`, or else the next `<name>` on `PATH`:

```dockerfile
RUN ln -s /usr/local/bin/dond /usr/local/sbin/mvn
```

### `dond daemon`

```bash
dond daemon
```

It serves the proxy at `/var/run/docker.sock` inside the container until `SIGTERM` or `SIGINT`. The container is started the usual way, with `-v /var/run/docker.sock:/var/run/docker.sock` and no extra permission. The daemon must run as root. The main process of the container does not have to.

### With s6-overlay

A complete image with [s6-overlay](https://github.com/just-containers/s6-overlay), which runs the daemon as a `longrun` service. The tests build this very example:

```dockerfile
FROM ubuntu:latest

ARG S6_OVERLAY_VERSION="3.2.1.0"
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz /tmp
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-x86_64.tar.xz /tmp
RUN apt-get update && apt-get install -y xz-utils &&   tar -C / -Jxpf /tmp/s6-overlay-noarch.tar.xz &&   tar -C / -Jxpf /tmp/s6-overlay-x86_64.tar.xz

COPY --from=ghcr.io/felipecrs/dond-shim-bin:0.10.0 /dond /usr/local/bin/dond

# A longrun service that runs dond daemon
RUN mkdir -p /etc/s6-overlay/s6-rc.d/dond /etc/s6-overlay/s6-rc.d/user/contents.d &&   echo longrun >/etc/s6-overlay/s6-rc.d/dond/type &&   printf '#!/bin/sh
exec dond daemon
' >/etc/s6-overlay/s6-rc.d/dond/run &&   chmod +x /etc/s6-overlay/s6-rc.d/dond/run &&   touch /etc/s6-overlay/s6-rc.d/user/contents.d/dond

ENTRYPOINT ["/init"]
```

s6 stops the daemon with `SIGTERM`, which puts the symlink back. Programs that start before the daemon serves reach the real socket, without the mapping.

To start it faster and with no helper container ([see below](#how-the-daemon-takes-the-place-of-the-socket)), ship this symlink in the image, or mount the socket at `/var/run/docker-host.sock`, as the docker-outside-of-docker feature of devcontainers does:

```dockerfile
RUN ln -s docker-host.sock /run/docker.sock
```

## With other tools

- **Docker Compose**: `docker compose` works behind the wrapper and the daemon, also with relative paths and with files that exist only in the container. For the standalone `docker-compose`, install the shim under that name, the same way as `docker`.
- **Buildx**: builds need no mapping, and work. See [the note on builders](#why-is-it-made-this-way) for `docker buildx create` behind the wrapper.
- **Testcontainers**: run the tests with `dond wrap`, or run `dond daemon`.
- **devcontainers**: the docker-outside-of-docker feature mounts the real socket at `/var/run/docker-host.sock`. For a non-root user it may start `socat` at `/var/run/docker.sock`. The wrapper works on top of that. `dond daemon` refuses to start then, because the place is taken.

## Environment variables

- `DOND_SHIM_DOCKER_PATH`: the docker cli to run, instead of `docker.orig` or the next `docker` on `PATH`.
- `DOND_SHIM_SKIP=true`: run the command without the proxy.
- `DOND_SHIM_DEBUG=true`: print the version, the endpoint the proxy forwards to, and each path it maps, to stderr.

The shim sets `DOND_SHIM_PROXY` for the command it runs, so that a shim started by that command does not start a second proxy. Do not set it yourself.

## How the daemon takes the place of the socket

The socket of the host is a bind mount at `/var/run/docker.sock`. A container with default permissions cannot unmount it, remove it or rename it. The kernel refuses the rename only in the mount namespace that has the mount, so the daemon asks Docker for a short helper container that shares its PID namespace and renames `/proc/<daemon pid>/root/run/docker.sock` to `/run/docker-host.sock`. The mount follows the rename. The helper is made from the image of your container, with the shim as entrypoint (or `mv`, when the image does not have the shim at the same path). It has no network and no privilege, takes 0.1 to 0.7 seconds, and is removed when done.

The result is always the same two files:

- `/var/run/docker-host.sock`: the real socket.
- `/var/run/docker.sock`: the proxy's socket while the daemon runs, with the owner, group and mode of the real one. After the daemon stops, a symlink to `docker-host.sock`, so that docker goes on working without the proxy.

At start, in this order:

1. The directory of the socket, usually `/run`, must belong to the container: its root filesystem or a `tmpfs`. When it is a bind or volume mount, such as `-v /run:/run`, its files may be the host's, so the daemon writes nothing there and exits with an error.
2. `/var/run/docker.sock` is a symlink to `docker-host.sock`, or a socket left by a daemon that was killed, or missing, and `docker-host.sock` is the real socket: the proxy takes the place at once. This is the case after a clean stop, with the symlink in the image, and with the socket mounted at `docker-host.sock`.
3. `/var/run/docker.sock` is the mount: the helper renames it through `/proc/<pid>/root`.
4. Docker refuses that, for example because the container has a capability the helper does not have, such as `--cap-add SYS_PTRACE`: the helper renames it through the root directory of the container on the host. This does not work when `/run` is a `tmpfs`.
5. Neither works: the daemon says why, and serves at `/run/dond.sock`. Programs must then be given `DOCKER_HOST=unix:///run/dond.sock`.

After a kill, the dead socket stays at `/var/run/docker.sock`, and docker in the container fails until the daemon is started again. It recovers by itself then, also after a restart of the container.

A wrapper inside a container that runs the daemon sees that it talks to a dond proxy, from a response header, and leaves the paths to the daemon, so that they are not mapped twice.

## Limits

- **Storage drivers**: `overlay2` and `overlayfs` (the containerd image store). With another one, a path that is not inside a mount of your container fails with an error that names the driver. Paths inside mounts work with any driver.
- **A daemon that does not know your container**, such as a docker-in-docker sidecar in GitLab CI: the shim changes nothing, because that daemon has its own files, and yours mean nothing to it.
- **Endpoints**: the proxy serves `unix://` and `tcp://` endpoints without TLS, from `-H`, `DOCKER_HOST` or the current docker context. With TLS (`--tls*`, `DOCKER_TLS_VERIFY`, a context with certificates), with `ssh://`, and with `--context` naming a context other than `default`, the command runs as if the shim were not installed.
- **The wrapper covers programs that honour `DOCKER_HOST`.** A program that opens `/var/run/docker.sock` by itself needs `dond daemon`.
- **The wrapper adds one process** for the time of each command, and needs a temporary directory it can write to (`TMPDIR`, or `/tmp`) for its socket. In a container started with `--read-only` and no `--tmpfs /tmp`, it prints a warning and runs the command without the proxy.
- **`dond daemon`**:
  - serves `/var/run/docker.sock` only, and ignores `DOCKER_HOST`;
  - needs Docker to accept `--pid=container:<id>`, or a bind mount of the root directory of the container, from it. A filtering socket proxy in front of Docker that refuses both leaves only `/run/dond.sock`;
  - refuses to start when something else serves `/var/run/docker.sock` already;
  - is tested in CI with AppArmor, with `userns-remap` (with `--userns=host` for the containers that use the socket) and with rootless Docker. It is not tested with SELinux.
- **Volumes made with a bind `device`** (`docker volume create --opt type=none --opt o=bind --opt device=/path`) are created with the path as given. Mounting such a volume that already exists works.
- **Podman** is not tested and not supported.
- **Docker versions**: tested with every release from `18.03` to the latest.

## Why is it made this way?

The aim is that nothing changes for whoever calls `docker`. Where that cannot be had, the shim says so or steps aside. It does not guess.

- **Docker resolves the paths on the host of the daemon, by design.** The [documentation](https://docs.docker.com/engine/storage/bind-mounts/) says so, and so do the maintainers in [moby/moby#22752](https://github.com/moby/moby/issues/22752), closed in 2016.
  - A request for path mapping rules in the docker cli, [docker/cli#3786](https://github.com/docker/cli/issues/3786), is open since 2022 with no answer.
  - The same was reported to Compose in [docker/compose#3918](https://github.com/docker/compose/issues/3918) and [docker/compose#4585](https://github.com/docker/compose/issues/4585), and closed without a change. [docker/compose#11867](https://github.com/docker/compose/issues/11867) is open, about copying configs and secrets instead of mounting them.
  - Volume subpaths ([Docker 26](https://github.com/moby/moby/pull/45687)) and image mounts ([Docker 28](https://docs.docker.com/engine/storage/image-mounts/)) do not share a directory of a running container. If Docker let a client say "this path is inside container X", this project would not be needed.
- **It works on the Docker API, not on the command line.** Up to 0.9 the shim was a Bash script that rewrote `--volume` and `--mount` of `docker run`. It had to know every docker option that takes a value ([#22](https://github.com/felipecrs/docker-on-docker-shim/pull/22)), Compose needed a shim of its own that was never finished ([#17](https://github.com/felipecrs/docker-on-docker-shim/pull/17)), and nothing could be done for Testcontainers. Every client sends the same request to create a container, so the shim changes that request and nothing else ([#24](https://github.com/felipecrs/docker-on-docker-shim/issues/24)).
- **The wrapper is one process for each command, on a private socket.** It needs no service, so it works in a CI step and on hosts you do not control.
  - The price is that buildx saves the address of the endpoint with each builder it creates ([docker/buildx#1410](https://github.com/docker/buildx/issues/1410), open since 2022), and the private socket is gone after the command. So when a command ends, the wrapper replaces its own address in the files of buildx, and `docker buildx use` behind the wrapper is saved as if it had `--global` ([#34](https://github.com/felipecrs/docker-on-docker-shim/issues/34)). A wrapper that is killed cannot do that, and leaves a builder that must be removed and created again.
  - One socket with a fixed name for all wrappers was considered and dropped: a long build could land on the proxy of a short command and break when that one exits.
- **`dond daemon` takes the place of `/var/run/docker.sock`**, where it could have asked for the socket at another path. This way programs that open the socket by themselves, and shells from `docker exec`, are covered with no setting, in a container that is started like any other.
  - The price is the helper container and the need for root. Mounting the socket at `docker-host.sock`, or the symlink in the image, avoids both the helper and the rename.
  - It never writes to a directory that may belong to the host, and it fails with a message where it cannot serve.
- **It asks Docker where the container is, once for each command.** 0.8.1 worked the root directory out without asking, and 0.8.2 took that back, because the data directory of Docker cannot be guessed everywhere, for example on Home Assistant OS ([#19](https://github.com/felipecrs/docker-on-docker-shim/pull/19), [#20](https://github.com/felipecrs/docker-on-docker-shim/pull/20)).
- **It steps aside when the daemon is not the one of the host.** A docker-in-docker sidecar sees its own files ([#3](https://github.com/felipecrs/docker-on-docker-shim/issues/3), [#21](https://github.com/felipecrs/docker-on-docker-shim/pull/21)), and a daemon reached with TLS or SSH is on another machine, where no path of the container exists.
- **Read-only stays read-only** ([#5](https://github.com/felipecrs/docker-on-docker-shim/issues/5)). With a nested Docker, a bind mount of a directory that is mounted read-only is read-only too, and the new container should see what your container sees. It is not a security measure: whoever reaches the Docker socket is root on the host.
- **No subpath mounts** ([#11](https://github.com/felipecrs/docker-on-docker-shim/issues/11)). A path inside a named volume is mapped to the directory of the volume on the host, which works with every Docker version. A subpath mount would need Docker 26 and would not make anything work that fails today.
- **No config file** ([#32](https://github.com/felipecrs/docker-on-docker-shim/issues/32)). The settings are three environment variables, which is how a container image is configured anyway.
- **Nothing is kept from the Bash shim.** The Go version is a rewrite, and options that have no meaning in it, such as `DOND_SHIM_PRINT_COMMAND`, are gone.
- **Windows is another project** ([#35](https://github.com/felipecrs/docker-on-docker-shim/issues/35)). Its users, installation and limits have nothing in common with the ones here. What they share is the proxy, which is a Go package of this module, [`proxy`](proxy), that [docker-wsl-proxy](https://github.com/felipecrs/docker-wsl-proxy) imports. The API of the package exists for that project and may change in any `v0` release.

## Alternatives

- **Mount the directory at the same path as on the host**: `-v "$PWD:$PWD" -w "$PWD"` when you start the outer container. It is free and needs no tool. [Testcontainers](https://java.testcontainers.org/supported_docker_environment/continuous_integration/dind_patterns/) asks for it, [Jenkins](https://www.jenkins.io/doc/book/pipeline/docker/) gets the same effect with `--volumes-from`, and [GitLab](https://docs.gitlab.com/ci/docker/using_docker_build/#known-issues-with-docker-socket-binding) lists the problem as a known issue. It works for directories that come from the host and whose path you control. It does not work for files that exist only in the container.
- **In devcontainers**, pass `${localWorkspaceFolder}` into the container and build host paths by hand, as [the feature's README](https://github.com/devcontainers/features/blob/main/src/docker-outside-of-docker/README.md) describes. It has the same limits, and there is no such folder with "Clone Repository in Container Volume".
- **Docker in Docker**, a daemon of its own inside the container ([`docker:dind`](https://hub.docker.com/_/docker)): paths just work, because that daemon sees the files of the container. It needs `--privileged`, and shares no images, caches or networks with the host. [Sysbox](https://github.com/nestybox/sysbox), free and owned by Docker since 2022, removes the need for `--privileged`, and must be installed on the host as a container runtime.
- **Testcontainers Cloud**, a paid service of Docker, runs the containers of your tests on its machines instead. It is for Testcontainers only.

## Development

See [AGENTS.md](AGENTS.md).
