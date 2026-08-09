FROM golang:1.25-alpine AS build
WORKDIR /src
COPY proto/gen/go ./proto/gen/go
COPY pkg/outbox ./pkg/outbox
COPY pkg/pgha ./pkg/pgha
COPY pkg/tracing ./pkg/tracing
COPY services/auth-svc ./services/auth-svc
WORKDIR /src/services/auth-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8081
CMD ["/server"]
