# syntax=docker/dockerfile:1.7
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG SERVICE
RUN test -n "${SERVICE}"
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SERVICE}

FROM alpine:3.21
RUN apk --no-cache add ca-certificates && adduser -D -u 10001 app
USER app
WORKDIR /
COPY --from=builder /out/app /app
ENTRYPOINT ["/app"]
