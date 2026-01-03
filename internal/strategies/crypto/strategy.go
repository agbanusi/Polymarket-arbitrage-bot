package crypto

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

// Strategy implements the crypto 15-minute arbitrage strategy
type Strategy struct {
	Config      *config.Config
	GammaClient *gamma.Client
	ClobClient  *clob.Client
	RiskManager *risk.Manager

	// Internal state
	trackedMarkets map[string]*TrackedCryptoMarket
	mu             sync.RWMutex
	ctx            context.Context
	cancel         context.CancelFunc
}

// TrackedCryptoMarket represents a crypto 15m market being monitored
type TrackedCryptoMarket struct {
	Market      *gamma.Market
	TokenPair   *gamma.ClobTokenPair
	Symbol      string  // BTC, ETH, SOL
	YesPrice    float64
	NoPrice     float64
	Spread      float64 // YesPrice + NoPrice
	EndTime     time.Time
	LastUpdate  time.Time
	HasPosition bool
	LegInfo     *LegInfo // For partial arb positions
}

// LegInfo tracks leg-in arbitrage state
type LegInfo struct {
	FirstLegSide    string  // "YES" or "NO"
	FirstLegPrice   float64
	FirstLegSize    float64
	FirstLegFilled  bool
	SecondLegPlaced bool
	TotalCost       float64
}

// NewStrategy creates a new crypto strategy
func NewStrategy(cfg *config.Config, g *gamma.Client, c *clob.Client, r *risk.Manager) *Strategy {
	ctx, cancel := context.WithCancel(context.Background())
	return &Strategy{
		Config:         cfg,
		GammaClient:    g,
		ClobClient:     c,
		RiskManager:    r,
		trackedMarkets: make(map[string]*TrackedCryptoMarket),
		ctx:            ctx,
		cancel:         cancel,
	}
}

// Run starts the strategy
func (s *Strategy) Run() {
	if !s.Config.CryptoEnabled {
		log.Println("Crypto: Strategy disabled")
		return
	}

	log.Println("Crypto: Starting strategy...")
	log.Printf("Crypto: Cheap threshold: %.2f, Max spread: %.2f, Symbols: %v",
		s.Config.CryptoCheapThreshold, s.Config.CryptoMaxSpread, s.Config.CryptoSymbols)

	// Market discovery ticker - runs more frequently for 15m markets
	discoveryTicker := time.NewTicker(30 * time.Second)
	defer discoveryTicker.Stop()

	// Price monitoring ticker - fast for 15m markets
	priceTicker := time.NewTicker(3 * time.Second)
	defer priceTicker.Stop()

	// Cleanup ticker for expired markets
	cleanupTicker := time.NewTicker(60 * time.Second)
	defer cleanupTicker.Stop()

	// Initial discovery
	s.discoverMarkets()

	for {
		select {
		case <-s.ctx.Done():
			log.Println("Crypto: Strategy stopped")
			return
		case <-discoveryTicker.C:
			s.discoverMarkets()
		case <-priceTicker.C:
			s.updatePricesAndTrade()
		case <-cleanupTicker.C:
			s.cleanupExpiredMarkets()
		}
	}
}

// Stop stops the strategy
func (s *Strategy) Stop() {
	s.cancel()
}

