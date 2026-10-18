FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/mariuz/firebirdrest/internal/server.Version=${VERSION}" \
      -o /out/firebirdrest ./cmd/firebirdrest

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/firebirdrest /usr/local/bin/firebirdrest
EXPOSE 3000
USER nonroot
ENTRYPOINT ["/usr/local/bin/firebirdrest"]
