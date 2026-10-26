FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /overgang ./cmd/overgang

# Runs as root: the host's /var/lib/overgang is mounted at /data, as for the rest of the
# Home Assistant stack.
FROM gcr.io/distroless/static-debian12
COPY --from=build /overgang /overgang
EXPOSE 5684/udp
ENTRYPOINT ["/overgang"]
CMD ["serve", "-config", "/data/overgang.toml"]
