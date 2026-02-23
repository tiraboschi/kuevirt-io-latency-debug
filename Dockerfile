# Stage 1: build a fully static binary.
FROM golang:1.25 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" \
    -o /kubevirt-io-latency-exporter ./cmd/exporter

# Stage 2: minimal UBI image for OCP compatibility.
# ubi9-micro has no shell but satisfies Red Hat certification requirements.
FROM registry.access.redhat.com/ubi9/ubi-micro:latest
COPY --from=builder /kubevirt-io-latency-exporter /kubevirt-io-latency-exporter
USER 0
ENTRYPOINT ["/kubevirt-io-latency-exporter"]
