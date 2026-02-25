package copytrading

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"polymarket-bot/config"
	"polymarket-bot/internal/clients/clob"
	"polymarket-bot/internal/clients/gamma"
	"polymarket-bot/internal/risk"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Strategy implements the Copy Trading strategy
type Strategy struct {
	Config      *config.Config
	GammaClient *gamma.Client
	ClobClient  *clob.Client
	RiskManager *risk.Manager

	trackedWallets  map[string]*TrackedWallet // Address -> Wallet Info
	positionWallets map[string]string         // PositionID -> WalletAddress
	mu              sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
}

// TrackedWallet tracks leaderboards wallets
type TrackedWallet struct {
	Address           string
	Category          string
	Rank              int
	ConsecutiveLosses int
	Blacklisted       bool
}

// DataAPITrade represents a trade from the Polymarket Data API
type DataAPITrade struct {
	ProxyWallet     string  `json:"proxyWallet"`
	Side            string  `json:"side"`  // "BUY" or "SELL"
	Asset           string  `json:"asset"` // Token ID
	ConditionID     string  `json:"conditionId"`
	Size            float64 `json:"size"`
	Price           float64 `json:"price"`
	Timestamp       int64   `json:"timestamp"`
	Title           string  `json:"title"`
	Outcome         string  `json:"outcome"`
	OutcomeIndex    int     `json:"outcomeIndex"`
	TransactionHash string  `json:"transactionHash"`
}

// LeaderboardEntry represents a user on the leaderboard
type LeaderboardEntry struct {
	Rank        string  `json:"rank"`
	ProxyWallet string  `json:"proxyWallet"`
	UserName    string  `json:"userName"`
	Vol         float64 `json:"vol"`
	PnL         float64 `json:"pnl"`
}

// NewStrategy creates a new copy trading strategy
func NewStrategy(cfg *config.Config, g *gamma.Client, c *clob.Client, r *risk.Manager) *Strategy {
	ctx, cancel := context.WithCancel(context.Background())
	return &Strategy{
		Config:          cfg,
		GammaClient:     g,
		ClobClient:      c,
		RiskManager:     r,
		trackedWallets:  make(map[string]*TrackedWallet),
		positionWallets: make(map[string]string),
		ctx:             ctx,
		cancel:          cancel,
	}
}

// Run starts the strategy
func (s *Strategy) Run() {
	if !s.Config.CopyTradingEnabled {
		log.Println("CopyTrading: Strategy disabled")
		return
	}

	log.Println("CopyTrading: Starting strategy...")
	log.Printf("CopyTrading: Max Positions: %d, Min Trade: $%.2f, Max Slippage: %.1f%%",
		s.Config.CopyTradingMaxPositions, s.Config.CopyTradingMinTradeSize, s.Config.CopyTradingMaxSlippage*100)

	// Refresh wallets daily
	walletTicker := time.NewTicker(24 * time.Hour)
	defer walletTicker.Stop()

	// Poll global trades every 3 seconds
	tradeTicker := time.NewTicker(3 * time.Second)
	defer tradeTicker.Stop()

	// Reconcile positions every 10 seconds
	reconcileTicker := time.NewTicker(10 * time.Second)
	defer reconcileTicker.Stop()

	// Initial fetch
	s.refreshWallets()

	for {
		select {
		case <-s.ctx.Done():
			log.Println("CopyTrading: Strategy stopped")
			return
		case <-walletTicker.C:
			s.refreshWallets()
		case <-tradeTicker.C:
			s.pollAndExecuteTrades()
		case <-reconcileTicker.C:
			s.reconcilePositions()
		}
	}
}

// Stop stops the strategy
func (s *Strategy) Stop() {
	s.cancel()
}

