module neobank/gateway

go 1.22

require neobank/pkg/health v0.0.0

require github.com/golang-jwt/jwt/v5 v5.3.1

replace neobank/pkg/health => ../pkg/health
