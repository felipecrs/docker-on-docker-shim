ARG DOCKER_VERSION="latest"
FROM docker:${DOCKER_VERSION} AS dond-shim

# Install deps
RUN apk add --no-cache bash

# Install dond-shim
ARG DOCKER_PATH="/usr/local/bin/docker"
RUN mv -f "${DOCKER_PATH}" "${DOCKER_PATH}.orig"
COPY dond "${DOCKER_PATH}"

FROM dond-shim AS test

# Create fixtures
RUN mkdir -p /test && \
  echo test | tee /test/only-inside-container

# Cleanup entrypoint to make execution faster
ENTRYPOINT []

# Set default stage
FROM dond-shim