// refreshWallets fetches the leaderboards and updates the tracked wallets
func (s *Strategy) refreshWallets() {
	log.Println("CopyTrading: Fetching top wallets from leaderboards...")

	categories := []string{"sports", "crypto"}
	newTracked := make(map[string]*TrackedWallet)

	// Keep existing blacklisted/loss info if we already track them
	s.mu.RLock()
	existingWallets := make(map[string]*TrackedWallet)
	for k, v := range s.trackedWallets {
		existingWallets[k] = v
	}
	s.mu.RUnlock()

	for _, category := range categories {
		leaders, err := s.fetchLeaderboard(category, 50) // Fetch top 50 to have buffer
		if err != nil {
			log.Printf("CopyTrading: Failed to fetch %s leaderboard: %v", category, err)
			continue
		}

		addedCount := 0
		for _, leader := range leaders {
			rankStr := leader.Rank
			rank, _ := strconv.Atoi(rankStr)

			if rank <= 5 {
				continue // Skip top 5 as requested
			}

			wallet := leader.ProxyWallet
			if wallet == "" {
				continue
			}

			// Check if already tracked and blacklisted
			if existing, ok := existingWallets[wallet]; ok {
				if existing.Blacklisted {
					continue
				}
				// Keep existing stats but update rank
				existing.Rank = rank
				newTracked[wallet] = existing
				addedCount++
			} else {
				newTracked[wallet] = &TrackedWallet{
					Address:           wallet,
					Category:          category,
					Rank:              rank,
					ConsecutiveLosses: 0,
					Blacklisted:       false,
				}
				addedCount++
			}

			// We only want 20 valid wallets per category
			if addedCount >= 20 {
				break
			}
		}
		log.Printf("CopyTrading: Tracking %d wallets for %s", addedCount, category)
	}

	s.mu.Lock()
	s.trackedWallets = newTracked
	s.mu.Unlock()
}

func (s *Strategy) fetchLeaderboard(category string, limit int) ([]LeaderboardEntry, error) {
	endpoint := fmt.Sprintf("https://data-api.polymarket.com/v1/leaderboard?category=%s&limit=%d", category, limit)
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var entries []LeaderboardEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, err
	}

	return entries, nil
}

// pollAndExecuteTrades queries the Data API for recent trades
func (s *Strategy) pollAndExecuteTrades() {
	// 1. Fetch recent global trades from the Data API
	// Using a small limit since we poll frequently
	endpoint := "https://data-api.polymarket.com/trades?limit=50"
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	var trades []DataAPITrade
	if err := json.NewDecoder(resp.Body).Decode(&trades); err != nil {
		return
	}

	// 2. Filter trades by tracked wallets
	var validTrades []DataAPITrade

	s.mu.RLock()
	for _, t := range trades {
		// Only copy BUYs for simplicity (or value bets) to start.
		// Exiting someone else's sell is harder without knowing if they are hedging or not.
		if t.Side != "BUY" {
			continue
		}

		// Filter out small "dust" trades
		tradeValue := t.Size * t.Price
		if tradeValue < s.Config.CopyTradingMinTradeSize {
			continue
		}

		wallet, tracked := s.trackedWallets[t.ProxyWallet]
		if tracked && !wallet.Blacklisted {
			// Check if we haven't processed this transaction recently (to avoid dupes within the short window)
			// A simple timeframe check: only act on trades in the last 60 seconds
			if time.Now().Unix()-t.Timestamp < 60 {
				validTrades = append(validTrades, t)
			}
		}
	}
	s.mu.RUnlock()

	if len(validTrades) == 0 {
		return
	}

	// 3. Sort by rank to prioritize higher ranked wallets if multiple trades occur
	sort.Slice(validTrades, func(i, j int) bool {
		s.mu.RLock()
		defer s.mu.RUnlock()
		w1 := s.trackedWallets[validTrades[i].ProxyWallet]
		w2 := s.trackedWallets[validTrades[j].ProxyWallet]
		if w1 != nil && w2 != nil {
			return w1.Rank < w2.Rank // Lower rank number is better
		}
		return false
	})

	// 4. Execute trades safely
	for _, trade := range validTrades {
		s.executeCopyTrade(trade)
	}
}

