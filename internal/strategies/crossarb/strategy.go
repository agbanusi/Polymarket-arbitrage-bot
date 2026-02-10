package crossarb

import (
	"context"
	"fmt"
	"log"
	"math"
	"polymarket-bot/config"
	"polymarket-bot/internal/clients/clob"
	"polymarket-bot/internal/clients/gamma"
	"polymarket-bot/internal/clients/kalshi"
	"polymarket-bot/internal/matcher"
	"polymarket-bot/internal/risk"
	"sync"
	"time"
)

const (
	// MinProfitThreshold is the minimum profit per contract (1 cent)
	MinProfitThreshold = 0.01
	// MaxCombinedCost is the maximum combined cost for both sides (99 cents)
	MaxCombinedCost = 0.99
	// EntryGracePeriod before stop loss/take profit can trigger
	EntryGracePeriod = 60 * time.Second
)

// ArbType represents the type of arbitrage opportunity
type ArbType string

const (
	PolyYesKalshiNo  ArbType = "poly_yes_kalshi_no"
	KalshiYesPolyNo  ArbType = "kalshi_yes_poly_no"
	PolySameMarket   ArbType = "poly_same_market"
	KalshiSameMarket ArbType = "kalshi_same_market"
)

// Strategy implements cross-platform arbitrage between Polymarket and Kalshi
type Strategy struct {
	Config       *config.Config
	GammaClient  *gamma.Client
	ClobClient   *clob.Client
	KalshiClient *kalshi.Client
	RiskManager  *risk.Manager
	Matcher      *matcher.Matcher

	// Tracked market pairs
	trackedPairs map[string]*TrackedPair
	mu           sync.RWMutex

	ctx    context.Context
	cancel context.CancelFunc
}

// TrackedPair represents a matched market pair being monitored
type TrackedPair struct {
	Pair *matcher.MarketPair

	// Polymarket prices
	PolyYesBid float64
	PolyYesAsk float64
	PolyNoBid  float64
	PolyNoAsk  float64

	// Kalshi prices
	KalshiYesBid float64
	KalshiYesAsk float64
	KalshiNoBid  float64
	KalshiNoAsk  float64

	LastUpdate time.Time

	// Position tracking per arb type
	HasPolyYesKalshiNo  bool
	HasKalshiYesPolyNo  bool
	HasPolySameMarket   bool
	HasKalshiSameMarket bool
}

// ArbOpportunity represents a detected arbitrage opportunity
type ArbOpportunity struct {
	Type ArbType
	Pair *TrackedPair

	// Leg 1 (what to buy first)
	Leg1Platform string // "poly" or "kalshi"
	Leg1Side     string // "yes" or "no"
	Leg1Price    float64
	Leg1TokenID  string // For Polymarket
	Leg1Ticker   string // For Kalshi

	// Leg 2 (what to buy second)
	Leg2Platform string
	Leg2Side     string
	Leg2Price    float64
	Leg2TokenID  string
	Leg2Ticker   string

	// Profit calculation
	CombinedCost   float64
	KalshiFee      float64
	ExpectedProfit float64
	ProfitPercent  float64
}

