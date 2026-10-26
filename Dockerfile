# syntax=docker/dockerfile:1.7

# ---- build ----------------------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath

# Dependencies first: this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags "-s -w -X github.com/iamBlissMedik/irate-core-go/internal/app.Version=${VERSION}" -o /out/api ./cmd/api && \
    go build -ldflags "-s -w" -o /out/migrate ./cmd/migrate && \
    go build -ldflags "-s -w" -o /out/seed ./cmd/seed

# ---- runtime --------------------------------------------------------------
# distroless/static: no shell, no package manager, runs as an unprivileged
# user. The image holds little more than the binaries and CA certificates.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
USER nonroot:nonroot
EXPOSE 8000 9090
ENTRYPOINT ["/app/api"]
