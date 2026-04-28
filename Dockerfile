FROM golang:1.23-bookworm AS build
WORKDIR /src
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
COPY . .
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/portflare/server/internal/buildinfo.Version=${VERSION} -X github.com/portflare/server/internal/buildinfo.Commit=${COMMIT} -X github.com/portflare/server/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/portflare-server ./cmd/portflare-server

FROM gcr.io/distroless/base-debian12
COPY --from=build /out/portflare-server /usr/local/bin/portflare-server
ENTRYPOINT ["/usr/local/bin/portflare-server"]
