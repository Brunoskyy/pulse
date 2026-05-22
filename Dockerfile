# syntax=docker/dockerfile:1
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go, so the binary is static and needs no libc.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo docker)" -o /out/pulse ./cmd/pulse

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pulse /pulse
VOLUME /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/pulse"]
CMD ["run", "-config", "/etc/pulse/pulse.yaml"]
