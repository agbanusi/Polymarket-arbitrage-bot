package sports

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

// Entry thresholds - buy both sides when prices are in these ranges
// Profit comes from selling at different times as odds shift during the game
const (
	MaxUnderdogPrice = 0.45 // Buy underdog if < 45%
	MaxFavoritePrice = 0.65 // Buy favorite if < 65%
	MinValidPrice    = 0.05 // Minimum price to consider
	MaxSpreadPercent = 0.10 // Max bid-ask spread allowed
	EntryGracePeriod = 60 * time.Second
)

// Strategy implements the sports moneyline strategy with delta neutral approach
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

// TrackedMarket represents a moneyline market being monitored
type TrackedMarket struct {
	Market       *gamma.Market
	TokenPair    *gamma.ClobTokenPair
	OutcomeNames []string // e.g., ["Lakers", "Warriors"] or ["Yes", "No"]

	// Prices - index 0 is typically underdog, index 1 favorite
	YesBid float64
	YesAsk float64
	YesMid float64
	NoBid  float64
	NoAsk  float64
	NoMid  float64

	LastUpdate  time.Time
	HasPosition bool
	GameTime    time.Time

	// Track which side is underdog (lower price = underdog)
	IsYesUnderdog bool

	// Live game tracking
	GameStatus      string       // "PRE_GAME", "LIVE", "FINAL"
	PriceHistory    []PricePoint // Last N price points for momentum detection
	LastOddsShift   float64      // Recent odds change (positive = improving)
	PeakUnderdogMid float64      // Highest underdog price seen (for exit detection)
	PeakFavoriteMid float64      // Highest favorite price seen
}

