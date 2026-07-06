FROM golang:1.22-alpine AS build
WORKDIR /src
COPY pkg/health ./pkg/health
COPY services/accounts-svc ./services/accounts-svc
WORKDIR /src/services/accounts-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8082
CMD ["/server"]
