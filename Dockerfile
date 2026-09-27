# syntax=docker/dockerfile:1

# ── 前端：Vite 构建 Web 控制台静态产物 ──────────────────────
FROM node:22-alpine AS webui
WORKDIR /webui
COPY webui/package.json ./
# npm 无锁文件安装时常漏装平台可选依赖（npm/cli#4828），tailwind v4 的
# lightningcss 在 alpine 上缺 musl 原生包即构建失败；显式补装。
RUN npm install --no-audit --no-fund \
 && npm install --no-audit --no-fund --no-save lightningcss-linux-x64-musl @tailwindcss/oxide-linux-x64-musl
COPY webui/ ./
RUN npm run build

# ── 后端：Go 编译（web 产物注入 go:embed）──────────────────
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN rm -rf cmd/wild-work/web/*
COPY --from=webui /webui/dist ./cmd/wild-work/web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wild-work ./cmd/wild-work

# ── 运行时 ─────────────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
USER app
WORKDIR /app
COPY --from=build /out/wild-work /app/wild-work
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wild-work", "--no-tray"]