// NewStrategy creates a new cross-arbitrage strategy
func NewStrategy(cfg *config.Config, g *gamma.Client, c *clob.Client, k *kalshi.Client, r *risk.Manager) *Strategy {
	ctx, cancel := context.WithCancel(context.Background())

	m := matcher.NewMatcher(g, k)

	return &Strategy{
		Config:       cfg,
		GammaClient:  g,
		ClobClient:   c,
		KalshiClient: k,
		RiskManager:  r,
		Matcher:      m,
		trackedPairs: make(map[string]*TrackedPair),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Run starts the cross-arbitrage strategy
func (s *Strategy) Run() {
	if !s.Config.CrossArbEnabled {
		log.Println("CrossArb: Strategy disabled")
		return
	}

	// Check Kalshi connectivity
	if !s.KalshiClient.IsConnected() {
		log.Println("CrossArb: WARNING - Cannot connect to Kalshi API, strategy may have limited functionality")
	} else {
		log.Println("CrossArb: Successfully connected to Kalshi API")
	}

	// Warn if credentials are missing in LIVE mode
	if s.Config.Mode == "live" && (s.Config.KalshiAPIKey == "" || s.Config.KalshiPrivateKey == "") {
		log.Println("CrossArb: ⚠️ CRITICAL WARNING: LIVE MODE enabled but Kalshi keys are missing! Trades will FAIL.")
	}

	log.Println("CrossArb: Starting cross-platform arbitrage strategy...")
	log.Printf("CrossArb: Min profit threshold: $%.2f, Max combined cost: $%.2f", MinProfitThreshold, MaxCombinedCost)
	log.Println("CrossArb: Monitoring 4 arb types: poly_yes_kalshi_no, kalshi_yes_poly_no, poly_same_market, kalshi_same_market")

	// Discovery ticker - find matching markets
	discoveryTicker := time.NewTicker(5 * time.Minute)
	defer discoveryTicker.Stop()

	// Price update ticker - check for opportunities
	priceTicker := time.NewTicker(3 * time.Second)
	defer priceTicker.Stop()

	// Initial discovery
	s.discoverMatchingMarkets()

	for {
		select {
		case <-s.ctx.Done():
			log.Println("CrossArb: Strategy stopped")
			return
		case <-discoveryTicker.C:
			s.discoverMatchingMarkets()
		case <-priceTicker.C:
			s.updatePricesAndTrade()
		}
	}
}

// Stop stops the strategy
func (s *Strategy) Stop() {
	s.cancel()
}

// discoverMatchingMarkets finds markets that exist on both platforms
func (s *Strategy) discoverMatchingMarkets() {
	log.Println("CrossArb: Discovering matched markets...")

	pairs, err := s.Matcher.FindMatchingMarkets(s.Config.SportsTags)
	if err != nil {
		log.Printf("CrossArb: Discovery error: %v", err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	newPairs := 0
	for i := range pairs {
		pair := &pairs[i]
		pairID := s.getPairID(pair)

		if _, exists := s.trackedPairs[pairID]; !exists {
			s.trackedPairs[pairID] = &TrackedPair{
				Pair: pair,
			}
			newPairs++

			log.Printf("CrossArb: 🔗 MATCHED [%.0f%%]: %s ↔ %s",
				pair.MatchScore*100,
				truncateString(pair.PolyMarket.Question, 40),
				truncateString(pair.KalshiMarket.Title, 40))
		}
	}

	log.Printf("CrossArb: Added %d new pairs, tracking %d total", newPairs, len(s.trackedPairs))
}

// updatePricesAndTrade fetches prices and looks for arbitrage opportunities
func (s *Strategy) updatePricesAndTrade() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	for pairID, tracked := range s.trackedPairs {
		// Fetch Polymarket prices
		polyTokenPair, err := tracked.Pair.PolyMarket.GetClobTokenPair()
		if err != nil {
			continue
		}

		polyYesBook, err := s.ClobClient.GetOrderBookWithPrices(polyTokenPair.Yes)
		if err != nil {
			continue
		}
		polyNoBook, err := s.ClobClient.GetOrderBookWithPrices(polyTokenPair.No)
		if err != nil {
			continue
		}

		tracked.PolyYesBid = polyYesBook.BestBid
		tracked.PolyYesAsk = polyYesBook.BestAsk
		tracked.PolyNoBid = polyNoBook.BestBid
		tracked.PolyNoAsk = polyNoBook.BestAsk

		// Fetch Kalshi prices
		kalshiPrices, err := s.KalshiClient.GetBestPrices(tracked.Pair.KalshiMarket.Ticker)
		if err != nil {
			// Log but continue - Kalshi might be temporarily unavailable
			log.Printf("CrossArb: Kalshi price fetch failed for %s: %v", tracked.Pair.KalshiMarket.Ticker, err)
			continue
		}

		tracked.KalshiYesBid = kalshiPrices.YesBid
		tracked.KalshiYesAsk = kalshiPrices.YesAsk
		tracked.KalshiNoBid = kalshiPrices.NoBid
		tracked.KalshiNoAsk = kalshiPrices.NoAsk
		tracked.LastUpdate = now

		// Check all 4 arbitrage types
		s.checkAndExecuteArbs(pairID, tracked)
	}
}

// checkAndExecuteArbs checks all 4 arb types for a given pair
func (s *Strategy) checkAndExecuteArbs(pairID string, tracked *TrackedPair) {
	// 1. Poly YES + Kalshi NO
	if !tracked.HasPolyYesKalshiNo {
		if arb := s.checkPolyYesKalshiNo(tracked); arb != nil {
			s.executeArb(pairID, arb)
		}
	}

	// 2. Kalshi YES + Poly NO
	if !tracked.HasKalshiYesPolyNo {
		if arb := s.checkKalshiYesPolyNo(tracked); arb != nil {
			s.executeArb(pairID, arb)
		}
	}

	// 3. Poly same market (YES + NO both on Poly)
	if !tracked.HasPolySameMarket {
		if arb := s.checkPolySameMarket(tracked); arb != nil {
			s.executeArb(pairID, arb)
		}
	}

	// 4. Kalshi same market (YES + NO both on Kalshi)
	if !tracked.HasKalshiSameMarket {
		if arb := s.checkKalshiSameMarket(tracked); arb != nil {
			s.executeArb(pairID, arb)
		}
	}
}

// checkPolyYesKalshiNo checks for Poly YES + Kalshi NO arbitrage
func (s *Strategy) checkPolyYesKalshiNo(tracked *TrackedPair) *ArbOpportunity {
	polyYesAsk := tracked.PolyYesAsk
	kalshiNoAsk := tracked.KalshiNoAsk

	if polyYesAsk <= 0 || kalshiNoAsk <= 0 {
		return nil
	}

	// Calculate Kalshi fee for NO side
	kalshiFee := s.KalshiClient.CalculateFee(1, kalshiNoAsk)

	combinedCost := polyYesAsk + kalshiNoAsk + kalshiFee

	if combinedCost >= MaxCombinedCost {
		return nil
	}

	profit := 1.0 - combinedCost
	if profit < MinProfitThreshold {
		return nil
	}

	polyTokenPair, _ := tracked.Pair.PolyMarket.GetClobTokenPair()

	return &ArbOpportunity{
		Type:           PolyYesKalshiNo,
		Pair:           tracked,
		Leg1Platform:   "poly",
		Leg1Side:       "yes",
		Leg1Price:      polyYesAsk,
		Leg1TokenID:    polyTokenPair.Yes,
		Leg2Platform:   "kalshi",
		Leg2Side:       "no",
		Leg2Price:      kalshiNoAsk,
		Leg2Ticker:     tracked.Pair.KalshiMarket.Ticker,
		CombinedCost:   combinedCost,
		KalshiFee:      kalshiFee,
		ExpectedProfit: profit,
		ProfitPercent:  profit / combinedCost * 100,
	}
}

// checkKalshiYesPolyNo checks for Kalshi YES + Poly NO arbitrage
func (s *Strategy) checkKalshiYesPolyNo(tracked *TrackedPair) *ArbOpportunity {
	kalshiYesAsk := tracked.KalshiYesAsk
	polyNoAsk := tracked.PolyNoAsk

	if kalshiYesAsk <= 0 || polyNoAsk <= 0 {
		return nil
	}

	// Calculate Kalshi fee for YES side
	kalshiFee := s.KalshiClient.CalculateFee(1, kalshiYesAsk)

	combinedCost := kalshiYesAsk + polyNoAsk + kalshiFee

	if combinedCost >= MaxCombinedCost {
		return nil
	}

	profit := 1.0 - combinedCost
	if profit < MinProfitThreshold {
		return nil
	}

	polyTokenPair, _ := tracked.Pair.PolyMarket.GetClobTokenPair()

	return &ArbOpportunity{
		Type:           KalshiYesPolyNo,
		Pair:           tracked,
		Leg1Platform:   "kalshi",
		Leg1Side:       "yes",
		Leg1Price:      kalshiYesAsk,
		Leg1Ticker:     tracked.Pair.KalshiMarket.Ticker,
		Leg2Platform:   "poly",
		Leg2Side:       "no",
		Leg2Price:      polyNoAsk,
		Leg2TokenID:    polyTokenPair.No,
		CombinedCost:   combinedCost,
		KalshiFee:      kalshiFee,
		ExpectedProfit: profit,
		ProfitPercent:  profit / combinedCost * 100,
	}
}

// checkPolySameMarket checks for Poly YES + Poly NO arbitrage
func (s *Strategy) checkPolySameMarket(tracked *TrackedPair) *ArbOpportunity {
	polyYesAsk := tracked.PolyYesAsk
	polyNoAsk := tracked.PolyNoAsk

	if polyYesAsk <= 0 || polyNoAsk <= 0 {
		return nil
	}

	// No Kalshi fees for same-platform arb
	combinedCost := polyYesAsk + polyNoAsk

	if combinedCost >= MaxCombinedCost {
		return nil
	}

	profit := 1.0 - combinedCost
	if profit < MinProfitThreshold {
		return nil
	}

	polyTokenPair, _ := tracked.Pair.PolyMarket.GetClobTokenPair()

	return &ArbOpportunity{
		Type:           PolySameMarket,
		Pair:           tracked,
		Leg1Platform:   "poly",
		Leg1Side:       "yes",
		Leg1Price:      polyYesAsk,
		Leg1TokenID:    polyTokenPair.Yes,
		Leg2Platform:   "poly",
		Leg2Side:       "no",
		Leg2Price:      polyNoAsk,
		Leg2TokenID:    polyTokenPair.No,
		CombinedCost:   combinedCost,
		KalshiFee:      0,
		ExpectedProfit: profit,
		ProfitPercent:  profit / combinedCost * 100,
	}
}

// checkKalshiSameMarket checks for Kalshi YES + Kalshi NO arbitrage
func (s *Strategy) checkKalshiSameMarket(tracked *TrackedPair) *ArbOpportunity {
	kalshiYesAsk := tracked.KalshiYesAsk
	kalshiNoAsk := tracked.KalshiNoAsk

	if kalshiYesAsk <= 0 || kalshiNoAsk <= 0 {
		return nil
	}

	// Calculate Kalshi fees for both sides
	yesFee := s.KalshiClient.CalculateFee(1, kalshiYesAsk)
	noFee := s.KalshiClient.CalculateFee(1, kalshiNoAsk)
	totalFee := yesFee + noFee

	combinedCost := kalshiYesAsk + kalshiNoAsk + totalFee

	if combinedCost >= MaxCombinedCost {
		return nil
	}

	profit := 1.0 - combinedCost
	if profit < MinProfitThreshold {
		return nil
	}

	return &ArbOpportunity{
		Type:           KalshiSameMarket,
		Pair:           tracked,
		Leg1Platform:   "kalshi",
		Leg1Side:       "yes",
		Leg1Price:      kalshiYesAsk,
		Leg1Ticker:     tracked.Pair.KalshiMarket.Ticker,
		Leg2Platform:   "kalshi",
		Leg2Side:       "no",
		Leg2Price:      kalshiNoAsk,
		Leg2Ticker:     tracked.Pair.KalshiMarket.Ticker,
		CombinedCost:   combinedCost,
		KalshiFee:      totalFee,
		ExpectedProfit: profit,
		ProfitPercent:  profit / combinedCost * 100,
	}
}

// executeArb executes an arbitrage opportunity
func (s *Strategy) executeArb(pairID string, arb *ArbOpportunity) {
	// Check position limits
	if !s.RiskManager.CanAddPositionForStrategy("crossarb") {
		return
	}

	log.Printf("CrossArb: 💰 %s OPPORTUNITY FOUND!", arb.Type)
	log.Printf("  Match: %s ↔ %s",
		truncateString(arb.Pair.Pair.PolyMarket.Question, 40),
		truncateString(arb.Pair.Pair.KalshiMarket.Title, 40))
	log.Printf("  Leg1: %s %s @ $%.4f", arb.Leg1Platform, arb.Leg1Side, arb.Leg1Price)
	log.Printf("  Leg2: %s %s @ $%.4f", arb.Leg2Platform, arb.Leg2Side, arb.Leg2Price)
	log.Printf("  Combined: $%.4f (fee: $%.4f)", arb.CombinedCost, arb.KalshiFee)
	log.Printf("  Expected profit: $%.4f (%.2f%%)", arb.ExpectedProfit, arb.ProfitPercent)

	// Calculate position size
	maxCost := s.Config.MaxPositionSize
	contracts := int(math.Floor(maxCost / arb.CombinedCost))
	if contracts < 1 {
		log.Printf("CrossArb: Position too small, skipping")
		return
	}

	// Calculate actual costs
	leg1Cost := float64(contracts) * arb.Leg1Price
	leg2Cost := float64(contracts) * arb.Leg2Price
	actualFee := s.KalshiClient.CalculateFee(contracts, arb.Leg2Price)
	if arb.Type == KalshiSameMarket {
		actualFee = s.KalshiClient.CalculateFee(contracts, arb.Leg1Price) + s.KalshiClient.CalculateFee(contracts, arb.Leg2Price)
	}
	totalCost := leg1Cost + leg2Cost + actualFee
	expectedPayout := float64(contracts) * 1.0
	expectedProfit := expectedPayout - totalCost

	log.Printf("  Contracts: %d, Total cost: $%.2f, Expected payout: $%.2f, Profit: $%.2f",
		contracts, totalCost, expectedPayout, expectedProfit)

	if s.Config.IsDryRun() {
		log.Printf("CrossArb: [DRY RUN] Would execute %s arb with %d contracts", arb.Type, contracts)
		s.markPositionOpened(pairID, arb.Type)

		// Track in risk manager for dry run
		s.trackDryRunPosition(arb, contracts, totalCost)
		return
	}

	// Execute leg 1
	leg1Err := s.executeLeg(arb.Leg1Platform, arb.Leg1Side, arb.Leg1Price, arb.Leg1TokenID, arb.Leg1Ticker, contracts)
	if leg1Err != nil {
		log.Printf("CrossArb: Leg1 failed: %v", leg1Err)
		return
	}

	// Execute leg 2
	leg2Err := s.executeLeg(arb.Leg2Platform, arb.Leg2Side, arb.Leg2Price, arb.Leg2TokenID, arb.Leg2Ticker, contracts)
	if leg2Err != nil {
		log.Printf("CrossArb: Leg2 failed: %v - WARNING: Leg1 already executed!", leg2Err)
		// TODO: Implement rollback or hedge logic
		return
	}

	s.markPositionOpened(pairID, arb.Type)
	log.Printf("CrossArb: ✅ %s arb executed successfully!", arb.Type)
}

// executeLeg executes a single leg of the arbitrage
func (s *Strategy) executeLeg(platform, side string, price float64, tokenID, ticker string, contracts int) error {
	if platform == "poly" {
		orderSide := clob.Buy
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tokenID,
			Price:     price,
			Size:      float64(contracts),
			Side:      orderSide,
			OrderType: clob.Limit,
		})
		return err
	} else if platform == "kalshi" {
		priceInCents := int(price * 100)
		_, err := s.KalshiClient.CreateOrder(kalshi.CreateOrderRequest{
			Ticker:   ticker,
			Side:     side,
			Action:   "buy",
			Type:     "limit",
			Count:    contracts,
			YesPrice: priceInCents,
		})
		return err
	}
	return fmt.Errorf("unknown platform: %s", platform)
}

// markPositionOpened marks a position as opened for a given arb type
func (s *Strategy) markPositionOpened(pairID string, arbType ArbType) {
	tracked, exists := s.trackedPairs[pairID]
	if !exists {
		return
	}

	switch arbType {
	case PolyYesKalshiNo:
		tracked.HasPolyYesKalshiNo = true
	case KalshiYesPolyNo:
		tracked.HasKalshiYesPolyNo = true
	case PolySameMarket:
		tracked.HasPolySameMarket = true
	case KalshiSameMarket:
		tracked.HasKalshiSameMarket = true
	}
}

// trackDryRunPosition tracks a position in dry run mode
func (s *Strategy) trackDryRunPosition(arb *ArbOpportunity, contracts int, totalCost float64) {
	// Track as paired positions
	pos1 := &risk.Position{
		MarketID:     arb.Pair.Pair.PolyMarket.ID,
		TokenID:      arb.Leg1TokenID,
		OutcomeName:  fmt.Sprintf("%s_%s", arb.Leg1Platform, arb.Leg1Side),
		Size:         float64(contracts),
		EntryPrice:   arb.Leg1Price,
		CurrentPrice: arb.Leg1Price,
		Side:         "BUY",
		Type:         risk.TypeArbitrage,
		Strategy:     "crossarb",
		TotalCost:    totalCost / 2,
	}
	pos1ID := s.RiskManager.AddPosition(pos1)

	pos2 := &risk.Position{
		MarketID:         arb.Pair.Pair.PolyMarket.ID,
		TokenID:          arb.Leg2TokenID,
		OutcomeName:      fmt.Sprintf("%s_%s", arb.Leg2Platform, arb.Leg2Side),
		Size:             float64(contracts),
		EntryPrice:       arb.Leg2Price,
		CurrentPrice:     arb.Leg2Price,
		Side:             "BUY",
		Type:             risk.TypeArbitrage,
		Strategy:         "crossarb",
		TotalCost:        totalCost / 2,
		PairedPositionID: pos1ID,
	}
	pos2ID := s.RiskManager.AddPosition(pos2)

	// Link positions
	if p1 := s.RiskManager.GetPosition(pos1ID); p1 != nil {
		p1.PairedPositionID = pos2ID
	}
}

// getPairID generates a unique ID for a market pair
func (s *Strategy) getPairID(pair *matcher.MarketPair) string {
	return fmt.Sprintf("%s_%s", pair.PolyMarket.ID, pair.KalshiMarket.Ticker)
}

// GetStatus returns the strategy status
func (s *Strategy) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	openPositions := len(s.RiskManager.GetPositionsByStrategy("crossarb"))
	return fmt.Sprintf("CrossArb: %d pairs, %d positions", len(s.trackedPairs), openPositions)
}

// truncateString truncates a string to max length
func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
