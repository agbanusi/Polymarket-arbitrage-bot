package overunder

import (
	"context"
	"fmt"
	"log"
	"polymarket-bot/config"
	"polymarket-bot/internal/clients/clob"
	"polymarket-bot/internal/clients/gamma"
	"polymarket-bot/internal/risk"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry thresholds for O/U markets
// Profit comes from selling at different times as game total shifts
const (
	MaxOverPrice     = 0.60 // Buy Over if < 60%
	MaxUnderPrice    = 0.60 // Buy Under if < 40%
	MinValidPrice    = 0.05
	MaxSpreadPercent = 1.50
	EntryGracePeriod = 60 * time.Second
)

// Strategy implements the Over/Under delta neutral strategy
type Strategy struct {
	Config      *config.Config
	GammaClient *gamma.Client
	ClobClient  *clob.Client
	RiskManager *risk.Manager

	trackedMarkets map[string]*TrackedMarket
	mu             sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
}

// TrackedMarket represents an O/U market being monitored
type TrackedMarket struct {
	Market    *gamma.Market
	TokenPair *gamma.ClobTokenPair
	TotalLine string // e.g., "220.5", "3.5", etc.

	OverBid  float64
	OverAsk  float64
	OverMid  float64
	UnderBid float64
	UnderAsk float64
	UnderMid float64

	LastUpdate  time.Time
	HasPosition bool
	GameTime    time.Time
}

func NewStrategy(cfg *config.Config, g *gamma.Client, c *clob.Client, r *risk.Manager) *Strategy {
	ctx, cancel := context.WithCancel(context.Background())
	return &Strategy{
		Config:         cfg,
		GammaClient:    g,
		ClobClient:     c,
		RiskManager:    r,
		trackedMarkets: make(map[string]*TrackedMarket),
		ctx:            ctx,
		cancel:         cancel,
	}
}

func (s *Strategy) Run() {
	if !s.Config.SportsEnabled || !s.Config.SportsOUEnabled {
		log.Println("O/U: Strategy disabled")
		return
	}

	log.Println("O/U: Starting OVER/UNDER DELTA NEUTRAL strategy...")
	log.Printf("O/U: Buy Over/Under if < %.0f%%", MaxOverPrice*100)
	log.Printf("O/U: Exit when either side hits %.0f%% profit", s.Config.TakeProfitPercent*100)

	discoveryTicker := time.NewTicker(60 * time.Second)
	defer discoveryTicker.Stop()

	priceTicker := time.NewTicker(1 * time.Second)
	defer priceTicker.Stop()

	s.discoverMarkets()

	for {
		select {
		case <-s.ctx.Done():
			log.Println("O/U: Strategy stopped")
			return
		case <-discoveryTicker.C:
			s.discoverMarkets()
		case <-priceTicker.C:
			s.updatePricesAndTrade()
		}
	}
}

func (s *Strategy) Stop() {
	s.cancel()
}

// isOverUnderMarket checks if this is an O/U market
func (s *Strategy) isOverUnderMarket(market *gamma.Market) bool {
	questionLower := strings.ToLower(market.Question)

	// MUST contain O/U indicators
	ouIndicators := []string{
		"o/u ", "o/u:", "o/u,",
		"over/under", "over under",
		"total points", "total goals", "total runs",
		"combined score",
	}

	hasOU := false
	for _, indicator := range ouIndicators {
		if strings.Contains(questionLower, indicator) {
			hasOU = true
			break
		}
	}

	if !hasOU {
		return false
	}

	// Must also be a game (has vs or @)
	hasVs := strings.Contains(questionLower, " vs ") || strings.Contains(questionLower, " vs. ")
	hasAt := strings.Contains(market.Question, " @ ")

	return hasVs || hasAt
}

// extractTotalLine extracts the O/U line from the question (e.g., "220.5" from "Lakers vs Warriors: O/U 220.5")
func (s *Strategy) extractTotalLine(question string) string {
	questionLower := strings.ToLower(question)

	// Look for pattern after "o/u" or "over/under"
	markers := []string{"o/u ", "over/under ", "total "}
	for _, marker := range markers {
		idx := strings.Index(questionLower, marker)
		if idx >= 0 {
			rest := question[idx+len(marker):]
			// Find the number
			var line strings.Builder
			for _, c := range rest {
				if (c >= '0' && c <= '9') || c == '.' {
					line.WriteRune(c)
				} else if line.Len() > 0 {
					break
				}
			}
			if line.Len() > 0 {
				return line.String()
			}
		}
	}
	return ""
}

func (s *Strategy) discoverMarkets() {
	log.Println("O/U: Discovering Over/Under markets...")

	ouFound := 0
	totalChecked := 0
	skippedTooFar := 0

	for _, tag := range s.Config.SportsTags {
		for page := 0; page < 20; page++ {
			events, err := s.GammaClient.GetEvents(gamma.GetEventsParams{
				Limit:  100,
				Offset: page * 100,
				Tag:    tag,
				Active: true,
				Order:  "volume",
			})
			if err != nil {
				break
			}

			if len(events) == 0 {
				break
			}

			totalChecked += len(events)

			for _, event := range events {
				for _, market := range event.Markets {
					if market.Closed || !market.Active || !market.EnableOrderBook {
						continue
					}

					// Parse game time and filter for games within 48 hours
					var gameTime time.Time
					if market.GameStartTime != "" {
						gameTime, _ = time.Parse(time.RFC3339, market.GameStartTime)
					} else if market.EndDate != "" {
						gameTime, _ = time.Parse(time.RFC3339, market.EndDate)
					}

					// Skip if game is too far in the future (>48 hours) or already ended (>4 hours after start)
					if !gameTime.IsZero() {
						hoursUntilGame := time.Until(gameTime).Hours()
						if hoursUntilGame > 48 {
							skippedTooFar++
							continue
						}
						// Skip if game likely ended (4+ hours after start)
						// Keep live games (0 to -4 hours) to enable in-game trading
						if hoursUntilGame < -4 {
							continue
						}
					}

					s.mu.RLock()
					_, exists := s.trackedMarkets[market.ID]
					s.mu.RUnlock()
					if exists {
						continue
					}

					if !s.isOverUnderMarket(&market) {
						continue
					}

					tokenPair, err := market.GetClobTokenPair()
					if err != nil {
						continue
					}

					totalLine := s.extractTotalLine(market.Question)

					s.mu.Lock()
					s.trackedMarkets[market.ID] = &TrackedMarket{
						Market:    &market,
						TokenPair: tokenPair,
						TotalLine: totalLine,
						GameTime:  gameTime,
					}
					s.mu.Unlock()

					ouFound++
					timeStr := "unknown"
					if !gameTime.IsZero() {
						timeStr = fmt.Sprintf("%.1fh", time.Until(gameTime).Hours())
					}
					log.Printf("O/U: 📊 TOTAL [%s]: %s (line: %s)", timeStr, truncateQuestion(market.Question), totalLine)
				}
			}
		}
	}

	s.mu.RLock()
	log.Printf("O/U: Found %d O/U markets, tracking %d total", ouFound, len(s.trackedMarkets))
	s.mu.RUnlock()
}

func (s *Strategy) fetchMarketPrices(tokenID string) (float64, float64, float64, bool) {
	book, err := s.ClobClient.GetOrderBook(tokenID)
	if err != nil {
		return 0, 0, 0, false
	}

	var bestBid, bestAsk float64

	// API returns bids sorted from WORST to BEST (low to high)
	// So best bid is the LAST element (highest price someone will pay)
	if len(book.Bids) > 0 {
		lastIdx := len(book.Bids) - 1
		fmt.Sscanf(book.Bids[lastIdx].Price, "%f", &bestBid)
	}
	// API returns asks sorted from WORST to BEST (high to low)
	// So best ask is the LAST element (lowest price someone will sell)
	if len(book.Asks) > 0 {
		lastIdx := len(book.Asks) - 1
		fmt.Sscanf(book.Asks[lastIdx].Price, "%f", &bestAsk)
	}

	if bestBid < MinValidPrice || bestAsk < MinValidPrice {
		return 0, 0, 0, false
	}
	if bestAsk <= bestBid {
		return 0, 0, 0, false
	}

	spread := (bestAsk - bestBid) / bestAsk
	if spread > MaxSpreadPercent {
		return 0, 0, 0, false
	}

	midpoint := (bestBid + bestAsk) / 2
	return bestBid, bestAsk, midpoint, true
}

func (s *Strategy) updatePricesAndTrade() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	for marketID, tracked := range s.trackedMarkets {
		if tracked.Market.Closed {
			delete(s.trackedMarkets, marketID)
			continue
		}

		if !tracked.GameTime.IsZero() && now.After(tracked.GameTime.Add(6*time.Hour)) {
			delete(s.trackedMarkets, marketID)
			continue
		}

		// For O/U markets: Yes = Over, No = Under
		overBid, overAsk, overMid, overValid := s.fetchMarketPrices(tracked.TokenPair.Yes)
		underBid, underAsk, underMid, underValid := s.fetchMarketPrices(tracked.TokenPair.No)

		if !overValid || !underValid {
			continue
		}

		tracked.OverBid = overBid
		tracked.OverAsk = overAsk
		tracked.OverMid = overMid
		tracked.UnderBid = underBid
		tracked.UnderAsk = underAsk
		tracked.UnderMid = underMid
		tracked.LastUpdate = now

		// Update risk manager with prices for existing positions (using BID for exit valuation)
		s.RiskManager.UpdatePrice(tracked.TokenPair.Yes, overBid)
		s.RiskManager.UpdatePrice(tracked.TokenPair.No, underBid)

		// Check for exits (TP, SL, or SCRAPE)
		s.checkExitsForDeltaNeutral(tracked, now)

		if s.RiskManager.HasPositionForMarket(marketID) {
			continue
		}
	}

	// === PRIORITY RANKING FOR ENTRIES ===
	// We want to sort markets so we check the most profitable ones first
	var rankedMarkets []*TrackedMarket

	for _, tracked := range s.trackedMarkets {
		if tracked.Market.Closed || (!tracked.GameTime.IsZero() && now.After(tracked.GameTime.Add(6*time.Hour))) {
			continue // Already handled in the cleanup loop
		}

		// Only consider markets where we have valid ask prices
		if tracked.OverAsk > 0 && tracked.UnderAsk > 0 {
			rankedMarkets = append(rankedMarkets, tracked)
		}
	}

	// Sort explicitly by potential arbitrage spread (OverAsk + UnderAsk) ascending
	// Smaller combinedAsk = better arbitrage spread
	sort.Slice(rankedMarkets, func(i, j int) bool {
		askI := rankedMarkets[i].OverAsk + rankedMarkets[i].UnderAsk
		askJ := rankedMarkets[j].OverAsk + rankedMarkets[j].UnderAsk
		return askI < askJ
	})

	// Now try to enter trades in order of profitability
	for _, tracked := range rankedMarkets {
		s.analyzeAndTradeDeltaNeutral(tracked)
	}
}

