# AGENTS.md

docker-on-docker-proxy makes docker-on-docker work: a container that uses the host's Docker daemon gets its bind mounts mapped to the host, through a Docker API proxy. Read the [README](README.md) first, as it explains what `dond` does and why.

## Layout

The command is `cmd/dond`, a `main` package for Linux only (every file has `//go:build linux`):

- `main.go`: the entry point, `dond wrap`, `dond daemon` and dond installed as `docker`, and how the real command is found
- `upstream.go`: the endpoint the docker CLI would use (`-H`, `DOCKER_HOST`, contexts), and whether to run without the proxy
- `proxy.go`: the proxy of dond on top of the `proxy` package: the inspection of the container dond runs in, and whether the upstream is a dond proxy already
- `rewrite.go`: the mapping of a path, through the mounts of the container dond runs in, or its root directory
- `daemon.go`: `dond daemon`, which serves the proxy at `/var/run/docker.sock`
- `process_unix.go`: the listener of the proxy, and how the child process is run and signalled
- `*_test.go`: unit tests

The package `proxy` (`github.com/felipecrs/docker-on-docker-proxy/proxy`) is the Docker API proxy itself: it forwards every request to a daemon through a dial function of the caller, and maps the bind sources of `POST /containers/create` through a mapping function of the caller. It builds on Linux and Windows, as [docker-wsl-proxy](https://github.com/felipecrs/docker-wsl-proxy) imports it to serve clients of Windows with Docker in WSL. Its API exists for that and may change in any `v0` release; change it together with docker-wsl-proxy.

Also:

- `Dockerfile`: the binary (`build`), the image with only the binary (`bin`), the `docker` image with dond as `docker` (`image`, the default), and the image of the container tests (`test`)
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
docker buildx build . --target bin --build-arg VERSION=1.2.3 \
  --platform linux/amd64,linux/arm64 --output type=local,dest=build
docker buildx build . --target image --platform linux/amd64,linux/arm64
```

The version is printed in the debug output: `DOND_DEBUG=true dond wrap true`.

## Releases

Push a tag such as `v1.2.3` on `master`. ci runs all tests, and then publishes `dond-linux-amd64.tar.gz` and `dond-linux-arm64.tar.gz` (each with `dond`, as `mise use github:felipecrs/docker-on-docker-proxy` expects), `checksums.txt`, the image `ghcr.io/felipecrs/docker-on-docker-proxy` with the tags `1.2.3`, `1.2`, `1` and `latest`, and only the binary at the same tags with `-bin` (`bin` for `latest`), and the GitHub release with generated notes. A tag with a pre-release suffix, such as `v1.2.3-rc.1`, makes a pre-release and gets only `1.2.3-rc.1` and `1.2.3-rc.1-bin`. Major version 0 gets no tag of its own. Nothing is published from pull requests or `master`, but they run the same build. The tag is also the version of the Go module.

Dependencies are updated by Renovate, see `renovate.json`.

## House rules

- The smallest complete change. Prefer removing over adding, and add no option nobody asked for.
- The standard library first. A dependency needs a reason.
- Comments say why, not what, in one line where possible.
- Every behaviour a user can see, and every limit, is written in the README with its reason.
- No compatibility code for 0.x, the Bash script docker-on-docker-shim.
- Pull request titles are plain sentences, as they become the commit message.
