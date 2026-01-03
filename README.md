# Polymarket Arbitrage Bot

A sophisticated Go-based trading bot for Polymarket prediction markets that implements multiple strategies including sports betting, over/under totals, and crypto price predictions.

## 🎯 Overview

This bot monitors Polymarket markets and executes trades based on configurable entry thresholds and delta-neutral hedging strategies. It supports:

- **Sports Moneyline** - Bet on game outcomes (NBA, NFL, MLB, Champions League, etc.)
- **Over/Under (O/U)** - Trade on total points/goals markets
- **Crypto Short-Term** - 15min/1hr/4hr price prediction arbitrage

## ⚙️ Features

### Trading Strategies

| Strategy | Description | Entry Criteria |
|----------|-------------|----------------|
| **Sports Moneyline** | Delta-neutral betting on game outcomes | Favorite <80%, Underdog <40% |
| **Over/Under** | Delta-neutral on O/U totals | Over <45%, Under <45% |
| **Crypto** | Spread arbitrage on 15min/1hr/4hr markets | Combined price <99% |

### Risk Management

- **Position Limits**: Max 40 total positions (30 sports, 10 crypto)
- **Stop Loss**: Configurable per-position stop loss (default 20%)
- **Take Profit**: Configurable take profit target (default 40%)
- **Trailing Stops**: Optional trailing stop support
- **Exposure Control**: Maximum single position and total exposure limits
- **Grace Period**: 60-second entry grace period before stop-loss activation

### Live Game Trading

- Pre-game and in-game entry support
- Late-game favorite detection (40+ minutes into NBA games)
- Odds momentum tracking for live entries
- Peak detection for exit timing

## 📁 Project Structure

```
polymarket-arbitrage-bot/
├── main.go                     # Entry point, orchestrates all components
├── config/
│   └── config.go               # Configuration loading from .env
├── internal/
│   ├── clients/
│   │   ├── clob/               # CLOB API client (order book, orders)
│   │   ├── gamma/              # Gamma API client (market discovery)
│   │   └── rtds/               # WebSocket client for real-time prices
│   ├── risk/
│   │   └── manager.go          # Position tracking, P&L, risk limits
│   ├── services/
│   │   └── monitor.go          # Position monitoring and exit signals
│   └── strategies/
│       ├── crypto/             # Crypto 15min/1hr/4hr arbitrage
│       ├── overunder/          # O/U delta neutral strategy  
│       └── sports/             # Sports moneyline strategy
├── .env.example                # Environment configuration template
├── run.sh                      # Launch script
└── go.mod                      # Go module definition
```

## 🚀 Quick Start

### Prerequisites

- Go 1.21+
- Polymarket API credentials (for live trading)

### Installation

```bash
# Clone and enter directory
cd polymarket-arbitrage-bot

# Install dependencies
go mod download

# Copy and configure environment
cp .env.example .env
# Edit .env with your settings
```

### Configuration

Configure the bot via `.env` file:

```bash
# Required for live trading
POLYMARKET_API_KEY=your_api_key
POLYMARKET_SECRET=your_secret
POLYMARKET_PASSPHRASE=your_passphrase
PRIVATE_KEY=your_private_key

# Mode: "dry-run" (default) or "live"
BOT_MODE=dry-run

# Risk Management
MAX_POSITION_SIZE=50        # Max $ per position
MAX_TOTAL_EXPOSURE=500      # Max total $ exposure
STOP_LOSS_PERCENT=0.15      # 15% stop loss
TAKE_PROFIT_PERCENT=0.30    # 30% take profit

# Enable/Disable Strategies
SPORTS_ENABLED=true
CRYPTO_ENABLED=true
SPORTS_OU_ENABLED=true
```

See `.env.example` for all configuration options.

### Running

```bash
# Build and run
go build -o polymarket-bot.exe
./polymarket-bot.exe

# Or run directly
go run main.go

# Using shell script (Linux/Mac)
chmod +x run.sh
./run.sh
```

## 🎲 Strategy Details

### Sports Moneyline (Delta Neutral)

1. **Market Discovery**: Scans Polymarket for NBA, NFL, MLB, EPL, Champions League games within 48 hours
2. **Entry Criteria**: Buys BOTH favorite and underdog when favorite <80% and underdog <40%
3. **Exit Strategy**: 
   - Take profit when favorite reaches 95%+ (live game winner likely)
   - Independent stop-loss on each leg (loser closes, winner continues)
   - Peak detection exits when odds start reversing

### Over/Under (Delta Neutral)

1. **Market Discovery**: Finds O/U markets for games within 48 hours
2. **Entry Criteria**: Buys both Over and Under when each is below 45%
3. **Exit**: Take profit when either leg hits target, independent stop-loss

### Crypto Short-Term

1. **Market Discovery**: Finds BTC, ETH, SOL price markets expiring in 15min/1hr/4hr
2. **Spread Arbitrage**: If Yes + No < 99%, buy both for guaranteed profit
3. **Leg-In Strategy**: Buy cheap side (<40%) then wait for second leg opportunity

## 📊 Monitoring

The bot provides detailed logging including:

```
╔════════════════════════════════════════════════════════════════════╗
║                    POSITION REPORT                                 ║
╠════════════════════════════════════════════════════════════════════╣
║ 🏀 SPORTS MONEYLINE (4 positions)                                 ║
║  Lakers          Entry: $0.4500 → $0.5200  ▲ $1.40 (15.6%)        ║
║  Warriors        Entry: $0.5500 → $0.4800  ▼ -$1.40 (-12.7%)      ║
╠════════════════════════════════════════════════════════════════════╣
║  TOTAL: 4/40 positions | Exposure: $100.00 | 📈 P&L: $8.50        ║
╚════════════════════════════════════════════════════════════════════╝
```

## ⚠️ Important Notes

- **Dry Run Mode**: Always test with `BOT_MODE=dry-run` first
- **No Financial Advice**: This bot is for educational purposes only
- **Losses Possible**: Delta-neutral strategies reduce but don't eliminate risk
- **API Limits**: Be mindful of Polymarket API rate limits
- **EIP-712 Signing**: Live trading requires proper order signing (see `clob/client.go`)

## 🔧 Development

### Building

```bash
go build -o polymarket-bot.exe
```

### Testing Dry Run

```bash
BOT_MODE=dry-run go run main.go
```

## 📜 License

This project is provided as-is for educational purposes. Use at your own risk.
