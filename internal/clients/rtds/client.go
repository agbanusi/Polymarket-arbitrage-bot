package rtds

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// WebSocket URLs - Correct Polymarket endpoints
	RTDSWebSocketURL = "wss://ws-live-data.polymarket.com"
	CLOBWebSocketURL = "wss://ws-subscriptions-clob.polymarket.com/ws/"
	
	// Topics
	TopicCryptoPrices = "crypto_prices"
	TopicMarket       = "market"
)

// Client handles real-time data streaming from Polymarket
type Client struct {
	URL           string
	Conn          *websocket.Conn
	Subscriptions map[string]bool
	mu            sync.Mutex
	
	// Channels for different update types
	CryptoPrices  chan CryptoPriceUpdate
	MarketUpdates chan MarketUpdate
	
	// Control
	done     chan struct{}
	isAlive  bool
}

// CryptoPriceUpdate represents a crypto price update from Binance/Chainlink
type CryptoPriceUpdate struct {
	Symbol    string  `json:"symbol"`
	Price     float64 `json:"price"`
	Timestamp int64   `json:"timestamp"`
	Source    string  `json:"source"` // "binance" or "chainlink"
}

// MarketUpdate represents a market price/book update
type MarketUpdate struct {
	AssetID   string  `json:"asset_id"`
	Price     float64 `json:"price"`
	Side      string  `json:"side"`
	Size      float64 `json:"size"`
	Timestamp int64   `json:"timestamp"`
}

// WSMessage represents a WebSocket message
type WSMessage struct {
	Type    string          `json:"type"`
	Topic   string          `json:"topic"`
	Payload json.RawMessage `json:"payload"`
}

// SubscriptionRequest represents a subscription message
type SubscriptionRequest struct {
	Action        string         `json:"action"`
	Subscriptions []Subscription `json:"subscriptions"`
}

// Subscription represents a single subscription
type Subscription struct {
	Topic   string `json:"topic"`
	Type    string `json:"type"`
	Filters string `json:"filters,omitempty"`
}

// NewClient creates a new RTDS WebSocket client
func NewClient() *Client {
	return &Client{
		URL:           RTDSWebSocketURL,
		Subscriptions: make(map[string]bool),
		CryptoPrices:  make(chan CryptoPriceUpdate, 1000),
		MarketUpdates: make(chan MarketUpdate, 1000),
		done:          make(chan struct{}),
	}
}

// Connect establishes the WebSocket connection
func (c *Client) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
	}

	conn, _, err := dialer.Dial(c.URL, nil)
	if err != nil {
		return fmt.Errorf("WebSocket dial failed: %w", err)
	}

	c.Conn = conn
	c.isAlive = true

	// Start listening for messages
	go c.listen()

	// Start ping/pong for keepalive
	go c.keepAlive()

	log.Println("RTDS: Connected to WebSocket")
	return nil
}

// SubscribeCryptoPrices subscribes to crypto price updates
func (c *Client) SubscribeCryptoPrices(symbols ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Conn == nil {
		return fmt.Errorf("not connected")
	}

	sub := Subscription{
		Topic: TopicCryptoPrices,
		Type:  "update",
	}

	if len(symbols) > 0 {
		// Format: "btcusdt,ethusdt,solusdt"
		filters := ""
		for i, s := range symbols {
			if i > 0 {
				filters += ","
			}
			filters += s
		}
		sub.Filters = filters
	}

	req := SubscriptionRequest{
		Action:        "subscribe",
		Subscriptions: []Subscription{sub},
	}

	if err := c.Conn.WriteJSON(req); err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	c.Subscriptions[TopicCryptoPrices] = true
	log.Printf("RTDS: Subscribed to crypto_prices (filters: %s)", sub.Filters)
	return nil
}

