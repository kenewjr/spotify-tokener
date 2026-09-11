FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build

WORKDIR /build

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg \
    CGO_ENABLED=0 \
    GOOS=$TARGETOS \
    GOARCH=$TARGETARCH \
    go build -o spotify-tokener github.com/topi314/spotify-tokener

FROM chromedp/headless-shell

COPY --from=build /build/spotify-tokener /usr/local/bin/spotify-tokener

ENV SPOTIFY_TOKENER_CHROME_PATH=/headless-shell/headless-shell
ENV SPOTIFY_TOKENER_ADDR=0.0.0.0:8080
ENV SPOTIFY_TOKENER_LOG_LEVEL=INFO

EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/spotify-tokener"]
