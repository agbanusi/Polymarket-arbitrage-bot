package risk

import (
	"fmt"
	"log"
	"polymarket-bot/config"
	"sync"
	"time"
)

// PositionState represents the state of a position
type PositionState string

const (
	StateOpen        PositionState = "OPEN"
	StatePendingExit PositionState = "PENDING_EXIT"
	StateClosed      PositionState = "CLOSED"
)

// PositionType represents the strategy type
type PositionType string

const (
	TypeValueBet     PositionType = "VALUE_BET"     // Single side bet
	TypeDeltaNeutral PositionType = "DELTA_NEUTRAL" // Both sides
	TypeArbitrage    PositionType = "ARBITRAGE"     // Crypto spread arb
)

// Position limits
const (
	MaxPositions       = 40 // Total max positions across all strategies
	MaxCryptoPositions = 10 // Crypto: only high-conviction 15min/1hr/4hr trades
	MaxSportsPositions = 30 // Sports/O/U: increased for live game opportunities
)

// Position represents an open position
type Position struct {
	ID              string  // Unique position ID
	MarketID        string  // Gamma market ID
	TokenID         string  // CLOB token ID (Yes or No token)
	OutcomeIndex    int     // 0=Yes/TeamA, 1=No/TeamB
	OutcomeName     string  // "Yes", "No", or team name
	Size            float64 // Number of shares
	EntryPrice      float64 // Average entry price
	CurrentPrice    float64 // Latest price
	HighestPrice    float64 // Highest price seen (for trailing stop)
	StopLossPrice   float64 // Calculated stop loss price
	TakeProfitPrice float64 // Calculated take profit price
	Side            string  // "BUY" or "SELL"
	State           PositionState
	Type            PositionType
	Strategy        string // "sports" or "crypto"
	EntryTime       time.Time
	LastUpdate      time.Time

	// For delta neutral / arb
	PairedPositionID string  // ID of the paired position
	TotalCost        float64 // Combined cost for arb

	// Settlement tracking
	ResolvedAt time.Time // When market resolved
	FinalPnL   float64   // Final realized P&L after resolution
	WonBet     bool      // Whether this position won
}

// Manager handles risk management
type Manager struct {
	Config    *config.Config
	Positions map[string]*Position
	mu        sync.RWMutex

	// Callbacks for position events
	OnStopLoss   func(pos *Position)
	OnTakeProfit func(pos *Position)

	// Realized P&L tracking
	RealizedPnL float64
}

// NewManager creates a new risk manager
func NewManager(cfg *config.Config) *Manager {
	return &Manager{
		Config:    cfg,
		Positions: make(map[string]*Position),
	}
}

// GeneratePositionID creates a unique position ID
func (m *Manager) GeneratePositionID() string {
	return fmt.Sprintf("pos_%d", time.Now().UnixNano())
}

// CheckEntry verifies if a new trade is within risk limits
func (m *Manager) CheckEntry(tokenID string, price, size float64) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Check position count limit
	openCount := m.getOpenPositionCountUnsafe()
	if openCount >= MaxPositions {
		return fmt.Errorf("max positions (%d) reached", MaxPositions)
	}

	cost := price * size
	if cost > m.Config.MaxPositionSize {
		return fmt.Errorf("cost %.2f exceeds max position size %.2f", cost, m.Config.MaxPositionSize)
	}

	// Check total exposure
	totalExposure := m.getTotalExposureUnsafe()
	if totalExposure+cost > m.Config.MaxTotalExposure {
		return fmt.Errorf("total exposure would exceed limit")
	}

	return nil
}

// getOpenPositionCountUnsafe returns count without lock (caller must hold lock)
func (m *Manager) getOpenPositionCountUnsafe() int {
	count := 0
	for _, pos := range m.Positions {
		if pos.State == StateOpen {
			count++
		}
	}
	return count
}

// getPositionCountByStrategyUnsafe returns count for specific strategy (caller must hold lock)
func (m *Manager) getPositionCountByStrategyUnsafe(strategy string) int {
	count := 0
	for _, pos := range m.Positions {
		if pos.State == StateOpen && pos.Strategy == strategy {
			count++
		}
	}
	return count
}

