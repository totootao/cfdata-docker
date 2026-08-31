# cfdata-docker

基于 [CFData-WEB](https://github.com/totootao/CFData-WEB) 的 Cloudflare 数据中心优选工具，**分别筛选出伦敦（LHR）、法兰克福（FRA）、西雅图（SEA）下载速度前十的 IP**，并打包为 Docker 镜像，通过 GitHub Actions 自动构建。

## 项目结构

```
├── Dockerfile                  # 多阶段构建：源码编译 CFData + Alpine 运行时
├── scan.sh                     # 核心扫描脚本：逐机房扫描 → 测速 → TopN 筛选
├── .github/workflows/
│   ├── docker.yml              # 构建并推送 Docker 镜像到 Docker Hub
│   └── scan.yml                # 运行扫描并把 Top10 结果提交到仓库
└── results/                    # 扫描结果（lhr/fra/sea 的 csv + txt）
```

## 工作原理

1. CFData 扫描 Cloudflare 官方 IP 段（每个 /24 子网随机取一个 IP），通过 `trace` 接口判断每个 IP 落地的数据中心（IATA 码）
2. 用 `-offdc` 指定数据中心（`LHR` 伦敦 / `FRA` 法兰克福 / `SEA` 西雅图），对命中的 IP 做详细延迟测试
3. 按延迟排序逐个测速，凑够达标数量后导出（导出结果已按下载速度降序）
4. `scan.sh` 按数据中心列二次过滤并截取前 10 条，输出到 `results/`

## Docker 镜像

镜像地址：`totootao/cfdata`（amd64 / arm64），由 `docker.yml` Workflow 自动构建并推送。

### 本地运行

```bash
# 默认筛选 LHR/FRA/SEA 前十（单次模式，扫完即退出）
docker run --rm -v "$PWD/results:/app/results" totootao/cfdata

# 自定义：只测法兰克福，取前 10，测速下限 5MB/s
docker run --rm -e DC_LIST=FRA -e TOP_N=10 -e SPEED_MIN=5 \
  -v "$PWD/results:/app/results" totootao/cfdata
```

### 定时模式（适合家里服务器常驻）

设置 `CRON_SCHEDULE` 后容器常驻，到点自动扫描，每次运行覆盖 `results/`，历史记录追加在 `results/cron.log`：

```bash
docker run -d --name cfdata --restart unless-stopped \
  -e CRON_SCHEDULE="0 6 * * *" \
  -e DC_LIST=LHR,FRA,SEA \
  -e TOP_N=10 \
  -e SPEED_MIN=5 \
  -v /opt/cfdata/results:/app/results \
  totootao/cfdata
```

`CRON_SCHEDULE` 支持两种写法：

- 完整 cron 表达式：`"0 6 * * *"`（每天 6 点）、`"0 6,18 * * *"`（每天 6 点和 18 点）、`"*/30 * * * *"`（每 30 分钟）
- 每日定时简写：`"06:00"` 等价于 `"0 6 * * *"`

docker-compose 示例：

```yaml
services:
  cfdata:
    image: totootao/cfdata
    container_name: cfdata
    restart: unless-stopped
    environment:
      TZ: Asia/Shanghai
      CRON_SCHEDULE: "0 6 * * *"   # 每天北京时间 06:00
      DC_LIST: LHR,FRA,SEA
      TOP_N: "10"
      SPEED_MIN: "5"
    volumes:
      - ./results:/app/results
```

查看运行情况：

```bash
docker logs -f cfdata      # 调度器输出
cat results/cron.log       # 每次扫描的详细日志
```

内置保护：扫描进行中若到下一个触发点，自动跳过该次（并发锁）；锁超过 3 小时视为残留自动清除。

### 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `DC_LIST` | `LHR,FRA,SEA` | 数据中心列表（Cloudflare 机房 IATA 码，逗号分隔） |
| `TOP_N` | `10` | 每个数据中心取前 N 个结果 |
| `IP_TYPE` | `4` | IP 类型：`4` / `6` |
| `PORT` | `443` | 测试/测速端口 |
| `THREADS` | `100` | 扫描并发数 |
| `DELAY_MS` | `500` | 延迟阈值（毫秒） |
| `SPEED_MIN` | `1` | 测速达标下限（MB/s） |
| `RESULTS_DIR` | `/app/results` | 结果输出目录 |
| `CRON_SCHEDULE` | （空） | 空 = 单次执行后退出；填 cron 表达式或 `HH:MM` = 常驻定时调度 |
| `RUN_ON_START` | `true` | 定时模式下容器启动时是否先立即执行一次 |
| `TZ` | `Asia/Shanghai` | 时区（影响调度时间） |

常用机房 IATA 码：`LHR` 伦敦、`FRA` 法兰克福、`SEA` 西雅图、`AMS` 阿姆斯特丹、`CDG` 巴黎、`IAD` 华盛顿、`LAX` 洛杉矶、`SJC` 圣何塞、`HKG` 香港、`NRT` 东京、`SIN` 新加坡。

## ⚠️ 重要：扫描结果取决于运行环境的网络位置

Cloudflare 是 anycast 网络，**从哪个网络出口跑扫描，扫到的 IP 就落地在附近的机房**：

- GitHub Actions 托管 Runner 出口在美国，`SEA` 通常有结果，`LHR` / `FRA` 大概率为空
- 要拿到伦敦/法兰克福的 Top10，请在对应地区的 VPS 上运行镜像，例如：

```bash
# 在伦敦的 VPS 上
docker run --rm -e DC_LIST=LHR -v "$PWD/results:/app/results" totootao/cfdata
```

- 也可以给仓库添加自托管 Runner（部署在目标地区）后手动触发 `scan.yml`

## Workflows

| Workflow | 触发条件 | 作用 |
|---|---|---|
| `docker.yml` | push 到 main / 手动 / 每周一 | 构建多架构镜像并推送到 Docker Hub（`latest` + commit SHA 标签） |
| `scan.yml` | 手动 / 每周一 | 本地构建镜像运行扫描，将 `results/` 提交回仓库 |

所需 Secrets（已在仓库中配置）：

- `DOCKERHUB_USERNAME`：Docker Hub 用户名
- `DOCKERHUB_TOKEN`：Docker Hub 密码/访问令牌

## 致谢

- [totootao/CFData-WEB](https://github.com/totootao/CFData-WEB) / [PoemMisty/CFData-WEB](https://github.com/PoemMisty/CFData-WEB)（GPL-3.0）

## 免责声明

本项目仅供学习与研究用途，请遵守所在地区法律法规。
