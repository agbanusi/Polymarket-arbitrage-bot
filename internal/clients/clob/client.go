package clob

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"polymarket-bot/config"
	"strconv"
	"time"
)

type Client struct {
	BaseURL    string
	APIKey     string
	Secret     string
	Passphrase string
	PrivateKey string
	HTTPClient *http.Client
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		BaseURL:    cfg.BaseURL,
		APIKey:     cfg.PolymarketAPIKey,
		Secret:     cfg.PolymarketSecret,
		Passphrase: cfg.PolymarketPassphrase,
		PrivateKey: cfg.PrivateKey,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// OrderSide represents buy or sell
type OrderSide string

const (
	Buy  OrderSide = "BUY"
	Sell OrderSide = "SELL"
)

// OrderType represents limit or market orders
type OrderType string

const (
	Limit  OrderType = "LIMIT"
	Market OrderType = "MARKET"
)

// PriceLevel represents a single price level in the order book
type PriceLevel struct {
	Price string `json:"price"`
	Size  string `json:"size"`
}

// OrderBook represents the order book for a token
type OrderBook struct {
	Market       string       `json:"market"`
	AssetID      string       `json:"asset_id"`
	Bids         []PriceLevel `json:"bids"`
	Asks         []PriceLevel `json:"asks"`
	Timestamp    string       `json:"timestamp"`
	Hash         string       `json:"hash"`
}

// PriceResponse represents a price query response
type PriceResponse struct {
	Price string `json:"price"`
}

// GetOrderBook fetches the order book for a specific token ID
// This is a PUBLIC endpoint - no authentication required
func (c *Client) GetOrderBook(tokenID string) (*OrderBook, error) {
	endpoint := fmt.Sprintf("%s/book", c.BaseURL)
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	q.Set("token_id", tokenID)
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("order book request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("order book API returned status %d: %s", resp.StatusCode, string(body))
	}

	var book OrderBook
	if err := json.NewDecoder(resp.Body).Decode(&book); err != nil {
		return nil, fmt.Errorf("failed to decode order book: %w", err)
	}

	return &book, nil
}

// OrderBookWithPrices contains parsed price information from order book
type OrderBookWithPrices struct {
	BestBid       float64
	BestAsk       float64
	Midpoint      float64
	BidLiquidity  float64 // Total USD on bid side
	AskLiquidity  float64 // Total USD on ask side
}

// GetOrderBookWithPrices fetches order book and parses prices with liquidity info
func (c *Client) GetOrderBookWithPrices(tokenID string) (*OrderBookWithPrices, error) {
	book, err := c.GetOrderBook(tokenID)
	if err != nil {
		return nil, err
	}

	result := &OrderBookWithPrices{}

	// API returns bids sorted from WORST to BEST (low to high)
	// Best bid is LAST element
	if len(book.Bids) > 0 {
		lastIdx := len(book.Bids) - 1
		fmt.Sscanf(book.Bids[lastIdx].Price, "%f", &result.BestBid)
		
		// Calculate total bid liquidity
		for _, level := range book.Bids {
			var price, size float64
			fmt.Sscanf(level.Price, "%f", &price)
			fmt.Sscanf(level.Size, "%f", &size)
			result.BidLiquidity += price * size
		}
	}

	// API returns asks sorted from WORST to BEST (high to low)
	// Best ask is LAST element
	if len(book.Asks) > 0 {
		lastIdx := len(book.Asks) - 1
		fmt.Sscanf(book.Asks[lastIdx].Price, "%f", &result.BestAsk)
		
		// Calculate total ask liquidity
		for _, level := range book.Asks {
			var price, size float64
			fmt.Sscanf(level.Price, "%f", &price)
			fmt.Sscanf(level.Size, "%f", &size)
			result.AskLiquidity += price * size
		}
	}

	if result.BestBid > 0 && result.BestAsk > 0 {
		result.Midpoint = (result.BestBid + result.BestAsk) / 2
	}

	return result, nil
}

// GetPrice fetches the best price for a token on a specific side
// This is a PUBLIC endpoint - no authentication required
func (c *Client) GetPrice(tokenID string, side OrderSide) (float64, error) {
	endpoint := fmt.Sprintf("%s/price", c.BaseURL)
	u, err := url.Parse(endpoint)
	if err != nil {
		return 0, err
	}

	q := u.Query()
	q.Set("token_id", tokenID)
	q.Set("side", string(side))
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("price request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("price API returned status %d: %s", resp.StatusCode, string(body))
	}

	var priceResp PriceResponse
	if err := json.NewDecoder(resp.Body).Decode(&priceResp); err != nil {
		return 0, fmt.Errorf("failed to decode price: %w", err)
	}

	price, err := strconv.ParseFloat(priceResp.Price, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse price: %w", err)
	}

	return price, nil
}

// GetMidpoint calculates the midpoint price between best bid and ask
func (c *Client) GetMidpoint(tokenID string) (float64, error) {
	book, err := c.GetOrderBook(tokenID)
	if err != nil {
		return 0, err
	}

	if len(book.Bids) == 0 || len(book.Asks) == 0 {
		return 0, fmt.Errorf("order book has no bids or asks")
	}

	// API returns bids sorted from WORST to BEST (low to high)
	// So best bid is the LAST element (highest price someone will pay)
	lastBidIdx := len(book.Bids) - 1
	bestBid, err := strconv.ParseFloat(book.Bids[lastBidIdx].Price, 64)
	if err != nil {
		return 0, err
	}

	// API returns asks sorted from WORST to BEST (high to low)
	// So best ask is the LAST element (lowest price someone will sell)
	lastAskIdx := len(book.Asks) - 1
	bestAsk, err := strconv.ParseFloat(book.Asks[lastAskIdx].Price, 64)
	if err != nil {
		return 0, err
	}

	return (bestBid + bestAsk) / 2, nil
}

// GetBestBid returns the highest bid price
func (c *Client) GetBestBid(tokenID string) (float64, error) {
	return c.GetPrice(tokenID, Buy)
}

// GetBestAsk returns the lowest ask price
func (c *Client) GetBestAsk(tokenID string) (float64, error) {
	return c.GetPrice(tokenID, Sell)
}

// CreateOrderRequest represents an order to be placed
type CreateOrderRequest struct {
	TokenID   string    `json:"token_id"`
	Price     float64   `json:"price,string"`
	Size      float64   `json:"size,string"`
	Side      OrderSide `json:"side"`
	OrderType OrderType `json:"order_type"`
}

// CreateOrderResponse represents the response from order creation
type CreateOrderResponse struct {
	OrderID     string `json:"orderID"`
	Status      string `json:"status"`
	Transaction string `json:"transactionHash,omitempty"`
}

// CreateOrder places a new order
// NOTE: This requires authentication and order signing (EIP-712)
// For live trading, proper signing implementation is needed
func (c *Client) CreateOrder(req CreateOrderRequest) (*CreateOrderResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/order", c.BaseURL)
	httpReq, err := http.NewRequest("POST", endpoint, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	c.setAuthHeaders(httpReq)

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("order API returned status %d: %s", resp.StatusCode, string(body))
	}

	var res CreateOrderResponse
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}

	return &res, nil
}

// CancelOrder cancels an existing order
func (c *Client) CancelOrder(orderID string) error {
	endpoint := fmt.Sprintf("%s/order/%s", c.BaseURL, orderID)

	req, err := http.NewRequest("DELETE", endpoint, nil)
	if err != nil {
		return err
	}

	c.setAuthHeaders(req)

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cancel order failed with status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// setAuthHeaders sets the authentication headers for authenticated requests
func (c *Client) setAuthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("POLY_API_KEY", c.APIKey)
		req.Header.Set("POLY_PASSPHRASE", c.Passphrase)
		req.Header.Set("POLY_SECRET", c.Secret)
		// TODO: Add proper HMAC signature for authenticated requests
		// timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		// req.Header.Set("POLY_TIMESTAMP", timestamp)
		// req.Header.Set("POLY_SIGNATURE", generateSignature(...))
	}
}
