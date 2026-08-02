FROM golang:1.25-alpine AS build
WORKDIR /src
COPY pkg/health ./pkg/health
COPY proto/gen/go ./proto/gen/go
COPY services/notifications-svc ./services/notifications-svc
WORKDIR /src/services/notifications-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8086
CMD ["/server"]
