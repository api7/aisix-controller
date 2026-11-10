# syntax=docker/dockerfile:1

# The aisix release whose `validate` the controller runs. Keep it equal to
# third_party/aisix/VERSION.
ARG AISIX_VERSION=1.9.0

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY api ./api
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/aisix-controller ./cmd/aisix-controller

FROM docker.io/api7/aisix:${AISIX_VERSION} AS aisix

# The gateway image grants the binary cap_net_bind_service. A container
# that drops all capabilities with no_new_privs cannot exec such a file,
# and validation binds nothing, so a plain copy drops it.
FROM debian:trixie-slim AS aisix-bin
COPY --from=aisix /usr/local/bin/aisix /aisix-with-caps
RUN cp /aisix-with-caps /aisix

# Same Debian release as the aisix image, for the binary's glibc.
FROM debian:trixie-slim
COPY --from=aisix-bin /aisix /usr/local/bin/aisix
COPY --from=build /out/aisix-controller /usr/local/bin/aisix-controller
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/aisix-controller"]
