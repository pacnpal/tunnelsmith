# Multi-stage build. This image is built only by the GitHub Actions
# workflow; do not build it on a developer host. See ADR-001.

ARG GO_VERSION=1.22

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

# Cache module downloads in a separate layer so source changes do not
# force a full re-download.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown
ARG TARGETOS
ARG TARGETARCH

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/tunnelsmith ./cmd/tunnelsmith

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tunnelsmith /usr/local/bin/tunnelsmith
COPY examples/tunnelsmith.toml /etc/tunnelsmith/config.toml.example

USER nonroot:nonroot
EXPOSE 8080 1080 9090 9091
ENTRYPOINT ["/usr/local/bin/tunnelsmith"]
