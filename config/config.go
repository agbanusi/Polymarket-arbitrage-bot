package config

import (
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/joho/godotenv"
)

// Config holds all configuration for the bot
type Config struct {
	// API Authentication
	PolymarketAPIKey     string
	PolymarketSecret     string
	PolymarketPassphrase string
	PrivateKey           string // For order signing (EIP-712)

	// API URLs
	BaseURL  string // CLOB API
	GammaURL string // Gamma API
	RTDSURL  string // WebSocket

	// Operation Mode
	Mode string // "live" or "dry-run"

	// Risk Management
	MaxPositionSize     float64 // Max $ per single position
	MaxTotalExposure    float64 // Max total $ exposure
	StopLossPercent     float64 // Stop loss percentage (0.15 = 15%)
	TakeProfitPercent   float64 // Take profit percentage (0.30 = 30%)
	TrailingStopPercent float64 // Trailing stop percentage (0 = disabled)
	MinSpread           float64 // Minimum spread for crypto arb
	TradingFeePercent   float64 // Round-trip trading fee (0.02 = 2%)
	MinProfitAfterFees  float64 // Minimum profit after fees to enter (0.03 = 3%)
	MinLiquidityUSD     float64 // Minimum order book liquidity in USD

	// Sports Strategy
	SportsEnabled        bool
	SportsOUEnabled      bool     // Enable O/U strategy
	SportsEntryThreshold float64  // Entry price for underdog (e.g., 0.35)
	SportsExitTarget     float64  // Exit target price (e.g., 0.45)
	SportsDeltaNeutral   bool     // Enable delta neutral mode
	SportsTags           []string // Tags to search for (NBA, NFL, etc.)

	// Crypto Strategy
	CryptoEnabled        bool
	CryptoCheapThreshold float64  // Entry threshold (e.g., 0.40)
	CryptoMaxSpread      float64  // Max combined Yes+No price (e.g., 0.99)
	CryptoMinTimeLeft    int      // Min seconds before expiry
	CryptoSymbols        []string // BTC, ETH, SOL
	CryptoTimeWindows    []string // Time windows to trade: "15min", "1hr", "4hr"

	// Sports Live Game Settings
	SportsLiveGameOnly    bool    // Only trade during live games
	SportsOddsShiftMin    float64 // Min odds shift to trigger entry (e.g., 0.10 = 10%)
	SportsOddsHistorySize int     // Number of price points to track for momentum
	SportsLateGameMinOdds float64 // Min odds for late-game favorite entry (e.g., 0.85)

	// Monitoring
	PriceUpdateInterval int // Seconds between price updates
	LogLevel            string
}

// LoadConfig loads configuration from .env file and environment variables
func LoadConfig() *Config {
	// Try to load .env file from current directory and parent directories
	envPaths := []string{
		".env",
		filepath.Join("..", ".env"),
		filepath.Join("..", "..", ".env"),
	}

	envLoaded := false
	for _, path := range envPaths {
		if err := godotenv.Load(path); err == nil {
			log.Printf("Config: Loaded environment from %s", path)
			envLoaded = true
			break
		}
	}

	if !envLoaded {
		log.Println("Config: No .env file found, using environment variables only")
	}

	cfg := &Config{
		// API Auth
		PolymarketAPIKey:     getEnv("POLYMARKET_API_KEY", ""),
		PolymarketSecret:     getEnv("POLYMARKET_SECRET", ""),
		PolymarketPassphrase: getEnv("POLYMARKET_PASSPHRASE", ""),
		PrivateKey:           getEnv("PRIVATE_KEY", ""),

		// API URLs
		BaseURL:  getEnv("CLOB_API_URL", "https://clob.polymarket.com"),
		GammaURL: getEnv("GAMMA_API_URL", "https://gamma-api.polymarket.com"),
		RTDSURL:  getEnv("RTDS_WS_URL", "wss://rtds.polymarket.com"),

		// Mode
		Mode: getEnv("BOT_MODE", "dry-run"),

		// Risk Management
		MaxPositionSize:     getEnvFloat("MAX_POSITION_SIZE", 5.0),
		MaxTotalExposure:    getEnvFloat("MAX_TOTAL_EXPOSURE", 200.0),
		StopLossPercent:     getEnvFloat("STOP_LOSS_PERCENT", 0.20),
		TakeProfitPercent:   getEnvFloat("TAKE_PROFIT_PERCENT", 0.50),
		TrailingStopPercent: getEnvFloat("TRAILING_STOP_PERCENT", 0.0),
		MinSpread:           getEnvFloat("MIN_SPREAD", 0.05),
		TradingFeePercent:   getEnvFloat("TRADING_FEE_PERCENT", 0.02),
		MinProfitAfterFees:  getEnvFloat("MIN_PROFIT_AFTER_FEES", 0.01),
		MinLiquidityUSD:     getEnvFloat("MIN_LIQUIDITY_USD", 50.0),

		// Sports Strategy
		SportsEnabled:        getEnvBool("SPORTS_ENABLED", true),
		SportsOUEnabled:      getEnvBool("SPORTS_OU_ENABLED", true),
		SportsEntryThreshold: getEnvFloat("SPORTS_ENTRY_THRESHOLD", 0.25),
		SportsExitTarget:     getEnvFloat("SPORTS_EXIT_TARGET", 0.55),
		SportsDeltaNeutral:   getEnvBool("SPORTS_DELTA_NEUTRAL", true), // Default: buy both sides for protection
		SportsTags: getEnvSlice("SPORTS_TAGS", []string{
			"NBA", "NFL", "MLB", "NHL",
			// "Soccer", "Football",
			"EPL", "laliga",
			// "La Liga", "Serie A", "Bundesliga", "Ligue 1",
			// "Champions League", "UEFA", //"MLS",
		}),

		// Crypto Strategy - SHORT-TERM ONLY (15min/1hr/4hr)
		CryptoEnabled:        getEnvBool("CRYPTO_ENABLED", true),
		CryptoCheapThreshold: getEnvFloat("CRYPTO_CHEAP_THRESHOLD", 0.45),
		CryptoMaxSpread:      getEnvFloat("CRYPTO_MAX_SPREAD", 0.99),
		CryptoMinTimeLeft:    getEnvInt("CRYPTO_MIN_TIME_LEFT", 120),
		CryptoSymbols:        getEnvSlice("CRYPTO_SYMBOLS", []string{"BTC", "ETH", "SOL"}),
		CryptoTimeWindows:    getEnvSlice("CRYPTO_TIME_WINDOWS", []string{"15min", "1hr", "4hr"}),

		// Sports Live Game Settings
		SportsLiveGameOnly:    getEnvBool("SPORTS_LIVE_GAME_ONLY", true),
		SportsOddsShiftMin:    getEnvFloat("SPORTS_ODDS_SHIFT_MIN", 0.10),
		SportsOddsHistorySize: getEnvInt("SPORTS_ODDS_HISTORY_SIZE", 10),
		SportsLateGameMinOdds: getEnvFloat("SPORTS_LATE_GAME_MIN_ODDS", 0.85),

		// Monitoring
		PriceUpdateInterval: getEnvInt("PRICE_UPDATE_INTERVAL", 5),
		LogLevel:            getEnv("LOG_LEVEL", "info"),
	}

	// Validation
	if cfg.Mode == "live" && cfg.PrivateKey == "" {
		log.Fatal("Config: PRIVATE_KEY is required for live mode")
	}

	if cfg.Mode == "live" && cfg.PolymarketAPIKey == "" {
		log.Fatal("Config: POLYMARKET_API_KEY is required for live mode")
	}

	return cfg
}

