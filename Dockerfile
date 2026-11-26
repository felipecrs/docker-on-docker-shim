ARG DOCKER_VERSION="latest"


FROM --platform=${BUILDPLATFORM} golang:1.27.4-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION="dev"
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
  go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /dond ./cmd/dond


FROM scratch AS dond-shim-bin

COPY --from=build /dond /


FROM docker:${DOCKER_VERSION} AS dond-shim

# Install dond-shim
ARG DOCKER_PATH="/usr/local/bin/docker"
RUN mv -f "${DOCKER_PATH}" "${DOCKER_PATH}.orig"
COPY --from=dond-shim-bin /dond "${DOCKER_PATH}"

FROM dond-shim AS test

# Create fixtures, and dond for the tests of dond wrap
RUN mkdir -p /test && \
  echo test | tee /test/only-inside-container && \
  ln -s docker /usr/local/bin/dond

# Cleanup entrypoint to make execution faster
ENTRYPOINT []

# Set default stage
FROM dond-shim
