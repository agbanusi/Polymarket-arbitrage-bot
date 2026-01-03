package gamma

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"polymarket-bot/config"
	"strings"
	"time"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewClient(cfg *config.Config) *Client {
	return &Client{
		BaseURL: cfg.GammaURL,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// ClobTokenPair represents the Yes/No token IDs for a market
type ClobTokenPair struct {
	Yes string // Token ID for "Yes" outcome
	No  string // Token ID for "No" outcome
}

// Market represents a Polymarket market from Gamma API
type Market struct {
	ID               string   `json:"id"`
	Question         string   `json:"question"`
	Slug             string   `json:"slug"`
	EndDate          string   `json:"endDate"`
	Volume           string   `json:"volume"`
	Volume24hr       float64  `json:"volume24hr"`
	Active           bool     `json:"active"`
	Closed           bool     `json:"closed"`
	Tags             []Tag    `json:"tags"`
	Outcomes         string   `json:"outcomes"` // JSON array string e.g. "[\"Yes\", \"No\"]"
	OutcomePrices    string   `json:"outcomePrices"` // JSON array string e.g. "[\"0.65\", \"0.35\"]"
	Description      string   `json:"description"`
	ConditionID      string   `json:"conditionId"`
	QuestionID       string   `json:"questionId"`
	ClobTokenIds     string   `json:"clobTokenIds"` // JSON array of token IDs
	ResolutionSource string   `json:"resolutionSource"`
	EndDateIso       string   `json:"endDateIso"`
	GameStartTime    string   `json:"gameStartTime,omitempty"`
	EnableOrderBook  bool     `json:"enableOrderBook"`
	SportsMarketType string   `json:"sportsMarketType,omitempty"` // "moneyline", "spreads", "totals"
}

// Tag represents a market tag
type Tag struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Slug  string `json:"slug"`
}

// Event represents a Polymarket event containing multiple markets
type Event struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	EndDate     string   `json:"endDate"`
	Markets     []Market `json:"markets"`
	Tags        []Tag    `json:"tags"`
	Active      bool     `json:"active"`
	Closed      bool     `json:"closed"`
	Volume      float64  `json:"volume"`
	Volume24hr  float64  `json:"volume24hr"`
}

// GetClobTokenPair extracts the CLOB token IDs for Yes and No outcomes
func (m *Market) GetClobTokenPair() (*ClobTokenPair, error) {
	if m.ClobTokenIds == "" {
		return nil, fmt.Errorf("market has no clobTokenIds")
	}

	var tokenIds []string
	if err := json.Unmarshal([]byte(m.ClobTokenIds), &tokenIds); err != nil {
		return nil, fmt.Errorf("failed to parse clobTokenIds: %w", err)
	}

	if len(tokenIds) < 2 {
		return nil, fmt.Errorf("market has less than 2 token IDs")
	}

	return &ClobTokenPair{
		Yes: tokenIds[0],
		No:  tokenIds[1],
	}, nil
}

// GetOutcomeNames parses the outcomes JSON string
func (m *Market) GetOutcomeNames() ([]string, error) {
	if m.Outcomes == "" {
		return []string{"Yes", "No"}, nil // Default
	}

	var outcomes []string
	if err := json.Unmarshal([]byte(m.Outcomes), &outcomes); err != nil {
		return nil, fmt.Errorf("failed to parse outcomes: %w", err)
	}

	return outcomes, nil
}

// GetOutcomePricesFloat parses the outcome prices as floats
func (m *Market) GetOutcomePricesFloat() ([]float64, error) {
	if m.OutcomePrices == "" {
		return nil, fmt.Errorf("market has no outcomePrices")
	}

	var priceStrings []string
	if err := json.Unmarshal([]byte(m.OutcomePrices), &priceStrings); err != nil {
		return nil, fmt.Errorf("failed to parse outcomePrices: %w", err)
	}

	prices := make([]float64, len(priceStrings))
	for i, ps := range priceStrings {
		var price float64
		if _, err := fmt.Sscanf(ps, "%f", &price); err != nil {
			return nil, fmt.Errorf("failed to parse price %s: %w", ps, err)
		}
		prices[i] = price
	}

	return prices, nil
}

// HasTag checks if a market has a specific tag
func (m *Market) HasTag(tagLabel string) bool {
	tagLower := strings.ToLower(tagLabel)
	for _, tag := range m.Tags {
		if strings.ToLower(tag.Label) == tagLower || strings.ToLower(tag.Slug) == tagLower {
			return true
		}
	}
	return false
}

