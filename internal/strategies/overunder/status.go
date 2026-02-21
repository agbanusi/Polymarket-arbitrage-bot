package overunder

import (
	"fmt"
)

// GetStatus returns the current status of the O/U strategy
func (s *Strategy) GetStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	openPositions := s.RiskManager.GetPositionCountByStrategy("overunder")
	return fmt.Sprintf("O/U: %d markets tracked, %d positions", len(s.trackedMarkets), openPositions)
}
