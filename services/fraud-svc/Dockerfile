FROM golang:1.22-alpine AS build
WORKDIR /src
COPY pkg/health ./pkg/health
COPY services/fraud-svc ./services/fraud-svc
WORKDIR /src/services/fraud-svc
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8085
CMD ["/server"]
