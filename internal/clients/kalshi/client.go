package kalshi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"polymarket-bot/config"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the production Kalshi API URL
	DefaultBaseURL = "https://api.elections.kalshi.com/trade-api/v2"
	// DemoBaseURL is the sandbox/demo API URL
	DemoBaseURL = "https://demo-api.kalshi.co/trade-api/v2"

	// RetryAttempts for failed requests
	RetryAttempts = 3
	// RetryDelay between retry attempts
	RetryDelay = 500 * time.Millisecond

	// KalshiFeeRate is 7% of (price * (1-price))
	KalshiFeeRate = 0.07
)

// Client represents a Kalshi API client
type Client struct {
	BaseURL    string
	APIKey     string
	PrivateKey string
	HTTPClient *http.Client
	config     *config.Config
}

// NewClient creates a new Kalshi API client
func NewClient(cfg *config.Config) *Client {
	baseURL := cfg.KalshiBaseURL
	if baseURL == "" {
		if cfg.IsDryRun() {
			baseURL = DemoBaseURL
		} else {
			baseURL = DefaultBaseURL
		}
	}

	return &Client{
		BaseURL:    baseURL,
		APIKey:     cfg.KalshiAPIKey,
		PrivateKey: cfg.KalshiPrivateKey,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		config: cfg,
	}
}

// Market represents a Kalshi market
type Market struct {
	Ticker          string  `json:"ticker"`
	EventTicker     string  `json:"event_ticker"`
	Title           string  `json:"title"`
	Subtitle        string  `json:"subtitle"`
	Category        string  `json:"category"`
	Status          string  `json:"status"`
	YesBid          float64 `json:"yes_bid"` // Best bid for YES
	YesAsk          float64 `json:"yes_ask"` // Best ask for YES
	NoBid           float64 `json:"no_bid"`  // Best bid for NO
	NoAsk           float64 `json:"no_ask"`  // Best ask for NO
	LastPrice       float64 `json:"last_price"`
	Volume          int     `json:"volume"`
	Volume24H       int     `json:"volume_24h"`
	OpenInterest    int     `json:"open_interest"`
	CloseTime       string  `json:"close_time"`
	ExpirationTime  string  `json:"expiration_time"`
	SettlementValue *int    `json:"settlement_value"` // nil if not settled
}

// OrderBook represents the order book for a market
type OrderBook struct {
	Ticker string       `json:"ticker"`
	Bids   []PriceLevel `json:"yes"` // Kalshi returns bids under "yes"
	Asks   []PriceLevel `json:"no"`  // and asks under "no" (inverted)
}

// PriceLevel represents a single price level
type PriceLevel struct {
	Price    int `json:"price"`    // Price in cents (1-99)
	Quantity int `json:"quantity"` // Number of contracts
}

// GetMarketsParams represents query parameters for fetching markets
type GetMarketsParams struct {
	Limit        int
	Cursor       string
	EventTicker  string
	SeriesTicker string
	Status       string // "open", "closed", "settled"
	Category     string
	MinCloseDate string
	MaxCloseDate string
}

// GetMarketsResponse represents the markets API response
type GetMarketsResponse struct {
	Markets []Market `json:"markets"`
	Cursor  string   `json:"cursor"`
}

// GetMarkets fetches markets with optional filters
func (c *Client) GetMarkets(params GetMarketsParams) ([]Market, error) {
	endpoint := "/markets"
	query := url.Values{}

	if params.Limit > 0 {
		query.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		query.Set("cursor", params.Cursor)
	}
	if params.EventTicker != "" {
		query.Set("event_ticker", params.EventTicker)
	}
	if params.Status != "" {
		query.Set("status", params.Status)
	}
	if params.Category != "" {
		query.Set("category", params.Category)
	}

	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var resp GetMarketsResponse
	if err := c.doRequestWithRetry("GET", endpoint, nil, &resp); err != nil {
		return nil, fmt.Errorf("get markets: %w", err)
	}

	return resp.Markets, nil
}

// GetMarketByTicker fetches a single market by ticker
func (c *Client) GetMarketByTicker(ticker string) (*Market, error) {
	endpoint := fmt.Sprintf("/markets/%s", url.PathEscape(ticker))

	var resp struct {
		Market Market `json:"market"`
	}
	if err := c.doRequestWithRetry("GET", endpoint, nil, &resp); err != nil {
		return nil, fmt.Errorf("get market %s: %w", ticker, err)
	}

	return &resp.Market, nil
}

// GetOrderBook fetches the order book for a market
func (c *Client) GetOrderBook(ticker string) (*OrderBook, error) {
	endpoint := fmt.Sprintf("/markets/%s/orderbook", url.PathEscape(ticker))

	var resp struct {
		OrderBook OrderBook `json:"orderbook"`
	}
	if err := c.doRequestWithRetry("GET", endpoint, nil, &resp); err != nil {
		return nil, fmt.Errorf("get orderbook %s: %w", ticker, err)
	}

	resp.OrderBook.Ticker = ticker
	return &resp.OrderBook, nil
}

// BestPrices contains the best bid/ask for both YES and NO sides
type BestPrices struct {
	YesBid float64 // Best YES bid (what you can sell YES for)
	YesAsk float64 // Best YES ask (what you can buy YES for)
	NoBid  float64 // Best NO bid (what you can sell NO for)
	NoAsk  float64 // Best NO ask (what you can buy NO for)
}

