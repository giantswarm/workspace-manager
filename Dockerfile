# The Go binary is built by CircleCI (architect/go-build) and attached to the
# build context as <binary>-<os>-<arch>; this image only assembles the runtime:
# the binary on alpine with git, which the sync Job clones and fetches the
# workspace mirrors with.
#
# The base must be a multi-arch image index: `apk add git` installs the git of
# the platform the base resolves to, so a single-arch base puts the arm64
# binary on another architecture's git ("exec format error"). The
# `-giantswarm` alpine tags are single-arch, so the giantswarm user they add
# (uid and gid 1000) is added here.
#
# For a local build, produce the binary first:
#   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o workspace-manager-linux-amd64 .
FROM gsoci.azurecr.io/giantswarm/alpine:3.24.3

ARG TARGETOS
ARG TARGETARCH

# The guard fails the build when the base's userland is not the target's.
RUN case "${TARGETARCH}:$(apk --print-arch)" in \
      amd64:x86_64|arm64:aarch64) ;; \
      *) echo "base userland $(apk --print-arch) is not ${TARGETARCH}" >&2; exit 1 ;; \
    esac \
    && apk add --no-cache ca-certificates git \
    && addgroup -g 1000 -S giantswarm \
    && adduser -u 1000 -S giantswarm -G giantswarm
USER giantswarm

COPY workspace-manager-${TARGETOS}-${TARGETARCH} /workspace-manager

ENTRYPOINT ["/workspace-manager"]
CMD ["serve"]
