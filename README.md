# docker-on-docker-shim

A shim that remaps volume mounts so they work when running docker on docker.

## Why?

Have you found yourself trying to run docker on docker, but the volume mounts don't work?

This is because when you run docker on docker (i.e. reusing the docker daemon from the host within a container - `--volume /var/run/docker.sock:...`) and you execute a `docker run` command, the volume mounts (with `--volume` or `--mount`) will be interpreted by the docker daemon as paths from the host, not from the container.

Well, this dilema is very common to me, and that's why I created this shim.

## Use cases

Although this shim can be used in many cases, some of the most common ones are:

- [devcontainers](https://containers.dev/) with [docker-outside-of-docker](https://github.com/devcontainers/features/tree/main/src/docker-outside-of-docker#readme)
- StackOverflow: [Docker in Docker cannot mount volume](https://stackoverflow.com/questions/31381322)

## Highlights

- Tested with all versions of docker from `18.03` to `latest`.
- Tested against containers based on `alpine`, `debian` and `ubuntu`, but it should work with any other container base.
- The only dependency is Bash.
- Rename the docker cli to `docker.orig` and place this shim on where the docker cli was and simply continue calling `docker` as usual.
- Or install this shim as the `dond` command if you want and just call `dond` instead of calling `docker`.

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

You have a few choices on how to install this shim:

### As `docker`

This allows you to continue calling `docker` as usual.

```dockerfile
FROM docker:latest

# Install Bash
RUN apk add --no-cache bash

# Rename the docker cli to docker.orig
ARG DOCKER_PATH="/usr/local/bin/docker"
RUN mv -f "${DOCKER_PATH}" "${DOCKER_PATH}.orig"

# Install dond-shim at the same path as the original docker cli
ARG DOND_SHIM_VERSION="0.7.1"
ADD "https://github.com/felipecrs/docker-on-docker-shim/raw/v${DOND_SHIM_VERSION}/dond" "${DOCKER_PATH}"
RUN chmod 755 "${DOCKER_PATH}"
```

### As `dond`

So that you can call `dond` instead of calling `docker`.

```dockerfile
FROM docker:latest

# Install Bash
RUN apk add --no-cache bash

# Install dond-shim to /usr/local/bin/dond
ARG DOND_SHIM_VERSION="0.7.1"
ARG DOND_SHIM_PATH="/usr/local/bin/dond"
ADD "https://github.com/felipecrs/docker-on-docker-shim/raw/v${DOND_SHIM_VERSION}/dond" "${DOND_SHIM_PATH}"
RUN chmod 755 "${DOND_SHIM_PATH}"
```

## Docker Compose

Unfortunately, using the shim through Docker Compose is not possible, since Docker Compose interacts with the docker daemon directly and does not call the `docker` cli.

It should be theoretically possible to develop a shim for Docker Compose, but I have not done it yet.
