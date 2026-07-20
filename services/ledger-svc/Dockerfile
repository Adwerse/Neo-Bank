FROM golang:1.25-alpine AS build
WORKDIR /src
COPY pkg/health ./pkg/health
COPY services/ledger-svc ./services/ledger-svc
WORKDIR /src/services/ledger-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8083
CMD ["/server"]
