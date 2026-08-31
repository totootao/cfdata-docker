#!/usr/bin/env bash
# =============================================================================
# 定时任务体：加载容器环境 → 并发锁保护 → 执行扫描 → 记录日志
# 由 entrypoint.sh（启动时）和 crond（到点时）调用
# =============================================================================
set -uo pipefail

# 加载容器启动时的环境变量（crond 派生的进程拿不到这些值）
if [ -f /app/.container-env ]; then
  set -a
  # shellcheck disable=SC1091
  . /app/.container-env
  set +a
fi
export TZ="${TZ:-Asia/Shanghai}"

RESULTS_DIR="${RESULTS_DIR:-/app/results}"
LOG_FILE="$RESULTS_DIR/cron.log"
LOCK_DIR="/tmp/cfdata-scan.lock"
# 锁超过 3 小时视为残留（3 机房扫描正常最长约 40 分钟）
STALE_MINUTES=180

mkdir -p "$RESULTS_DIR"

# 残留锁清理：容器曾在扫描中被强杀导致锁未释放
if [ -d "$LOCK_DIR" ]; then
  if [ -z "$(find "$LOCK_DIR" -maxdepth 0 -mmin -"$STALE_MINUTES" 2>/dev/null)" ]; then
    echo "[$(date '+%F %T')] 检测到残留锁（超过 ${STALE_MINUTES} 分钟），自动清除" >> "$LOG_FILE"
    rm -rf "$LOCK_DIR"
  fi
fi

# mkdir 是原子操作，用作并发锁：上一次扫描未结束时跳过本次触发
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  echo "[$(date '+%F %T')] 上一次扫描仍在进行，跳过本次触发" >> "$LOG_FILE"
  exit 0
fi
trap 'rm -rf "$LOCK_DIR"' EXIT

echo "[$(date '+%F %T')] ===== 开始定时扫描 =====" >> "$LOG_FILE"
if /app/scan.sh >> "$LOG_FILE" 2>&1; then
  echo "[$(date '+%F %T')] ===== 扫描完成 =====" >> "$LOG_FILE"
else
  echo "[$(date '+%F %T')] ===== 扫描异常退出（exit=$?）=====" >> "$LOG_FILE"
fi