func (s *Strategy) executeCopyTrade(trade DataAPITrade) {
	// Check if we already have a position in this market/token for this strategy
	// We'll use the MarketID derived from the Gamma API later, but first check risk manager
	if !s.RiskManager.CanAddPositionForStrategy("copytrading") {
		return
	}

	// We need to fetch the current price from the CLOB to ensure no massive slippage
	// The copy trade happened at `trade.Price`. We want to buy at current CLOB Ask.
	askPrice, err := s.ClobClient.GetBestAsk(trade.Asset)
	if err != nil || askPrice <= 0 {
		return
	}

	// Calculate slippage from the copied trade
	slippage := (askPrice - trade.Price) / trade.Price
	if slippage > s.Config.CopyTradingMaxSlippage {
		log.Printf("CopyTrading: Skipped trade due to slippage. Copied at %.4f, current Ask %.4f (Slippage: %.2f%%)",
			trade.Price, askPrice, slippage*100)
		return
	}

	// Allocate portion of MaxPositionSize
	cost := s.Config.MaxPositionSize
	size := cost / askPrice

	// Risk check
	if err := s.RiskManager.CheckEntry(trade.Asset, askPrice, size); err != nil {
		log.Printf("CopyTrading: Risk check failed: %v", err)
		return
	}

	// Grab the wallet info for logging
	s.mu.RLock()
	wallet := s.trackedWallets[trade.ProxyWallet]
	s.mu.RUnlock()

	if s.Config.IsDryRun() {
		log.Printf("CopyTrading: [DRY RUN] COPY BUY - Wallet Rank %d (%s)", wallet.Rank, wallet.Category)
		log.Printf("  %s %s @ %.4f (Cost: $%.2f)", trade.Outcome, trade.Title, askPrice, cost)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   trade.Asset,
			Price:     askPrice,
			Size:      size,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("CopyTrading: Order failed: %v", err)
			return
		}
	}

	// Store in Risk Manager
	pos := &risk.Position{
		MarketID:     trade.Title, // We don't have Market ID easily accessible from just trades API, using Title temporarily for logs
		TokenID:      trade.Asset,
		OutcomeName:  trade.Outcome,
		Size:         size,
		EntryPrice:   askPrice,
		CurrentPrice: askPrice,
		Side:         "BUY",
		Type:         risk.TypeValueBet,
		Strategy:     "copytrading",
		TotalCost:    cost,
	}

	posID := s.RiskManager.AddPosition(pos)

	// Link position to wallet for penalization logic
	s.mu.Lock()
	s.positionWallets[posID] = trade.ProxyWallet
	s.mu.Unlock()

	log.Printf("CopyTrading: ✅ Copied Wallet Rank %d - %s %s @ %.4f", wallet.Rank, trade.Outcome, trade.Title, askPrice)
}

// reconcilePositions checks for closed CopyTrading positions to penalize wallets for losses
func (s *Strategy) reconcilePositions() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check RiskManager realized PnLs (in a real setup we'd probably have an event channel or check closed state)
	// For now, we will inspect positionWallets map against active positions.
	// If a positionID is no longer active in RiskManager, it means it was closed/stopped out.
	// We then need to penalize/reward the wallet.

	// This relies on the RiskManager exposing a way to get a closed position, or we verify its PnL.
	// We'll iterate through our locally tracked positionWallets and see if they are still open.

	// To perform this robustly without changing risk manager deeply:
	// The risk manager could expose a GetClosedPosition(id).
	// For simplicity, we assume we check `RiskManager.GetPosition` and if it returns `StateClosed`, we process it.

	for posID, walletAddress := range s.positionWallets {
		pos := s.RiskManager.GetPosition(posID)
		if pos == nil {
			// Position missing, clean up map to prevent leak
			delete(s.positionWallets, posID)
			continue
		}

		if pos.State == risk.StateClosed {
			wallet, exists := s.trackedWallets[walletAddress]
			if !exists || wallet.Blacklisted {
				delete(s.positionWallets, posID)
				continue
			}

			// PnL analysis
			if pos.FinalPnL < 0 {
				wallet.ConsecutiveLosses++
				log.Printf("CopyTrading: Wallet %s (Rank %d) took a loss. Consecutive: %d/5",
					walletAddress, wallet.Rank, wallet.ConsecutiveLosses)

				if wallet.ConsecutiveLosses >= 5 {
					wallet.Blacklisted = true
					log.Printf("CopyTrading: 🛑 BLACKLISTED Wallet %s (5 consecutive losses)", walletAddress)
					// Trigger a background refresh to replace the slot
					go s.refreshWallets()
				}
			} else if pos.FinalPnL > 0 {
				wallet.ConsecutiveLosses = 0 // Reset
				log.Printf("CopyTrading: Wallet %s (Rank %d) won. Resetting loss counter to 0.", walletAddress, wallet.Rank)
			}

			// Clean up our map since it's processed
			delete(s.positionWallets, posID)
		}
	}
}

// GetStatus returns the current status of the copy trading strategy
func (s *Strategy) GetStatus() string {
	if !s.Config.CopyTradingEnabled {
		return "📋 copytrading: Disabled"
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	trackedCount := len(s.trackedWallets)
	blacklistedCount := 0
	for _, w := range s.trackedWallets {
		if w.Blacklisted {
			blacklistedCount++
		}
	}
	active := trackedCount - blacklistedCount

	return fmt.Sprintf("📋 copytrading: Active | Tracking %d wallets (%d blacklisted)", active, blacklistedCount)
}
