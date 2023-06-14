FROM felipecrs/fixdockergid:latest

USER root

ARG DOCKER_PATH="/usr/bin/docker"
RUN mv -f "${DOCKER_PATH}" "${DOCKER_PATH}.orig"
COPY docker "${DOCKER_PATH}"

USER rootless

# Create fixtures
RUN echo test | tee /home/rootless/only-inside-container
