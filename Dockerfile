# syntax=docker/dockerfile:1
ARG GO_VERSION=1.25

FROM golang:${GO_VERSION}-alpine AS ci
ENV CGO_ENABLED=0 GOTOOLCHAIN=auto
WORKDIR /src
RUN apk add --no-cache git ca-certificates

# cache deps
COPY go.mod go.sum* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

# build + test
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy && \
    go vet ./... && \
    go build ./... && \
    go test -v ./...

FROM alpine:3.20 AS done
CMD ["true"]
