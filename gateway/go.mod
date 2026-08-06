module neobank/gateway

go 1.23

require neobank/pkg/health v0.0.0

require github.com/golang-jwt/jwt/v5 v5.3.1

require github.com/coder/websocket v1.8.15

replace neobank/pkg/health => ../pkg/health
