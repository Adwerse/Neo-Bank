FROM golang:1.25-alpine AS build
WORKDIR /src
COPY proto/gen/go ./proto/gen/go
COPY pkg/outbox ./pkg/outbox
COPY services/accounts-svc ./services/accounts-svc
WORKDIR /src/services/accounts-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8082
EXPOSE 9082
CMD ["/server"]