// GetBestPrices fetches the best bid/ask prices for a market
func (c *Client) GetBestPrices(ticker string) (*BestPrices, error) {
	market, err := c.GetMarketByTicker(ticker)
	if err != nil {
		return nil, err
	}

	// Kalshi prices are in cents (1-99), convert to decimal (0.01-0.99)
	return &BestPrices{
		YesBid: market.YesBid / 100.0,
		YesAsk: market.YesAsk / 100.0,
		NoBid:  market.NoBid / 100.0,
		NoAsk:  market.NoAsk / 100.0,
	}, nil
}

// CalculateFee calculates the Kalshi trading fee for a trade
// Fee = ceil(0.07 × contracts × price × (1-price))
func (c *Client) CalculateFee(contracts int, price float64) float64 {
	if contracts <= 0 || price <= 0 || price >= 1 {
		return 0
	}
	fee := KalshiFeeRate * float64(contracts) * price * (1 - price)
	return math.Ceil(fee*100) / 100 // Round up to nearest cent
}

// CreateOrderRequest represents an order to be placed
type CreateOrderRequest struct {
	Ticker     string     `json:"ticker"`
	Side       string     `json:"side"`                // "yes" or "no"
	Action     string     `json:"action"`              // "buy" or "sell"
	Type       string     `json:"type"`                // "limit" or "market"
	Count      int        `json:"count"`               // Number of contracts
	YesPrice   int        `json:"yes_price,omitempty"` // Price in cents (1-99) for limit orders
	NoPrice    int        `json:"no_price,omitempty"`
	Expiration *time.Time `json:"expiration_time,omitempty"`
}

// CreateOrderResponse represents the response from order creation
type CreateOrderResponse struct {
	OrderID string `json:"order_id"`
	Status  string `json:"status"`
}

// CreateOrder places a new order on Kalshi
func (c *Client) CreateOrder(req CreateOrderRequest) (*CreateOrderResponse, error) {
	if c.APIKey == "" || c.PrivateKey == "" {
		return nil, fmt.Errorf("kalshi API credentials not configured")
	}

	endpoint := "/portfolio/orders"

	var resp struct {
		Order CreateOrderResponse `json:"order"`
	}
	if err := c.doAuthenticatedRequest("POST", endpoint, req, &resp); err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}

	return &resp.Order, nil
}

// FindSportsMarkets finds all sports-related markets
func (c *Client) FindSportsMarkets() ([]Market, error) {
	var allMarkets []Market
	cursor := ""

	// Paginate through all markets in sports category
	for i := 0; i < 20; i++ { // Max 20 pages
		markets, err := c.GetMarkets(GetMarketsParams{
			Limit:    200,
			Cursor:   cursor,
			Status:   "open",
			Category: "Sports",
		})
		if err != nil {
			return nil, err
		}

		if len(markets) == 0 {
			break
		}

		allMarkets = append(allMarkets, markets...)

		// Check if there are more pages (Kalshi uses cursor pagination)
		// If we got fewer than limit, we're done
		if len(markets) < 200 {
			break
		}
	}

	return allMarkets, nil
}

// IsConnected tests if the API is reachable
func (c *Client) IsConnected() bool {
	endpoint := "/exchange/status"
	var resp map[string]interface{}
	err := c.doRequestWithRetry("GET", endpoint, nil, &resp)
	return err == nil
}

// doRequestWithRetry performs an HTTP request with automatic retries
func (c *Client) doRequestWithRetry(method, endpoint string, body interface{}, result interface{}) error {
	var lastErr error

	for attempt := 0; attempt < RetryAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(RetryDelay * time.Duration(attempt)) // Exponential backoff
		}

		err := c.doRequest(method, endpoint, body, result)
		if err == nil {
			return nil
		}

		lastErr = err

		// Don't retry on client errors (4xx)
		if strings.Contains(err.Error(), "status 4") {
			return err
		}
	}

	return fmt.Errorf("after %d attempts: %w", RetryAttempts, lastErr)
}

// doRequest performs a single HTTP request
func (c *Client) doRequest(method, endpoint string, body interface{}, result interface{}) error {
	fullURL := c.BaseURL + endpoint

	req, err := http.NewRequest(method, fullURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
	}

	return nil
}

// doAuthenticatedRequest performs an authenticated request with HMAC signature
func (c *Client) doAuthenticatedRequest(method, endpoint string, body interface{}, result interface{}) error {
	fullURL := c.BaseURL + endpoint

	var bodyBytes []byte
	var err error
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
	}

	req, err := http.NewRequest(method, fullURL, strings.NewReader(string(bodyBytes)))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	// Set authentication headers
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	c.setAuthHeaders(req, method, endpoint, timestamp, bodyBytes)

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil {
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("parse response: %w", err)
		}
	}

	return nil
}

// setAuthHeaders sets the Kalshi authentication headers
func (c *Client) setAuthHeaders(req *http.Request, method, path, timestamp string, body []byte) {
	// Kalshi uses HMAC-SHA256 signature
	// Signature = HMAC(timestamp + method + path + body)
	message := timestamp + method + path
	if len(body) > 0 {
		message += string(body)
	}

	h := hmac.New(sha256.New, []byte(c.PrivateKey))
	h.Write([]byte(message))
	signature := base64.StdEncoding.EncodeToString(h.Sum(nil))

	req.Header.Set("KALSHI-ACCESS-KEY", c.APIKey)
	req.Header.Set("KALSHI-ACCESS-SIGNATURE", signature)
	req.Header.Set("KALSHI-ACCESS-TIMESTAMP", timestamp)
}