// GetOpenPositionCount returns count of open positions
func (m *Manager) GetOpenPositionCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getOpenPositionCountUnsafe()
}

// GetPositionCountByStrategy returns count for specific strategy
func (m *Manager) GetPositionCountByStrategy(strategy string) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getPositionCountByStrategyUnsafe(strategy)
}

// CanAddPosition checks if we can add more positions (for priority logic)
func (m *Manager) CanAddPosition() bool {
	return m.GetOpenPositionCount() < MaxPositions
}

// CanAddPositionForStrategy checks if we can add more positions for a specific strategy
func (m *Manager) CanAddPositionForStrategy(strategy string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Check total limit first
	if m.getOpenPositionCountUnsafe() >= MaxPositions {
		return false
	}

	// Check per-strategy limits
	count := m.getPositionCountByStrategyUnsafe(strategy)
	switch strategy {
	case "crypto":
		return count < MaxCryptoPositions
	case "sports", "overunder":
		return count < MaxSportsPositions
	default:
		return true
	}
}

// AddPosition adds a new position
func (m *Manager) AddPosition(pos *Position) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	if pos.ID == "" {
		pos.ID = fmt.Sprintf("pos_%d", time.Now().UnixNano())
	}

	pos.State = StateOpen
	pos.EntryTime = time.Now()
	pos.LastUpdate = time.Now()
	pos.HighestPrice = pos.EntryPrice

	// Calculate stop loss and take profit prices
	if pos.Side == "BUY" {
		pos.StopLossPrice = pos.EntryPrice * (1 - m.Config.StopLossPercent)
		pos.TakeProfitPrice = pos.EntryPrice * (1 + m.Config.TakeProfitPercent)
	} else {
		pos.StopLossPrice = pos.EntryPrice * (1 + m.Config.StopLossPercent)
		pos.TakeProfitPrice = pos.EntryPrice * (1 - m.Config.TakeProfitPercent)
	}

	m.Positions[pos.ID] = pos

	log.Printf("Risk: Added position %s - %s @ %.4f (SL: %.4f, TP: %.4f)",
		pos.ID, pos.OutcomeName, pos.EntryPrice, pos.StopLossPrice, pos.TakeProfitPrice)

	return pos.ID
}

// AddSimplePosition is a convenience method for simple position creation
func (m *Manager) AddSimplePosition(marketID, tokenID string, size, price float64, side, strategy string) string {
	pos := &Position{
		MarketID:   marketID,
		TokenID:    tokenID,
		Size:       size,
		EntryPrice: price,
		Side:       side,
		Strategy:   strategy,
		Type:       TypeValueBet,
	}
	return m.AddPosition(pos)
}

// UpdatePrice updates the current price for a position and checks exit conditions
func (m *Manager) UpdatePrice(tokenID string, price float64) []ExitSignal {
	m.mu.Lock()
	defer m.mu.Unlock()

	var signals []ExitSignal

	for _, pos := range m.Positions {
		if pos.TokenID == tokenID && pos.State == StateOpen {
			pos.CurrentPrice = price
			pos.LastUpdate = time.Now()

			// Update highest price for trailing stop
			if price > pos.HighestPrice {
				pos.HighestPrice = price
				// Update trailing stop loss
				if m.Config.TrailingStopPercent > 0 {
					newStopLoss := price * (1 - m.Config.TrailingStopPercent)
					if newStopLoss > pos.StopLossPrice {
						pos.StopLossPrice = newStopLoss
					}
				}
			}

			// Check exit conditions
			if signal := m.checkExitConditions(pos); signal != nil {
				signals = append(signals, *signal)
			}
		}
	}

	return signals
}

// ExitSignal represents a signal to exit a position
type ExitSignal struct {
	PositionID string
	Position   *Position
	Reason     string
	PnL        float64
	PnLPercent float64
}