func (s *Strategy) analyzeAndTradeDeltaNeutral(tracked *TrackedMarket) {
	// Check if we have room for more O/U positions
	if !s.RiskManager.CanAddPositionForStrategy("overunder") {
		return
	}

	overAsk, underAsk := tracked.OverAsk, tracked.UnderAsk

	hasOver := len(s.RiskManager.GetPositionByToken(tracked.TokenPair.Yes)) > 0
	hasUnder := len(s.RiskManager.GetPositionByToken(tracked.TokenPair.No)) > 0

	// If we already hold both, nothing to do
	if hasOver && hasUnder {
		return
	}

	// === 1. SIMULTANEOUS SPREAD ARBITRAGE (PRIORITY) ===
	combinedAsk := overAsk + underAsk
	maxSpread := 0.98 // Allow 2% room for fees

	if combinedAsk > 0 && combinedAsk < maxSpread && !hasOver && !hasUnder {
		log.Printf("O/U: 💰 SPREAD ARB FOUND - Market: %s (OverAsk: %.4f + UnderAsk: %.4f = %.4f)",
			truncateQuestion(tracked.Market.Question), overAsk, underAsk, combinedAsk)
		s.executeSpreadArb(tracked)
		return
	}

	// === 2. SECOND LEG / HEDGE ===
	if hasOver && !hasUnder {
		overPos := s.RiskManager.GetPositionByToken(tracked.TokenPair.Yes)[0]
		if overPos.EntryPrice+underAsk <= maxSpread {
			log.Printf("O/U: 🍖 HEDGE ARB - %s (Over @ %.4f + Under Ask @ %.4f = %.4f)",
				tracked.TotalLine, overPos.EntryPrice, underAsk, overPos.EntryPrice+underAsk)
			s.executeSecondLeg(tracked, "UNDER", tracked.TokenPair.No, underAsk, tracked.UnderMid)
		}
		return
	}

	if hasUnder && !hasOver {
		underPos := s.RiskManager.GetPositionByToken(tracked.TokenPair.No)[0]
		if underPos.EntryPrice+overAsk <= maxSpread {
			log.Printf("O/U: 🍖 HEDGE ARB - %s (Under @ %.4f + Over Ask @ %.4f = %.4f)",
				tracked.TotalLine, underPos.EntryPrice, overAsk, underPos.EntryPrice+overAsk)
			s.executeSecondLeg(tracked, "OVER", tracked.TokenPair.Yes, overAsk, tracked.OverMid)
		}
		return
	}

	// === 3. FIRST LEG / ENTRY ===
	// Only enter if we hold nothing and find a good value
	if !hasOver && !hasUnder {
		// Prefer the cheaper side that meets thresholds
		if overAsk <= MaxOverPrice && overAsk < underAsk {
			log.Printf("O/U: 🎯 ENTRY OVER - %s @ %.4f (mid: %.4f)", tracked.TotalLine, overAsk, tracked.OverMid)
			s.executeFirstLeg(tracked, "OVER", tracked.TokenPair.Yes, overAsk, tracked.OverMid)
		} else if underAsk <= MaxUnderPrice {
			log.Printf("O/U: 🎯 ENTRY UNDER - %s @ %.4f (mid: %.4f)", tracked.TotalLine, underAsk, tracked.UnderMid)
			s.executeFirstLeg(tracked, "UNDER", tracked.TokenPair.No, underAsk, tracked.UnderMid)
		}
	}
}

