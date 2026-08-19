FROM golang:1.25-alpine AS build
WORKDIR /src
COPY proto/gen/go ./proto/gen/go
COPY pkg/iban ./pkg/iban
COPY pkg/outbox ./pkg/outbox
COPY pkg/pgha ./pkg/pgha
COPY pkg/tracing ./pkg/tracing
COPY services/accounts-svc ./services/accounts-svc
WORKDIR /src/services/accounts-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8082
EXPOSE 9082
CMD ["/server"]
