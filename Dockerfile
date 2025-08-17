FROM golang:1.25.0 AS builder
ARG TARGETOS TARGETARCH

WORKDIR /app

RUN \
  --mount=type=cache,target=/go/pkg/mod \
  --mount=type=bind,source=./go.mod,target=/app/go.mod \
  --mount=type=bind,source=./go.sum,target=/app/go.sum \
  go mod download -x

RUN \
  --mount=type=cache,target=/root/.cache \
  --mount=type=cache,target=/go/pkg/mod \
  --mount=type=bind,source=./go.mod,target=/app/go.mod \
  --mount=type=bind,source=./go.sum,target=/app/go.sum \
  --mount=type=bind,source=./cmd,target=/app/cmd \
  <<EOF
  go test -v ./...
  CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go install -ldflags="-w -s" ./cmd/...
EOF


FROM scratch
COPY --from=builder /go/bin/kueuecapacity /usr/local/bin/kueuecapacity

ENTRYPOINT ["kueuecapacity"]
