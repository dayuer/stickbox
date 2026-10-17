# stickboxd 镜像。构建上下文是 server/go：
#   docker build -t stickboxd:$(git describe --always) server/go
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/dayuer/stickbox/server/go/internal/version.Version=${VERSION}" \
      -o /out/stickboxd ./cmd/stickboxd

# 纯 Go sqlite，静态二进制，不需要 libc。
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/stickboxd /stickboxd
EXPOSE 8883 9443
ENTRYPOINT ["/stickboxd", "-conf", "/etc/stickbox/config.json"]