// discoverMarkets finds new 15-minute crypto prediction markets
func (s *Strategy) discoverMarkets() {
	log.Println("Crypto: Discovering 15m markets (fetching 1000+ events)...")

	now := time.Now()
	newMarkets := 0
	totalEventsChecked := 0

	// Paginate through multiple pages to get more events
	for page := 0; page < 20; page++ { // 20 pages x 100 = 2000 events
		events, err := s.GammaClient.GetEvents(gamma.GetEventsParams{
			Limit:  100,
			Offset: page * 100,
			Active: true,
			Order:  "id",
		})
		if err != nil {
			log.Printf("Crypto: Failed to fetch events page %d: %v", page, err)
			break
		}
		
		if len(events) == 0 {
			break // No more events
		}
		
		totalEventsChecked += len(events)

		for _, event := range events {
			// Filter for crypto-related events
			if !s.isCryptoEvent(event) {
				continue
			}

			for _, market := range event.Markets {
				if market.Closed || !market.Active || !market.EnableOrderBook {
					continue
				}

				// Check if it's a short-term market (15 min or similar)
				if !s.isShortTermMarket(market) {
					continue
				}

				// Parse end time
				endTime, err := time.Parse(time.RFC3339, market.EndDateIso)
				if err != nil {
					// Try other formats
					endTime, err = time.Parse("2006-01-02T15:04:05Z", market.EndDate)
					if err != nil {
						continue
					}
				}

				// Skip if too close to expiry or already expired
				timeLeft := endTime.Sub(now)
				if timeLeft < time.Duration(s.Config.CryptoMinTimeLeft)*time.Second || timeLeft <= 0 {
					continue
				}

				// Skip if already tracking
				s.mu.RLock()
				_, exists := s.trackedMarkets[market.ID]
				s.mu.RUnlock()
				if exists {
					continue
				}

				// Get CLOB token pair
				tokenPair, err := market.GetClobTokenPair()
				if err != nil {
					continue
				}

				// Detect symbol
				symbol := s.detectSymbol(market.Question)

				// Skip if symbol not found
				// if symbol == "" || symbol == "UNKNOWN" {
				// 	continue
				// }

				s.mu.Lock()
				s.trackedMarkets[market.ID] = &TrackedCryptoMarket{
					Market:    &market,
					TokenPair: tokenPair,
					Symbol:    symbol,
					EndTime:   endTime,
				}
				s.mu.Unlock()

				newMarkets++
				log.Printf("Crypto: Tracking: %s (expires in %.0f min)", truncateQuestion(market.Question), timeLeft.Minutes())
			}
		}
	} // end pagination loop

	s.mu.RLock()
	log.Printf("Crypto: Checked %d events, added %d markets, tracking %d total", totalEventsChecked, newMarkets, len(s.trackedMarkets))
	s.mu.RUnlock()
}

// isCryptoEvent checks if an event is crypto-related
func (s *Strategy) isCryptoEvent(event gamma.Event) bool {
	titleLower := strings.ToLower(event.Title)
	
	for _, symbol := range s.Config.CryptoSymbols {
		if strings.Contains(titleLower, strings.ToLower(symbol)) {
			return true
		}
	}

	// Check for common crypto keywords
	keywords := []string{"bitcoin", "ethereum", "solana", "btc", "eth", "sol"}
	for _, kw := range keywords {
		if strings.Contains(titleLower, kw) {
			return true
		}
	}

	// Check tags
	for _, tag := range event.Tags {
		tagLower := strings.ToLower(tag.Label)
		if strings.Contains(tagLower, "crypto") || strings.Contains(tagLower, "bitcoin") {
			return true
		}
	}

	return false
}

// isShortTermMarket checks if a market is a short-term prediction
// STRICT FILTERING: Only 15min, 1hr, or 4hr markets
func (s *Strategy) isShortTermMarket(market gamma.Market) bool {
	// Parse end time to check duration
	endTime, err := time.Parse(time.RFC3339, market.EndDateIso)
	if err != nil {
		endTime, err = time.Parse("2006-01-02T15:04:05Z", market.EndDate)
		if err != nil{
			// If we can't parse time, check for question indicators instead
			return s.hasShortTermIndicators(market.Question)
		}
	}
	
	// Calculate time until expiry
	timeUntilExpiry := time.Until(endTime)
	
	// STRICT TIME WINDOWS for short-term crypto trading
	// 15-minute window: 5-20 minutes (allows some buffer for entry/exit)
	is15Min := timeUntilExpiry >= 5*time.Minute && timeUntilExpiry <= 20*time.Minute
	
	// 1-hour window: 40-80 minutes
	is1Hr := timeUntilExpiry >= 40*time.Minute && timeUntilExpiry <= 80*time.Minute
	
	// 4-hour window: 3-5 hours
	is4Hr := timeUntilExpiry >= 3*time.Hour && timeUntilExpiry <= 5*time.Hour
	
	return is15Min || is1Hr || is4Hr
}

// hasShortTermIndicators checks if question contains time-related phrases
func (s *Strategy) hasShortTermIndicators(question string) bool {
	questionLower := strings.ToLower(question)
	
	// Look for time indicators in the question (15min, 1hr, 4hr)
	shortTermIndicators := []string{
		"15 min", "15min", "15 minute", "15-min", "15-minute",
		"1 hr", "1hr", "1 hour", "1-hr", "1-hour",
		"4 hr", "4hr", "4 hour", "4-hr", "4-hour",
	}

	for _, indicator := range shortTermIndicators {
		if strings.Contains(questionLower, indicator) {
			return true
		}
	}

	return false
}

