# cfdata-docker

基于 [CFData-WEB](https://github.com/totootao/CFData-WEB) 的 Cloudflare 数据中心优选工具，**分别筛选出伦敦（LHR）、法兰克福（FRA）、西雅图（SEA）下载速度前十的 IP**，并打包为 Docker 镜像，通过 GitHub Actions 自动构建。

## 项目结构

```
├── Dockerfile                  # 多阶段构建：源码编译 CFData + Web 控制台 + Alpine 运行时
├── scan.sh                     # 核心扫描脚本：逐机房扫描 → 测速 → TopN 筛选
├── entrypoint.sh               # 入口：单次 / 定时 / Web 三种模式分流
├── run-scheduled.sh            # 定时任务体：环境加载 + 并发锁 + 日志
├── webapp/                     # Web 控制台（Go 单二进制，前端经 go:embed 内嵌）
│   ├── main.go                 # API：触发测速 / 实时进度 / 结果 / 日志
│   ├── regions.go              # 地区定时任务：存储 / CRUD / cron 调度 / txt 输出 API
│   ├── cronexpr.go             # 最小 cron 解析器（5 段表达式 + HH:MM 简写）
│   └── static/index.html       # 前端界面（机房选择 + 地区任务管理 + 实时输出 + 结果表格）
├── .github/workflows/
│   ├── docker.yml              # 构建并推送 Docker 镜像到 Docker Hub
│   └── scan.yml                # 运行扫描并把 Top10 结果提交到仓库
└── results/                    # 扫描结果（lhr/fra/sea 的 csv + txt + regions.json）
```

## 工作原理

1. CFData 扫描 Cloudflare 官方 IP 段（每个 /24 子网随机取一个 IP），通过 `trace` 接口判断每个 IP 落地的数据中心（IATA 码）
2. 用 `-offdc` 指定数据中心（`LHR` 伦敦 / `FRA` 法兰克福 / `SEA` 西雅图），对命中的 IP 做详细延迟测试
3. 按延迟排序逐个测速，凑够达标数量后导出（导出结果已按下载速度降序）
4. `scan.sh` 按数据中心列二次过滤并截取前 10 条，输出到 `results/`

## Docker 镜像

镜像地址：`totootao/cfdata`（amd64 / arm64），由 `docker.yml` Workflow 自动构建并推送。

三种运行模式（可组合）：

| 模式 | 开启方式 | 行为 |
|---|---|---|
| 单次 | 不设任何参数 | 扫描一次后退出 |
| Web 控制台 | `WEB_PORT=8080` | 常驻，浏览器里选地点触发测速 |
| 定时 | `CRON_SCHEDULE="0 6 * * *"` | 常驻，到点自动扫描（可与 Web 共存） |

### Web 控制台（推荐家里服务器使用）

```bash
docker run -d --name cfdata -p 8080:8080 --restart unless-stopped \
  -e WEB_PORT=8080 \
  -e DC_LIST=LHR,FRA,SEA \
  -e SPEED_MIN=5 \
  -v /opt/cfdata/results:/app/results \
  totootao/cfdata
```

浏览器打开 `http://服务器IP:8080`：

- **按地点触发测速**：勾选机房（内置 22 个常用机房 + 自定义 IATA 码输入框），设置 TopN 与测速下限，点"开始测速"
- **地区定时任务**：每个地区一条独立 cron（详见下节），到点自动扫描该地区的机房
- **实时进度**：当前正在扫描的机房、已运行时长、扫描器实时输出
- **结果展示**：各机房 TopN 表格（IP:端口 / 城市 / 延迟 / 速度），按下载速度降序，带速度条
- **互斥保护**：Web 触发、地区定时、CRON_SCHEDULE 共用同一把锁，扫描中不会重复触发（返回 409）

Web + 定时共存：

```bash
docker run -d --name cfdata -p 8080:8080 --restart unless-stopped \
  -e WEB_PORT=8080 \
  -e CRON_SCHEDULE="0 6 * * *" \
  -v /opt/cfdata/results:/app/results \
  totootao/cfdata
```

API（可供外部调用）：

| 接口 | 说明 |
|---|---|
| `GET /api/status` | 运行状态 + 最近输出 |
| `GET /api/results` | 各机房 TopN 结果 JSON |
| `GET /api/config` | 默认配置、机房列表、国家→机房预设 |
| `GET /api/logs?lines=200` | cron.log 尾部 |
| `POST /api/scan` | 触发扫描：`{"dc_list":"LHR,FRA","top_n":10,"speed_min":"5","source":"official","nsb_url":"..."}`（source 可选 `official`/`nsb`/`all`） |
| `GET /api/regions` | 地区任务列表（含结果数 / 上次运行 / 下次运行） |
| `POST /api/regions` | 创建/更新地区任务（见下节） |
| `DELETE /api/regions/{region}` | 删除地区任务（结果文件不受影响） |
| `POST /api/regions/{region}/scan` | 立即触发该地区扫描 |
| `GET /region/{region}.txt` | 该地区优选结果（按下载速度降序，最多 100 条） |
| `GET /random-region/{region}/{n}.txt` | 该地区随机 n 个 IP（bestcf 风格） |

### 地区定时任务（每个地区一条 cron）

在 Web 控制台左侧「地区定时任务」卡片中配置：**一个地区 = 一条独立 cron**，到点自动扫描该地区包含的所有机房。配置保存在挂载卷的 `results/regions.json`，改完即生效（进程内调度，无需重启容器），容器重启后自动恢复。

- **地区标识**：输入国家码（如 `GB`、`JP`、`US`）自动填充该国家的预设机房列表，也可用任意自定义标识（如 `MY-HOME`）
- **机房列表**：逗号分隔的 IATA 码，预设值仅作参考，可自由增删（单个任务最多 10 个机房）
- **cron**：支持标准 5 段表达式（`0 6 * * *`）或 `HH:MM` 每日简写（`06:00`）
- **其余参数**：每个任务独立设置 TopN 与测速下限（MB/s），可随时停用/启用

一个任务示例（等价的 JSON 请求体）：

```json
POST /api/regions
{
  "region": "GB",
  "name": "英国",
  "colos": ["LHR", "MAN"],
  "cron": "0 6 * * *",
  "top_n": 10,
  "speed_min": "1",
  "source": "official",
  "enabled": true
}
```

配置好后即可用 txt API 直取该地区的结果（供订阅/脚本消费）：

```bash
curl http://服务器IP:8080/region/GB.txt              # 按速度降序的优选列表
curl http://服务器IP:8080/random-region/GB/1.txt     # 随机 1 个（bestcf 风格，可换任意 n）
```

说明：

- 地区未配置任务时，txt API 回退到内置的国家→机房预设（共 40+ 国家），但需要对应机房已有扫描数据
- 扫描互斥：某地区扫描进行中，其他地区/手动触发的任务会排队等待（下轮调度重试），不会并发冲突
- 容器重启期间错过的触发点不补跑，`next_run` 按当前时间重算

### 非标优选（bestcf 风格 random-region 的数据源）

除 Cloudflare 官方公布的 IP 段外，还支持扫描**非标 IP 库**（每行 `IP 端口` 或 `域名 端口`，支持域名自动解析）。非标扫描一次跑全部地址，按落地机房自动分组存到 `results/nsb/`，与官方结果（`results/*.csv`）互不覆盖。

数据源三种选择（地区任务与 Web 主扫描均可选）：

| 数据源 | 说明 |
|---|---|
| `official` | 官方 IP 段（默认，行为同前） |
| `nsb` | 非标 IP 库：只跑非标扫描，txt API 只输出非标结果 |
| `all` | 两者都跑（先官方后非标），txt API 合并输出并按 ip:port 去重 |

非标 IP 库 URL 的配置方式（二选一，任务级优先）：

1. 容器全局环境变量（所有非标任务共用）：

```bash
docker run -d --name cfdata -p 8080:8080 --restart unless-stopped \
  -e WEB_PORT=8080 \
  -e NSB_SOURCE_URL="https://example.com/your-nsb-ips.txt" \
  -v /opt/cfdata/results:/app/results \
  totootao/cfdata
```

2. 地区任务的 `nsb_url` 字段（Web 表单或 JSON 请求体指定，仅该任务使用）

非标任务示例：

```json
POST /api/regions
{
  "region": "KR",
  "name": "韩国",
  "colos": ["ICN"],
  "cron": "0 6 * * *",
  "top_n": 10,
  "speed_min": "1",
  "source": "nsb",
  "nsb_url": "https://example.com/your-nsb-ips.txt",
  "enabled": true
}
```

之后 `GET /random-region/KR/50.txt` 即随机返回 50 个落地韩国机房的非标优选 IP（与 bestcf 的接口语义一致）。

单次/定时模式跑非标（无 Web）：

```bash
docker run --rm \
  -e MODE=nsb \
  -e NSB_SOURCE_URL="https://example.com/your-nsb-ips.txt" \
  -e TOP_N=10 -e SPEED_MIN=1 \
  -v "$PWD/results:/app/results" totootao/cfdata
```

### 本地运行（单次模式）

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
| `MODE` | `official` | 扫描模式：`official`（官方 IP 段）/ `nsb`（非标 IP 库） |
| `NSB_SOURCE_URL` | （空） | 非标 IP 库 URL（每行 `IP 端口`，支持域名）；`MODE=nsb` 时必填 |
| `NSB_FILE` | （空） | 非标本地文件（挂载进容器，优先于 URL） |
| `NSB_SPEED_LIMIT` | `200` | 非标测速达标结果上限（凑够即停止测速） |
| `NSB_RESULT_LIMIT` | `1000` | 非标延迟测试结果上限 |
| `RESULTS_DIR` | `/app/results` | 结果输出目录 |
| `CRON_SCHEDULE` | （空） | 空 = 单次执行后退出；填 cron 表达式或 `HH:MM` = 常驻定时调度 |
| `RUN_ON_START` | `true` | 定时模式下容器启动时是否先立即执行一次 |
| `WEB_PORT` | （空） | 空 = 不启用 Web；填端口（如 `8080`）= 启动 Web 控制台，可与定时共存 |
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
