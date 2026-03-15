# 跨交易所价差套利

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

一个 **Maker-Taker 套利机器人**，利用两个永续合约交易所之间的价差进行套利。通过 WebSocket 实时监控 BBO（最优买卖报价），使用 **Z-Score 均值回归模型** 检测统计偏离，在 Maker 交易所挂限价单，成交后立即在 Taker 交易所对冲。

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

## 支持的交易所

| 交易所 | 角色 | 备注 |
|:-------|:-----|:-----|
| [EdgeX](https://www.edgex.exchange/) | Maker 或 Taker | 永续合约，需要 API Key + Account ID |
| [Lighter](https://lighter.xyz/) | Maker 或 Taker | 永续合约，零手续费，推荐作为 Taker |

基于 [exchanges](https://github.com/QuantProcessing/exchanges) 统一 SDK 构建。添加新交易所只需实现 `Exchange` 接口。

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
go run . --maker EDGEX --taker LIGHTER --symbol BTC --observe-only
```
输出 CSV 文件供离线分析。

**阶段 1 — 模拟运行**（模拟信号，不下真单）：
```bash
go run . --maker EDGEX --taker LIGHTER --symbol BTC --qty 0.003 \
  --z-open 2.0 --z-close 0.5 --z-stop -1.5 \
  --window 300 --max-hold 5m --dry-run
```

**阶段 2 — 实盘交易**：
```bash
go run . --maker EDGEX --taker LIGHTER --symbol BTC --qty 0.003 \
  --z-open 2.0 --z-close 0.5 --z-stop -1.5 \
  --window 300 --max-hold 5m
```

## 配置说明

### 环境变量

```bash
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
| `--symbol` | `BTC` | 交易币种（基础货币） |
| `--qty` | `0.001` | 下单数量（基础货币单位） |
| `--z-open` | `2.0` | 开仓 Z-Score 阈值 |
| `--z-close` | `0.5` | 平仓 Z-Score 阈值（止盈） |
| `--z-stop` | `-1.0` | 止损 Z-Score 阈值 |
| `--window` | `500` | 滚动窗口大小（tick 数） |
| `--min-profit` | `1.0` | 最低净利润（BPS，扣除手续费后） |
| `--warmup-ticks` | `200` | 预热期最少 tick 数 |
| `--cooldown` | `5s` | 交易冷却时间 |
| `--max-hold` | `30m` | 最大持仓时间 |
| `--slippage` | `0.002` | 滑点容忍度 (0.2%) |
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

## License

MIT
