#!/usr/bin/env bash
# =============================================================================
# CFData 数据中心优选脚本：分别筛选指定数据中心（默认 LHR/FRA/SEA）的前 N 个结果
# 用法: 直接运行（容器入口），通过环境变量调整行为
# =============================================================================
set -uo pipefail

# ---------- 可配置环境变量 ----------
DC_LIST="${DC_LIST:-LHR,FRA,SEA}"    # 数据中心列表（Cloudflare 机房 IATA 码，逗号分隔）
TOP_N="${TOP_N:-10}"                 # 每个数据中心取前 N 个结果
IP_TYPE="${IP_TYPE:-4}"              # IP 类型: 4 或 6
PORT="${PORT:-443}"                  # 测试/测速端口
THREADS="${THREADS:-100}"            # 扫描并发数
DELAY_MS="${DELAY_MS:-500}"          # 延迟阈值（毫秒）
SPEED_MIN="${SPEED_MIN:-1}"          # 测速达标下限（MB/s）
SCAN_MODE="${SCAN_MODE:-tcping}"     # 扫描方式: tcping 或 httping
RESULTS_DIR="${RESULTS_DIR:-/app/results}"
WORK_DIR="${WORK_DIR:-/app}"
CFDATA_BIN="${CFDATA_BIN:-/app/cfdata}"

mkdir -p "$RESULTS_DIR"

# 使用构建时预下载的 IP 库/机房表作为种子缓存（存在且非空才使用）
for f in ips-v4.txt ips-v6.txt locations.json; do
  if [ ! -f "$WORK_DIR/$f" ] && [ -s "$WORK_DIR/seed/$f" ]; then
    cp "$WORK_DIR/seed/$f" "$WORK_DIR/$f"
  fi
done

# 首次运行生成 CLI 配置模板（程序设计：生成后退出，属正常行为）
if [ ! -f "$WORK_DIR/cfdata-config.json" ]; then
  "$CFDATA_BIN" -cli -config "$WORK_DIR/cfdata-config.json" -nocolor >/dev/null 2>&1 || true
fi

# 探测当前网络出口机房（用于结果可追溯）
EGRESS_COLO=$(curl -s --max-time 8 https://speed.cloudflare.com/cdn-cgi/trace 2>/dev/null | grep -E '^colo=' | cut -d= -f2)
EGRESS_LOC=$(curl -s --max-time 8 https://speed.cloudflare.com/cdn-cgi/trace 2>/dev/null | grep -E '^loc=' | cut -d= -f2)

SUMMARY="$RESULTS_DIR/summary.txt"
{
  echo "# CFData 数据中心优选 Top${TOP_N}"
  echo "- 时间: $(date -u '+%Y-%m-%d %H:%M:%S UTC')"
  echo "- 目标数据中心: ${DC_LIST}"
  echo "- 运行环境出口: colo=${EGRESS_COLO:-未知} loc=${EGRESS_LOC:-未知}"
  echo "- 注意: 出口机房决定能扫到哪些数据中心的 IP（anycast 就近落地）"
  echo ""
} > "$SUMMARY"

TMP_DIR=$(mktemp -d)
trap 'rm -rf "$TMP_DIR"' EXIT

for DC in $(echo "$DC_LIST" | tr ',' ' '); do
  DC=$(echo "$DC" | xargs)
  [ -z "$DC" ] && continue
  DC=$(echo "$DC" | tr 'a-z' 'A-Z')
  echo ""
  echo "==================== [$DC] 开始扫描（Top${TOP_N}） ===================="

  # 注意：cfdata 的 -offout 会经 safeFilename() 剥掉目录部分（仅保留文件名），
  # 因此这里 cd 到 WORK_DIR 用相对文件名输出，再用绝对路径读取
  RAW="$WORK_DIR/${DC}-raw.csv"
  ( cd "$WORK_DIR" && "$CFDATA_BIN" -cli \
    -config "$WORK_DIR/cfdata-config.json" \
    -skipgeo \
    -mode official \
    -scanmode "$SCAN_MODE" \
    -offiptype "$IP_TYPE" \
    -offport "$PORT" \
    -offthreads "$THREADS" \
    -offdelay "$DELAY_MS" \
    -offdc "$DC" \
    -offspeedlimit "$TOP_N" \
    -offspeedmin "$SPEED_MIN" \
    -offurl auto \
    -format csv \
    -fields ip,port,ipport,dc,city,latency,speed \
    -offout "${DC}-raw.csv" \
    -nocolor )

  DC_LOWER=$(echo "$DC" | tr 'A-Z' 'a-z')
  CSV="$RESULTS_DIR/${DC_LOWER}.csv"
  TXT="$RESULTS_DIR/${DC_LOWER}.txt"

  if [ ! -s "$RAW" ]; then
    echo "[$DC] 未生成结果文件"
    : > "$TXT"
    echo "- $DC: 无结果（当前出口网络未扫描到该机房 IP）" >> "$SUMMARY"
    continue
  fi

  # 1. 去掉 UTF-8 BOM
  # 2. 仅保留目标数据中心的行（第 4 列，防止目标机房无 IP 时工具回退导出全部机房数据）
  # 3. 剔除测速失败行（速度列非 "xxMB/s" 格式，如"测速失败"）——延迟达标但下载不行的 IP 不能要
  # 4. 取前 TOP_N 行（工具导出已按下载速度降序排列）
  {
    head -n 1 "$RAW" | tail -c +4
    tail -n +2 "$RAW" | awk -F',' -v dc="$DC" \
      'toupper($4) == toupper(dc) && $7 ~ /^[0-9]+(\.[0-9]+)?MB\/s$/'
  } | head -n $((TOP_N + 1)) > "$CSV"

  # 提取 ip:port 纯文本列表（跳过表头，供客户端直接使用）
  tail -n +2 "$CSV" | awk -F',' '$3 != "" {print $3}' > "$TXT"

  COUNT=$(wc -l < "$TXT" | tr -d ' ')
  echo "[$DC] 完成，共 $COUNT 条结果 -> $CSV / $TXT"
  {
    echo "- $DC: $COUNT 条"
    if [ "$COUNT" -gt 0 ]; then
      tail -n +2 "$CSV" | head -n 3 | sed 's/^/    /'
    fi
  } >> "$SUMMARY"
  rm -f "$RAW"
done

echo ""
echo "==================== 汇总 ===================="
cat "$SUMMARY"
echo ""
echo "结果目录: $RESULTS_DIR"
