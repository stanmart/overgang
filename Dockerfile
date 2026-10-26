FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /overgang ./cmd/overgang

# Runs as distroless's nonroot user (65532): the process holds a Home Assistant token
# and faces the LAN, and UDP 5684 needs no privilege. /data must be writable by 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /overgang /overgang
EXPOSE 5684/udp
ENTRYPOINT ["/overgang"]
CMD ["serve", "-config", "/data/overgang.toml"]
