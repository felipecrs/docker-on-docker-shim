ARG DOCKER_VERSION="latest"
FROM docker:${DOCKER_VERSION} AS test

# Install APK deps
RUN apk add --no-cache bash

# Install dond-shim
ARG DOCKER_PATH="/usr/local/bin/docker"
RUN mv -f "${DOCKER_PATH}" "${DOCKER_PATH}.orig"
COPY docker "${DOCKER_PATH}"

# Create fixtures
RUN mkdir -p /test && \
  echo test | tee /test/only-inside-container

# Cleanup entrypoint to make execution faster
ENTRYPOINT []

# Set default stage
FROM felipecrs/fixdockergid:latest

USER root

ARG DOCKER_PATH="/usr/bin/docker"
RUN mv -f "${DOCKER_PATH}" "${DOCKER_PATH}.orig"
COPY docker "${DOCKER_PATH}"

USER rootless
