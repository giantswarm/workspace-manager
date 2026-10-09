# The Go binary is built by CircleCI (architect/go-build) and attached to the
# build context as <binary>-<os>-<arch>; this image only assembles the runtime:
# the binary on the Giant Swarm alpine image with git, which the sync Job
# clones and fetches the workspace mirrors with.
# For a local build, produce the binary first:
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o workspace-manager-linux-amd64 .
FROM gsoci.azurecr.io/giantswarm/alpine:3.20.3-giantswarm

USER root
RUN apk add --no-cache git
USER giantswarm

ARG TARGETOS
ARG TARGETARCH
COPY workspace-manager-${TARGETOS}-${TARGETARCH} /workspace-manager

ENTRYPOINT ["/workspace-manager"]
CMD ["serve"]
