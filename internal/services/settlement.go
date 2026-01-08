package services

import (
	"context"
	"log"
	"polymarket-bot/config"
	"polymarket-bot/internal/clients/clob"
	"polymarket-bot/internal/clients/gamma"
	"polymarket-bot/internal/risk"
	"time"
)

// SettlementService handles market resolution detection and position cleanup
type SettlementService struct {
	Config      *config.Config
	GammaClient *gamma.Client
	ClobClient  *clob.Client
	RiskManager *risk.Manager

	ctx    context.Context
	cancel context.CancelFunc
}

// NewSettlementService creates a new settlement service
func NewSettlementService(cfg *config.Config, g *gamma.Client, c *clob.Client, r *risk.Manager) *SettlementService {
	ctx, cancel := context.WithCancel(context.Background())
	return &SettlementService{
		Config:      cfg,
		GammaClient: g,
		ClobClient:  c,
		RiskManager: r,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Run starts the settlement checking loop
func (s *SettlementService) Run() {
	log.Println("Settlement: Starting settlement service...")

	// Check every 5 minutes for settled markets
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	// Initial check after 1 minute
	time.Sleep(1 * time.Minute)
	s.CheckAndSettlePositions()

	for {
		select {
		case <-s.ctx.Done():
			log.Println("Settlement: Service stopped")
			return
		case <-ticker.C:
			s.CheckAndSettlePositions()
		}
	}
}

// Stop stops the settlement service
func (s *SettlementService) Stop() {
	s.cancel()
}

// CheckAndSettlePositions checks all open positions for market resolution
func (s *SettlementService) CheckAndSettlePositions() {
	positions := s.RiskManager.GetOpenPositions()
	if len(positions) == 0 {
		return
	}

	log.Printf("Settlement: Checking %d positions for resolution...", len(positions))

	// Group positions by market ID to avoid duplicate API calls
	marketPositions := make(map[string][]*risk.Position)
	for _, pos := range positions {
		marketPositions[pos.MarketID] = append(marketPositions[pos.MarketID], pos)
	}

	settledCount := 0
	for marketID, positionsForMarket := range marketPositions {
		// Skip empty market IDs
		if marketID == "" {
			continue
		}

		// Fetch market status from Gamma API
		market, err := s.GammaClient.GetMarketByID(marketID)
		if err != nil {
			log.Printf("Settlement: Failed to fetch market %s: %v", marketID, err)
			continue
		}

		if market == nil {
			log.Printf("Settlement: Market %s not found, marking positions for cleanup", marketID)
			for _, pos := range positionsForMarket {
				s.settlePosition(pos, false, 0)
				settledCount++
			}
			continue
		}

		// Check if market is closed/resolved
		if market.Closed {
			log.Printf("Settlement: Market resolved: %s", truncateQuestion(market.Question))

			// Determine winning outcome
			winningOutcome := s.determineWinningOutcome(market)

			for _, pos := range positionsForMarket {
				// Check if this position won
				won := s.didPositionWin(pos, market, winningOutcome)
				var finalPnL float64
				if won {
					// Winner gets $1.00 per share
					finalPnL = (1.0 - pos.EntryPrice) * pos.Size
				} else {
					// Loser gets $0.00, loses entire stake
					finalPnL = -pos.EntryPrice * pos.Size
				}

				s.settlePosition(pos, won, finalPnL)
				settledCount++

				if won {
					log.Printf("Settlement: ✅ WON %s - P&L: $%.2f", pos.OutcomeName, finalPnL)
				} else {
					log.Printf("Settlement: ❌ LOST %s - P&L: $%.2f", pos.OutcomeName, finalPnL)
				}
			}
		}

		// Also check for stale positions (24+ hours past entry with no price updates)
		for _, pos := range positionsForMarket {
			if time.Since(pos.LastUpdate) > 24*time.Hour {
				log.Printf("Settlement: Stale position detected (no updates for 24h): %s", pos.OutcomeName)
				// Mark as pending settlement - actual settlement will happen when market data available
				if pos.State == risk.StateOpen {
					pos.State = risk.StatePendingExit
				}
			}
		}
	}

	if settledCount > 0 {
		log.Printf("Settlement: Settled %d positions", settledCount)
		log.Printf("Settlement: Realized P&L: $%.2f", s.RiskManager.GetRealizedPnL())
	}
}

// settlePosition marks a position as settled with final P&L
func (s *SettlementService) settlePosition(pos *risk.Position, won bool, finalPnL float64) {
	s.RiskManager.SettlePosition(pos.ID, won, finalPnL)
}

// determineWinningOutcome determines which outcome won based on market data
func (s *SettlementService) determineWinningOutcome(market *gamma.Market) string {
	// Try to parse outcome prices - winner should be at or near $1.00
	prices, err := market.GetOutcomePricesFloat()
	if err != nil || len(prices) < 2 {
		return ""
	}

	// Winner typically has price >= 0.99 (resolved to $1.00)
	if prices[0] >= 0.99 {
		return "YES"
	}
	if len(prices) > 1 && prices[1] >= 0.99 {
		return "NO"
	}

	// Also check outcome names if available
	outcomes, _ := market.GetOutcomeNames()
	for i, price := range prices {
		if price >= 0.99 && i < len(outcomes) {
			return outcomes[i]
		}
	}

	return ""
}

// didPositionWin checks if a position was the winning outcome
func (s *SettlementService) didPositionWin(pos *risk.Position, market *gamma.Market, winningOutcome string) bool {
	if winningOutcome == "" {
		return false
	}

	// Direct match
	if pos.OutcomeName == winningOutcome {
		return true
	}

	// Check by token ID - if position token matches winning outcome index
	tokenPair, err := market.GetClobTokenPair()
	if err != nil {
		return false
	}

	if winningOutcome == "YES" && pos.TokenID == tokenPair.Yes {
		return true
	}
	if winningOutcome == "NO" && pos.TokenID == tokenPair.No {
		return true
	}

	// For sports markets, compare against outcome names
	outcomes, _ := market.GetOutcomeNames()
	for i, outcome := range outcomes {
		if outcome == winningOutcome {
			// Check if position matches this outcome index
			if i == 0 && pos.TokenID == tokenPair.Yes {
				return true
			}
			if i == 1 && pos.TokenID == tokenPair.No {
				return true
			}
		}
	}

	return false
}

func truncateQuestion(q string) string {
	if len(q) > 50 {
		return q[:47] + "..."
	}
	return q
}
