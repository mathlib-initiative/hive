# Build from the repository root: docker build -f gateway/Dockerfile .
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
COPY gateway ./gateway
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /hive-gateway ./gateway

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /hive-gateway /hive-gateway
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/hive-gateway"]