// detectSymbol extracts the crypto symbol from the market question
func (s *Strategy) detectSymbol(question string) string {
	questionLower := strings.ToLower(question)
	
	if strings.Contains(questionLower, "bitcoin") || strings.Contains(questionLower, "btc") {
		return "BTC"
	}
	if strings.Contains(questionLower, "ethereum") || strings.Contains(questionLower, "eth") {
		return "ETH"
	}
	if strings.Contains(questionLower, "solana") || strings.Contains(questionLower, "sol") {
		return "SOL"
	}
	
	return "UNKNOWN"
}

// updatePricesAndTrade fetches prices and looks for arbitrage opportunities
func (s *Strategy) updatePricesAndTrade() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	for marketID, tracked := range s.trackedMarkets {
		// Skip if too close to expiry
		timeLeft := tracked.EndTime.Sub(now)
		if timeLeft < time.Duration(s.Config.CryptoMinTimeLeft)*time.Second {
			continue
		}

		// Fetch real prices from CLOB
		yesPrice, yesErr := s.ClobClient.GetBestBid(tracked.TokenPair.Yes)
		noPrice, noErr := s.ClobClient.GetBestBid(tracked.TokenPair.No)

		if yesErr != nil || noErr != nil {
			continue
		}

		tracked.YesPrice = yesPrice
		tracked.NoPrice = noPrice
		tracked.Spread = yesPrice + noPrice
		tracked.LastUpdate = now

		// Update risk manager with prices for existing positions
		s.RiskManager.UpdatePrice(tracked.TokenPair.Yes, yesPrice)
		s.RiskManager.UpdatePrice(tracked.TokenPair.No, noPrice)

		// Skip if already has position
		if s.RiskManager.HasPositionForMarket(marketID) {
			// Check for second leg opportunity if first leg is filled
			if tracked.LegInfo != nil && tracked.LegInfo.FirstLegFilled && !tracked.LegInfo.SecondLegPlaced {
				s.checkSecondLeg(tracked)
			}
			continue
		}

		// Look for arbitrage opportunity
		s.analyzeAndTrade(tracked)
	}
}

// analyzeAndTrade analyzes a market and executes trades if opportunity found
func (s *Strategy) analyzeAndTrade(tracked *TrackedCryptoMarket) {
	cheapThreshold := s.Config.CryptoCheapThreshold
	maxSpread := s.Config.CryptoMaxSpread

	// Check if we have room for more crypto positions (max 10 for crypto)
	if !s.RiskManager.CanAddPositionForStrategy("crypto") {
		return // Max crypto positions reached
	}

	// Priority check: BTC and SOL 15-min markets get priority
	isPriority := (tracked.Symbol == "BTC" || tracked.Symbol == "SOL") && 
		time.Until(tracked.EndTime) <= 30*time.Minute

	// Position slot allocation:
	// - Slots 0-9 (first 10): Crypto priority (BTC/SOL 15-min)
	// - Slots 10-19 (next 10): Sports priority
	// - Slots 20-34 (remaining 15): Open for all (crypto, sports, O/U)
	// DISABLED FOR DEBUGGING - priority check
	_ = s.RiskManager.GetOpenPositionCount()
	_ = isPriority
	// posCount := s.RiskManager.GetOpenPositionCount()
	// if posCount >= risk.MaxPositions-5 && !isPriority {
	// 	return
	// }

	// SPREAD ARBITRAGE
	// If Yes + No < 1.0 (minus fees), there's theoretical arbitrage
	if tracked.Spread < maxSpread && tracked.Spread > 0.5 {
		log.Printf("Crypto: SPREAD ARB - %s (Yes: %.4f + No: %.4f = %.4f)%s",
			tracked.Symbol, tracked.YesPrice, tracked.NoPrice, tracked.Spread,
			func() string { if isPriority { return " ⭐ PRIORITY" } else { return "" } }())
		
		// Buy both sides
		s.executeSpreadArb(tracked)
		return
	}

	// LEG-IN STRATEGY
	// Wait for one side to dip below threshold, then try to leg in
	
	// Check YES side
	if tracked.YesPrice < cheapThreshold && tracked.YesPrice > 0.10 {
		// Calculate if second leg would be profitable
		maxSecondLegPrice := 1.0 - tracked.YesPrice - 0.02 // 2% buffer for fees
		if tracked.NoPrice <= maxSecondLegPrice {
			log.Printf("Crypto: LEG-IN OPP - %s YES @ %.4f (max NO: %.4f, current NO: %.4f)",
				tracked.Symbol, tracked.YesPrice, maxSecondLegPrice, tracked.NoPrice)
			
			s.executeFirstLeg(tracked, "YES", tracked.TokenPair.Yes, tracked.YesPrice)
		}
		return
	}

	// Check NO side
	if tracked.NoPrice < cheapThreshold && tracked.NoPrice > 0.10 {
		maxSecondLegPrice := 1.0 - tracked.NoPrice - 0.02
		if tracked.YesPrice <= maxSecondLegPrice {
			log.Printf("Crypto: LEG-IN OPP - %s NO @ %.4f (max YES: %.4f, current YES: %.4f)",
				tracked.Symbol, tracked.NoPrice, maxSecondLegPrice, tracked.YesPrice)
			
			s.executeFirstLeg(tracked, "NO", tracked.TokenPair.No, tracked.NoPrice)
		}
	}
}

