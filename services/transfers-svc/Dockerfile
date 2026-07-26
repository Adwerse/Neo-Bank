FROM golang:1.25-alpine AS build
WORKDIR /src
COPY services/transfers-svc ./services/transfers-svc
WORKDIR /src/services/transfers-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8084
CMD ["/server"]
