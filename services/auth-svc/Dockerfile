FROM golang:1.25-alpine AS build
WORKDIR /src/services/auth-svc
COPY services/auth-svc .
RUN go build -o /out/server .

FROM alpine:latest
COPY --from=build /out/server /server
EXPOSE 8081
CMD ["/server"]
