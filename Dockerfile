# syntax=docker/dockerfile:1

# =============================================================================
# 阶段 1: 从源码编译 CFData（Go 静态编译；在构建平台原生运行、交叉编译到目标架构，避免 QEMU 模拟）
# =============================================================================
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

# CFData 源码仓库（默认使用 totootao 的 fork，可通过 build-args 切换）
ARG CFDATA_REPO=https://github.com/totootao/CFData-WEB.git
ARG CFDATA_REF=main
ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache git
WORKDIR /src
RUN git clone --depth 1 "${CFDATA_REPO}" cfdata
WORKDIR /src/cfdata/combined_refactor
RUN CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
    go build -trimpath -ldflags="-s -w" -o /out/cfdata .

# 编译 Web 控制台（独立 module，静态文件经 go:embed 内嵌，单二进制无依赖）
COPY webapp/ /src/webapp/
WORKDIR /src/webapp
RUN CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
    go build -trimpath -ldflags="-s -w" -o /out/cfdata-web .

# 构建时预下载官方 IP 库与机房位置表作为种子缓存（运行时优先使用缓存，下载失败不阻断构建）
RUN mkdir -p /out/seed \
    && (wget -q -T 30 -O /out/seed/ips-v4.txt https://www.baipiao.eu.org/cloudflare/ips-v4 || rm -f /out/seed/ips-v4.txt) \
    && (wget -q -T 30 -O /out/seed/ips-v6.txt https://www.baipiao.eu.org/cloudflare/ips-v6 || rm -f /out/seed/ips-v6.txt) \
    && (wget -q -T 30 -O /out/seed/locations.json https://www.baipiao.eu.org/cloudflare/locations || rm -f /out/seed/locations.json) \
    || true

# =============================================================================
# 阶段 2: 运行时镜像
# =============================================================================
FROM alpine:3.21

# busybox 自带 crond/crontab，满足定时模式；tzdata 支持时区调度
RUN apk add --no-cache bash curl tzdata ca-certificates

WORKDIR /app
COPY --from=builder /out/cfdata /app/cfdata
COPY --from=builder /out/cfdata-web /app/cfdata-web
COPY --from=builder /out/seed/ /app/seed/
COPY scan.sh run-scheduled.sh entrypoint.sh /app/

RUN chmod +x /app/cfdata /app/cfdata-web /app/scan.sh /app/run-scheduled.sh /app/entrypoint.sh \
    # 生成 CLI 配置模板（程序首次运行会生成后退出，属正常行为）
    && /app/cfdata -cli -config /app/cfdata-config.json -nocolor >/dev/null 2>&1 || true

# 运行模式：单次（默认）/ 定时（CRON_SCHEDULE）/ Web 控制台（WEB_PORT，可与定时共存）
ENV TZ=Asia/Shanghai \
    CRON_SCHEDULE="" \
    RUN_ON_START="true" \
    WEB_PORT=""

VOLUME ["/app/results"]
ENTRYPOINT ["/app/entrypoint.sh"]