// IsDryRun returns true if running in dry-run mode
func (c *Config) IsDryRun() bool {
	return c.Mode != "live"
}

// LogConfig logs the current configuration (masking sensitive values)
func (c *Config) LogConfig() {
	log.Println("=== Bot Configuration ===")
	log.Printf("Mode: %s", c.Mode)
	log.Printf("CLOB URL: %s", c.BaseURL)
	log.Printf("Gamma URL: %s", c.GammaURL)
	log.Printf("API Key: %s", maskString(c.PolymarketAPIKey))
	log.Printf("Private Key: %s", maskString(c.PrivateKey))
	log.Printf("Max Position Size: $%.2f", c.MaxPositionSize)
	log.Printf("Stop Loss: %.0f%%", c.StopLossPercent*100)
	log.Printf("Take Profit: %.0f%%", c.TakeProfitPercent*100)
	log.Printf("Sports Enabled: %v (Entry: %.3f, Exit: %.3f, Tags: %v)",
		c.SportsEnabled, c.SportsEntryThreshold, c.SportsExitTarget, c.SportsTags)
	log.Printf("Crypto Enabled: %v (Threshold: %.2f, Symbols: %v)",
		c.CryptoEnabled, c.CryptoCheapThreshold, c.CryptoSymbols)
	log.Println("=========================")
}

// maskString masks a string for secure logging
func maskString(s string) string {
	if s == "" {
		return "(not set)"
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

// Helper functions

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	str := getEnv(key, "")
	if str == "" {
		return fallback
	}
	val, err := strconv.ParseFloat(str, 64)
	if err != nil {
		log.Printf("Config: Invalid float for %s: %v, using default %.2f", key, err, fallback)
		return fallback
	}
	return val
}

func getEnvInt(key string, fallback int) int {
	str := getEnv(key, "")
	if str == "" {
		return fallback
	}
	val, err := strconv.Atoi(str)
	if err != nil {
		log.Printf("Config: Invalid int for %s: %v, using default %d", key, err, fallback)
		return fallback
	}
	return val
}

func getEnvBool(key string, fallback bool) bool {
	str := getEnv(key, "")
	if str == "" {
		return fallback
	}
	val, err := strconv.ParseBool(str)
	if err != nil {
		log.Printf("Config: Invalid bool for %s: %v, using default %v", key, err, fallback)
		return fallback
	}
	return val
}

func getEnvSlice(key string, fallback []string) []string {
	str := getEnv(key, "")
	if str == "" {
		return fallback
	}
	// Simple comma-separated parsing
	var result []string
	current := ""
	for _, c := range str {
		if c == ',' {
			if current != "" {
				result = append(result, current)
			}
			current = ""
		} else if c != ' ' {
			current += string(c)
		}
	}
	if current != "" {
		result = append(result, current)
	}
	if len(result) == 0 {
		return fallback
	}
	return result
}
