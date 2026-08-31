#!/usr/bin/env bash
# =============================================================================
# 容器入口：根据 CRON_SCHEDULE 决定运行模式
#   - 未设置 CRON_SCHEDULE：单次模式，扫描完成后容器退出（原行为）
#   - 设置 CRON_SCHEDULE：定时模式，容器常驻，到点自动扫描
#     支持两种写法：完整 cron 表达式（"0 6 * * *"）或 "HH:MM"（每天定时）
# =============================================================================
set -uo pipefail

export TZ="${TZ:-Asia/Shanghai}"

# ---------- 单次模式 ----------
if [ -z "${CRON_SCHEDULE:-}" ]; then
  exec /app/scan.sh
fi

# ---------- 定时模式 ----------
RESULTS_DIR="${RESULTS_DIR:-/app/results}"
LOG_FILE="$RESULTS_DIR/cron.log"
RUN_ON_START="${RUN_ON_START:-true}"
CRON_SCHEDULE="$(echo "$CRON_SCHEDULE" | xargs)"

# 支持 "HH:MM" 简写（每天定时），转成 cron 表达式
if [[ "$CRON_SCHEDULE" =~ ^([0-9]{1,2}):([0-9]{1,2})$ ]]; then
  CRON_SCHEDULE="${BASH_REMATCH[2]} ${BASH_REMATCH[1]} * * *"
fi

# 基本校验：标准 5 段 cron
if [ "$(echo "$CRON_SCHEDULE" | wc -w)" -ne 5 ]; then
  echo "CRON_SCHEDULE 格式错误，应为 5 段 cron 表达式（如 '0 6 * * *'）或 'HH:MM'：${CRON_SCHEDULE}" >&2
  exit 1
fi

mkdir -p "$RESULTS_DIR"

# 把扫描相关环境变量落盘，供 cron 触发的任务加载（crond 派生的进程环境不完整）
env | grep -E '^(DC_LIST|TOP_N|IP_TYPE|PORT|THREADS|DELAY_MS|SPEED_MIN|SCAN_MODE|RESULTS_DIR|TZ)=' > /app/.container-env || true

# 启动时立即执行一次（RUN_ON_START=false 关闭）
if [ "$RUN_ON_START" = "true" ]; then
  echo "[$(date '+%F %T')] 容器启动，先立即执行一次扫描" >> "$LOG_FILE"
  /app/run-scheduled.sh
fi

# 注册定时任务（每行必须以换行结尾）
echo "$CRON_SCHEDULE /app/run-scheduled.sh" | crontab -

echo "定时模式已启动"
echo "  调度表达式: $CRON_SCHEDULE"
echo "  时区: $TZ ($(date '+%Z %z'))"
echo "  立即执行: $RUN_ON_START"
echo "  运行日志: $LOG_FILE"

# 前台运行 crond（作为 PID 1）
exec crond -f -l 8
