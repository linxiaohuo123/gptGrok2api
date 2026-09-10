FROM golang:1.23-alpine AS build
WORKDIR /src

# 依赖单独成层，源码改动不必重下模块。
COPY go.mod go.sum ./
RUN go mod download

# 必须用通配符而不是逐个列名。此前写的是 `COPY main.go ./`，新增 client.go 后
# 镜像直接编译失败（undefined: newBackendClient 等四个符号），而本机
# `go build ./...` 却是绿的——本地有全部源码，镜像里没有。
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /image-gateway .

FROM alpine:3.21
RUN adduser -D -H -u 10001 gateway
USER gateway
COPY --from=build /image-gateway /image-gateway
EXPOSE 8080
ENTRYPOINT ["/image-gateway"]
