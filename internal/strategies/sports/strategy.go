package sports

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

// Entry thresholds - buy both sides when prices are in these ranges
const (
	MaxUnderdogPrice = 0.40 // Buy underdog if < 50%
	MaxFavoritePrice = 0.65 // Buy favorite if < 65% (since underdog+favorite ≈ 1.0)
	MinValidPrice    = 0.05 // Minimum price to consider
	MaxSpreadPercent = 0.50 // Allow wider spreads for sports
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
	Market      *gamma.Market
	TokenPair   *gamma.ClobTokenPair
	OutcomeNames []string // e.g., ["Lakers", "Warriors"] or ["Yes", "No"]
	
	// Prices - index 0 is typically underdog, index 1 favorite
	YesBid      float64
	YesAsk      float64
	YesMid      float64
	NoBid       float64
	NoAsk       float64
	NoMid       float64
	
	LastUpdate  time.Time
	HasPosition bool
	GameTime    time.Time
	
	// Track which side is underdog (lower price = underdog)
	IsYesUnderdog bool
	
	// Live game tracking
	GameStatus      string        // "PRE_GAME", "LIVE", "FINAL"
	PriceHistory    []PricePoint  // Last N price points for momentum detection
	LastOddsShift   float64       // Recent odds change (positive = improving)
	PeakUnderdogMid float64       // Highest underdog price seen (for exit detection)
	PeakFavoriteMid float64       // Highest favorite price seen
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
		
		// Determine game status (PRE_GAME or LIVE)
		if !tracked.GameTime.IsZero() {
			if now.Before(tracked.GameTime) {
				tracked.GameStatus = "PRE_GAME"
			} else if now.After(tracked.GameTime) && now.Before(tracked.GameTime.Add(4*time.Hour)) {
				// Most games are 2-3 hours, 4 hours is safe buffer
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

		// DEBUG: Log first few markets to see why prices fail
		if !yesValid || !noValid {
			log.Printf("Sports: PRICE FAIL %s | YesValid=%v (bid=%.4f ask=%.4f) | NoValid=%v (bid=%.4f ask=%.4f)",
				truncateQuestion(tracked.Market.Question), yesValid, yesBid, yesAsk, noValid, noBid, noAsk)
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
		
		// Add to price history (keep last N points)
		maxHistory := s.Config.SportsOddsHistorySize
		tracked.PriceHistory = append(tracked.PriceHistory, PricePoint{
			Timestamp: now,
			YesMid:    yesMid,
			NoMid:     noMid,
		})
		if len(tracked.PriceHistory) > maxHistory {
			tracked.PriceHistory = tracked.PriceHistory[1:] // Remove oldest
		}
		
		// Calculate odds shift if we have enough history
		if len(tracked.PriceHistory) >= 3 {
			tracked.LastOddsShift = s.calculateOddsShift(tracked)
		}
		
		// Track peak prices for exit detection
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

		// Update risk manager with prices for existing positions
		s.RiskManager.UpdatePrice(tracked.TokenPair.Yes, yesMid)
		s.RiskManager.UpdatePrice(tracked.TokenPair.No, noMid)

		// Check for exits first
		s.checkExitsForDeltaNeutral(tracked, now)

		// Skip if already has position
		if s.RiskManager.HasPositionForMarket(marketID) {
			continue
		}

		// Look for entry opportunities (pre-game or live)
		s.analyzeAndTradeDeltaNeutral(tracked)
	}
}

func (s *Strategy) analyzeAndTradeDeltaNeutral(tracked *TrackedMarket) {
	// Check if we have room for more sports positions (max 30 for sports)
	if !s.RiskManager.CanAddPositionForStrategy("sports") {
		return // Max sports positions reached
	}

	// Determine which is underdog and which is favorite
	var underdogMid, favoriteMid float64
	var underdogAsk, favoriteAsk float64
	var underdogToken, favoriteToken string
	var underdogName, favoriteName string

	if tracked.IsYesUnderdog {
		underdogMid, underdogAsk, underdogToken = tracked.YesMid, tracked.YesAsk, tracked.TokenPair.Yes
		favoriteMid, favoriteAsk, favoriteToken = tracked.NoMid, tracked.NoAsk, tracked.TokenPair.No
		if len(tracked.OutcomeNames) >= 2 {
			underdogName = tracked.OutcomeNames[0]
			favoriteName = tracked.OutcomeNames[1]
		} else {
			underdogName, favoriteName = "YES", "NO"
		}
	} else {
		underdogMid, underdogAsk, underdogToken = tracked.NoMid, tracked.NoAsk, tracked.TokenPair.No
		favoriteMid, favoriteAsk, favoriteToken = tracked.YesMid, tracked.YesAsk, tracked.TokenPair.Yes
		if len(tracked.OutcomeNames) >= 2 {
			underdogName = tracked.OutcomeNames[1]
			favoriteName = tracked.OutcomeNames[0]
		} else {
			underdogName, favoriteName = "NO", "YES"
		}
	}

	// Check for LIVE GAME opportunities first (if enabled)
	if tracked.GameStatus == "LIVE" {
		// LIVE GAME STRATEGY 1: Underdog odds improvement (momentum play)
		// This is for opportunistic entries during the game
		if s.shouldEnterOnOddsImprovement(tracked) {
			log.Printf("Sports: 🔥 LIVE GAME - Odds Shift Detected!")
			log.Printf("  Market: %s (%s)", truncateQuestion(tracked.Market.Question), tracked.GameStatus)
			log.Printf("  %s (underdog): %.4f mid (shift: +%.1f%%), ask: %.4f", 
				underdogName, underdogMid, tracked.LastOddsShift*100, underdogAsk)
			
			if s.Config.SportsDeltaNeutral {
				s.executeDeltaNeutralEntry(tracked, underdogToken, favoriteToken, underdogAsk, favoriteAsk, 
					underdogMid, favoriteMid, underdogName, favoriteName)
			} else {
				s.executeSingleSideEntry(tracked, underdogToken, underdogAsk, underdogMid, underdogName)
			}
			return
		}
		
		// LIVE GAME STRATEGY 2: Late game favorite (high probability win)
		if s.shouldEnterLateGameFavorite(tracked, favoriteMid) {
			log.Printf("Sports: ⚡ LATE GAME FAVORITE (40+ mins)")
			log.Printf("  Market: %s", truncateQuestion(tracked.Market.Question))
			log.Printf("  %s (favorite): %.4f mid (%.0f%% prob), ask: %.4f", 
				favoriteName, favoriteMid, favoriteMid*100, favoriteAsk)
			
			// Single-side entry on favorite (high conviction)
			s.executeSingleSideEntry(tracked, favoriteToken, favoriteAsk, favoriteMid, favoriteName)
			return
		}
		
		// If LIVE_GAME_ONLY mode, skip pre-game style entries
		if s.Config.SportsLiveGameOnly {
			return
		}
	}

	// PRE-GAME STRATEGY: Buy FAVORITE (and optionally underdog for delta neutral)
	// Main strategy: Buy favorite before game, sell during game when odds increase to ~100%
	// Delta neutral: Also buy underdog to hedge against favorite losing
	
	// DEBUG: Log actual prices for first few markets each cycle
	combinedSpread := underdogMid + favoriteMid
	if underdogMid > 0 && favoriteMid > 0 {
		log.Printf("Sports: DEBUG %s [%s] | Favorite: %.4f | Underdog: %.4f | Combined: %.4f | Shift: %.2f%%",
			truncateQuestion(tracked.Market.Question), tracked.GameStatus,
			favoriteMid, underdogMid, combinedSpread, tracked.LastOddsShift*100)
	}

	// Check if FAVORITE is in reasonable range for pre-game entry
	// We want to buy favorite when it's not too expensive (< 80%)
	favoriteInRange := favoriteMid >= MinValidPrice && favoriteMid <= 0.80

	// Check mode: delta neutral (buy both) or single-side (buy favorite only)
	if s.Config.SportsDeltaNeutral {
		// DELTA NEUTRAL MODE: Buy both sides pre-game
		// This protects against favorite losing (underdog offsets loss)
		underdogInRange := underdogMid >= MinValidPrice && underdogMid <= MaxUnderdogPrice

		if favoriteInRange && underdogInRange {
			log.Printf("Sports: 💰 DELTA NEUTRAL PRE-GAME (%s)", tracked.GameStatus)
			log.Printf("  Market: %s", truncateQuestion(tracked.Market.Question))
			log.Printf("  %s (favorite): %.4f mid (ask: %.4f) - Will sell when odds → 95-100%%", favoriteName, favoriteMid, favoriteAsk)
			log.Printf("  %s (underdog): %.4f mid (ask: %.4f) - Hedge position", underdogName, underdogMid, underdogAsk)
			log.Printf("  Combined: %.4f (%.2f%% edge)", combinedSpread, (1.0-combinedSpread)*100)

			s.executeDeltaNeutralEntry(tracked, underdogToken, favoriteToken, underdogAsk, favoriteAsk, underdogMid, favoriteMid, underdogName, favoriteName)
		}
	} else {
		// SINGLE-SIDE MODE: Buy favorite only
		if favoriteInRange {
			log.Printf("Sports: 💰 FAVORITE PRE-GAME (%s)", tracked.GameStatus)
			log.Printf("  Market: %s", truncateQuestion(tracked.Market.Question))
			log.Printf("  %s (favorite): %.4f mid (ask: %.4f) - Will sell when odds → 95-100%%", favoriteName, favoriteMid, favoriteAsk)

			s.executeSingleSideEntry(tracked, favoriteToken, favoriteAsk, favoriteMid, favoriteName)
		}
	}
}

func (s *Strategy) executeDeltaNeutralEntry(
	tracked *TrackedMarket, 
	underdogToken, favoriteToken string,
	underdogAsk, favoriteAsk float64,
	underdogMid, favoriteMid float64,
	underdogName, favoriteName string,
) {
	// Split position equally
	maxCost := s.Config.MaxPositionSize
	halfCost := maxCost / 2

	underdogSize := halfCost / underdogAsk
	favoriteSize := halfCost / favoriteAsk

	totalCost := (underdogAsk * underdogSize) + (favoriteAsk * favoriteSize)

	// Risk check
	if err := s.RiskManager.CheckEntry(underdogToken, underdogAsk, underdogSize); err != nil {
		log.Printf("Sports: Risk check failed: %v", err)
		return
	}

	if s.Config.IsDryRun() {
		log.Printf("Sports: [DRY RUN] DELTA NEUTRAL ENTRY")
		log.Printf("  BUY %s @ %.4f (size: %.2f, cost: $%.2f)", underdogName, underdogAsk, underdogSize, underdogAsk*underdogSize)
		log.Printf("  BUY %s @ %.4f (size: %.2f, cost: $%.2f)", favoriteName, favoriteAsk, favoriteSize, favoriteAsk*favoriteSize)
		log.Printf("  Total cost: $%.2f", totalCost)
	} else {
		// Place both orders
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   underdogToken,
			Price:     underdogAsk,
			Size:      underdogSize,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Sports: Underdog order failed: %v", err)
			return
		}

		_, err = s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   favoriteToken,
			Price:     favoriteAsk,
			Size:      favoriteSize,
			Side:      clob.Buy,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Sports: Favorite order failed: %v", err)
			return
		}
	}

	// Track positions as paired
	underdogPos := &risk.Position{
		MarketID:     tracked.Market.ID,
		TokenID:      underdogToken,
		OutcomeName:  underdogName,
		Size:         underdogSize,
		EntryPrice:   underdogMid,
		CurrentPrice: underdogMid,
		Side:         "BUY",
		Type:         risk.TypeDeltaNeutral,
		Strategy:     "sports",
		TotalCost:    totalCost,
	}
	underdogID := s.RiskManager.AddPosition(underdogPos)

	favoritePos := &risk.Position{
		MarketID:         tracked.Market.ID,
		TokenID:          favoriteToken,
		OutcomeName:      favoriteName,
		Size:             favoriteSize,
		EntryPrice:       favoriteMid,
		CurrentPrice:     favoriteMid,
		Side:             "BUY",
		Type:             risk.TypeDeltaNeutral,
		Strategy:         "sports",
		TotalCost:        totalCost,
		PairedPositionID: underdogID,
	}
	favoriteID := s.RiskManager.AddPosition(favoritePos)

	// Link the underdog to favorite
	if uPos := s.RiskManager.GetPosition(underdogID); uPos != nil {
		uPos.PairedPositionID = favoriteID
	}

	tracked.HasPosition = true
	log.Printf("Sports: ✅ Delta neutral position opened - Total cost: $%.2f", totalCost)
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

func (s *Strategy) checkExitsForDeltaNeutral(tracked *TrackedMarket, now time.Time) {
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
	
	// LIVE GAME EXIT STRATEGY 1: Sell favorite when odds → 95-100%
	// This is the main profit mechanism for pre-game favorite entries
	// Exit BOTH positions when favorite reaches 95%+
	if tracked.GameStatus == "LIVE" {
		for _, pos := range marketPositions {
			// Skip grace period
			if now.Sub(pos.EntryTime) < EntryGracePeriod {
				continue
			}
			
			var currentPrice float64
			var isFavorite bool
			
			// Determine if this position is the favorite
			if tracked.IsYesUnderdog {
				// No is favorite
				isFavorite = pos.TokenID == tracked.TokenPair.No
				if isFavorite {
					currentPrice = tracked.NoMid
				} else {
					currentPrice = tracked.YesMid
				}
			} else {
				// Yes is favorite
				isFavorite = pos.TokenID == tracked.TokenPair.Yes
				if isFavorite {
					currentPrice = tracked.YesMid
				} else {
					currentPrice = tracked.NoMid
				}
			}
			
			pos.CurrentPrice = currentPrice
			pnlPercent := (currentPrice - pos.EntryPrice) / pos.EntryPrice
			
			// If this is a favorite position and odds are now 95%+, sell BOTH for profit
			if isFavorite && currentPrice >= 0.95 {
				log.Printf("Sports: 💎 FAVORITE AT 95%%+ - Closing full position!")
				log.Printf("  %s (favorite) @ %.4f (entry: %.4f, +%.1f%%)",
					pos.OutcomeName, currentPrice, pos.EntryPrice, pnlPercent*100)
				s.executeExit(pos)
				
				// Also exit paired position if delta neutral
				if pos.PairedPositionID != "" {
					if pairedPos := s.RiskManager.GetPosition(pos.PairedPositionID); pairedPos != nil {
						if pairedPos.State == risk.StateOpen {
							var pairedPrice float64
							if pairedPos.TokenID == tracked.TokenPair.Yes {
								pairedPrice = tracked.YesMid
							} else {
								pairedPrice = tracked.NoMid
							}
							pairedPos.CurrentPrice = pairedPrice
							pairedPnlPercent := (pairedPrice - pairedPos.EntryPrice) / pairedPos.EntryPrice
							
							log.Printf("  %s (underdog) @ %.4f (entry: %.4f, %.1f%%) - Closing hedge",
								pairedPos.OutcomeName, pairedPrice, pairedPos.EntryPrice, pairedPnlPercent*100)
							s.executeExit(pairedPos)
						}
					}
				}
				tracked.HasPosition = false
				return
			}
		}
	}
	
	// LIVE GAME EXIT STRATEGY 2: Check for peak detection (odds reversal)
	if tracked.GameStatus == "LIVE" && s.detectPeakAndExit(tracked) {
		log.Printf("Sports: 📊 PEAK DETECTED - Odds reversing, exiting all positions")
		for _, pos := range marketPositions {
			var currentPrice float64
			if pos.TokenID == tracked.TokenPair.Yes {
				currentPrice = tracked.YesMid
			} else {
				currentPrice = tracked.NoMid
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

	// Check for TAKE PROFIT - exit BOTH positions (strategy complete)
	for _, pos := range marketPositions {
		// Grace period
		if now.Sub(pos.EntryTime) < EntryGracePeriod {
			continue
		}

		// Get current price
		var currentPrice float64
		if pos.TokenID == tracked.TokenPair.Yes {
			currentPrice = tracked.YesMid
		} else {
			currentPrice = tracked.NoMid
		}

		if currentPrice < MinValidPrice {
			continue
		}

		pos.CurrentPrice = currentPrice
		pnlPercent := (currentPrice - pos.EntryPrice) / pos.EntryPrice

		// If ANY position hit take profit, exit BOTH (strategy successful)
		if pnlPercent >= s.Config.TakeProfitPercent {
			log.Printf("Sports: 🎯 TAKE PROFIT - Closing full position")
			log.Printf("  %s @ %.4f (entry: %.4f, +%.1f%%)",
				pos.OutcomeName, currentPrice, pos.EntryPrice, pnlPercent*100)

			// Exit this position
			s.executeExit(pos)

			// Exit paired position too
			if pos.PairedPositionID != "" {
				if pairedPos := s.RiskManager.GetPosition(pos.PairedPositionID); pairedPos != nil {
					if pairedPos.State == risk.StateOpen {
						var pairedPrice float64
						if pairedPos.TokenID == tracked.TokenPair.Yes {
							pairedPrice = tracked.YesMid
						} else {
							pairedPrice = tracked.NoMid
						}
						pairedPos.CurrentPrice = pairedPrice
						pairedPnlPercent := (pairedPrice - pairedPos.EntryPrice) / pairedPos.EntryPrice
						
						log.Printf("  %s @ %.4f (entry: %.4f, %.1f%%)",
							pairedPos.OutcomeName, pairedPrice, pairedPos.EntryPrice, pairedPnlPercent*100)
						s.executeExit(pairedPos)
					}
				}
			}
			tracked.HasPosition = false
			return
		}
	}

	// INDEPENDENT STOP LOSS - Each position can exit on its own stop-loss
	// This allows closing a losing underdog while holding a winning favorite
	for _, pos := range marketPositions {
		if now.Sub(pos.EntryTime) < EntryGracePeriod {
			continue
		}

		var currentPrice float64
		if pos.TokenID == tracked.TokenPair.Yes {
			currentPrice = tracked.YesMid
		} else {
			currentPrice = tracked.NoMid
		}

		pos.CurrentPrice = currentPrice
		pnlPercent := (currentPrice - pos.EntryPrice) / pos.EntryPrice

		// INDEPENDENT exit if this position hits stop-loss
		// Don't close the paired position - let it run independently
		if pnlPercent <= -s.Config.StopLossPercent {
			log.Printf("Sports: 🛑 STOP LOSS (independent) - %s @ %.4f (entry: %.4f, %.1f%%)",
				pos.OutcomeName, currentPrice, pos.EntryPrice, pnlPercent*100)
			log.Printf("  Paired position continues independently")
			s.executeExit(pos)
			
			// Note: We do NOT close the paired position here
			// This allows the winning side to continue running
		}
	}
}

func (s *Strategy) executeExit(pos *risk.Position) {
	if s.Config.IsDryRun() {
		pnl := (pos.CurrentPrice - pos.EntryPrice) * pos.Size
		log.Printf("Sports: [DRY RUN] Would SELL %s @ %.4f (P&L: $%.2f)",
			pos.OutcomeName, pos.CurrentPrice, pnl)
	} else {
		_, err := s.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   pos.TokenID,
			Price:     pos.CurrentPrice,
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

func (s *Strategy) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	openPositions := len(s.RiskManager.GetPositionsByStrategy("sports"))
	return fmt.Sprintf("Sports: %d moneylines, %d positions", len(s.trackedMarkets), openPositions)
}
