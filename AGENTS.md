# AGENTS.md

dond-shim runs a command behind a Docker API proxy that maps the sources of bind mounts to the host. Read the [README](README.md) first, as it explains what the shim does and why.

## Layout

The command is `cmd/dond`, a single `main` package:

- `main.go`: the entry point, `dond wrap`, `dond daemon` and the shim installed as `docker`, and how the real command is found
- `upstream.go`: the endpoint the docker CLI would use (`-H`, `DOCKER_HOST`, contexts), and whether to run without the proxy
- `proxy.go`: the proxy, which passes everything through and maps the paths of `POST /containers/create`
- `rewrite.go`: the mapping of a create request, through the mounts of the container the shim runs in
- `wsl.go`: the paths of Windows mapped to WSL
- `daemon.go`: `dond daemon` on Linux, which serves the proxy at `/var/run/docker.sock`
- `daemon_windows.go`: `dond daemon` on Windows, which serves the proxy at the default named pipe of Docker
- `process_unix.go`, `process_windows.go`: the listener of the proxy, and how the child process is run and signalled
- `wsl_windows.go`: how Windows reaches the Docker daemon in WSL, through a Hyper-V socket or `docker system dial-stdio`
- `*_test.go`: unit tests; `e2e_*_windows_test.go`: the end to end tests on Windows

Also:

- `Dockerfile`: the binaries (`build`), the image with only the binary (`dond-shim-bin`), the `docker` image with the shim (`dond-shim`), and the image of the container tests (`test`)
- `scripts/test.sh`: the container tests, with `tests/fixtures`
- `.github/workflows/ci.yaml`: all tests, the build, and the release; `build.yaml`: the build, called by it

## Build and test

```sh
go test ./...                 # unit tests, on Linux and on Windows
scripts/test.sh 29            # container tests against the host's Docker, with the docker:29 image
scripts/test.sh 20.10 29      # several versions, as ci does from 18.03 on
```

`scripts/test.sh` needs Docker on Linux, and builds the `test` stage of the `Dockerfile`. On Windows, `go test ./...` also runs the end to end tests when Docker runs in the default WSL distribution and `DOND_SHIM_DOCKER_PATH` names a `docker.exe` of Windows, for example `$env:DOND_SHIM_DOCKER_PATH = (Get-Command docker.exe).Source`. Without them they skip themselves, as in ci.

To build what a release publishes, without pushing it:

```sh
docker buildx build . --target dond-shim-bin --build-arg VERSION=1.2.3 \
  --platform linux/amd64,linux/arm64,windows/amd64,windows/arm64 --output type=local,dest=dist
docker buildx build . --target dond-shim --platform linux/amd64,linux/arm64
```

The version is printed in the debug output: `DOND_SHIM_DEBUG=true dond wrap true`.

## Releases

Push a tag such as `v1.2.3` on `master`. ci runs all tests, and then builds and publishes the binaries, `checksums.txt`, both images with the tags `1.2.3` and `latest`, and the GitHub release with generated notes. A tag with a pre-release suffix, such as `v1.2.3-rc.1`, makes a pre-release and leaves `latest` alone. Nothing is published from pull requests or `master`, but they run the same build.

Dependencies are updated by Renovate, see `renovate.json`.

## House rules

- The smallest complete change. Prefer removing over adding, and add no option nobody asked for.
- The standard library first. A dependency needs a reason.
- Comments say why, not what, in one line where possible.
- Every behaviour a user can see, and every limit, is written in the README with its reason.
- No compatibility code for the Bash shim (0.x).
- Pull request titles are plain sentences, as they become the commit message.