// checkExitConditions checks if a position should be exited
func (m *Manager) checkExitConditions(pos *Position) *ExitSignal {
	if pos.CurrentPrice == 0 || pos.State != StateOpen {
		return nil
	}

	var pnlPercent float64
	if pos.Side == "BUY" {
		pnlPercent = (pos.CurrentPrice - pos.EntryPrice) / pos.EntryPrice
	} else {
		pnlPercent = (pos.EntryPrice - pos.CurrentPrice) / pos.EntryPrice
	}

	pnl := (pos.CurrentPrice - pos.EntryPrice) * pos.Size
	if pos.Side == "SELL" {
		pnl = -pnl
	}

	// Check stop loss
	if pos.Side == "BUY" && pos.CurrentPrice <= pos.StopLossPrice {
		pos.State = StatePendingExit
		return &ExitSignal{
			PositionID: pos.ID,
			Position:   pos,
			Reason:     fmt.Sprintf("STOP_LOSS (%.2f%% loss)", pnlPercent*100),
			PnL:        pnl,
			PnLPercent: pnlPercent,
		}
	}

	if pos.Side == "SELL" && pos.CurrentPrice >= pos.StopLossPrice {
		pos.State = StatePendingExit
		return &ExitSignal{
			PositionID: pos.ID,
			Position:   pos,
			Reason:     fmt.Sprintf("STOP_LOSS (%.2f%% loss)", pnlPercent*100),
			PnL:        pnl,
			PnLPercent: pnlPercent,
		}
	}

	// Check take profit
	if pos.Side == "BUY" && pos.CurrentPrice >= pos.TakeProfitPrice {
		pos.State = StatePendingExit
		return &ExitSignal{
			PositionID: pos.ID,
			Position:   pos,
			Reason:     fmt.Sprintf("TAKE_PROFIT (%.2f%% gain)", pnlPercent*100),
			PnL:        pnl,
			PnLPercent: pnlPercent,
		}
	}

	if pos.Side == "SELL" && pos.CurrentPrice <= pos.TakeProfitPrice {
		pos.State = StatePendingExit
		return &ExitSignal{
			PositionID: pos.ID,
			Position:   pos,
			Reason:     fmt.Sprintf("TAKE_PROFIT (%.2f%% gain)", pnlPercent*100),
			PnL:        pnl,
			PnLPercent: pnlPercent,
		}
	}

	return nil
}

// GetPosition returns a position by ID
func (m *Manager) GetPosition(positionID string) *Position {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.Positions[positionID]
}

// GetPositionByToken returns positions for a specific token
func (m *Manager) GetPositionByToken(tokenID string) []*Position {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var positions []*Position
	for _, pos := range m.Positions {
		if pos.TokenID == tokenID && pos.State == StateOpen {
			positions = append(positions, pos)
		}
	}
	return positions
}

// GetOpenPositions returns all open positions
func (m *Manager) GetOpenPositions() []*Position {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var positions []*Position
	for _, pos := range m.Positions {
		if pos.State == StateOpen {
			positions = append(positions, pos)
		}
	}
	return positions
}

// GetPositionsByStrategy returns open positions for a specific strategy
func (m *Manager) GetPositionsByStrategy(strategy string) []*Position {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var positions []*Position
	for _, pos := range m.Positions {
		if pos.Strategy == strategy && pos.State == StateOpen {
			positions = append(positions, pos)
		}
	}
	return positions
}

// ClosePosition marks a position as closed
func (m *Manager) ClosePosition(positionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if pos, exists := m.Positions[positionID]; exists {
		pos.State = StateClosed
		log.Printf("Risk: Closed position %s", positionID)
	}
}

// RemovePosition removes a position from tracking
func (m *Manager) RemovePosition(positionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.Positions, positionID)
}

// GetTotalExposure returns the total value of all open positions
func (m *Manager) GetTotalExposure() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.getTotalExposureUnsafe()
}

func (m *Manager) getTotalExposureUnsafe() float64 {
	var total float64
	for _, pos := range m.Positions {
		if pos.State == StateOpen {
			total += pos.EntryPrice * pos.Size
		}
	}
	return total
}

