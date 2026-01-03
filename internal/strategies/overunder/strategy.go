package overunder

import (
	"context"
	"fmt"
	"log"
	"polymarket-bot/config"
	"polymarket-bot/internal/clients/clob"
	"polymarket-bot/internal/clients/gamma"
	"polymarket-bot/internal/risk"
	"strings"
	"sync"
	"time"
)

// Entry thresholds for O/U markets
const (
	MaxOverPrice     = 0.40 // Buy Over if < 40% (tightened from 45%)
	MaxUnderPrice    = 0.40 // Buy Under if < 40% (tightened from 45%)
	MinValidPrice    = 0.05
	MaxSpreadPercent = 0.50
	MaxCombinedPrice = 0.94 // Combined must be < 94% for edge after fees
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
	Market      *gamma.Market
	TokenPair   *gamma.ClobTokenPair
	TotalLine   string // e.g., "220.5", "3.5", etc.
	
	OverBid     float64
	OverAsk     float64
	OverMid     float64
	UnderBid    float64
	UnderAsk    float64
	UnderMid    float64
	
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

	priceTicker := time.NewTicker(time.Duration(s.Config.PriceUpdateInterval) * time.Second)
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

					// Skip if game is too far in the future (>48 hours)
					if !gameTime.IsZero() {
						hoursUntilGame := time.Until(gameTime).Hours()
						if hoursUntilGame > 48 {
							skippedTooFar++
							continue
						}
						// Also skip if game has already started (time < 0)
						if hoursUntilGame < 0 {
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

		s.RiskManager.UpdatePrice(tracked.TokenPair.Yes, overMid)
		s.RiskManager.UpdatePrice(tracked.TokenPair.No, underMid)

		s.checkExitsForDeltaNeutral(tracked, now)

		if s.RiskManager.HasPositionForMarket(marketID) {
			continue
		}

		s.analyzeAndTradeDeltaNeutral(tracked)
	}
}

func (s *Strategy) analyzeAndTradeDeltaNeutral(tracked *TrackedMarket) {
	// Check if we have room for more O/U positions (shares limit with sports: max 25 combined)
	if !s.RiskManager.CanAddPositionForStrategy("overunder") {
		return // Max sports/O/U positions reached
	}

	overInRange := tracked.OverMid >= MinValidPrice && tracked.OverMid <= MaxOverPrice
	underInRange := tracked.UnderMid >= MinValidPrice && tracked.UnderMid <= MaxUnderPrice

	combinedSpread := tracked.OverMid + tracked.UnderMid
	
	// CRITICAL: Combined price must leave room for profit after fees
	hasEdge := combinedSpread < MaxCombinedPrice
	edgePercent := (1.0 - combinedSpread) * 100

	if overInRange && underInRange && hasEdge {
		log.Printf("O/U: 💰 DELTA NEUTRAL OPPORTUNITY")
		log.Printf("  Market: %s", truncateQuestion(tracked.Market.Question))
		log.Printf("  OVER: %.4f mid (ask: %.4f)", tracked.OverMid, tracked.OverAsk)
		log.Printf("  UNDER: %.4f mid (ask: %.4f)", tracked.UnderMid, tracked.UnderAsk)
		log.Printf("  Combined: %.4f (%.2f%% edge, min required: %.0f%%)", combinedSpread, edgePercent, (1.0-MaxCombinedPrice)*100)

		s.executeDeltaNeutralEntry(tracked)
	} else if overInRange && underInRange && !hasEdge {
		log.Printf("O/U: SKIP %s - combined %.4f exceeds max %.4f (only %.2f%% edge)",
			truncateQuestion(tracked.Market.Question), combinedSpread, MaxCombinedPrice, edgePercent)
	}
}

func (s *Strategy) executeDeltaNeutralEntry(tracked *TrackedMarket) {
	maxCost := s.Config.MaxPositionSize
	halfCost := maxCost / 2

	overSize := halfCost / tracked.OverAsk
	underSize := halfCost / tracked.UnderAsk

	totalCost := (tracked.OverAsk * overSize) + (tracked.UnderAsk * underSize)

	if err := s.RiskManager.CheckEntry(tracked.TokenPair.Yes, tracked.OverAsk, overSize); err != nil {
		log.Printf("O/U: Risk check failed: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("O/U: [DRY RUN] DELTA NEUTRAL ENTRY")
		log.Printf("  BUY OVER @ %.4f (size: %.2f, cost: $%.2f)", tracked.OverAsk, overSize, tracked.OverAsk*overSize)
		log.Printf("  BUY UNDER @ %.4f (size: %.2f, cost: $%.2f)", tracked.UnderAsk, underSize, tracked.UnderAsk*underSize)
		log.Printf("  Total cost: $%.2f", totalCost)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tracked.TokenPair.Yes,
			Price:     tracked.OverAsk,
			Size:      overSize,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("O/U: Over order failed: %v", err)
			return
		}

		_, err = s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tracked.TokenPair.No,
			Price:     tracked.UnderAsk,
			Size:      underSize,
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
		Size:         overSize,
		EntryPrice:   tracked.OverMid,
		CurrentPrice: tracked.OverMid,
		Side:         "BUY",
		Type:         risk.TypeDeltaNeutral,
		Strategy:     "overunder",
		TotalCost:    totalCost,
	}
	overID := s.RiskManager.AddPosition(overPos)

	underPos := &risk.Position{
		MarketID:         tracked.Market.ID,
		TokenID:          tracked.TokenPair.No,
		OutcomeName:      fmt.Sprintf("UNDER %s", tracked.TotalLine),
		Size:             underSize,
		EntryPrice:       tracked.UnderMid,
		CurrentPrice:     tracked.UnderMid,
		Side:             "BUY",
		Type:             risk.TypeDeltaNeutral,
		Strategy:         "overunder",
		TotalCost:        totalCost,
		PairedPositionID: overID,
	}
	underID := s.RiskManager.AddPosition(underPos)

	if oPos := s.RiskManager.GetPosition(overID); oPos != nil {
		oPos.PairedPositionID = underID
	}

	tracked.HasPosition = true
	log.Printf("O/U: ✅ Delta neutral position opened - Total cost: $%.2f", totalCost)
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

	for _, pos := range marketPositions {
		if now.Sub(pos.EntryTime) < EntryGracePeriod {
			continue
		}

		var currentPrice float64
		if pos.TokenID == tracked.TokenPair.Yes {
			currentPrice = tracked.OverMid
		} else {
			currentPrice = tracked.UnderMid
		}

		if currentPrice < MinValidPrice {
			continue
		}

		pos.CurrentPrice = currentPrice
		pnlPercent := (currentPrice - pos.EntryPrice) / pos.EntryPrice

		if pnlPercent >= s.Config.TakeProfitPercent {
			log.Printf("O/U: 🎯 TAKE PROFIT - %s @ %.4f (entry: %.4f, +%.1f%%)",
				pos.OutcomeName, currentPrice, pos.EntryPrice, pnlPercent*100)

			s.executeExit(pos)

			if pos.PairedPositionID != "" {
				if pairedPos := s.RiskManager.GetPosition(pos.PairedPositionID); pairedPos != nil {
					if pairedPos.State == risk.StateOpen {
						s.executeExit(pairedPos)
					}
				}
			}
			tracked.HasPosition = false
			return
		}
	}

	// Stop loss check
	for _, pos := range marketPositions {
		if now.Sub(pos.EntryTime) < EntryGracePeriod {
			continue
		}

		var currentPrice float64
		if pos.TokenID == tracked.TokenPair.Yes {
			currentPrice = tracked.OverMid
		} else {
			currentPrice = tracked.UnderMid
		}

		pos.CurrentPrice = currentPrice
		pnlPercent := (currentPrice - pos.EntryPrice) / pos.EntryPrice

		if pnlPercent <= -s.Config.StopLossPercent {
			log.Printf("O/U: 🛑 STOP LOSS - %s @ %.4f (entry: %.4f, %.1f%%)",
				pos.OutcomeName, currentPrice, pos.EntryPrice, pnlPercent*100)
			s.executeExit(pos)

			if pos.PairedPositionID != "" {
				if pairedPos := s.RiskManager.GetPosition(pos.PairedPositionID); pairedPos != nil {
					if pairedPos.State == risk.StateOpen {
						s.executeExit(pairedPos)
					}
				}
			}
			tracked.HasPosition = false
			return
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

func (s *Strategy) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	openPositions := len(s.RiskManager.GetPositionsByStrategy("overunder"))
	return fmt.Sprintf("O/U: %d markets, %d positions", len(s.trackedMarkets), openPositions)
}