// PricePoint represents a price snapshot for tracking odds movements
type PricePoint struct {
	Timestamp time.Time
	YesMid    float64
	NoMid     float64
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
	if !s.Config.SportsEnabled {
		log.Println("Sports: Strategy disabled")
		return
	}

	if s.Config.SportsDeltaNeutral {
		log.Println("Sports: Starting MONEYLINE DELTA NEUTRAL strategy...")
		log.Printf("Sports: Buy underdog if < %.0f%%, favorite if < %.0f%%", MaxUnderdogPrice*100, MaxFavoritePrice*100)
	} else {
		log.Println("Sports: Starting MONEYLINE SINGLE-SIDE strategy...")
		log.Printf("Sports: Buy underdog only if < %.0f%%", MaxUnderdogPrice*100)
	}
	log.Printf("Sports: Exit when position hits %.0f%% profit", s.Config.TakeProfitPercent*100)

	// Increase discovery interval - discovery takes 3+ minutes due to many sports tags
	discoveryTicker := time.NewTicker(5 * time.Minute)
	defer discoveryTicker.Stop()

	priceInterval := time.Duration(s.Config.PriceUpdateInterval) * time.Second
	log.Printf("Sports: Setting price update interval to %v", priceInterval)
	priceTicker := time.NewTicker(priceInterval)
	defer priceTicker.Stop()

	s.discoverMarkets()
	log.Printf("Sports: Initial discovery complete, entering main loop with %d markets", len(s.trackedMarkets))

	for {
		select {
		case <-s.ctx.Done():
			log.Println("Sports: Strategy stopped")
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

// isMoneylineMarket checks if this is a simple win/lose moneyline market
// Excludes: O/U, Spread, futures, player props
func (s *Strategy) isMoneylineMarket(market *gamma.Market) bool {
	question := market.Question
	questionLower := strings.ToLower(question)

	// EXCLUDE Over/Under, Spread, and other non-moneyline markets
	excludePatterns := []string{
		// O/U variants
		"o/u ", "o/u:", ": o/u", "over/under", "total points", "total goals",
		// Halves (1H = first half)
		"1h ", "1h:", ": 1h", "2h ", "2h:", "first half", "second half",
		// Spreads
		"spread", "spread:", "handicap",
		"+1.5", "+2.5", "+3.5", "+4.5", "+5.5", "+6.5", "+7.5", "+8.5", "+9.5", "+10.5", "+11.5",
		"-1.5", "-2.5", "-3.5", "-4.5", "-5.5", "-6.5", "-7.5", "-8.5", "-9.5", "-10.5", "-11.5",
		// Totals
		"team total", "total runs", "total touchdowns", "total sacks",
		// Futures
		"first to", "most valuable", "mvp", "rookie of", "coach of",
		"will win the 202", "win the 203", "championship",
		"make the playoffs", "all-star",
		// Player props
		"passing yards", "receiving yards", "rushing yards", "receptions",
		"home run", "touchdown", "strikeout", "goals scored",
		"how many", "both teams to score",
		// Special events
		"battle of the", "games total",
	}

	for _, pattern := range excludePatterns {
		if strings.Contains(questionLower, pattern) {
			return false
		}
	}

	// MUST be a direct matchup (vs or @)
	hasVs := strings.Contains(questionLower, " vs ") || strings.Contains(questionLower, " vs. ")
	// hasAt := strings.Contains(question, " @ ")

	if !hasVs {
		return false
	}

	// Additional exclusions
	hardExclude := []string{
		"will ", "who will", "what will",
		"gold ", "bitcoin", "btc ", "eth ", "crypto",
		"movie", "oscar", "grammy", "emmy", "award",
		"stock", "close between", "close above", "close below",
		"president", "election", "trump", "biden", "elon",
		"ai model", "gpt", "claude", "openai",
		"weather", "temperature",
	}

	for _, exclude := range hardExclude {
		if strings.Contains(questionLower, exclude) {
			return false
		}
	}

	// Debug: log what passes through the filter
	log.Printf("Sports: FILTER PASSED: %s", question)
	return true
}

func (s *Strategy) discoverMarkets() {
	log.Println("Sports: Discovering MONEYLINE markets via API filter...")

	moneylineFound := 0
	skippedTooFar := 0

	// Use the sports_market_types=moneyline filter to get moneyline markets directly
	// This is much more reliable than string matching
	for page := 0; page < 50; page++ { // 50 pages = 5000 markets max
		markets, err := s.GammaClient.GetMarkets(gamma.GetMarketsParams{
			Limit:             100,
			Offset:            page * 100,
			Active:            true,
			SportsMarketTypes: "moneyline",
			Order:             "volume",
		})
		if err != nil {
			log.Printf("Sports: Failed to fetch moneyline markets page %d: %v", page, err)
			break
		}

		if len(markets) == 0 {
			break
		}

		for _, market := range markets {
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

			if !s.isMoneylineMarket(&market) {
				continue
			}

			tokenPair, err := market.GetClobTokenPair()
			if err != nil {
				continue
			}

			outcomeNames, _ := market.GetOutcomeNames()

			// Copy market to avoid pointer issues
			marketCopy := market
			s.mu.Lock()
			s.trackedMarkets[market.ID] = &TrackedMarket{
				Market:       &marketCopy,
				TokenPair:    tokenPair,
				OutcomeNames: outcomeNames,
				GameTime:     gameTime,
			}
			s.mu.Unlock()

			moneylineFound++
			timeStr := "unknown"
			if !gameTime.IsZero() {
				timeStr = fmt.Sprintf("%.1fh", time.Until(gameTime).Hours())
			}
			log.Printf("Sports: 🏀 MONEYLINE [%s]: %s", timeStr, truncateQuestion(market.Question))
		}
	}

	s.mu.RLock()
	log.Printf("Sports: Found %d moneylines (<48h), skipped %d too far, tracking %d total", moneylineFound, skippedTooFar, len(s.trackedMarkets))
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

	log.Printf("Sports: updatePricesAndTrade starting with %d markets...", len(s.trackedMarkets))
	now := time.Now()

	for marketID, tracked := range s.trackedMarkets {
		if tracked.Market.Closed {
			delete(s.trackedMarkets, marketID)
			continue
		}

		// Remove stale markets
		if !tracked.GameTime.IsZero() && now.After(tracked.GameTime.Add(6*time.Hour)) {
			log.Printf("Sports: Game ended: %s", truncateQuestion(tracked.Market.Question))
			delete(s.trackedMarkets, marketID)
			continue
		}

		// Determine game status
		if !tracked.GameTime.IsZero() {
			if now.Before(tracked.GameTime) {
				tracked.GameStatus = "PRE_GAME"
			} else if now.After(tracked.GameTime) && now.Before(tracked.GameTime.Add(4*time.Hour)) {
				tracked.GameStatus = "LIVE"
			} else {
				tracked.GameStatus = "FINAL"
			}
		} else {
			tracked.GameStatus = "PRE_GAME"
		}

		// Fetch prices for both sides
		yesBid, yesAsk, yesMid, yesValid := s.fetchMarketPrices(tracked.TokenPair.Yes)
		noBid, noAsk, noMid, noValid := s.fetchMarketPrices(tracked.TokenPair.No)

		if !yesValid || !noValid {
			continue
		}

		tracked.YesBid = yesBid
		tracked.YesAsk = yesAsk
		tracked.YesMid = yesMid
		tracked.NoBid = noBid
		tracked.NoAsk = noAsk
		tracked.NoMid = noMid
		tracked.IsYesUnderdog = yesMid < noMid
		tracked.LastUpdate = now

		// Add to price history
		maxHistory := s.Config.SportsOddsHistorySize
		tracked.PriceHistory = append(tracked.PriceHistory, PricePoint{
			Timestamp: now,
			YesMid:    yesMid,
			NoMid:     noMid,
		})
		if len(tracked.PriceHistory) > maxHistory {
			tracked.PriceHistory = tracked.PriceHistory[1:]
		}

		// Calculate odds shift
		if len(tracked.PriceHistory) >= 3 {
			tracked.LastOddsShift = s.calculateOddsShift(tracked)
		}

		// Track peak prices
		var underdogMid, favoriteMid float64
		if tracked.IsYesUnderdog {
			underdogMid, favoriteMid = yesMid, noMid
		} else {
			underdogMid, favoriteMid = noMid, yesMid
		}

		if underdogMid > tracked.PeakUnderdogMid {
			tracked.PeakUnderdogMid = underdogMid
		}
		if favoriteMid > tracked.PeakFavoriteMid {
			tracked.PeakFavoriteMid = favoriteMid
		}

		// Update risk manager with BID prices (actual exit value)
		s.RiskManager.UpdatePrice(tracked.TokenPair.Yes, yesBid)
		s.RiskManager.UpdatePrice(tracked.TokenPair.No, noBid)

		// Check for exits first (freeing up slots)
		s.checkExitsForSports(tracked, now)
	}

	// === PRIORITY RANKING FOR ENTRIES ===
	// We want to sort markets so we check the most profitable ones first
	var rankedMarkets []*TrackedMarket

	for _, tracked := range s.trackedMarkets {
		if tracked.Market.Closed || (!tracked.GameTime.IsZero() && now.After(tracked.GameTime.Add(6*time.Hour))) {
			continue // Already handled in the cleanup loop
		}

		// Only consider markets where we have valid ask prices
		if tracked.YesAsk > 0 && tracked.NoAsk > 0 {
			rankedMarkets = append(rankedMarkets, tracked)
		}
	}

	// Sort explicitly by potential arbitrage spread (YesAsk + NoAsk) ascending
	// Smaller combinedAsk = better arbitrage spread
	sort.Slice(rankedMarkets, func(i, j int) bool {
		askI := rankedMarkets[i].YesAsk + rankedMarkets[i].NoAsk
		askJ := rankedMarkets[j].YesAsk + rankedMarkets[j].NoAsk
		return askI < askJ
	})

	// Now try to enter trades in order of profitability
	for _, tracked := range rankedMarkets {
		// Look for entry or hedge opportunities
		s.analyzeAndTradeSports(tracked)
	}
}

func (s *Strategy) analyzeAndTradeSports(tracked *TrackedMarket) {
	// Identify favorite and underdog tokens
	var underdogToken, favoriteToken string
	var underdogAsk, favoriteAsk float64
	var underdogMid, favoriteMid float64
	var favoriteName string

	if tracked.IsYesUnderdog {
		underdogToken, favoriteToken = tracked.TokenPair.Yes, tracked.TokenPair.No
		underdogAsk, favoriteAsk = tracked.YesAsk, tracked.NoAsk
		underdogMid, favoriteMid = tracked.YesMid, tracked.NoMid
		if len(tracked.OutcomeNames) >= 2 {
			favoriteName = tracked.OutcomeNames[1]
		}
	} else {
		underdogToken, favoriteToken = tracked.TokenPair.No, tracked.TokenPair.Yes
		underdogAsk, favoriteAsk = tracked.NoAsk, tracked.YesAsk
		underdogMid, favoriteMid = tracked.NoMid, tracked.YesMid
		if len(tracked.OutcomeNames) >= 2 {
			favoriteName = tracked.OutcomeNames[0]
		}
	}

	// Check if already has ANY position in this market
	hasUnderdog := len(s.RiskManager.GetPositionByToken(underdogToken)) > 0
	hasFavorite := len(s.RiskManager.GetPositionByToken(favoriteToken)) > 0

	// Check if we have room for more sports positions
	if !s.RiskManager.CanAddPositionForStrategy("sports") {
		return
	}

	// === 1. SIMULTANEOUS SPREAD ARBITRAGE (PRIORITY) ===
	combinedAsk := tracked.YesAsk + tracked.NoAsk

	// If AskYes + AskNo < 1.0 (minus fees), there's guaranteed profit if filled
	maxSpread := 0.98 // Can make this configurable later
	if combinedAsk > 0 && combinedAsk < maxSpread && !hasUnderdog && !hasFavorite {
		log.Printf("Sports: 💰 SPREAD ARB FOUND - Market: %s (AskYes: %.4f + AskNo: %.4f = %.4f)",
			truncateQuestion(tracked.Market.Question), tracked.YesAsk, tracked.NoAsk, combinedAsk)

		s.executeSpreadArb(tracked)
		return
	}

	// === 2. DYNAMIC HEDGING LOGIC (Gabagool Sports Fallback) ===

	// Case 2A: Already holding Favorite, check for Arb opportunity on Underdog
	if hasFavorite && !hasUnderdog {
		favPositions := s.RiskManager.GetPositionByToken(favoriteToken)
		if len(favPositions) > 0 {
			favPos := favPositions[0]
			if favPos.EntryPrice+underdogAsk <= maxSpread {
				log.Printf("Sports: 🍖 HEDGE ARB - %s (Favorite @ %.4f + Underdog Ask @ %.4f = %.4f)",
					favoriteName, favPos.EntryPrice, underdogAsk, favPos.EntryPrice+underdogAsk)
				s.executeSecondLeg(tracked, underdogToken, underdogAsk, underdogMid, "Underdog")
			}
		}
		return
	}

	// Case 2B: Already holding Underdog, check for Arb opportunity on Favorite
	if hasUnderdog && !hasFavorite {
		dogPositions := s.RiskManager.GetPositionByToken(underdogToken)
		if len(dogPositions) > 0 {
			dogPos := dogPositions[0]
			if dogPos.EntryPrice+favoriteAsk <= maxSpread {
				log.Printf("Sports: 🍖 HEDGE ARB - %s (Underdog @ %.4f + Favorite Ask @ %.4f = %.4f)",
					"Underdog", dogPos.EntryPrice, favoriteAsk, dogPos.EntryPrice+favoriteAsk)
				s.executeSecondLeg(tracked, favoriteToken, favoriteAsk, favoriteMid, favoriteName)
			}
		}
		return
	}

	// === 3. FIRST LEG / ENTRY ===
	// Case 3C: No positions, look for initial Favorite entry
	if !hasFavorite && !hasUnderdog {
		// Entry criteria for Favorite
		if favoriteMid <= MaxFavoritePrice && favoriteMid >= MinValidPrice {
			log.Printf("Sports: 🎯 ENTRY FAVORITE - %s @ %.4f (ask: %.4f)",
				favoriteName, favoriteMid, favoriteAsk)
			s.executeSingleSideEntry(tracked, favoriteToken, favoriteAsk, favoriteMid, favoriteName)
		}
	}
}

// executeSpreadArb executes a full spread arbitrage (both legs immediately)
// Uses EQUAL SHARE QUANTITIES to ensure guaranteed profit
func (s *Strategy) executeSpreadArb(tracked *TrackedMarket) {
	yesPrice := tracked.YesAsk
	noPrice := tracked.NoAsk

	// Calculate sizes - EQUAL SHARE QUANTITIES
	maxCost := s.Config.MaxPositionSize
	halfCost := maxCost / 2

	yesSharesMax := halfCost / yesPrice
	noSharesMax := halfCost / noPrice

	shareQty := yesSharesMax
	if noSharesMax < yesSharesMax {
		shareQty = noSharesMax
	}

	yesCost := shareQty * yesPrice
	noCost := shareQty * noPrice
	totalCost := yesCost + noCost
	potentialPayout := shareQty
	fees := totalCost * s.Config.TradingFeePercent
	guaranteedProfit := potentialPayout - totalCost - fees
	profitPercent := guaranteedProfit / totalCost

	if profitPercent < s.Config.MinProfitAfterFees {
		log.Printf("Sports: SKIP spread arb - profit %.2f%% below min %.2f%% (fees: $%.4f)",
			profitPercent*100, s.Config.MinProfitAfterFees*100, fees)
		return
	}

	if guaranteedProfit <= 0 {
		return
	}

	// Risk check
	if err := s.RiskManager.CheckEntry(tracked.TokenPair.Yes, yesPrice, shareQty); err != nil {
		log.Printf("Sports: Risk check failed: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("Sports: [DRY RUN] SPREAD ARB (Equal shares: %.2f)", shareQty)
		log.Printf("  YES @ %.4f × %.2f shares = $%.2f", yesPrice, shareQty, yesCost)
		log.Printf("  NO @ %.4f × %.2f shares = $%.2f", noPrice, shareQty, noCost)
		log.Printf("  Total cost: $%.2f, Guaranteed profit: $%.4f (%.2f%%)",
			totalCost, guaranteedProfit, profitPercent*100)
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
			log.Printf("Sports: YES order failed: %v", err)
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
			log.Printf("Sports: NO order failed: %v", err)
			return
		}
	}

	// Track both positions
	var yesName, noName string
	if len(tracked.OutcomeNames) >= 2 {
		yesName = tracked.OutcomeNames[0]
		noName = tracked.OutcomeNames[1]
	} else {
		yesName = "Yes"
		noName = "No"
	}

	yesPos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      tracked.TokenPair.Yes,
		OutcomeName:  yesName,
		Size:         shareQty,
		EntryPrice:   yesPrice,
		CurrentPrice: yesPrice,
		Side:         "BUY",
		Type:         risk.TypeArbitrage,
		Strategy:     "sports",
		TotalCost:    yesCost,
	}
	yesID := s.RiskManager.AddPosition(yesPos)

	noPos := &risk.Position{
		MarketID:         tracked.Market.ID,
		TokenID:          tracked.TokenPair.No,
		OutcomeName:      noName,
		Size:             shareQty,
		EntryPrice:       noPrice,
		CurrentPrice:     noPrice,
		Side:             "BUY",
		Type:             risk.TypeArbitrage,
		Strategy:         "sports",
		TotalCost:        noCost,
		PairedPositionID: yesID, // Link to YES position
	}
	noID := s.RiskManager.AddPosition(noPos)

	if yesPosition := s.RiskManager.GetPosition(yesID); yesPosition != nil {
		yesPosition.PairedPositionID = noID
	}

	tracked.HasPosition = true
	log.Printf("Sports: Spread arb executed - %.2f shares each side, guaranteed profit: $%.4f", shareQty, guaranteedProfit)
}

// executeSecondLeg completes the arb/delta-neutral position
func (s *Strategy) executeSecondLeg(tracked *TrackedMarket, tokenID string, ask, mid float64, name string) {
	maxCost := s.Config.MaxPositionSize / 2
	size := maxCost / ask

	if s.Config.IsDryRun() {
		log.Printf("Sports: [DRY RUN] HEDGING %s @ %.4f (size: %.2f)", name, ask, size)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   tokenID,
			Price:     ask,
			Size:      size,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Sports: Hedge order failed: %v", err)
			return
		}
	}

	pos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      tokenID,
		OutcomeName:  name,
		Size:         size,
		EntryPrice:   mid,
		CurrentPrice: mid,
		Side:         "BUY",
		Type:         risk.TypeArbitrage,
		Strategy:     "sports",
	}
	s.RiskManager.AddPosition(pos)
}