// executeSpreadArb executes a full spread arbitrage (both legs immediately)
func (s *Strategy) executeSpreadArb(tracked *TrackedCryptoMarket) {
	yesPrice := tracked.YesPrice
	noPrice := tracked.NoPrice
	
	// Calculate sizes (equal dollar amounts)
	maxCost := s.Config.MaxPositionSize
	halfCost := maxCost / 2
	
	yesSize := halfCost / yesPrice
	noSize := halfCost / noPrice

	totalCost := (yesPrice * yesSize) + (noPrice * noSize)
	potentialPayout := yesSize // Both sides give $1 payout per share (whichever wins)
	guaranteedProfit := potentialPayout - totalCost

	if guaranteedProfit <= 0 {
		log.Printf("Crypto: No profit in spread arb (cost: $%.4f, payout: $%.4f)", totalCost, potentialPayout)
		return
	}

	// Risk check
	if err := s.RiskManager.CheckEntry(tracked.TokenPair.Yes, yesPrice, yesSize); err != nil {
		log.Printf("Crypto: Risk check failed: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("Crypto: [DRY RUN] SPREAD ARB %s", tracked.Symbol)
		log.Printf("  YES @ %.4f (size: %.2f, cost: $%.2f)", yesPrice, yesSize, yesPrice*yesSize)
		log.Printf("  NO @ %.4f (size: %.2f, cost: $%.2f)", noPrice, noSize, noPrice*noSize)
		log.Printf("  Total cost: $%.2f, Guaranteed profit: $%.4f", totalCost, guaranteedProfit)
	} else {
		// Execute both orders
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tracked.TokenPair.Yes,
			Price:     yesPrice,
			Size:      yesSize,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Crypto: YES order failed: %v", err)
			return
		}

		_, err = s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tracked.TokenPair.No,
			Price:     noPrice,
			Size:      noSize,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Crypto: NO order failed: %v", err)
			return
		}
	}

	// Track both positions
	yesPos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      tracked.TokenPair.Yes,
		OutcomeName:  "YES",
		Size:         yesSize,
		EntryPrice:   yesPrice,
		CurrentPrice: yesPrice,
		Side:         "BUY",
		Type:         risk.TypeArbitrage,
		Strategy:     "crypto",
		TotalCost:    totalCost,
	}
	yesID := s.RiskManager.AddPosition(yesPos)

	noPos := &risk.Position{
		MarketID:         tracked.Market.ID,
		TokenID:          tracked.TokenPair.No,
		OutcomeName:      "NO",
		Size:             noSize,
		EntryPrice:       noPrice,
		CurrentPrice:     noPrice,
		Side:             "BUY",
		Type:             risk.TypeArbitrage,
		Strategy:         "crypto",
		TotalCost:        totalCost,
		PairedPositionID: yesID,
	}
	noID := s.RiskManager.AddPosition(noPos)

	// Link positions
	if yesPosition := s.RiskManager.GetPosition(yesID); yesPosition != nil {
		yesPosition.PairedPositionID = noID
	}

	tracked.HasPosition = true
	log.Printf("Crypto: Spread arb executed - guaranteed profit: $%.4f", guaranteedProfit)
}

