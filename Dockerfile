# Stage 1: Compile Go binaries
# $BUILDPLATFORM = native (ARM64 on M1) — Go cross-compila para amd64 sem QEMU
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder

WORKDIR /build
COPY src/go.mod ./
RUN go mod download
COPY src/ ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /bin/server      ./cmd/server
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /bin/buildindex  ./cmd/buildindex
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /bin/healthcheck ./cmd/healthcheck

# Stage 2: Build compact rule/KNN fallback index from 3M references
# Runs at build time — zero startup overhead at runtime
FROM golang:1.22-alpine AS indexer

WORKDIR /app
COPY --from=builder /bin/buildindex ./buildindex
COPY resources/references.json.gz   ./resources/references.json.gz

RUN REFS_PATH=./resources/references.json.gz \
    INDEX_PATH=./resources/index.bin \
    ./buildindex

# Stage 3: Minimal runtime image
FROM alpine:3.19

WORKDIR /app

COPY --from=builder  /bin/server       ./server
COPY --from=builder  /bin/healthcheck  ./healthcheck
COPY --from=indexer  /app/resources/index.bin    ./resources/index.bin
COPY resources/mcc_risk.json           ./resources/mcc_risk.json
COPY resources/normalization.json      ./resources/normalization.json

CMD ["/app/server"]
