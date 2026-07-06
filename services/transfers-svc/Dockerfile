FROM golang:1.22-alpine AS build
WORKDIR /src
COPY pkg/health ./pkg/health
COPY services/transfers-svc ./services/transfers-svc
WORKDIR /src/services/transfers-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8084
CMD ["/server"]