type GetMarketsParams struct {
	Limit            int
	Offset           int
	Order            string // "volume", "created_at", "id"
	Ascending        bool
	Tag              string
	TagID            string
	Active           bool
	Closed           bool
	Slug             string
	SportsMarketTypes string // "moneyline", "spreads", "totals", etc.
}

// GetMarkets fetches markets with filters
func (c *Client) GetMarkets(params GetMarketsParams) ([]Market, error) {
	endpoint := fmt.Sprintf("%s/markets", c.BaseURL)
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	if params.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", params.Limit))
	}
	if params.Offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", params.Offset))
	}
	if params.Order != "" {
		q.Set("order", params.Order)
	}
	if params.Ascending {
		q.Set("ascending", "true")
	} else {
		q.Set("ascending", "false")
	}
	if params.Tag != "" {
		q.Set("tag", params.Tag)
	}
	if params.TagID != "" {
		q.Set("tag_id", params.TagID)
	}
	if params.Active {
		q.Set("active", "true")
	}
	if params.Closed {
		q.Set("closed", "true")
	} else {
		q.Set("closed", "false")
	}
	if params.Slug != "" {
		q.Set("slug", params.Slug)
	}
	if params.SportsMarketTypes != "" {
		q.Set("sports_market_types", params.SportsMarketTypes)
	}

	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gamma markets API returned status: %d", resp.StatusCode)
	}

	var markets []Market
	if err := json.NewDecoder(resp.Body).Decode(&markets); err != nil {
		return nil, err
	}

	return markets, nil
}

// GetMarketBySlug fetches a single market by its slug
func (c *Client) GetMarketBySlug(slug string) (*Market, error) {
	endpoint := fmt.Sprintf("%s/markets/slug/%s", c.BaseURL, slug)

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gamma market API returned status: %d", resp.StatusCode)
	}

	var market Market
	if err := json.NewDecoder(resp.Body).Decode(&market); err != nil {
		return nil, err
	}

	return &market, nil
}

// GetEventsParams filters for fetching events
type GetEventsParams struct {
	Limit     int
	Offset    int
	Order     string
	Ascending bool
	Active    bool
	Closed    bool
	Tag       string
	TagID     string
}

// GetEvents fetches events (which contain markets)
func (c *Client) GetEvents(params GetEventsParams) ([]Event, error) {
	endpoint := fmt.Sprintf("%s/events", c.BaseURL)
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}

	q := u.Query()
	if params.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", params.Limit))
	}
	if params.Offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", params.Offset))
	}
	if params.Order != "" {
		q.Set("order", params.Order)
	}
	if params.Ascending {
		q.Set("ascending", "true")
	} else {
		q.Set("ascending", "false")
	}
	if !params.Closed {
		q.Set("closed", "false")
	}
	if params.Active {
		q.Set("active", "true")
	}
	if params.Tag != "" {
		q.Set("tag", params.Tag)
	}
	if params.TagID != "" {
		q.Set("tag_id", params.TagID)
	}

	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gamma events API returned status: %d", resp.StatusCode)
	}

	var events []Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, err
	}

	return events, nil
}

// GetEventBySlug fetches a single event by its slug
func (c *Client) GetEventBySlug(slug string) (*Event, error) {
	endpoint := fmt.Sprintf("%s/events/slug/%s", c.BaseURL, slug)

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gamma event API returned status: %d", resp.StatusCode)
	}

	var event Event
	if err := json.NewDecoder(resp.Body).Decode(&event); err != nil {
		return nil, err
	}

	return &event, nil
}

// GetTags fetches all available tags
func (c *Client) GetTags() ([]Tag, error) {
	endpoint := fmt.Sprintf("%s/tags", c.BaseURL)

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gamma tags API returned status: %d", resp.StatusCode)
	}

	var tags []Tag
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}

	return tags, nil
}

// Sport represents a sport category with tag info
type Sport struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Slug     string `json:"slug"`
	TagID    string `json:"tagId"`
	ImageURL string `json:"imageUrl"`
}

// GetSports fetches all available sports categories (including football leagues)
// This is useful for discovering tags like "EPL", "La Liga", "Serie A", etc.
func (c *Client) GetSports() ([]Sport, error) {
	endpoint := fmt.Sprintf("%s/sports", c.BaseURL)

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gamma sports API returned status: %d", resp.StatusCode)
	}

	var sports []Sport
	if err := json.NewDecoder(resp.Body).Decode(&sports); err != nil {
		return nil, err
	}

	return sports, nil
}
