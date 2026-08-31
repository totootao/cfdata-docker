#!/usr/bin/env bash
# =============================================================================
# CFData 数据中心优选脚本：分别筛选指定数据中心（默认 LHR/FRA/SEA）的前 N 个结果
# 用法: 直接运行（容器入口），通过环境变量调整行为
#   MODE=official（默认）: 扫描 Cloudflare 官方 IP 段，按 DC_LIST 逐机房优选
#   MODE=nsb            : 非标优选，从 NSB_SOURCE_URL/NSB_FILE 加载 IP 库，
#                         一次扫描全部，结果按落地机房拆分到 results/nsb/
# =============================================================================
set -uo pipefail

# ---------- 可配置环境变量 ----------
MODE="${MODE:-official}"            # 扫描模式: official（官方IP段）或 nsb（非标IP库）
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
# 非标模式专用
NSB_SOURCE_URL="${NSB_SOURCE_URL:-}"        # 非标 IP 库 URL（每行 "IP [端口]"，支持域名）
NSB_FILE="${NSB_FILE:-}"                    # 非标本地文件（优先于 URL）
NSB_SPEED_LIMIT="${NSB_SPEED_LIMIT:-200}"   # 非标测速达标结果上限（凑够即停）
NSB_RESULT_LIMIT="${NSB_RESULT_LIMIT:-1000}" # 非标延迟测试结果上限

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

# 非标 CSV 行过滤条件：机房列匹配 + 速度为有效数值（剔除"测速失败"行）
csv_filter() {
  awk -F',' -v dc="$1" 'toupper($4) == toupper(dc) && $7 ~ /^[0-9]+(\.[0-9]+)?MB\/s$/'
}

# ---------- 非标模式：一次扫描全部 IP，按落地机房拆分 ----------
if [ "$MODE" = "nsb" ]; then
  if [ -z "$NSB_SOURCE_URL" ] && [ -z "$NSB_FILE" ]; then
    echo "[nsb] 非标模式需要 NSB_SOURCE_URL 或 NSB_FILE 指定 IP 库" >&2
    exit 1
  fi
  echo "==================== [nsb] 开始非标优选（Top${TOP_N}/机房） ===================="
  echo "- 模式: 非标（数据源: ${NSB_FILE:-$NSB_SOURCE_URL}）" >> "$SUMMARY"

  RAW="$WORK_DIR/nsb-raw.csv"
  NSB_ARGS=(
    -mode nsb
    -nsbtls true
    -nsbfallbackport "$PORT"
    -threads "$THREADS"
    -nsbdelay "$DELAY_MS"
    -nsbspeedtest 1
    -nsbspeedmin "$SPEED_MIN"
    -nsbspeedlimit "$NSB_SPEED_LIMIT"
    -nsbresultlimit "$NSB_RESULT_LIMIT"
  )
  if [ -n "$NSB_FILE" ]; then
    NSB_ARGS+=(-nsbfile "$NSB_FILE")
  else
    NSB_ARGS+=(-nsbsourceurl "$NSB_SOURCE_URL")
  fi

  ( cd "$WORK_DIR" && "$CFDATA_BIN" -cli \
    -config "$WORK_DIR/cfdata-config.json" \
    -skipgeo \
    -scanmode "$SCAN_MODE" \
    "${NSB_ARGS[@]}" \
    -format csv \
    -fields ip,port,ipport,dc,city,latency,speed \
    -out nsb-raw.csv \
    -nocolor )

  if [ ! -s "$RAW" ]; then
    echo "[nsb] 未生成结果文件"
    echo "- nsb: 无结果（IP 库拉取失败或全部不达标）" >> "$SUMMARY"
    exit 0
  fi

  mkdir -p "$RESULTS_DIR/nsb"
  # 清掉本次未覆盖的旧机房文件，避免残留过期数据
  find "$RESULTS_DIR/nsb" -name '*.csv' -delete 2>/dev/null || true

  # 统计结果中出现的机房（速度列有效，剔除"测速失败"行）
  DCS=$(tail -n +2 "$RAW" | awk -F',' '$7 ~ /^[0-9]+(\.[0-9]+)?MB\/s$/ && toupper($4) != "" {print toupper($4)}' | sort -u)

  HEADER=$(head -n 1 "$RAW" | tail -c +4)
  for DC in $DCS; do
    DC_LOWER=$(echo "$DC" | tr 'A-Z' 'a-z')
    CSV="$RESULTS_DIR/nsb/${DC_LOWER}.csv"
    {
      echo "$HEADER"
      tail -n +2 "$RAW" | csv_filter "$DC"
    } | head -n $((TOP_N + 1)) > "$CSV"
    COUNT=$(( $(wc -l < "$CSV") - 1 ))
    echo "[nsb] $DC: $COUNT 条 -> nsb/${DC_LOWER}.csv"
    echo "- nsb/$DC: $COUNT 条" >> "$SUMMARY"
  done
  rm -f "$RAW"

  echo ""
  echo "==================== 汇总 ===================="
  cat "$SUMMARY"
  echo ""
  echo "结果目录: $RESULTS_DIR/nsb"
  exit 0
fi

# ---------- 官方模式：逐机房扫描 ----------
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
    tail -n +2 "$RAW" | csv_filter "$DC"
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
