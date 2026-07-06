FROM golang:1.22-alpine AS build
WORKDIR /src
COPY pkg/health ./pkg/health
COPY gateway ./gateway
WORKDIR /src/gateway
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8080
CMD ["/server"]