// executeFirstLeg executes the first leg of a leg-in arbitrage
func (s *Strategy) executeFirstLeg(tracked *TrackedCryptoMarket, side string, tokenID string, price float64) {
	maxCost := s.Config.MaxPositionSize / 2 // Reserve half for second leg
	size := maxCost / price

	// Risk check
	if err := s.RiskManager.CheckEntry(tokenID, price, size); err != nil {
		log.Printf("Crypto: Risk check failed for first leg: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("Crypto: [DRY RUN] First leg %s %s @ %.4f (size: %.2f)",
			side, tracked.Symbol, price, size)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tokenID,
			Price:     price,
			Size:      size,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Crypto: First leg order failed: %v", err)
			return
		}
	}

	// Track leg info
	tracked.LegInfo = &LegInfo{
		FirstLegSide:   side,
		FirstLegPrice:  price,
		FirstLegSize:   size,
		FirstLegFilled: true, // Assume filled for simplicity (TODO: check order status)
	}

	// Track position
	pos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      tokenID,
		OutcomeName:  side,
		Size:         size,
		EntryPrice:   price,
		CurrentPrice: price,
		Side:         "BUY",
		Type:         risk.TypeArbitrage,
		Strategy:     "crypto",
	}
	s.RiskManager.AddPosition(pos)
	tracked.HasPosition = true
}

// checkSecondLeg checks if second leg should be placed
func (s *Strategy) checkSecondLeg(tracked *TrackedCryptoMarket) {
	if tracked.LegInfo == nil || tracked.LegInfo.SecondLegPlaced {
		return
	}

	// Determine second leg details
	var secondTokenID string
	var secondPrice float64
	var secondSide string

	if tracked.LegInfo.FirstLegSide == "YES" {
		secondTokenID = tracked.TokenPair.No
		secondPrice = tracked.NoPrice
		secondSide = "NO"
	} else {
		secondTokenID = tracked.TokenPair.Yes
		secondPrice = tracked.YesPrice
		secondSide = "YES"
	}

	// Check if second leg would complete profitable arb
	totalCost := tracked.LegInfo.FirstLegPrice + secondPrice
	if totalCost >= 0.99 {
		// Not profitable enough, skip
		return
	}

	size := tracked.LegInfo.FirstLegSize // Match first leg size

	if s.Config.IsDryRun() {
		log.Printf("Crypto: [DRY RUN] Second leg %s %s @ %.4f (total cost: %.4f)",
			secondSide, tracked.Symbol, secondPrice, totalCost)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   secondTokenID,
			Price:     secondPrice,
			Size:      size,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Crypto: Second leg order failed: %v", err)
			return
		}
	}

	tracked.LegInfo.SecondLegPlaced = true
	tracked.LegInfo.TotalCost = totalCost

	// Track second position
	pos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      secondTokenID,
		OutcomeName:  secondSide,
		Size:         size,
		EntryPrice:   secondPrice,
		CurrentPrice: secondPrice,
		Side:         "BUY",
		Type:         risk.TypeArbitrage,
		Strategy:     "crypto",
		TotalCost:    totalCost,
	}
	s.RiskManager.AddPosition(pos)

	log.Printf("Crypto: Second leg completed - total cost: $%.4f (locked profit: $%.4f)",
		totalCost, 1.0-totalCost)
}

// cleanupExpiredMarkets removes expired markets from tracking
func (s *Strategy) cleanupExpiredMarkets() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	cleaned := 0

	for marketID, tracked := range s.trackedMarkets {
		if now.After(tracked.EndTime) {
			delete(s.trackedMarkets, marketID)
			cleaned++
		}
	}

	if cleaned > 0 {
		log.Printf("Crypto: Cleaned up %d expired markets", cleaned)
	}
}

// GetStatus returns strategy status
func (s *Strategy) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	openPositions := len(s.RiskManager.GetPositionsByStrategy("crypto"))
	return fmt.Sprintf("Crypto: Tracking %d markets, %d open positions",
		len(s.trackedMarkets), openPositions)
}

// truncateQuestion shortens a market question for logging
func truncateQuestion(q string) string {
	if len(q) > 50 {
		return q[:47] + "..."
	}
	return q
}
