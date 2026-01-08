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
	Symbol      string // BTC, ETH, SOL
	YesPrice    float64
	NoPrice     float64
	Spread      float64 // YesPrice + NoPrice
	EndTime     time.Time
	LastUpdate  time.Time
	HasPosition bool
	LegInfo     *LegInfo // For partial arb positions
}

// LegInfo tracks leg-in arbitrage state with accumulated cost tracking
// for asymmetric buying (buy YES cheap at T1, buy NO cheap at T2)
type LegInfo struct {
	FirstLegSide    string // "YES" or "NO"
	FirstLegPrice   float64
	FirstLegSize    float64
	FirstLegFilled  bool
	SecondLegPlaced bool
	TotalCost       float64

	// Accumulated tracking for asymmetric entries (Gabagool strategy)
	YesAccumulatedCost   float64
	YesAccumulatedShares float64
	NoAccumulatedCost    float64
	NoAccumulatedShares  float64
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
		if err != nil {
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

		// Check for exits on existing positions (stop loss, take profit, or near-expiry)
		if s.RiskManager.HasPositionForMarket(marketID) {
			s.checkExitsForCrypto(tracked, now)
		}

		// Skip if already has position (check again after potential exits)
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
	if tracked.Spread < maxSpread && tracked.Spread > 0.20 {
		log.Printf("Crypto: SPREAD ARB - %s (Yes: %.4f + No: %.4f = %.4f)%s",
			tracked.Symbol, tracked.YesPrice, tracked.NoPrice, tracked.Spread,
			func() string {
				if isPriority {
					return " ⭐ PRIORITY"
				} else {
					return ""
				}
			}())

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
// Uses EQUAL SHARE QUANTITIES to ensure guaranteed profit (Gabagool strategy)
func (s *Strategy) executeSpreadArb(tracked *TrackedCryptoMarket) {
	yesPrice := tracked.YesPrice
	noPrice := tracked.NoPrice

	// Calculate sizes - EQUAL SHARE QUANTITIES (not equal dollar amounts)
	// This ensures min(Qty_YES, Qty_NO) = shareQty for profit calculation
	maxCost := s.Config.MaxPositionSize
	halfCost := maxCost / 2

	// Calculate max shares we can afford on each side
	yesSharesMax := halfCost / yesPrice
	noSharesMax := halfCost / noPrice

	// Use the smaller quantity so we have equal shares on both sides
	shareQty := yesSharesMax
	if noSharesMax < yesSharesMax {
		shareQty = noSharesMax
	}

	// Now calculate actual costs with equal quantities
	yesCost := shareQty * yesPrice
	noCost := shareQty * noPrice
	totalCost := yesCost + noCost

	// Guaranteed payout is $1.00 per share (one side always wins)
	potentialPayout := shareQty

	// CRITICAL: Account for trading fees (round-trip: buy + sell)
	fees := totalCost * s.Config.TradingFeePercent
	guaranteedProfit := potentialPayout - totalCost - fees
	profitPercent := guaranteedProfit / totalCost

	// Check minimum profit threshold after fees
	if profitPercent < s.Config.MinProfitAfterFees {
		log.Printf("Crypto: SKIP spread arb - profit %.2f%% below min %.2f%% (fees: $%.4f)",
			profitPercent*100, s.Config.MinProfitAfterFees*100, fees)
		return
	}

	if guaranteedProfit <= 0 {
		log.Printf("Crypto: No profit in spread arb (cost: $%.4f, payout: $%.4f, fees: $%.4f)",
			totalCost, potentialPayout, fees)
		return
	}

	// Risk check
	if err := s.RiskManager.CheckEntry(tracked.TokenPair.Yes, yesPrice, shareQty); err != nil {
		log.Printf("Crypto: Risk check failed: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("Crypto: [DRY RUN] SPREAD ARB %s (Equal shares: %.2f)", tracked.Symbol, shareQty)
		log.Printf("  YES @ %.4f × %.2f shares = $%.2f", yesPrice, shareQty, yesCost)
		log.Printf("  NO @ %.4f × %.2f shares = $%.2f", noPrice, shareQty, noCost)
		log.Printf("  Total cost: $%.2f, Payout: $%.2f, Guaranteed profit: $%.4f (%.2f%%)",
			totalCost, potentialPayout, guaranteedProfit, profitPercent*100)
	} else {
		// Execute both orders
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tracked.TokenPair.Yes,
			Price:     yesPrice,
			Size:      shareQty,
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
			Size:      shareQty,
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
		Size:         shareQty,
		EntryPrice:   yesPrice,
		CurrentPrice: yesPrice,
		Side:         "BUY",
		Type:         risk.TypeArbitrage,
		Strategy:     "crypto",
		TotalCost:    yesCost,
	}
	yesID := s.RiskManager.AddPosition(yesPos)

	noPos := &risk.Position{
		MarketID:         tracked.Market.ID,
		TokenID:          tracked.TokenPair.No,
		OutcomeName:      "NO",
		Size:             shareQty,
		EntryPrice:       noPrice,
		CurrentPrice:     noPrice,
		Side:             "BUY",
		Type:             risk.TypeArbitrage,
		Strategy:         "crypto",
		TotalCost:        noCost,
		PairedPositionID: yesID,
	}
	noID := s.RiskManager.AddPosition(noPos)

	// Link positions
	if yesPosition := s.RiskManager.GetPosition(yesID); yesPosition != nil {
		yesPosition.PairedPositionID = noID
	}

	tracked.HasPosition = true

	// Initialize leg info for potential future additions
	tracked.LegInfo = &LegInfo{
		YesAccumulatedCost:   yesCost,
		YesAccumulatedShares: shareQty,
		NoAccumulatedCost:    noCost,
		NoAccumulatedShares:  shareQty,
	}

	log.Printf("Crypto: Spread arb executed - %.2f shares each side, guaranteed profit: $%.4f", shareQty, guaranteedProfit)
}

// executeFirstLeg executes the first leg of a leg-in arbitrage
// Tracks accumulated costs for asymmetric buying (Gabagool strategy)
func (s *Strategy) executeFirstLeg(tracked *TrackedCryptoMarket, side string, tokenID string, price float64) {
	maxCost := s.Config.MaxPositionSize / 2 // Reserve half for second leg
	size := maxCost / price
	cost := size * price

	// Risk check
	if err := s.RiskManager.CheckEntry(tokenID, price, size); err != nil {
		log.Printf("Crypto: Risk check failed for first leg: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("Crypto: [DRY RUN] First leg %s %s @ %.4f × %.2f shares = $%.2f",
			side, tracked.Symbol, price, size, cost)
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

	// Track leg info with accumulated costs
	tracked.LegInfo = &LegInfo{
		FirstLegSide:   side,
		FirstLegPrice:  price,
		FirstLegSize:   size,
		FirstLegFilled: true, // Assume filled for simplicity (TODO: check order status)
	}

	// Initialize accumulated tracking based on which side we bought
	if side == "YES" {
		tracked.LegInfo.YesAccumulatedCost = cost
		tracked.LegInfo.YesAccumulatedShares = size
	} else {
		tracked.LegInfo.NoAccumulatedCost = cost
		tracked.LegInfo.NoAccumulatedShares = size
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
		TotalCost:    cost,
	}
	s.RiskManager.AddPosition(pos)
	tracked.HasPosition = true

	log.Printf("Crypto: First leg %s executed - %.2f shares @ $%.4f = $%.2f", side, size, price, cost)
}

// checkSecondLeg checks if second leg should be placed
// Uses average cost calculation for asymmetric buying (Gabagool strategy)
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

	// Match first leg size for equal quantities
	size := tracked.LegInfo.FirstLegSize
	secondCost := size * secondPrice

	// Calculate avg cost per share for completed arb (Gabagool formula)
	// avgYesCost + avgNoCost < $1.00 = guaranteed profit
	var avgYesCost, avgNoCost float64
	if tracked.LegInfo.FirstLegSide == "YES" {
		avgYesCost = tracked.LegInfo.FirstLegPrice // First leg YES price
		avgNoCost = secondPrice                    // Second leg NO price
	} else {
		avgYesCost = secondPrice                  // Second leg YES price
		avgNoCost = tracked.LegInfo.FirstLegPrice // First leg NO price
	}

	combinedCostPerShare := avgYesCost + avgNoCost

	// Need combined cost < $0.98 to account for fees (2% buffer)
	if combinedCostPerShare >= 0.98 {
		// Not profitable enough, skip for now
		return
	}

	profitPerShare := 1.0 - combinedCostPerShare
	totalProfit := profitPerShare * size

	if s.Config.IsDryRun() {
		log.Printf("Crypto: [DRY RUN] Second leg %s %s @ %.4f × %.2f shares",
			secondSide, tracked.Symbol, secondPrice, size)
		log.Printf("  Combined cost per share: $%.4f (YES: $%.4f + NO: $%.4f)",
			combinedCostPerShare, avgYesCost, avgNoCost)
		log.Printf("  Guaranteed profit: $%.4f (%.2f%% per share)", totalProfit, profitPerShare*100)
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
	tracked.LegInfo.TotalCost = combinedCostPerShare

	// Update accumulated tracking
	if secondSide == "YES" {
		tracked.LegInfo.YesAccumulatedCost += secondCost
		tracked.LegInfo.YesAccumulatedShares += size
	} else {
		tracked.LegInfo.NoAccumulatedCost += secondCost
		tracked.LegInfo.NoAccumulatedShares += size
	}

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
		TotalCost:    secondCost,
	}
	s.RiskManager.AddPosition(pos)

	log.Printf("Crypto: Second leg completed - combined cost: $%.4f/share, locked profit: $%.4f",
		combinedCostPerShare, totalProfit)
}

// Minimum time before expiry to force exit (30 seconds before market closes)
const cryptoExpiryBuffer = 10 * time.Second

// Grace period after entry before stop loss / take profit can trigger
const cryptoEntryGracePeriod = 30 * time.Second

// checkExitsForCrypto monitors crypto positions for exit conditions:
// 1. Stop loss - if position drops below stop loss threshold
// 2. Take profit - if position gains above take profit threshold
// 3. Near expiry - if market is about to expire, exit to capture any remaining value
func (s *Strategy) checkExitsForCrypto(tracked *TrackedCryptoMarket, now time.Time) {
	positions := s.RiskManager.GetPositionsByStrategy("crypto")

	var marketPositions []*risk.Position
	for _, pos := range positions {
		if pos.MarketID == tracked.Market.ID && pos.State == risk.StateOpen {
			marketPositions = append(marketPositions, pos)
		}
	}

	if len(marketPositions) == 0 {
		return
	}

	// Check if near expiry - if so, exit ALL positions in this market
	timeToExpiry := tracked.EndTime.Sub(now)
	if timeToExpiry <= cryptoExpiryBuffer && timeToExpiry > 0 {
		log.Printf("Crypto: ⏰ NEAR EXPIRY (%.0fs left) - Closing all positions for: %s",
			timeToExpiry.Seconds(), truncateQuestion(tracked.Market.Question))

		for _, pos := range marketPositions {
			var currentPrice float64
			if pos.TokenID == tracked.TokenPair.Yes {
				currentPrice = tracked.YesPrice
			} else {
				currentPrice = tracked.NoPrice
			}
			pos.CurrentPrice = currentPrice
			pnlPercent := (currentPrice - pos.EntryPrice) / pos.EntryPrice

			log.Printf("  %s @ %.4f (entry: %.4f, %.1f%%)",
				pos.OutcomeName, currentPrice, pos.EntryPrice, pnlPercent*100)
			s.executeExit(pos)
		}
		tracked.HasPosition = false
		return
	}

	// Check individual positions for TAKE PROFIT and STOP LOSS
	for _, pos := range marketPositions {
		// Skip grace period - don't trigger stop loss / take profit immediately after entry
		if now.Sub(pos.EntryTime) < cryptoEntryGracePeriod {
			continue
		}

		// Get current price for this position's token
		var currentPrice float64
		if pos.TokenID == tracked.TokenPair.Yes {
			currentPrice = tracked.YesPrice
		} else {
			currentPrice = tracked.NoPrice
		}

		if currentPrice <= 0 {
			continue
		}

		pos.CurrentPrice = currentPrice
		pnlPercent := (currentPrice - pos.EntryPrice) / pos.EntryPrice

		// TAKE PROFIT - exit this position and its pair if profitable
		if pnlPercent >= s.Config.TakeProfitPercent {
			log.Printf("Crypto: 🎯 TAKE PROFIT - %s @ %.4f (entry: %.4f, +%.1f%%)",
				pos.OutcomeName, currentPrice, pos.EntryPrice, pnlPercent*100)

			s.executeExit(pos)

			// Also exit paired position if it exists (for arb positions, take profit together)
			if pos.PairedPositionID != "" {
				if pairedPos := s.RiskManager.GetPosition(pos.PairedPositionID); pairedPos != nil {
					if pairedPos.State == risk.StateOpen {
						var pairedPrice float64
						if pairedPos.TokenID == tracked.TokenPair.Yes {
							pairedPrice = tracked.YesPrice
						} else {
							pairedPrice = tracked.NoPrice
						}
						pairedPos.CurrentPrice = pairedPrice
						pairedPnlPercent := (pairedPrice - pairedPos.EntryPrice) / pairedPos.EntryPrice

						log.Printf("  Paired %s @ %.4f (entry: %.4f, %.1f%%)",
							pairedPos.OutcomeName, pairedPrice, pairedPos.EntryPrice, pairedPnlPercent*100)
						s.executeExit(pairedPos)
					}
				}
			}
			tracked.HasPosition = false
			return
		}

		// STOP LOSS - exit this position independently (paired position may still recover)
		if pnlPercent <= -s.Config.StopLossPercent {
			log.Printf("Crypto: 🛑 STOP LOSS - %s @ %.4f (entry: %.4f, %.1f%%)",
				pos.OutcomeName, currentPrice, pos.EntryPrice, pnlPercent*100)

			s.executeExit(pos)

			// For crypto arb, when one side stops out, the other side might be winning
			// Don't automatically close the paired position - let it run
			if pos.PairedPositionID != "" {
				log.Printf("  Paired position continues independently")
			}

			// Check if all positions in this market are now closed
			remainingOpen := false
			for _, p := range marketPositions {
				if p.ID != pos.ID && p.State == risk.StateOpen {
					remainingOpen = true
					break
				}
			}
			if !remainingOpen {
				tracked.HasPosition = false
			}
		}
	}
}

// executeExit sells a position at the best available bid price
func (s *Strategy) executeExit(pos *risk.Position) {
	// Use bid price for exits (what buyers are willing to pay)
	bidPrice, err := s.ClobClient.GetBestBid(pos.TokenID)
	if err != nil || bidPrice <= 0 {
		// Fallback to current price if we can't get bid
		bidPrice = pos.CurrentPrice
	}

	if bidPrice <= 0 {
		log.Printf("Crypto: Cannot exit %s - no valid bid price", pos.OutcomeName)
		return
	}

	pnl := (bidPrice - pos.EntryPrice) * pos.Size

	if s.Config.IsDryRun() {
		log.Printf("Crypto: [DRY RUN] Would SELL %s @ %.4f (bid, P&L: $%.2f)",
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
			log.Printf("Crypto: Exit order failed: %v", err)
			return
		}
	}

	s.RiskManager.ClosePosition(pos.ID)
	log.Printf("Crypto: ✅ Closed position %s - P&L: $%.2f", pos.OutcomeName, pnl)
}

// cleanupExpiredMarkets removes expired markets from tracking and settles positions
func (s *Strategy) cleanupExpiredMarkets() {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	cleaned := 0
	positionsSettled := 0

	for marketID, tracked := range s.trackedMarkets {
		if now.After(tracked.EndTime) {
			// Check for any open positions in this expired market
			positions := s.RiskManager.GetPositionsByMarket(marketID)
			for _, pos := range positions {
				if pos.State == risk.StateOpen {
					// For crypto arb positions that expired, they should have settled
					// The settlement service will handle the actual resolution
					// Mark as pending exit so settlement service picks it up
					log.Printf("Crypto: Marking expired position for settlement: %s (market: %s)",
						pos.OutcomeName, truncateQuestion(tracked.Market.Question))
					positionsSettled++
				}
			}

			delete(s.trackedMarkets, marketID)
			cleaned++
		}
	}

	if cleaned > 0 {
		log.Printf("Crypto: Cleaned up %d expired markets, %d positions pending settlement", cleaned, positionsSettled)
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
