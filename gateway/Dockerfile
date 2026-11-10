# Build from the repository root: docker build -f gateway/Dockerfile .
# The Go stage runs on the build machine and cross-compiles, so that
# multi-platform builds do not emulate the compiler.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY gateway ./gateway
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags=-s -o /hive-gateway ./gateway

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /hive-gateway /hive-gateway
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/hive-gateway"]
