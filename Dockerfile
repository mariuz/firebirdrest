# Builds natively on the build machine and cross-compiles for the target
# platform, so multi-arch images (docker buildx --platform ...) build quickly.
FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X github.com/mariuz/firebirdrest/internal/server.Version=${VERSION}" \
      -o /out/firebirdrest ./cmd/firebirdrest

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/mariuz/firebirdrest" \
      org.opencontainers.image.description="REST API for any Firebird database" \
      org.opencontainers.image.licenses="MIT"
COPY --from=build /out/firebirdrest /usr/local/bin/firebirdrest
EXPOSE 3000
USER nonroot
ENTRYPOINT ["/usr/local/bin/firebirdrest"]
