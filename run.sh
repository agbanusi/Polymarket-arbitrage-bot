#!/bin/bash

# Polymarket Arbitrage Bot - Run Script
# Usage: ./run.sh [mode]
#   mode: "live" or "dry-run" (default: dry-run)

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}===============================================${NC}"
echo -e "${GREEN}       POLYMARKET ARBITRAGE BOT               ${NC}"
echo -e "${GREEN}===============================================${NC}"

# Check if Go is installed
if ! command -v go &> /dev/null; then
    echo -e "${RED}Error: Go is not installed or not in PATH${NC}"
    exit 1
fi

# Set mode from argument or default to dry-run
MODE=${1:-dry-run}
export BOT_MODE="$MODE"

if [ "$MODE" == "live" ]; then
    echo -e "${RED}⚠️  WARNING: RUNNING IN LIVE MODE!${NC}"
    echo -e "${RED}Real trades will be executed with real money!${NC}"
    echo ""
    read -p "Are you sure you want to continue? (yes/no): " confirm
    if [ "$confirm" != "yes" ]; then
        echo "Aborted."
        exit 0
    fi
else
    echo -e "${YELLOW}Running in DRY-RUN mode (no real trades)${NC}"
fi

echo ""

# Default configuration (override with environment variables)
export CLOB_API_URL="${CLOB_API_URL:-https://clob.polymarket.com}"
export GAMMA_API_URL="${GAMMA_API_URL:-https://gamma-api.polymarket.com}"

# Risk defaults
export MAX_POSITION_SIZE="${MAX_POSITION_SIZE:-3}"
export STOP_LOSS_PERCENT="${STOP_LOSS_PERCENT:-0.20}"
export TAKE_PROFIT_PERCENT="${TAKE_PROFIT_PERCENT:-0.50}"

# Strategy defaults
export SPORTS_ENABLED="${SPORTS_ENABLED:-true}"
export SPORTS_OU_ENABLED="${SPORTS_OU_ENABLED:-true}"
export SPORTS_ENTRY_THRESHOLD="${SPORTS_ENTRY_THRESHOLD:-0.25}"
export SPORTS_EXIT_TARGET="${SPORTS_EXIT_TARGET:-0.70}"
export SPORTS_DELTA_NEUTRAL="${SPORTS_DELTA_NEUTRAL:-true}"

export CRYPTO_ENABLED="${CRYPTO_ENABLED:-true}"
export CRYPTO_CHEAP_THRESHOLD="${CRYPTO_CHEAP_THRESHOLD:-0.25}"
export CRYPTO_MAX_SPREAD="${CRYPTO_MAX_SPREAD:-0.99}"
export CRYPTO_MIN_TIME_LEFT="${CRYPTO_MIN_TIME_LEFT:-120}"

export PRICE_UPDATE_INTERVAL="${PRICE_UPDATE_INTERVAL:-2}"

echo "Configuration:"
echo "  Mode: $BOT_MODE"
echo "  Max Position: \$$MAX_POSITION_SIZE"
echo "  Stop Loss: $(awk "BEGIN {printf \"%.0f\", $STOP_LOSS_PERCENT * 100}")%"
echo "  Take Profit: $(awk "BEGIN {printf \"%.0f\", $TAKE_PROFIT_PERCENT * 100}")%"
echo "  Sports Enabled: $SPORTS_ENABLED"
echo "  Crypto Enabled: $CRYPTO_ENABLED"
echo ""

# Run the bot
echo -e "${GREEN}Starting bot...${NC}"
echo ""

go run cmd/bot/main.go