// executeSingleSideEntry buys only the underdog side (no delta neutral)
func (s *Strategy) executeSingleSideEntry(
	tracked *TrackedMarket,
	underdogToken string,
	underdogAsk, underdogMid float64,
	underdogName string,
) {
	maxCost := s.Config.MaxPositionSize
	underdogSize := maxCost / underdogAsk
	totalCost := underdogAsk * underdogSize

	// Risk check
	if err := s.RiskManager.CheckEntry(underdogToken, underdogAsk, underdogSize); err != nil {
		log.Printf("Sports: Risk check failed: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("Sports: [DRY RUN] SINGLE-SIDE ENTRY")
		log.Printf("  BUY %s (underdog) @ %.4f (size: %.2f, cost: $%.2f)", underdogName, underdogAsk, underdogSize, totalCost)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   underdogToken,
			Price:     underdogAsk,
			Size:      underdogSize,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Sports: Order failed: %v", err)
			return
		}
	}

	// Track position (no paired position for single-side)
	pos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      underdogToken,
		OutcomeName:  underdogName,
		Size:         underdogSize,
		EntryPrice:   underdogMid,
		CurrentPrice: underdogMid,
		Side:         "BUY",
		Type:         risk.TypeValueBet,
		Strategy:     "sports",
		TotalCost:    totalCost,
	}
	s.RiskManager.AddPosition(pos)

	tracked.HasPosition = true
	log.Printf("Sports: ✅ Single-side position opened - Cost: $%.2f", totalCost)
}

