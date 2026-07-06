FROM golang:1.22-alpine AS build
WORKDIR /src
COPY pkg/health ./pkg/health
COPY services/auth-svc ./services/auth-svc
WORKDIR /src/services/auth-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8081
CMD ["/server"]
