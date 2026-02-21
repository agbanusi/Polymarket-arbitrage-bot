package sports

import (
	"fmt"
)

// GetStatus returns the current status of the sports strategy
func (s *Strategy) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	openPositions := s.RiskManager.GetPositionCountByStrategy("sports")
	return fmt.Sprintf("Sports: %d moneylines tracked, %d positions", len(s.trackedMarkets), openPositions)
}
