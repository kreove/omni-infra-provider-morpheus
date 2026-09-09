# The builder always runs on the native build platform and cross-compiles for
# the target, so multi-platform builds don't pay for QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.7-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
RUN go mod verify

COPY . .

RUN test "$(go list -m)" = "github.com/kreove/omni-infra-provider-morpheus" \
    && go list -find \
       ./internal/pkg/provider/meta \
       ./internal/pkg/provider/resources

# Runs natively on the build platform.
RUN go test ./...

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/omni-infra-provider-morpheus \
    ./cmd/omni-infra-provider-morpheus

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.title="Omni Infrastructure Provider for Morpheus"
LABEL org.opencontainers.image.description="Community Sidero Omni infrastructure provider for HPE Morpheus VM Essentials"
LABEL org.opencontainers.image.source="https://github.com/kreove/omni-infra-provider-morpheus"
LABEL org.opencontainers.image.licenses="MPL-2.0"

COPY --from=build \
    /out/omni-infra-provider-morpheus \
    /omni-infra-provider-morpheus

ENTRYPOINT ["/omni-infra-provider-morpheus"]
