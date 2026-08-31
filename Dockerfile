# syntax=docker/dockerfile:1

# =============================================================================
# 阶段 1: 从源码编译 CFData（Go 静态编译，天然支持多架构交叉构建）
# =============================================================================
FROM golang:1.25-alpine AS builder

# CFData 源码仓库（默认使用 totootao 的 fork，可通过 build-args 切换）
ARG CFDATA_REPO=https://github.com/totootao/CFData-WEB.git
ARG CFDATA_REF=main

RUN apk add --no-cache git
WORKDIR /src
RUN git clone --depth 1 "${CFDATA_REPO}" cfdata
WORKDIR /src/cfdata/combined_refactor
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cfdata .

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

RUN apk add --no-cache bash curl tzdata ca-certificates

WORKDIR /app
COPY --from=builder /out/cfdata /app/cfdata
COPY --from=builder /out/seed/ /app/seed/
COPY scan.sh /app/scan.sh

RUN chmod +x /app/cfdata /app/scan.sh \
    # 生成 CLI 配置模板（程序首次运行会生成后退出，属正常行为）
    && /app/cfdata -cli -config /app/cfdata-config.json -nocolor >/dev/null 2>&1 || true

VOLUME ["/app/results"]
ENTRYPOINT ["/app/scan.sh"]
