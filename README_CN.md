# 跨交易所价差套利

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

一个 **Maker-Taker 套利机器人**，利用两个永续合约交易所之间的价差进行套利。通过 WebSocket 实时监控 BBO（最优买卖报价），使用 **Z-Score 均值回归模型** 检测统计偏离，在 Maker 交易所挂单，成交后立即在 Taker 交易所对冲。

[🇬🇧 English](README.md)

## 工作原理

```
交易所 A (Maker)            交易所 B (Taker)
     │                          │
     └──── BBO WebSocket ───────┘
                │
        ┌───────▼───────┐
        │   价差引擎     │  滚动窗口 → μ, σ → Z-Score
        │  Z-Score 模型  │  |Z| > 阈值 → 发出信号
        └───────┬───────┘
                │ 信号
        ┌───────▼───────┐
        │    交易执行     │  Maker 挂限价单
        │  Maker-Taker   │  成交 → Taker 市价对冲
        └───────┬───────┘
                │
        ┌───────▼───────┐
        │   Telegram     │  交易/异常通知
        └───────────────┘
```

1. **价差引擎** 通过 WebSocket 订阅两个交易所的订单簿
2. 在滚动窗口内计算均值 (μ) 和标准差 (σ)
3. 先清洗异常盘口，再检查目标数量对应的可执行价差，只有同时超过硬性净边际门槛和动态 `mean + z-open * rolling std` 阈值时才开仓
4. **交易器** 在 Maker 交易所下 **Post-Only 限价单**
5. 成交后，立即在 Taker 交易所 **市价对冲**
6. 持续监控仓位：当清洗后的可执行价差回落到动态 `mean + z-close * rolling std` 阈值以下时平仓，同时保留 **止损**（`--z-stop`）和 **超时**（`--max-hold`）

## 交易所支持

