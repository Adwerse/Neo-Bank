FROM golang:1.22-alpine AS build
WORKDIR /src
COPY pkg/health ./pkg/health
COPY services/notifications-svc ./services/notifications-svc
WORKDIR /src/services/notifications-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8086
CMD ["/server"]