// executeSpreadArb executes a full spread arbitrage (both legs immediately)
// Uses EQUAL SHARE QUANTITIES to ensure guaranteed risk-free profit
func (s *Strategy) executeSpreadArb(tracked *TrackedMarket) {
	overPrice := tracked.OverAsk
	underPrice := tracked.UnderAsk

	// Calculate sizes - EQUAL SHARE QUANTITIES
	maxCost := s.Config.MaxPositionSize
	halfCost := maxCost / 2

	overSharesMax := halfCost / overPrice
	underSharesMax := halfCost / underPrice

	shareQty := overSharesMax
	if underSharesMax < overSharesMax {
		shareQty = underSharesMax
	}

	overCost := shareQty * overPrice
	underCost := shareQty * underPrice
	totalCost := overCost + underCost

	// Payout is exactly 1 share size, since exactly one side wins
	potentialPayout := shareQty
	fees := totalCost * s.Config.TradingFeePercent
	guaranteedProfit := potentialPayout - totalCost - fees
	profitPercent := guaranteedProfit / totalCost

	if profitPercent < s.Config.MinProfitAfterFees {
		log.Printf("O/U: SKIP spread arb - profit %.2f%% below min %.2f%% (fees: $%.4f)",
			profitPercent*100, s.Config.MinProfitAfterFees*100, fees)
		return
	}

	if guaranteedProfit <= 0 {
		return
	}

	// Risk check
	if err := s.RiskManager.CheckEntry(tracked.TokenPair.Yes, overPrice, shareQty); err != nil {
		log.Printf("O/U: Risk check failed: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("O/U: [DRY RUN] SPREAD ARB (Equal shares: %.2f)", shareQty)
		log.Printf("  OVER @ %.4f × %.2f shares = $%.2f", overPrice, shareQty, overCost)
		log.Printf("  UNDER @ %.4f × %.2f shares = $%.2f", underPrice, shareQty, underCost)
		log.Printf("  Total cost: $%.2f, Guaranteed profit: $%.4f (%.2f%%)",
			totalCost, guaranteedProfit, profitPercent*100)
	} else {
		// Execute both orders
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tracked.TokenPair.Yes,
			Price:     overPrice,
			Size:      shareQty,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("O/U: Over order failed: %v", err)
			return
		}

		_, err = s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tracked.TokenPair.No,
			Price:     underPrice,
			Size:      shareQty,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("O/U: Under order failed: %v", err)
			return
		}
	}

	overPos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      tracked.TokenPair.Yes,
		OutcomeName:  fmt.Sprintf("OVER %s", tracked.TotalLine),
		Size:         shareQty,
		EntryPrice:   overPrice,
		CurrentPrice: overPrice,
		Side:         "BUY",
		Type:         risk.TypeArbitrage,
		Strategy:     "overunder",
		TotalCost:    overCost,
	}
	overID := s.RiskManager.AddPosition(overPos)

	underPos := &risk.Position{
		MarketID:         tracked.Market.ID,
		TokenID:          tracked.TokenPair.No,
		OutcomeName:      fmt.Sprintf("UNDER %s", tracked.TotalLine),
		Size:             shareQty,
		EntryPrice:       underPrice,
		CurrentPrice:     underPrice,
		Side:             "BUY",
		Type:             risk.TypeArbitrage,
		Strategy:         "overunder",
		TotalCost:        underCost,
		PairedPositionID: overID,
	}
	underID := s.RiskManager.AddPosition(underPos)

	if oPos := s.RiskManager.GetPosition(overID); oPos != nil {
		oPos.PairedPositionID = underID
	}

	tracked.HasPosition = true
	log.Printf("O/U: ✅ Spread arb position opened - Guaranteed profit: $%.4f", guaranteedProfit)
}

