package risk

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// PositionData represents the serializable position state
type PositionData struct {
	Positions   map[string]*Position `json:"positions"`
	RealizedPnL float64              `json:"realized_pnl"`
}

// SavePositions saves all positions to a JSON file
func (m *Manager) SavePositions(filename string) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data := PositionData{
		Positions:   m.Positions,
		RealizedPnL: m.RealizedPnL,
	}

	// Ensure directory exists
	dir := filepath.Dir(filename)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}

	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(data); err != nil {
		return err
	}

	log.Printf("Risk: Saved %d positions to %s", len(m.Positions), filename)
	return nil
}

// LoadPositions loads positions from a JSON file
func (m *Manager) LoadPositions(filename string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	file, err := os.Open(filename)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("Risk: No existing positions file found at %s", filename)
			return nil
		}
		return err
	}
	defer file.Close()

	var data PositionData
	if err := json.NewDecoder(file).Decode(&data); err != nil {
		return err
	}

	// Load positions preserving state
	if data.Positions != nil {
		m.Positions = data.Positions
	}
	m.RealizedPnL = data.RealizedPnL

	openCount := 0
	for _, pos := range m.Positions {
		if pos.State == StateOpen {
			openCount++
		}
	}

	log.Printf("Risk: Loaded %d positions (%d open), realized P&L: $%.2f from %s",
		len(m.Positions), openCount, m.RealizedPnL, filename)
	return nil
}

// GetPositionsFilePath returns the default path for positions file
func GetPositionsFilePath() string {
	// Use current directory for simplicity
	return "positions.json"
}