// SubscribeMarket subscribes to market updates for specific asset IDs
func (c *Client) SubscribeMarket(assetIDs ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.Conn == nil {
		return fmt.Errorf("not connected")
	}

	sub := Subscription{
		Topic: TopicMarket,
		Type:  "update",
	}

	if len(assetIDs) > 0 {
		filters := ""
		for i, id := range assetIDs {
			if i > 0 {
				filters += ","
			}
			filters += id
		}
		sub.Filters = filters
	}

	req := SubscriptionRequest{
		Action:        "subscribe",
		Subscriptions: []Subscription{sub},
	}

	if err := c.Conn.WriteJSON(req); err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	c.Subscriptions[TopicMarket] = true
	log.Printf("RTDS: Subscribed to market updates")
	return nil
}

// listen processes incoming WebSocket messages
func (c *Client) listen() {
	defer func() {
		c.mu.Lock()
		c.isAlive = false
		c.mu.Unlock()
	}()

	for {
		select {
		case <-c.done:
			return
		default:
			_, message, err := c.Conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("RTDS: WebSocket error: %v", err)
				}
				return
			}

			c.handleMessage(message)
		}
	}
}

// handleMessage parses and routes incoming messages
func (c *Client) handleMessage(data []byte) {
	var msg WSMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		// Try to parse as array (some messages come as arrays)
		var msgArray []json.RawMessage
		if err := json.Unmarshal(data, &msgArray); err != nil {
			log.Printf("RTDS: Failed to parse message: %v", err)
			return
		}
		// Handle array messages if needed
		return
	}

	switch msg.Topic {
	case TopicCryptoPrices:
		c.handleCryptoPrice(msg.Payload)
	case TopicMarket:
		c.handleMarketUpdate(msg.Payload)
	default:
		// Ignore unknown topics
	}
}

// handleCryptoPrice processes crypto price updates
func (c *Client) handleCryptoPrice(payload json.RawMessage) {
	var update CryptoPriceUpdate
	if err := json.Unmarshal(payload, &update); err != nil {
		log.Printf("RTDS: Failed to parse crypto price: %v", err)
		return
	}

	select {
	case c.CryptoPrices <- update:
	default:
		// Channel full, drop oldest
		<-c.CryptoPrices
		c.CryptoPrices <- update
	}
}

// handleMarketUpdate processes market price updates
func (c *Client) handleMarketUpdate(payload json.RawMessage) {
	var update MarketUpdate
	if err := json.Unmarshal(payload, &update); err != nil {
		log.Printf("RTDS: Failed to parse market update: %v", err)
		return
	}

	select {
	case c.MarketUpdates <- update:
	default:
		// Channel full, drop oldest
		<-c.MarketUpdates
		c.MarketUpdates <- update
	}
}

// keepAlive sends periodic pings to keep the connection alive
func (c *Client) keepAlive() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.mu.Lock()
			if c.Conn != nil && c.isAlive {
				if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("RTDS: Ping failed: %v", err)
				}
			}
			c.mu.Unlock()
		}
	}
}

// Reconnect attempts to reconnect the WebSocket
func (c *Client) Reconnect() error {
	c.Close()
	
	// Wait before reconnecting
	time.Sleep(2 * time.Second)
	
	if err := c.Connect(); err != nil {
		return err
	}

	// Re-subscribe to previous subscriptions
	c.mu.Lock()
	subs := make(map[string]bool)
	for k, v := range c.Subscriptions {
		subs[k] = v
	}
	c.mu.Unlock()

	if subs[TopicCryptoPrices] {
		if err := c.SubscribeCryptoPrices(); err != nil {
			log.Printf("RTDS: Failed to resubscribe to crypto prices: %v", err)
		}
	}

	return nil
}

// IsConnected returns whether the WebSocket is connected
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isAlive && c.Conn != nil
}

// Close closes the WebSocket connection
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	close(c.done)
	
	if c.Conn != nil {
		c.Conn.Close()
		c.Conn = nil
	}
	
	c.isAlive = false
	c.done = make(chan struct{})
	
	log.Println("RTDS: Connection closed")
}