func (s *Strategy) executeFirstLeg(tracked *TrackedMarket, sideName string, tokenID string, ask float64, mid float64) {
	maxCost := s.Config.MaxPositionSize / 2 // Reserve half for second leg
	size := maxCost / ask
	totalCost := size * ask

	if err := s.RiskManager.CheckEntry(tokenID, ask, size); err != nil {
		log.Printf("O/U: Risk check failed: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("O/U: [DRY RUN] LEG-IN ENTRY")
		log.Printf("  BUY %s @ %.4f (size: %.2f, cost: $%.2f)", sideName, ask, size, totalCost)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tokenID,
			Price:     ask,
			Size:      size,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("O/U: %s order failed: %v", sideName, err)
			return
		}
	}

	pos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      tokenID,
		OutcomeName:  fmt.Sprintf("%s %s", sideName, tracked.TotalLine),
		Size:         size,
		EntryPrice:   mid,
		CurrentPrice: mid,
		Side:         "BUY",
		Type:         risk.TypeValueBet,
		Strategy:     "overunder",
		TotalCost:    totalCost,
	}
	s.RiskManager.AddPosition(pos)

	tracked.HasPosition = true
	log.Printf("O/U: ✅ Single-side position opened - Cost: $%.2f", totalCost)
}

func (s *Strategy) executeSecondLeg(tracked *TrackedMarket, sideName string, tokenID string, ask float64, mid float64) {
	maxCost := s.Config.MaxPositionSize / 2
	size := maxCost / ask
	totalCost := size * ask

	if err := s.RiskManager.CheckEntry(tokenID, ask, size); err != nil {
		log.Printf("O/U: Risk check failed for second leg: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("O/U: [DRY RUN] HEDGE ARB ENTRY")
		log.Printf("  BUY %s @ %.4f (size: %.2f, cost: $%.2f)", sideName, ask, size, totalCost)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tokenID,
			Price:     ask,
			Size:      size,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("O/U: Second leg order failed: %v", err)
			return
		}
	}

	pos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      tokenID,
		OutcomeName:  fmt.Sprintf("%s %s", sideName, tracked.TotalLine),
		Size:         size,
		EntryPrice:   mid,
		CurrentPrice: mid,
		Side:         "BUY",
		Type:         risk.TypeArbitrage, // It's complete risk-free arb now
		Strategy:     "overunder",
		TotalCost:    totalCost,
	}
	newID := s.RiskManager.AddPosition(pos)

	// Link positions
	var otherSideToken string
	if tokenID == tracked.TokenPair.Yes {
		otherSideToken = tracked.TokenPair.No
	} else {
		otherSideToken = tracked.TokenPair.Yes
	}

	if existingPositions := s.RiskManager.GetPositionByToken(otherSideToken); len(existingPositions) > 0 {
		existing := existingPositions[0]
		existing.PairedPositionID = newID

		if newPos := s.RiskManager.GetPosition(newID); newPos != nil {
			newPos.PairedPositionID = existing.ID
		}

		// Upgrade existing to Arbitrage
		existing.Type = risk.TypeArbitrage
	}

	log.Printf("O/U: ✅ Second leg (HEDGE) filled - Arb secured!")
}

