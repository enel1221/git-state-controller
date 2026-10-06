ARG BASE_IMAGE=golang:1.26.8-bookworm
FROM ${BASE_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -trimpath -o /manager ./cmd
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
