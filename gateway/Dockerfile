# 1.25 to match gateway/go.mod's `go 1.25.0` and every other service's
# image. On 1.23 this still built, but only because the Go toolchain
# silently downloads the newer one a go.mod demands — a network fetch on
# every image build, and a build that breaks the moment that fetch can't
# happen.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY proto/gen/go ./proto/gen/go
COPY pkg/health ./pkg/health
COPY pkg/tracing ./pkg/tracing
COPY gateway ./gateway
WORKDIR /src/gateway
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8080
CMD ["/server"]
