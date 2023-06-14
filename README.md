# docker-on-docker-shim

This is a shim that remaps volume mounts when running docker on docker. Aimed to fix <https://stackoverflow.com/questions/31381322> with no hassle.

## Compare it yourself

Using the [`fixdockergid`](https://github.com/felipecrs/fixdockergid) image to test, where the `/usr/local/bin/fixdockergid` is only available within the container:

```bash
# This does not work
docker run --rm -u "$(id -u):$(id -g)" -v /var/run/docker.sock:/var/run/docker.sock felipecrs/fixdockergid docker run --rm -v /usr/local/bin/fixdockergid:/fixdockergid ubuntu test -f /fixdockergid

# A non-zero exit code indicates that it did not work
echo $?

# This does
docker run --rm -u "$(id -u):$(id -g)" -v /var/run/docker.sock:/var/run/docker.sock ghcr.io/felipecrs/dond-shim docker run --rm -v /usr/local/bin/fixdockergid:/fixdockergid ubuntu test -f /fixdockergid

# A zero exit code indicates that it worked
echo $?
```

## Installation

Considering your container already has the `docker` CLI installed at `/usr/bin/docker`:

```dockerfile
FROM my-image-with-docker-cli

USER root

ARG DOCKER_PATH="/usr/bin/docker"
ARG DOND_SHIM_REVISION="main"
RUN mv -f "${DOCKER_PATH}" "${DOCKER_PATH}.orig" && \
  curl -fsSL "https://github.com/felipecrs/docker-on-docker-shim/raw/${DOND_SHIM_REVISION}/docker" \
    --output "${DOCKER_PATH}" && \
  chmod +x "${DOCKER_PATH}"

USER non-root-user
```
