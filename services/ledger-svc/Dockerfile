FROM golang:1.25-alpine AS build
WORKDIR /src
COPY proto/gen/go ./proto/gen/go
COPY services/ledger-svc ./services/ledger-svc
WORKDIR /src/services/ledger-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8083
CMD ["/server"]
