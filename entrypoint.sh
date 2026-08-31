#!/usr/bin/env bash
# =============================================================================
# 容器入口：按环境变量决定运行模式
#   - WEB_PORT 非空：Web 控制台模式（可与 CRON_SCHEDULE 定时共存，crond 后台运行）
#       地区定时任务（regions.json，每地区一条 cron）由 Web 进程内调度，无需重启容器
#   - CRON_SCHEDULE 非空：定时模式，容器常驻到点扫描
#     支持完整 cron 表达式（"0 6 * * *"）或 "HH:MM"（每天定时）
#   - 两者均空：单次模式，扫描完成后容器退出
# =============================================================================
set -uo pipefail

export TZ="${TZ:-Asia/Shanghai}"

# 把扫描相关环境变量落盘，供 cron 触发的任务加载（crond 派生的进程环境不完整）
dump_env() {
  env | grep -E '^(DC_LIST|TOP_N|IP_TYPE|PORT|THREADS|DELAY_MS|SPEED_MIN|SCAN_MODE|RESULTS_DIR|TZ)=' > /app/.container-env || true
}

# "HH:MM" 简写转 cron 表达式并校验（输出到全局 SCHED，非法则返回非 0）
normalize_schedule() {
  SCHED="$(echo "$1" | xargs)"
  if [[ "$SCHED" =~ ^([0-9]{1,2}):([0-9]{1,2})$ ]]; then
    SCHED="${BASH_REMATCH[2]} ${BASH_REMATCH[1]} * * *"
  fi
  [ "$(echo "$SCHED" | wc -w)" -eq 5 ]
}

# ---------- Web 控制台模式 ----------
if [ -n "${WEB_PORT:-}" ]; then
  RESULTS_DIR="${RESULTS_DIR:-/app/results}"
  mkdir -p "$RESULTS_DIR"
  dump_env

  # 与定时模式共存：注册 crontab 后台运行（Web 服务保持前台，容器日志不被阻塞）
  if [ -n "${CRON_SCHEDULE:-}" ]; then
    if normalize_schedule "$CRON_SCHEDULE"; then
      echo "$SCHED /app/run-scheduled.sh" | crontab -
      crond -b -l 8
      echo "定时任务已启用: $SCHED（crond 后台运行，与 Web 触发共用锁互斥）"
    else
      echo "CRON_SCHEDULE 格式错误，已忽略（应为 5 段 cron 或 HH:MM）: $CRON_SCHEDULE" >&2
    fi
  fi

  echo "Web 控制台启动: http://0.0.0.0:${WEB_PORT}（时区 $TZ）"
  exec /app/cfdata-web
fi

# ---------- 单次模式 ----------
if [ -z "${CRON_SCHEDULE:-}" ]; then
  exec /app/scan.sh
fi

# ---------- 定时模式 ----------
RESULTS_DIR="${RESULTS_DIR:-/app/results}"
LOG_FILE="$RESULTS_DIR/cron.log"
RUN_ON_START="${RUN_ON_START:-true}"

if ! normalize_schedule "$CRON_SCHEDULE"; then
  echo "CRON_SCHEDULE 格式错误，应为 5 段 cron 表达式（如 '0 6 * * *'）或 'HH:MM'：${CRON_SCHEDULE}" >&2
  exit 1
fi

mkdir -p "$RESULTS_DIR"
dump_env

# 启动时立即执行一次（RUN_ON_START=false 关闭）
if [ "$RUN_ON_START" = "true" ]; then
  echo "[$(date '+%F %T')] 容器启动，先立即执行一次扫描" >> "$LOG_FILE"
  /app/run-scheduled.sh
fi

# 注册定时任务（每行必须以换行结尾）
echo "$SCHED /app/run-scheduled.sh" | crontab -

echo "定时模式已启动"
echo "  调度表达式: $SCHED"
echo "  时区: $TZ ($(date '+%Z %z'))"
echo "  立即执行: $RUN_ON_START"
echo "  运行日志: $LOG_FILE"

# 前台运行 crond（作为 PID 1）
exec crond -f -l 8
