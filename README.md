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
- On Windows, the same binary in front of the native docker cli talks to Docker in WSL and maps the paths of Windows. See [Windows with Docker in WSL](#windows-with-docker-in-wsl).
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

The socket of the host is a bind mount at `/var/run/docker.sock`, which a container with default permissions cannot unmount, remove or rename. The kernel refuses the rename only in the mount namespace that has the mount, so the daemon asks Docker to run a short helper container that shares its PID namespace and renames `/proc/<daemon pid>/root/run/docker.sock` to `/run/docker-host.sock`. The mount follows the rename. The helper is made from the container's own image, with the shim as entrypoint (or `mv`, when the image does not have the shim at the same path), with no network and no privilege, and is removed when done. It takes 0.1 to 0.7 seconds.

Every route ends in the same layout:

- `/var/run/docker-host.sock`: the real socket, where the [docker-outside-of-docker](https://github.com/devcontainers/features/tree/main/src/docker-outside-of-docker#readme) feature of devcontainers mounts it too.
- `/var/run/docker.sock`: the proxy's socket while the daemon runs, with the owner, group and mode of the real one. Otherwise a symlink to `docker-host.sock`, so that docker keeps working without the proxy.

At start, in this order:

0. The directory of the socket, usually `/run`, must belong to the container: its root filesystem or a tmpfs. When it is a bind or volume mount, for example `-v /run:/run`, its files may be the host's, so the daemon writes nothing there and exits with an error. This needs the API only when the directory is not on the root filesystem.
1. `/var/run/docker.sock` is a symlink to `docker-host.sock`, a socket left by a daemon that was killed, or missing, and `docker-host.sock` is the real socket: the proxy takes the place at once, with no API call. This is the state after a clean stop, with docker-outside-of-docker, and with an image that ships the symlink.
2. `/var/run/docker.sock` is the mount: the helper renames it through `/proc/<pid>/root`.
3. That is refused, for example because the container has a capability the helper does not have, such as `--cap-add SYS_PTRACE`: the helper renames it through the container's root filesystem on the host instead. This does not work when `/run` is a tmpfs.
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
- The daemon refuses to start when the directory of the socket is a bind or volume mount, such as `-v /run:/run`, because it would rename or create files in a directory of the host.
- Tested in CI with AppArmor, `userns-remap` (with `--userns=host` for the containers that use the socket) and rootless Docker. Not tested with SELinux.

## Windows with Docker in WSL

On Windows, the shim runs in front of the native Windows docker cli and talks to a Docker daemon inside WSL, with no Docker Desktop. It works as on Linux, with two differences:

- The proxy listens on a named pipe with a random name, which only the current user may open, and the command gets `DOCKER_HOST=npipe:////./pipe/dond-shim-…`.
- It maps the paths of Windows in bind mounts (`-v`, `--mount`, Compose `volumes`, and the API) to WSL: `C:\dir` and `C:/dir` become `/mnt/c/dir`, and `\\wsl.localhost\<distro>\dir` becomes `/dir`. The root of the drives is read from the `[automount]` section of `/etc/wsl.conf` in the distribution, `/mnt/` by default. Paths of WSL, such as `/home/me`, are left alone.

Everything else the cli does with files of Windows works by itself, because the cli reads them on Windows: build contexts, `-f`, `--env-file`, `--cidfile`, `docker cp`, Compose files and relative paths.

It replaces [docker-wsl-shim](https://github.com/felipecrs/docker-wsl-shim), which ran the Linux docker cli through `wsl.exe` and rewrote the command line.

### Installation on Windows

1. Install Docker in the default WSL distribution, with the docker cli, and add your user there to the `docker` group.
2. Install the native docker cli for Windows, and Compose and Buildx if you use them. For example with [Scoop](https://scoop.sh), whose `docker-compose` and `docker-buildx` put the plugins in `%USERPROFILE%\.docker\cli-plugins`:

   ```powershell
   scoop install docker docker-compose docker-buildx
   ```

   Scoop's `docker` also brings `dockerd.exe`, which runs Windows containers only and is not needed. Other sources: the zips at <https://download.docker.com/win/static/stable/>, the Chocolatey packages `docker-cli` and `docker-compose`, and the Windows binaries in the releases of [docker/compose](https://github.com/docker/compose/releases) and [docker/buildx](https://github.com/docker/buildx/releases), which go to `%USERPROFILE%\.docker\cli-plugins` as `docker-compose.exe` and `docker-buildx.exe`.
3. Download `dond-windows-amd64.exe` (or `arm64`) from the [releases](https://github.com/felipecrs/docker-on-docker-shim/releases), and save it as `docker.exe` in a directory that comes before the real `docker.exe` on `PATH`. The shim runs the real one: `DOND_SHIM_DOCKER_PATH`, else `docker.orig.exe` on `PATH`, else the next `docker.exe` on `PATH`. Or save it as `dond.exe` and call `dond` instead of `docker`.

### How it reaches WSL

With nothing set up in WSL, each connection runs `wsl.exe -e docker system dial-stdio`. `wsl.exe` takes about 100 ms to start, so the shim starts two of them ahead, while the cli starts.

A listener in WSL on the vsock port 2375 is faster: the shim looks for it at start, in about 3 ms, and then connects through a Hyper-V socket in about 1 ms, with no `wsl.exe`. To set it up, in the distribution that runs Docker:

```bash
sudo apt-get install -y socat
sudo tee /etc/systemd/system/dond-vsock.service >/dev/null <<'EOF'
[Unit]
Description=Docker API on vsock port 2375, for dond-shim on Windows
Requires=docker.socket
After=docker.socket

[Service]
# -t: an attach whose input ended still receives the output of the container.
ExecStart=/usr/bin/socat -t 86400 VSOCK-LISTEN:2375,fork UNIX-CONNECT:/run/docker.sock
DynamicUser=yes
SupplementaryGroups=docker
# socat ends with 143 on SIGTERM.
SuccessExitStatus=143

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl enable --now dond-vsock.service
```

`systemd-socket-proxyd` does not work for this: an attach without input, such as `docker run` without `-i`, loses the output.

The listener gives the Docker socket, which is root in WSL, to whoever can open a Hyper-V socket to the VM of WSL: processes of your Windows user. Users in WSL outside the `docker` group cannot reach it, as vsock in WSL has no loopback. Whether other Windows users or administrators can was not tested.

### `dond daemon` on Windows

`dond daemon` serves the proxy at `\\.\pipe\docker_engine`, the default pipe of the docker cli, until Ctrl+C or Ctrl+Break. Then any client of Windows reaches Docker in WSL with the paths mapped and no wrapper: the docker cli as it is, Testcontainers, IDEs. It refuses to start when the pipe exists, for example when Docker Desktop runs, and only the current user may open it. It is also the fastest way, as no wrapper process runs for each command. Starting it with the session, for example as a scheduled task, is up to you.

### Limits on Windows

- Only the default endpoint, `npipe:////./pipe/docker_engine` with no `-H`, `DOCKER_HOST` or context other than `default`, is taken for the daemon in WSL. Another endpoint runs without the proxy.
- `\\wsl.localhost\<distro>\…` is taken for the distribution of the daemon, whatever its name.
- Paths of network shares, and `device=C:\…` in the options of a volume, are left as they are.
- `docker buildx create` with the `docker-container` driver stores the endpoint of the command, the pipe of one wrapper, which is gone the next time. Create such builders with the plain docker cli while `dond daemon` runs. The same happens on Linux with the wrapper's socket ([#34](https://github.com/felipecrs/docker-on-docker-shim/issues/34)).
- A shell in `docker run -it` can drop what is typed in the same few milliseconds as its first resize, which can come right after the prompt with `wsl.exe`.

## Environment variables

- `DOND_SHIM_DOCKER_PATH`: the docker cli to run, instead of `docker.orig` or the next `docker` on `PATH`.
- `DOND_SHIM_SKIP=true`: run the command without the proxy.
- `DOND_SHIM_WSL_DISTRO`: on Windows, the WSL distribution of the daemon, instead of the default one.
- `DOND_SHIM_DEBUG=true`: print the shim version, the endpoint it forwards to, and each mapped mount to stderr.
- `DOND_SHIM_PROXY`: set by the shim for the command it runs, so that a shim started by that command does not start a second proxy. Do not set it yourself.

`DOND_SHIM_PRINT_COMMAND` was removed, because the shim no longer changes the command line.

## Limits

- A client that ignores `DOCKER_HOST` and opens `/var/run/docker.sock` by itself is not covered by the wrapper. The [daemon mode](#daemon-mode) covers it.
- These run without the proxy, as if the shim were not installed: the `--context` flag with a context other than `default`, TLS (`--tls*` flags, `DOCKER_TLS_VERIFY`, `DOCKER_TLS`, or a context with TLS material), and endpoints other than `unix://` and `tcp://`, such as `ssh://`.
- When the daemon does not know the container the shim runs in, for example a daemon in a sidecar container, the shim changes nothing.
- The supported storage drivers are `overlay2` and `overlayfs` (the containerd image store). With another one, mounting a path that is not under one of the container's mounts fails with an error that names the driver.
- As a wrapper, one extra process lives for the duration of each command.
