#!/usr/bin/env bash
set -euo pipefail
: "${RELEASE_VERSION:?}" "${RELEASE_SHA:?}" "${GITHUB_REPOSITORY:?}" "${RUNNER_TEMP:?}"
[[ "$RELEASE_SHA" =~ ^[0-9a-f]{40}$ ]] || { echo "invalid release SHA" >&2; exit 1; }
if [[ ${DRY_RUN:-false} != true ]]; then
  : "${RELEASE_UPSTREAM_REPOSITORY:?}" "${RELEASE_UPSTREAM_TAG:?}" "${RELEASE_UPSTREAM_SHA:?}"
fi
owner=${GITHUB_REPOSITORY%%/*}
owner=$(printf '%s' "$owner" | tr '[:upper:]' '[:lower:]')
registries=("ghcr.io/$owner/sub2api")
if [[ ${SIMPLE_RELEASE:-false} != true && ${DOCKERHUB_USERNAME:-skip} != skip ]]; then
  registries+=("${DOCKERHUB_USERNAME}/sub2api")
fi
arches=(amd64 arm64)
if [[ ${SIMPLE_RELEASE:-false} == true ]]; then arches=(amd64); fi
# Refuse to move existing version/source tags. Authentication/network failures
# are not evidence of absence: only registry not-found responses permit publish.
if [[ ${DRY_RUN:-false} != true ]]; then
  for registry in "${registries[@]}"; do
    tags=("$RELEASE_VERSION" "sha-$RELEASE_SHA")
    for arch in "${arches[@]}"; do tags+=("$RELEASE_VERSION-$arch"); done
    for tag in "${tags[@]}"; do
      if docker buildx imagetools inspect "$registry:$tag" >"$RUNNER_TEMP/image-exists.log" 2>&1; then
        echo "refusing to overwrite existing release image: $registry:$tag" >&2
        exit 1
      elif ! grep -Eqi 'manifest unknown|not found|name unknown' "$RUNNER_TEMP/image-exists.log"; then
        echo "cannot establish absence of release image: $registry:$tag" >&2
        exit 1
      fi
    done
  done
fi
for arch in "${arches[@]}"; do
  args=(--platform "linux/$arch" --file ".release-context/$arch/Dockerfile"
    --label "org.opencontainers.image.version=$RELEASE_VERSION"
    --label "org.opencontainers.image.revision=$RELEASE_SHA"
    --label "org.opencontainers.image.source=https://github.com/$GITHUB_REPOSITORY"
    --label "io.pixcook.upstream.repository=${RELEASE_UPSTREAM_REPOSITORY:-unrecorded}"
    --label "io.pixcook.upstream.tag=${RELEASE_UPSTREAM_TAG:-unrecorded}"
    --label "io.pixcook.upstream.revision=${RELEASE_UPSTREAM_SHA:-unrecorded}")
  for registry in "${registries[@]}"; do
    args+=(--tag "$registry:$RELEASE_VERSION-$arch")
    if [[ ${SIMPLE_RELEASE:-false} == true ]]; then
      args+=(--tag "$registry:$RELEASE_VERSION" --tag "$registry:sha-$RELEASE_SHA")
    fi
  done
  if [[ ${DRY_RUN:-false} == true ]]; then
    args+=(--output "type=oci,dest=$RUNNER_TEMP/sub2api-$arch.oci.tar")
  else
    args+=(--push)
  fi
  docker buildx build "${args[@]}" ".release-context/$arch"
done
if [[ ${DRY_RUN:-false} != true && ${SIMPLE_RELEASE:-false} != true ]]; then
  for registry in "${registries[@]}"; do
    docker buildx imagetools create \
      --tag "$registry:$RELEASE_VERSION" --tag "$registry:sha-$RELEASE_SHA" \
      "$registry:$RELEASE_VERSION-amd64" "$registry:$RELEASE_VERSION-arm64"
  done
fi

if [[ ${DRY_RUN:-false} != true ]]; then
  for registry in "${registries[@]}"; do
    docker buildx imagetools inspect "$registry:$RELEASE_VERSION" --format '{{json .Manifest}}' > "$RUNNER_TEMP/published-manifest-$(echo "$registry" | tr / _).json"
  done
fi
