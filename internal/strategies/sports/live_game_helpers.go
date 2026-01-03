package sports

import (
	"time"
)

// calculateOddsShift calculates the recent momentum/shift in odds
// Positive value = odds improving (price increasing)
// Negative value = odds declining (price decreasing)
func (s *Strategy) calculateOddsShift(tracked *TrackedMarket) float64 {
	history := tracked.PriceHistory
	if len(history) < 3 {
		return 0
	}
	
	// Get the underdog price from history
	var currentUnderdogPrice, previousUnderdogPrice float64
	
	// Most recent price
	latest := history[len(history)-1]
	if tracked.IsYesUnderdog {
		currentUnderdogPrice = latest.YesMid
	} else {
		currentUnderdogPrice = latest.NoMid
	}
	
	// Average of previous 2-3 prices for smoothing
	lookback := 3
	if len(history) < lookback+1 {
		lookback = len(history) - 1
	}
	
	sum := 0.0
	for i := len(history) - lookback - 1; i < len(history)-1; i++ {
		if tracked.IsYesUnderdog {
			sum += history[i].YesMid
		} else {
			sum += history[i].NoMid
		}
	}
	previousUnderdogPrice = sum / float64(lookback)
	
	// Calculate shift as percentage change
	if previousUnderdogPrice > 0 {
		shift := (currentUnderdogPrice - previousUnderdogPrice) / previousUnderdogPrice
		return shift
	}
	
	return 0
}

// shouldEnterOnOddsImprovement checks if we should enter based on live game odds shift
func (s *Strategy) shouldEnterOnOddsImprovement(tracked *TrackedMarket) bool {
	// Only for live games
	if tracked.GameStatus != "LIVE" {
		return false
	}
	
	// Need sufficient history
	if len(tracked.PriceHistory) < 3 {
		return false
	}
	
	// Check if odds have improved significantly
	minShift := s.Config.SportsOddsShiftMin
	if tracked.LastOddsShift >= minShift {
		return true
	}
	
	return false
}

// shouldEnterLateGameFavorite checks if we should enter favorite position near game end
func (s *Strategy) shouldEnterLateGameFavorite(tracked *TrackedMarket, favoriteMid float64) bool {
	// Only for live games
	if tracked.GameStatus != "LIVE" {
		return false
	}
	
	// Need game to be at least 40 minutes in (NBA games are 48 mins)
	// For other sports, this serves as a reasonable late-game threshold
	if tracked.GameTime.IsZero() {
		return false
	}
	
	gameElapsed := tracked.LastUpdate.Sub(tracked.GameTime)
	if gameElapsed < 40*time.Minute {
		return false
	}
	
	// Favorite must be heavily favored
	return favoriteMid >= s.Config.SportsLateGameMinOdds
}

// detectPeakAndExit checks if underdog position has peaked and should exit
// Returns true if we detect a peak (3 consecutive drops from peak)
func (s *Strategy) detectPeakAndExit(tracked *TrackedMarket) bool {
	if len(tracked.PriceHistory) < 4 {
		return false
	}
	
	// Get current underdog price
	var currentPrice float64
	latest := tracked.PriceHistory[len(tracked.PriceHistory)-1]
	if tracked.IsYesUnderdog {
		currentPrice = latest.YesMid
	} else {
		currentPrice = latest.NoMid
	}
	
	// Check if we've dropped more than 5% from peak
	if tracked.PeakUnderdogMid > 0 {
		dropFromPeak := (tracked.PeakUnderdogMid - currentPrice) / tracked.PeakUnderdogMid
		
		// If dropped more than 5% from peak, consider it a reversal
		if dropFromPeak >= 0.05 {
			// Check that we've had at least 3 consecutive drops
			if len(tracked.PriceHistory) >= 4 {
				consecutiveDrops := 0
				for i := len(tracked.PriceHistory) - 3; i < len(tracked.PriceHistory); i++ {
					var price float64
					if tracked.IsYesUnderdog {
						price = tracked.PriceHistory[i].YesMid
					} else {
						price = tracked.PriceHistory[i].NoMid
					}
					
					if i > 0 {
						var prevPrice float64
						if tracked.IsYesUnderdog {
							prevPrice = tracked.PriceHistory[i-1].YesMid
						} else {
							prevPrice = tracked.PriceHistory[i-1].NoMid
						}
						
						if price < prevPrice {
							consecutiveDrops++
						}
					}
				}
				
				return consecutiveDrops >= 2
			}
		}
	}
	
	return false
}