func (s *Strategy) checkExitsForDeltaNeutral(tracked *TrackedMarket, now time.Time) {
	positions := s.RiskManager.GetPositionsByStrategy("overunder")

	var marketPositions []*risk.Position
	for _, pos := range positions {
		if pos.MarketID == tracked.Market.ID && pos.State == risk.StateOpen {
			marketPositions = append(marketPositions, pos)
		}
	}

	if len(marketPositions) == 0 {
		return
	}

	// 1. SCRAPE CENTS LOGIC (Guaranteed Profit)
	// If we hold both sides (hedged), check if we can exit for combined profit
	if len(marketPositions) >= 2 {
		totalCost, totalValue := 0.0, 0.0
		for _, pos := range marketPositions {
			totalCost += pos.EntryPrice * pos.Size
			// Value based on BID (what we can sell at)
			if pos.TokenID == tracked.TokenPair.Yes {
				totalValue += tracked.OverBid * pos.Size
			} else {
				totalValue += tracked.UnderBid * pos.Size
			}
		}

		profitPercent := (totalValue - totalCost) / totalCost
		// If guaranteed profit exceeds 2%, take it and "scrape"
		if profitPercent >= 0.02 {
			log.Printf("O/U: 🤏 SCRAPE CENTS - Combined profit @ %.1f%%, closing all", profitPercent*100)
			for _, pos := range marketPositions {
				s.executeExit(pos)
			}
			tracked.HasPosition = false
			return
		}
	}

	// 2. INDEPENDENT MONITORING (Target 95%+, SL, TP)
	for _, pos := range marketPositions {
		if now.Sub(pos.EntryTime) < EntryGracePeriod {
			continue
		}

		var currentExitPrice float64
		if pos.TokenID == tracked.TokenPair.Yes {
			currentExitPrice = tracked.OverBid
		} else {
			currentExitPrice = tracked.UnderBid
		}

		pos.CurrentPrice = currentExitPrice
		pnlPercent := (currentExitPrice - pos.EntryPrice) / pos.EntryPrice

		// TAKE PROFIT (Target 95%+)
		if currentExitPrice >= 0.95 || pnlPercent >= s.Config.TakeProfitPercent {
			log.Printf("O/U: 🎯 EXIT TARGET - %s @ %.4f (entry: %.4f, +%.1f%%)",
				pos.OutcomeName, currentExitPrice, pos.EntryPrice, pnlPercent*100)
			s.executeExit(pos)

			// Close paired side too to capture the gain
			for _, other := range marketPositions {
				if other.ID != pos.ID && other.State == risk.StateOpen {
					s.executeExit(other)
				}
			}
			tracked.HasPosition = false
			return
		}

		// STOP LOSS (Independent)
		if pnlPercent <= -s.Config.StopLossPercent {
			log.Printf("O/U: 🛑 STOP LOSS - %s @ %.4f (entry: %.4f, %.1f%%)",
				pos.OutcomeName, currentExitPrice, pos.EntryPrice, pnlPercent*100)
			s.executeExit(pos)
			// Paired position continues independently
		}
	}
}

func (s *Strategy) executeExit(pos *risk.Position) {
	// Use bid price for exits (what buyers are willing to pay), not midpoint
	bidPrice := pos.CurrentPrice // fallback
	if book, err := s.ClobClient.GetOrderBookWithPrices(pos.TokenID); err == nil && book.BestBid > 0 {
		bidPrice = book.BestBid
	}

	if s.Config.IsDryRun() {
		pnl := (bidPrice - pos.EntryPrice) * pos.Size
		log.Printf("O/U: [DRY RUN] Would SELL %s @ %.4f (bid, P&L: $%.2f)",
			pos.OutcomeName, bidPrice, pnl)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   pos.TokenID,
			Price:     bidPrice,
			Size:      pos.Size,
			Side:      clob.Sell,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("O/U: Exit failed: %v", err)
			return
		}
	}

	s.RiskManager.ClosePosition(pos.ID)
}

func truncateQuestion(q string) string {
	if len(q) > 60 {
		return q[:57] + "..."
	}
	return q
}
