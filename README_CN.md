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
3. 当 Z-Score 超过 `--z-open` 阈值，且预期利润 > 手续费 + `--min-profit` 时，发出交易信号
4. **交易器** 在 Maker 交易所下 **Post-Only 限价单**
5. 成交后，立即在 Taker 交易所 **市价对冲**
6. 持续监控仓位：**止盈**（Z < `--z-close`）、**止损**（Z < `--z-stop`）、**超时**（`--max-hold`）

## 交易所支持

| 交易所 | 角色 | 备注 |
|:-------|:-----|:-----|
| [Decibel](https://www.decibel.exchange/) | Maker 或 Taker | 永续合约，当前验证路径优先支持 |
| [EdgeX](https://www.edgex.exchange/) | Maker 或 Taker | 永续合约，需要 API Key + Account ID |
| [Lighter](https://lighter.xyz/) | Maker 或 Taker | 永续合约，零手续费，推荐作为 Taker |

基于 [exchanges](https://github.com/QuantProcessing/exchanges) `v0.2.0` 统一 SDK 构建。当前项目已改为通过 SDK registry 创建交易所，不再在策略层硬编码构造函数，业务逻辑始终只依赖 `exchanges.Exchange`。

当前仓库已内置 `DECIBEL`、`LIGHTER`、`EDGEX` 的凭证映射。整体执行模型面向任意两家 perp 交易所，但目前实际优先验证的是 `DECIBEL` maker + `LIGHTER` taker 这条路径。

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

### 运行模式

**阶段 0 — 观察模式**（仅采集价差数据）：
```bash
go run . --maker DECIBEL --taker LIGHTER --symbol BTC --observe-only
```
输出 CSV 文件供离线分析。

**阶段 1 — 模拟运行**（模拟信号，不下真单）：
```bash
go run . --maker DECIBEL --taker LIGHTER --symbol BTC --qty 0.003 \
  --z-open 2.0 --z-close 0.5 --z-stop -1.5 \
  --window 300 --max-hold 5m --dry-run
```

**阶段 2 — 实盘验证**：
```bash
go run . --maker DECIBEL --taker LIGHTER --symbol BTC --qty 0.003 \
  --z-open 2.0 --z-close 0.5 --z-stop -1.5 \
  --window 300 --max-hold 5m \
  --maker-timeout 15s --max-rounds 1 --live-validate=true
```

实盘验证模式默认带有限制：

- 同一时间只允许一个完整轮次
- maker 单超时后会进入确认终态的撤单流程
- maker 部分成交会立即在 taker 腿对冲
- 成功平仓后先进入 cooldown，再允许下一轮
- hedge / close 未决失败会进入 `manual_intervention`

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
| `--z-open` | `2.0` | 开仓 Z-Score 阈值 |
| `--z-close` | `0.5` | 平仓 Z-Score 阈值（止盈） |
| `--z-stop` | `-1.0` | 止损 Z-Score 阈值 |
| `--window` | `500` | 滚动窗口大小（tick 数） |
| `--min-profit` | `1.0` | 最低净利润（BPS，扣除手续费后） |
| `--warmup-ticks` | `200` | 预热期最少 tick 数 |
| `--warmup-duration` | `3m` | 开始交易前的最短时间预热 |
| `--cooldown` | `5s` | 交易冷却时间 |
| `--max-hold` | `30m` | 最大持仓时间 |
| `--slippage` | `0.002` | 滑点容忍度 (0.2%) |
| `--maker-timeout` | `15s` | Maker 订单超时后的撤单/重置阈值 |
| `--max-rounds` | `1` | 实盘验证模式下允许的最大完成轮数 |
| `--live-validate` | `true` | 开启实盘验证保护逻辑 |
| `--dry-run` | `false` | 模拟模式 |
| `--observe-only` | `false` | 观察模式（仅输出 CSV） |

## 部署

### 编译

```bash
# 交叉编译 Linux 版本
GOOS=linux GOARCH=amd64 go build -o cross-arb .
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

## 项目结构

```
├── main.go             # 入口，适配器创建，优雅关闭
├── config.go           # 命令行参数解析与配置
├── spread_engine.go    # Z-Score 模型，BBO 监控，信号生成
├── trader.go           # Maker-Taker 执行，仓位管理
├── ecosystem.config.js # PM2 进程管理配置
├── .env.example        # 环境变量模板
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
