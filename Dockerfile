# syntax=docker/dockerfile:1

# Build from source. The release image published to GHCR is built by GoReleaser
# from Dockerfile.goreleaser.
FROM golang:1.26 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w \
      -X github.com/1985epma/k8scli/cmd.version=${VERSION} \
      -X github.com/1985epma/k8scli/cmd.commit=${COMMIT} \
      -X github.com/1985epma/k8scli/cmd.date=${DATE}" \
    -o /out/k8scli .

FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/k8scli /usr/local/bin/k8scli

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/k8scli"]