// GetTotalPnL returns the total unrealized P&L
func (m *Manager) GetTotalPnL() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var total float64
	for _, pos := range m.Positions {
		if pos.State == StateOpen && pos.CurrentPrice > 0 {
			if pos.Side == "BUY" {
				total += (pos.CurrentPrice - pos.EntryPrice) * pos.Size
			} else {
				total += (pos.EntryPrice - pos.CurrentPrice) * pos.Size
			}
		}
	}
	return total
}

// HasPositionForMarket checks if there's already a position for a market
func (m *Manager) HasPositionForMarket(marketID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for _, pos := range m.Positions {
		if pos.MarketID == marketID && pos.State == StateOpen {
			return true
		}
	}
	return false
}

// GetPositionSummary returns a summary of current positions
func (m *Manager) GetPositionSummary() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	openCount := 0
	totalExposure := 0.0
	totalPnL := 0.0

	for _, pos := range m.Positions {
		if pos.State == StateOpen {
			openCount++
			totalExposure += pos.EntryPrice * pos.Size
			if pos.CurrentPrice > 0 {
				if pos.Side == "BUY" {
					totalPnL += (pos.CurrentPrice - pos.EntryPrice) * pos.Size
				} else {
					totalPnL += (pos.EntryPrice - pos.CurrentPrice) * pos.Size
				}
			}
		}
	}

	return fmt.Sprintf("Positions: %d/%d | Exposure: $%.2f | Unrealized P&L: $%.2f",
		openCount, MaxPositions, totalExposure, totalPnL)
}

// GetDetailedReport returns a detailed report of all open positions
func (m *Manager) GetDetailedReport() string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var report string
	report = "\n╔════════════════════════════════════════════════════════════════════╗\n"
	report += "║                    POSITION REPORT                                 ║\n"
	report += "╠════════════════════════════════════════════════════════════════════╣\n"
	report += fmt.Sprintf("║  Time: %s                                   ║\n", time.Now().Format("2006-01-02 15:04:05"))
	report += "╠════════════════════════════════════════════════════════════════════╣\n"

	// Group by strategy
	cryptoPositions := []*Position{}
	sportsPositions := []*Position{}
	ouPositions := []*Position{}

	totalPnL := 0.0
	totalExposure := 0.0

	for _, pos := range m.Positions {
		if pos.State != StateOpen {
			continue
		}
		switch pos.Strategy {
		case "crypto":
			cryptoPositions = append(cryptoPositions, pos)
		case "sports":
			sportsPositions = append(sportsPositions, pos)
		case "overunder":
			ouPositions = append(ouPositions, pos)
		}

		exposure := pos.EntryPrice * pos.Size
		totalExposure += exposure
		if pos.CurrentPrice > 0 {
			pnl := (pos.CurrentPrice - pos.EntryPrice) * pos.Size
			if pos.Side == "SELL" {
				pnl = -pnl
			}
			totalPnL += pnl
		}
	}

	// Crypto positions
	if len(cryptoPositions) > 0 {
		report += fmt.Sprintf("║ 🪙  CRYPTO (%d positions)                                          ║\n", len(cryptoPositions))
		report += "╟────────────────────────────────────────────────────────────────────╢\n"
		for _, pos := range cryptoPositions {
			pnl := (pos.CurrentPrice - pos.EntryPrice) * pos.Size
			pnlPercent := (pos.CurrentPrice - pos.EntryPrice) / pos.EntryPrice * 100
			symbol := "▲"
			if pnl < 0 {
				symbol = "▼"
			}
			report += fmt.Sprintf("║  %s %-10s Entry: $%.4f → $%.4f  %s $%.2f (%.1f%%)\n",
				pos.OutcomeName, truncateStr(pos.MarketID, 8), pos.EntryPrice, pos.CurrentPrice, symbol, pnl, pnlPercent)
		}
	}

	// Sports positions
	if len(sportsPositions) > 0 {
		report += "╟────────────────────────────────────────────────────────────────────╢\n"
		report += fmt.Sprintf("║ 🏀 SPORTS MONEYLINE (%d positions)                                ║\n", len(sportsPositions))
		report += "╟────────────────────────────────────────────────────────────────────╢\n"
		for _, pos := range sportsPositions {
			pnl := (pos.CurrentPrice - pos.EntryPrice) * pos.Size
			pnlPercent := (pos.CurrentPrice - pos.EntryPrice) / pos.EntryPrice * 100
			symbol := "▲"
			if pnl < 0 {
				symbol = "▼"
			}
			report += fmt.Sprintf("║  %-15s Entry: $%.4f → $%.4f  %s $%.2f (%.1f%%)\n",
				truncateStr(pos.OutcomeName, 15), pos.EntryPrice, pos.CurrentPrice, symbol, pnl, pnlPercent)
		}
	}

	// O/U positions
	if len(ouPositions) > 0 {
		report += "╟────────────────────────────────────────────────────────────────────╢\n"
		report += fmt.Sprintf("║ 📊 OVER/UNDER (%d positions)                                      ║\n", len(ouPositions))
		report += "╟────────────────────────────────────────────────────────────────────╢\n"
		for _, pos := range ouPositions {
			pnl := (pos.CurrentPrice - pos.EntryPrice) * pos.Size
			pnlPercent := (pos.CurrentPrice - pos.EntryPrice) / pos.EntryPrice * 100
			symbol := "▲"
			if pnl < 0 {
				symbol = "▼"
			}
			report += fmt.Sprintf("║  %-15s Entry: $%.4f → $%.4f  %s $%.2f (%.1f%%)\n",
				truncateStr(pos.OutcomeName, 15), pos.EntryPrice, pos.CurrentPrice, symbol, pnl, pnlPercent)
		}
	}

	// Summary
	report += "╠════════════════════════════════════════════════════════════════════╣\n"
	totalPositions := len(cryptoPositions) + len(sportsPositions) + len(ouPositions)
	pnlSymbol := "📈"
	if totalPnL < 0 {
		pnlSymbol = "📉"
	}
	report += fmt.Sprintf("║  TOTAL: %d/%d positions | Exposure: $%.2f | %s P&L: $%.2f\n",
		totalPositions, MaxPositions, totalExposure, pnlSymbol, totalPnL)
	report += "╚════════════════════════════════════════════════════════════════════╝\n"

	if totalPositions == 0 {
		return "\n📊 POSITION REPORT: No open positions\n"
	}

	return report
}