func (s *Strategy) checkExitsForSports(tracked *TrackedMarket, now time.Time) {
	positions := s.RiskManager.GetPositionsByStrategy("sports")

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
			if pos.TokenID == tracked.TokenPair.Yes {
				totalValue += tracked.YesBid * pos.Size
			} else {
				totalValue += tracked.NoBid * pos.Size
			}
		}

		profitPercent := (totalValue - totalCost) / totalCost
		// If guaranteed profit exceeds 2%, take it and "scrape"
		if profitPercent >= 0.02 {
			log.Printf("Sports: 🤏 SCRAPE CENTS - Combined profit @ %.1f%%, closing all", profitPercent*100)
			for _, pos := range marketPositions {
				s.executeExit(pos)
			}
			tracked.HasPosition = false
			return
		}
	}

	// 2. INDEPENDENT POSITION MONITORING (TP, SL, Peak)
	for _, pos := range marketPositions {
		if now.Sub(pos.EntryTime) < EntryGracePeriod {
			continue
		}

		var currentExitPrice float64
		if pos.TokenID == tracked.TokenPair.Yes {
			currentExitPrice = tracked.YesBid
		} else {
			currentExitPrice = tracked.NoBid
		}

		pos.CurrentPrice = currentExitPrice
		pnlPercent := (currentExitPrice - pos.EntryPrice) / pos.EntryPrice

		// TAKE PROFIT (Target 95%+)
		if currentExitPrice >= 0.95 || pnlPercent >= s.Config.TakeProfitPercent {
			log.Printf("Sports: 🎯 EXIT TARGET - %s @ %.4f (entry: %.4f, +%.1f%%)",
				pos.OutcomeName, currentExitPrice, pos.EntryPrice, pnlPercent*100)
			s.executeExit(pos)

			// If this was part of a hedged pair, we might want to close the other side too
			// to fully "scrape" the win.
			if len(marketPositions) >= 2 {
				log.Printf("  Closing hedge partner to capture full arb profit")
				for _, other := range marketPositions {
					if other.ID != pos.ID && other.State == risk.StateOpen {
						s.executeExit(other)
					}
				}
			}
			tracked.HasPosition = false
			return
		}

		// STOP LOSS (Independent)
		if pnlPercent <= -s.Config.StopLossPercent {
			log.Printf("Sports: 🛑 STOP LOSS - %s @ %.4f (entry: %.4f, %.1f%%)",
				pos.OutcomeName, currentExitPrice, pos.EntryPrice, pnlPercent*100)
			s.executeExit(pos)
			// Let the other side run (it's either a hedge or a winning bet)
		}
	}

	// 3. PEAK DETECTION (Dynamic Reversal for Underdogs)
	if tracked.GameStatus == "LIVE" && s.detectPeakAndExit(tracked) {
		log.Printf("Sports: 📊 PEAK DETECTED - Odds reversing, exiting remaining positions")
		for _, pos := range marketPositions {
			s.executeExit(pos)
		}
		tracked.HasPosition = false
	}
}

func (s *Strategy) executeExit(pos *risk.Position) {
	// Use bid price for exits (what buyers are willing to pay), not midpoint
	// This ensures the order actually fills
	bidPrice := pos.CurrentPrice // fallback to current price
	if book, err := s.ClobClient.GetOrderBookWithPrices(pos.TokenID); err == nil && book.BestBid > 0 {
		bidPrice = book.BestBid
	}

	if s.Config.IsDryRun() {
		pnl := (bidPrice - pos.EntryPrice) * pos.Size
		log.Printf("Sports: [DRY RUN] Would SELL %s @ %.4f (bid, P&L: $%.2f)",
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
			log.Printf("Sports: Exit failed: %v", err)
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
