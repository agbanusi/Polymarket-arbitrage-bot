package services

import (
	"context"
	"log"
	"polymarket-bot/config"
	"polymarket-bot/internal/clients/clob"
	"polymarket-bot/internal/risk"
	"time"
)

// PositionMonitor monitors all open positions and triggers exits
type PositionMonitor struct {
	Config      *config.Config
	ClobClient  *clob.Client
	RiskManager *risk.Manager
	
	// Control
	ctx    context.Context
	cancel context.CancelFunc
}

// NewPositionMonitor creates a new position monitor
func NewPositionMonitor(cfg *config.Config, c *clob.Client, r *risk.Manager) *PositionMonitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &PositionMonitor{
		Config:      cfg,
		ClobClient:  c,
		RiskManager: r,
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Run starts the position monitoring loop
func (m *PositionMonitor) Run() {
	log.Println("Monitor: Starting position monitoring...")
	
	// Price update ticker
	priceTicker := time.NewTicker(time.Duration(m.Config.PriceUpdateInterval) * time.Second)
	defer priceTicker.Stop()

	// Status logging ticker
	statusTicker := time.NewTicker(60 * time.Second)
	defer statusTicker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			log.Println("Monitor: Stopped")
			return
		case <-priceTicker.C:
			m.updatePrices()
		case <-statusTicker.C:
			m.logStatus()
		}
	}
}

// Stop stops the monitor
func (m *PositionMonitor) Stop() {
	m.cancel()
}

// updatePrices fetches current prices for all open positions
func (m *PositionMonitor) updatePrices() {
	positions := m.RiskManager.GetOpenPositions()
	
	if len(positions) == 0 {
		return
	}

	// Track unique tokens to avoid duplicate API calls
	tokenPrices := make(map[string]float64)
	var exitSignals []risk.ExitSignal

	for _, pos := range positions {
		// Check if we already fetched this token's price
		if _, ok := tokenPrices[pos.TokenID]; !ok {
			price, err := m.ClobClient.GetBestBid(pos.TokenID)
			if err != nil {
				continue
			}
			tokenPrices[pos.TokenID] = price
		}

		// Update position price
		if price, ok := tokenPrices[pos.TokenID]; ok {
			signals := m.RiskManager.UpdatePrice(pos.TokenID, price)
			exitSignals = append(exitSignals, signals...)
		}
	}

	// Process exit signals
	for _, signal := range exitSignals {
		m.handleExitSignal(signal)
	}
}

// handleExitSignal processes an exit signal
func (m *PositionMonitor) handleExitSignal(signal risk.ExitSignal) {
	pos := signal.Position
	
	log.Printf("Monitor: EXIT SIGNAL [%s] %s - %s (Entry: %.4f, Current: %.4f, P&L: $%.2f)",
		pos.Strategy, pos.OutcomeName, signal.Reason, pos.EntryPrice, pos.CurrentPrice, signal.PnL)

	if m.Config.IsDryRun() {
		log.Printf("Monitor: [DRY RUN] Would SELL %s @ %.4f", pos.OutcomeName, pos.CurrentPrice)
		m.RiskManager.ClosePosition(pos.ID)
	} else {
		// Execute exit order
		_, err := m.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   pos.TokenID,
			Price:     pos.CurrentPrice,
			Size:      pos.Size,
			Side:      clob.Sell,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Monitor: Exit order failed: %v", err)
			return
		}
		
		m.RiskManager.ClosePosition(pos.ID)
		log.Printf("Monitor: Position closed - %s P&L: $%.2f (%.2f%%)", 
			pos.OutcomeName, signal.PnL, signal.PnLPercent*100)
	}

	// Handle paired positions (delta neutral / arbs)
	if pos.PairedPositionID != "" {
		if pairedPos := m.RiskManager.GetPosition(pos.PairedPositionID); pairedPos != nil {
			if pairedPos.State == risk.StateOpen {
				log.Printf("Monitor: Closing paired position %s", pairedPos.OutcomeName)
				m.handlePairedExit(pairedPos)
			}
		}
	}
}

// handlePairedExit closes a paired position
func (m *PositionMonitor) handlePairedExit(pos *risk.Position) {
	if m.Config.IsDryRun() {
		log.Printf("Monitor: [DRY RUN] Would SELL paired %s @ %.4f", pos.OutcomeName, pos.CurrentPrice)
	} else {
		_, err := m.ClobClient.CreateOrder(clob.CreateOrderRequest{
			TokenID:   pos.TokenID,
			Price:     pos.CurrentPrice,
			Size:      pos.Size,
			Side:      clob.Sell,
			OrderType: clob.Limit,
		})
		if err != nil {
			log.Printf("Monitor: Paired exit order failed: %v", err)
			return
		}
	}
	m.RiskManager.ClosePosition(pos.ID)
}

// logStatus logs the current position summary with detailed breakdown
func (m *PositionMonitor) logStatus() {
	summary := m.RiskManager.GetPositionSummary()
	if summary != "" {
		// Show summary line
		log.Printf("\n📊 ════════════════════════════════════════════════════════════════")
		log.Printf("📊 %s", summary)
		log.Printf("📊 ════════════════════════════════════════════════════════════════")
		
		// Show detailed report every time
		report := m.RiskManager.GetDetailedReport()
		if report != "" {
			log.Print(report)
		}
	}
}