| 交易所 | 角色 | 备注 |
|:-------|:-----|:-----|
| [Decibel](https://www.decibel.exchange/) | Maker 或 Taker | 永续合约，当前验证路径优先支持 |
| [EdgeX](https://www.edgex.exchange/) | Maker 或 Taker | 永续合约，需要 API Key + Account ID |
| [Hyperliquid](https://hyperliquid.xyz/) | Maker 或 Taker | 永续合约，已通过 registry 接线支持 |
| [Lighter](https://lighter.xyz/) | Maker 或 Taker | 永续合约，零手续费，推荐作为 Taker |

基于 [exchanges](https://github.com/QuantProcessing/exchanges) `v0.2.0` 统一 SDK 构建。当前项目已改为通过 SDK registry 创建交易所，不再在策略层硬编码构造函数，业务逻辑始终只依赖 `exchanges.Exchange`。

当前仓库已内置 `DECIBEL`、`LIGHTER`、`EDGEX`、`HYPERLIQUID` 的凭证映射。整体执行模型面向任意两家 perp 交易所，但目前实际优先验证的是 `DECIBEL` maker + `LIGHTER` taker 这条路径。

## 快速开始

### 环境要求

- Go 1.26+
- 两个交易所的 API 凭证

### 安装

```bash
git clone https://github.com/QuantProcessing/cross-exchanges-arb.git
cd cross-exchanges-arb

# 配置凭证
cp .env.example .env
# 编辑 .env 填入你的 API Key
```

### 运行方式

现在运行路径固定直接走 `live`，CLI 不再提供 dry-run / observe-only 之类的模式选择。

每次运行都会在 `logs/<timestamp>_<maker>_<taker>_<symbol>/` 下生成独立产物目录：

- `run.log`：面向值班视角的运行日志，包含低频 `STAT` 心跳和交易 / PnL 生命周期 `EVT` 事件
- `events.jsonl`：低频结构化事件流，只记录 `session`、`round`、`pnl`、`health` 四类事件
- `raw.jsonl`：原始单边订单簿流，maker 和 taker 共用一个文件，但每一行只记录一个 side

### `raw.jsonl` 字段说明

`raw.jsonl` 每一行都是一个 JSON 对象，对应某一个交易所 side 的一次订单簿快照。它不是 unified 结构：maker 更新和 taker 更新会作为两条独立记录写在同一个文件里。

顶层字段：

| 字段 | 含义 |
| --- | --- |
| `ts_local` | 本地写入这条 raw 记录时的墙钟时间。 |
| `side` | 这条记录属于哪条腿：`maker` 或 `taker`。 |
| `exchange` | 当前记录所属交易所名称。 |
| `symbol` | 当前跟踪的基础币种，例如 `BTC`。 |
| `exchange_ts` | 本地订单簿快照里携带的时间戳。如果适配器拿不到原生服务端时间，可能会退化为本地接收时间。EdgeX 当前就属于这种 best-effort 情况。 |
| `quote_lag_ms` | best-effort 的行情延迟，单位毫秒；当 `exchange_ts` 存在时，计算方式为 `ts_local - exchange_ts`。 |
| `bids` | 这条 raw 快照中的买盘档位。 |
| `asks` | 这条 raw 快照中的卖盘档位。 |

嵌套盘口字段：

| 字段 | 含义 |
| --- | --- |
| `bids[].price` | 某一档买盘价格。 |
| `bids[].qty` | 该买盘档位的数量。 |
| `asks[].price` | 某一档卖盘价格。 |
| `asks[].qty` | 该卖盘档位的数量。 |

Raw 流说明：

- `raw.jsonl` 故意不包含 `schema_version`。
- 当前运行时会记录每侧各 2 档盘口。
- 因为 maker 和 taker 是分开输出的，如果离线工具想得到 unified 视图，需要自行合并。

### `run.log`

`run.log` 现在优先服务值班的人：

- `STAT ...` 行用于快速回答当前是否安全、是否赚钱、卡在哪一步
- `EVT ...` 行用于记录关键生命周期事件，例如 signal、maker 下单、hedge 成功 / 失败、close 成功 / 失败、manual intervention、PnL 更新
- 不再在每次订单簿更新时输出一条详细 `MKT ...` 市场日志

### `events.jsonl`

`events.jsonl` 是复盘导向的结构化事件流，保持刻意简化，只记录：

- `session`：运行开始 / 停止
- `health`：与 `STAT` 心跳同频率的低频健康快照
- `round`：关键交易生命周期事件
- `pnl`：周期余额刷新和轮次已实现收益摘要

**运行**：
```bash
go run . --maker DECIBEL --taker LIGHTER --symbol BTC --qty 0.003 \
  --z-open 2.0 --z-close 0.5 --z-stop -1.5 \
  --window 300 --max-hold 5m \
  --maker-timeout 15s --max-rounds 1
```

当前运行时默认带有限制：

- 同一时间只允许一个完整轮次
- maker 单超时后会进入确认终态的撤单流程
- maker 部分成交会立即在 taker 腿对冲
- 成功平仓后先进入 cooldown，再允许下一轮
- hedge / close 未决失败会进入 `manual_intervention`
- 每次开仓都会输出结构化 recap，记录 signal / maker / fill / hedge 各阶段的 maker+taker BBO 与耗时

## 配置说明

### 环境变量

```bash
# Decibel 凭证
EXCHANGES_DECIBEL_API_KEY=...
EXCHANGES_DECIBEL_PRIVATE_KEY=...
EXCHANGES_DECIBEL_SUBACCOUNT_ADDR=...

# EdgeX 凭证
EXCHANGES_EDGEX_PRIVATE_KEY=...
EXCHANGES_EDGEX_ACCOUNT_ID=...

# Lighter 凭证
EXCHANGES_LIGHTER_PRIVATE_KEY=...
EXCHANGES_LIGHTER_ACCOUNT_INDEX=...
EXCHANGES_LIGHTER_KEY_INDEX=...
EXCHANGES_LIGHTER_RO_TOKEN=...

# Telegram 通知（可选）
TELEGRAM_BOT_TOKEN=...
TELEGRAM_CHAT_ID=...
```

### 命令行参数

| 参数 | 默认值 | 说明 |
|:-----|:-------|:-----|
| `--maker` | `EDGEX` | Maker 交易所（挂限价单） |
| `--taker` | `LIGHTER` | Taker 交易所（市价对冲） |
| `--maker-quote-currency` | `` | 可选的 maker 报价币种覆盖 |
| `--taker-quote-currency` | `` | 可选的 taker 报价币种覆盖 |
| `--symbol` | `BTC` | 交易币种（基础货币） |
| `--qty` | `0.001` | 下单数量（基础货币单位） |
| `--z-open` | `2.0` | 基于清洗后可执行价差的动态开仓阈值系数 |
| `--z-close` | `0.5` | 基于清洗后可执行价差的动态平仓阈值系数 |
| `--z-stop` | `-1.0` | 止损 Z-Score 阈值 |
| `--window` | `500` | 滚动窗口大小（tick 数） |
| `--min-profit` | `1.0` | 最低净利润（BPS，扣除手续费后） |
| `--impact-buffer-bps` | `0` | 预留给冲击成本的额外 BPS buffer |
| `--latency-buffer-bps` | `0` | 预留给延迟 / 报价漂移的额外 BPS buffer |
| `--warmup-ticks` | `200` | 预热期最少 tick 数 |
| `--warmup-duration` | `3m` | 开始交易前的最短时间预热 |
| `--cooldown` | `5s` | 交易冷却时间 |
| `--max-hold` | `30m` | 最大持仓时间 |
| `--slippage` | `0.002` | 滑点容忍度 (0.2%) |
| `--maker-timeout` | `15s` | Maker 订单超时后的撤单/重置阈值 |
| `--max-quote-age` | `1.5s` | 超过该时长的盘口会被视为 stale 而忽略 |
| `--max-rounds` | `1` | 达到该完成轮数后不再开启新一轮 |

## 部署

### 编译

```bash
# 打包 Linux amd64 可执行文件到 dist/
bash scripts/build-linux-amd64.sh

# 输出
# dist/cross-arb-linux-amd64
```

### PM2 管理

```bash
# 上传 cross-arb + .env + ecosystem.config.js 到服务器
pm2 start ecosystem.config.js

# 常用操作
pm2 logs cross-arb      # 查看日志
pm2 restart cross-arb   # 重启
pm2 stop cross-arb      # 停止
```

## 测试

```bash
go test ./...
```

## 项目结构

```
├── main.go                  # 精简入口，只负责 CLI 启动
├── internal/
│   ├── app/                # 启动编排、运行时接线
│   ├── config/             # CLI 解析与配置校验
│   ├── exchange/           # 交易所 registry 接线与凭证映射
│   ├── spread/             # 滚动统计、盘口监控、信号生成
│   └── trading/            # 交易状态机、执行流程、PnL 跟踪
├── ecosystem.config.js     # PM2 进程管理配置
├── .env.example            # 环境变量模板
└── .gitignore
```

## 算法详解

Z-Score 模型追踪两个交易所之间的价格差：

```
spread = 交易所A价格 - 交易所B价格
Z = (spread - μ) / σ
```

其中 μ 和 σ 在滚动窗口内计算。模型假设价差具有 **均值回归** 特性 — 大幅偏离均值后倾向于回归。

**手续费过滤**：仅当 `预期利润 > 往返手续费 + 最低利润` 时才发出信号。往返手续费 = (Maker 费率 + Taker 费率) × 2（开仓 + 平仓）。

## 局限性

> ⚠️ **这是一个 MVP（最小可行产品），用于验证算法有效性。** 它是一个可运行的原型，不是生产级软件。

当前版本距离生产化还差这些能力：

- **限流** — 没有 API 请求节流；信号密集时可能触发交易所限流导致订单被拒
- **重试机制** — 下单失败后不会重试
- **单腿失败自动修复** — 程序会阻断在 `manual_intervention`，但不会自主修复残余风险
- **多仓位** — 同时只支持一个仓位
- **手续费动态刷新** — 手续费仅在启动时获取一次
- **断线重连** — WebSocket 断连后不会自动恢复
- **通用交易所参数接线** — 策略层已泛化，但当前仓库只预置了少数交易所的 env/option 映射

## License

MIT
