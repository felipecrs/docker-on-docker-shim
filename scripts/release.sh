#!/usr/bin/env bash

set -euxo pipefail

version="${1}"

echo "Releasing version ${version}..."

script_path=$(realpath "$0")
script_dir=$(dirname "${script_path}")
build_dir="$(realpath "${script_dir}/..")"
readonly build_dir
unset script_path script_dir

readonly platforms="linux/amd64,linux/arm64"

docker buildx build "${build_dir}" --pull --push --platform "${platforms}" \
  --build-arg "VERSION=${version}" \
  --tag "ghcr.io/felipecrs/dond-shim:${version}" \
  --tag "ghcr.io/felipecrs/dond-shim:latest"
docker buildx build "${build_dir}" --pull --push --platform "${platforms}" \
  --build-arg "VERSION=${version}" \
  --target dond-shim-bin --tag "ghcr.io/felipecrs/dond-shim-bin:${version}" \
  --tag "ghcr.io/felipecrs/dond-shim-bin:latest"

# The Windows binaries, attached to the release
dist_dir="$(mktemp -d)"
readonly dist_dir
trap 'rm -rf "${dist_dir}"' EXIT
docker buildx build "${build_dir}" --pull --platform "windows/amd64,windows/arm64" \
  --build-arg "VERSION=${version}" \
  --target dond-shim-bin --output "type=local,dest=${dist_dir}"
for arch in amd64 arm64; do
  mv "${dist_dir}/windows_${arch}/dond" "${dist_dir}/dond-windows-${arch}.exe"
done

git_sha=$(git rev-parse HEAD)
gh release create "v${version}" --title "v${version}" --target "${git_sha}" \
  "${dist_dir}/dond-windows-amd64.exe" "${dist_dir}/dond-windows-arm64.exe"
