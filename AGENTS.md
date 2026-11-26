# AGENTS.md

dond-shim makes docker-on-docker work: a container that uses the host's Docker daemon gets its bind mounts mapped to the host, through a Docker API proxy. Read the [README](README.md) first, as it explains what the shim does and why.

## Layout

The command is `cmd/dond`, a `main` package for Linux only (every file has `//go:build linux`):

- `main.go`: the entry point, `dond wrap`, `dond daemon` and the shim installed as `docker`, and how the real command is found
- `upstream.go`: the endpoint the docker CLI would use (`-H`, `DOCKER_HOST`, contexts), and whether to run without the proxy
- `proxy.go`: the proxy of dond on top of the `proxy` package: the inspection of the container the shim runs in, and whether the upstream is a dond proxy already
- `rewrite.go`: the mapping of a path, through the mounts of the container the shim runs in, or its root directory
- `daemon.go`: `dond daemon`, which serves the proxy at `/var/run/docker.sock`
- `process_unix.go`: the listener of the proxy, and how the child process is run and signalled
- `*_test.go`: unit tests

The package `proxy` (`github.com/felipecrs/docker-on-docker-shim/proxy`) is the Docker API proxy itself: it forwards every request to a daemon through a dial function of the caller, and maps the bind sources of `POST /containers/create` through a mapping function of the caller. It builds on Linux and Windows, as [docker-wsl-proxy](https://github.com/felipecrs/docker-wsl-proxy) imports it to serve clients of Windows with Docker in WSL. Its API exists for that and may change in any `v0` release; change it together with docker-wsl-proxy.

Also:

- `Dockerfile`: the binary (`build`), the image with only the binary (`dond-shim-bin`), the `docker` image with the shim (`dond-shim`), and the image of the container tests (`test`)
- `scripts/test.sh`: the container tests, with `tests/fixtures`
- `.github/workflows/ci.yaml`: all tests, the build, and the release; `build.yaml`: the build, called by it

## Build and test

```sh
go test ./...                 # unit tests; on Windows they cover the proxy package only
scripts/test.sh 29            # container tests against the host's Docker, with the docker:29 image
scripts/test.sh 20.10 29      # several versions, as ci does from 18.03 on
```

`scripts/test.sh` needs Docker, and builds the `test` stage of the `Dockerfile`.

To build what a release publishes, without pushing it:

```sh
docker buildx build . --target dond-shim-bin --build-arg VERSION=1.2.3 \
  --platform linux/amd64,linux/arm64 --output type=local,dest=build
docker buildx build . --target dond-shim --platform linux/amd64,linux/arm64
```

The version is printed in the debug output: `DOND_SHIM_DEBUG=true dond wrap true`.

## Releases

Push a tag such as `v1.2.3` on `master`. ci runs all tests, and then publishes `dond-shim-linux-amd64.tar.gz` and `dond-shim-linux-arm64.tar.gz` (each with `dond`, as `mise use github:felipecrs/docker-on-docker-shim` expects), `checksums.txt`, both images with the tags `1.2.3` and `latest`, and the GitHub release with generated notes. A tag with a pre-release suffix, such as `v1.2.3-rc.1`, makes a pre-release and leaves `latest` alone. Nothing is published from pull requests or `master`, but they run the same build. The tag is also the version of the Go module.

Dependencies are updated by Renovate, see `renovate.json`.

## House rules

- The smallest complete change. Prefer removing over adding, and add no option nobody asked for.
- The standard library first. A dependency needs a reason.
- Comments say why, not what, in one line where possible.
- Every behaviour a user can see, and every limit, is written in the README with its reason.
- No compatibility code for the Bash shim (0.x).
- Pull request titles are plain sentences, as they become the commit message.