func truncateStr(s string, maxLen int) string {
	if len(s) > maxLen {
		return s[:maxLen-3] + "..."
	}
	return s
}

// SettlePosition marks a position as resolved with final P&L
func (m *Manager) SettlePosition(positionID string, won bool, finalPnL float64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if pos, exists := m.Positions[positionID]; exists {
		pos.State = StateClosed
		pos.WonBet = won
		pos.FinalPnL = finalPnL
		pos.ResolvedAt = time.Now()
		m.RealizedPnL += finalPnL

		log.Printf("Risk: Settled position %s - Won: %v, P&L: $%.2f, Total Realized: $%.2f",
			positionID, won, finalPnL, m.RealizedPnL)
	}
}

// GetRealizedPnL returns total realized P&L from closed positions
func (m *Manager) GetRealizedPnL() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.RealizedPnL
}

// GetPositionsByMarket returns all positions for a specific market ID
func (m *Manager) GetPositionsByMarket(marketID string) []*Position {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var positions []*Position
	for _, pos := range m.Positions {
		if pos.MarketID == marketID {
			positions = append(positions, pos)
		}
	}
	return positions
}

// CleanupStalePositions removes positions closed more than 24 hours ago
func (m *Manager) CleanupStalePositions() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	cleaned := 0
	cutoff := time.Now().Add(-24 * time.Hour)

	for id, pos := range m.Positions {
		if pos.State == StateClosed && !pos.ResolvedAt.IsZero() && pos.ResolvedAt.Before(cutoff) {
			delete(m.Positions, id)
			cleaned++
		}
	}

	if cleaned > 0 {
		log.Printf("Risk: Cleaned up %d stale closed positions", cleaned)
	}
	return cleaned
}
