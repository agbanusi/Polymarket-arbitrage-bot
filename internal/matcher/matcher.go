package matcher

import (
	"log"
	"polymarket-bot/internal/clients/gamma"
	"polymarket-bot/internal/clients/kalshi"
	"regexp"
	"strings"
	"sync"
)

// MarketPair represents a matched pair of markets across platforms
type MarketPair struct {
	PolyMarket   *gamma.Market
	KalshiMarket *kalshi.Market
	MatchScore   float64 // 0.0 - 1.0 confidence
	MatchReason  string  // e.g., "exact_teams", "fuzzy_match"

	// Cached team names for display
	PolyTeams   [2]string
	KalshiTeams [2]string
}

// Matcher finds and matches markets across Polymarket and Kalshi
type Matcher struct {
	gammaClient  *gamma.Client
	kalshiClient *kalshi.Client

	// Cache of known team name mappings
	teamAliases map[string]string
	mu          sync.RWMutex
}

// NewMatcher creates a new market matcher
func NewMatcher(g *gamma.Client, k *kalshi.Client) *Matcher {
	m := &Matcher{
		gammaClient:  g,
		kalshiClient: k,
		teamAliases:  make(map[string]string),
	}
	m.initTeamAliases()
	return m
}

// initTeamAliases populates common team name variations
func (m *Matcher) initTeamAliases() {
	// NBA teams
	m.teamAliases["lakers"] = "los angeles lakers"
	m.teamAliases["lal"] = "los angeles lakers"
	m.teamAliases["la lakers"] = "los angeles lakers"
	m.teamAliases["clippers"] = "los angeles clippers"
	m.teamAliases["lac"] = "los angeles clippers"
	m.teamAliases["celtics"] = "boston celtics"
	m.teamAliases["bos"] = "boston celtics"
	m.teamAliases["warriors"] = "golden state warriors"
	m.teamAliases["gsw"] = "golden state warriors"
	m.teamAliases["heat"] = "miami heat"
	m.teamAliases["mia"] = "miami heat"
	m.teamAliases["bulls"] = "chicago bulls"
	m.teamAliases["chi"] = "chicago bulls"
	m.teamAliases["knicks"] = "new york knicks"
	m.teamAliases["nyk"] = "new york knicks"
	m.teamAliases["nets"] = "brooklyn nets"
	m.teamAliases["bkn"] = "brooklyn nets"
	m.teamAliases["76ers"] = "philadelphia 76ers"
	m.teamAliases["sixers"] = "philadelphia 76ers"
	m.teamAliases["phi"] = "philadelphia 76ers"
	m.teamAliases["bucks"] = "milwaukee bucks"
	m.teamAliases["mil"] = "milwaukee bucks"
	m.teamAliases["suns"] = "phoenix suns"
	m.teamAliases["phx"] = "phoenix suns"
	m.teamAliases["mavericks"] = "dallas mavericks"
	m.teamAliases["mavs"] = "dallas mavericks"
	m.teamAliases["dal"] = "dallas mavericks"
	m.teamAliases["nuggets"] = "denver nuggets"
	m.teamAliases["den"] = "denver nuggets"
	m.teamAliases["timberwolves"] = "minnesota timberwolves"
	m.teamAliases["wolves"] = "minnesota timberwolves"
	m.teamAliases["min"] = "minnesota timberwolves"

	// NFL teams
	m.teamAliases["chiefs"] = "kansas city chiefs"
	m.teamAliases["kc"] = "kansas city chiefs"
	m.teamAliases["eagles"] = "philadelphia eagles"
	m.teamAliases["cowboys"] = "dallas cowboys"
	m.teamAliases["packers"] = "green bay packers"
	m.teamAliases["gb"] = "green bay packers"
	m.teamAliases["49ers"] = "san francisco 49ers"
	m.teamAliases["niners"] = "san francisco 49ers"
	m.teamAliases["sf"] = "san francisco 49ers"
	m.teamAliases["ravens"] = "baltimore ravens"
	m.teamAliases["bal"] = "baltimore ravens"
	m.teamAliases["bills"] = "buffalo bills"
	m.teamAliases["buf"] = "buffalo bills"
	m.teamAliases["bengals"] = "cincinnati bengals"
	m.teamAliases["cin"] = "cincinnati bengals"
	m.teamAliases["lions"] = "detroit lions"
	m.teamAliases["det"] = "detroit lions"

	// NHL teams
	m.teamAliases["rangers"] = "new york rangers"
	m.teamAliases["nyr"] = "new york rangers"
	m.teamAliases["islanders"] = "new york islanders"
	m.teamAliases["nyi"] = "new york islanders"
	m.teamAliases["bruins"] = "boston bruins"
	m.teamAliases["canadiens"] = "montreal canadiens"
	m.teamAliases["habs"] = "montreal canadiens"
	m.teamAliases["mtl"] = "montreal canadiens"
	m.teamAliases["maple leafs"] = "toronto maple leafs"
	m.teamAliases["leafs"] = "toronto maple leafs"
	m.teamAliases["tor"] = "toronto maple leafs"
	m.teamAliases["penguins"] = "pittsburgh penguins"
	m.teamAliases["pens"] = "pittsburgh penguins"
	m.teamAliases["pit"] = "pittsburgh penguins"
	m.teamAliases["kings"] = "los angeles kings"
	m.teamAliases["lak"] = "los angeles kings"

	// EPL teams
	m.teamAliases["man utd"] = "manchester united"
	m.teamAliases["man u"] = "manchester united"
	m.teamAliases["mufc"] = "manchester united"
	m.teamAliases["man city"] = "manchester city"
	m.teamAliases["mcfc"] = "manchester city"
	m.teamAliases["spurs"] = "tottenham hotspur"
	m.teamAliases["tottenham"] = "tottenham hotspur"
	m.teamAliases["thfc"] = "tottenham hotspur"
	m.teamAliases["gunners"] = "arsenal"
	m.teamAliases["afc"] = "arsenal"
	m.teamAliases["reds"] = "liverpool"
	m.teamAliases["lfc"] = "liverpool"
	m.teamAliases["blues"] = "chelsea"
	m.teamAliases["cfc"] = "chelsea"
}

// FindMatchingMarkets finds all matching markets between platforms
func (m *Matcher) FindMatchingMarkets(sportsTags []string) ([]MarketPair, error) {
	log.Println("Matcher: Finding matching markets between Polymarket and Kalshi...")

	// Fetch Polymarket sports markets
	polyMarkets, err := m.fetchPolymarketSports(sportsTags)
	if err != nil {
		return nil, err
	}
	log.Printf("Matcher: Found %d Polymarket sports markets", len(polyMarkets))

	// Fetch Kalshi sports markets
	kalshiMarkets, err := m.kalshiClient.FindSportsMarkets()
	if err != nil {
		return nil, err
	}
	log.Printf("Matcher: Found %d Kalshi sports markets", len(kalshiMarkets))

	// Match markets
	var pairs []MarketPair
	for i := range polyMarkets {
		polyMarket := &polyMarkets[i]

		// Extract team names from Polymarket question
		polyTeams := m.extractTeams(polyMarket.Question)
		log.Printf("Matcher: Extracted teams from Polymarket: %s vs %s", polyTeams[0], polyTeams[1])
		if polyTeams[0] == "" || polyTeams[1] == "" {
			continue
		}

		// Find best match in Kalshi
		var bestMatch *kalshi.Market
		var bestScore float64
		var bestKalshiTeams [2]string

		for j := range kalshiMarkets {
			kalshiMarket := &kalshiMarkets[j]

			// Skip non-open markets
			if kalshiMarket.Status != "open" {
				continue
			}

			// Extract team names from Kalshi title
			kalshiTeams := m.extractTeams(kalshiMarket.Title)
			log.Printf("Matcher: Extracted teams from Kalshi: %s vs %s", kalshiTeams[0], kalshiTeams[1])
			if kalshiTeams[0] == "" || kalshiTeams[1] == "" {
				continue
			}

			// DEBUG: Log candidates to see what's being compared
			log.Printf("Matcher: Comparing Poly [%s vs %s] <-> Kalshi [%s vs %s]",
				polyTeams[0], polyTeams[1], kalshiTeams[0], kalshiTeams[1])

			// Calculate match score
			score := m.calculateMatchScore(polyTeams, kalshiTeams)

			if score > bestScore && score >= 0.7 { // Minimum 70% match
				bestScore = score
				bestMatch = kalshiMarket
				bestKalshiTeams = kalshiTeams
			}
		}

		if bestMatch != nil {
			pairs = append(pairs, MarketPair{
				PolyMarket:   polyMarket,
				KalshiMarket: bestMatch,
				MatchScore:   bestScore,
				MatchReason:  m.getMatchReason(bestScore),
				PolyTeams:    polyTeams,
				KalshiTeams:  bestKalshiTeams,
			})
		}
	}

	log.Printf("Matcher: Found %d matched pairs", len(pairs))
	return pairs, nil
}

// fetchPolymarketSports fetches moneyline sports markets from Polymarket
func (m *Matcher) fetchPolymarketSports(tags []string) ([]gamma.Market, error) {
	var allMarkets []gamma.Market
	seen := make(map[string]bool)

	// Iterate over each tag to ensure we get what the user wants
	for _, tag := range tags {
		// Try both original tag and lowercase slug
		searchTags := []string{tag}
		if lower := strings.ToLower(tag); lower != tag {
			searchTags = append(searchTags, lower)
		}

		for _, t := range searchTags {
			log.Printf("Matcher: Fetching Polymarket sports for tag: %s", t)

			for page := 0; page < 5; page++ { // Reduced pages per tag since we are more specific
				markets, err := m.gammaClient.GetMarkets(gamma.GetMarketsParams{
					Limit:             100,
					Offset:            page * 100,
					Active:            true,
					SportsMarketTypes: "moneyline",
					Order:             "volume", // Prioritize volume
					Tag:               t,
				})
				if err != nil {
					log.Printf("Matcher: Error fetching tag %s page %d: %v", t, page, err)
					break
				}

				if len(markets) == 0 {
					break
				}

				for _, market := range markets {
					if !seen[market.ID] {
						allMarkets = append(allMarkets, market)
						seen[market.ID] = true
					}
				}
			}
		}
	}

	return allMarkets, nil
}

// extractTeams extracts team names from a market question/title
func (m *Matcher) extractTeams(text string) [2]string {
	var teams [2]string

	// Common patterns:
	// "Lakers vs. Warriors"
	// "Lakers @ Warriors"
	// "Team A vs Team B: Moneyline"
	// "Will Lakers beat Warriors?"

	textLower := strings.ToLower(text)

	// Try "vs" pattern first
	vsPatterns := []string{" vs ", " vs. ", " versus "}
	for _, pattern := range vsPatterns {
		if idx := strings.Index(textLower, pattern); idx > 0 {
			team1 := m.cleanTeamName(text[:idx])
			rest := text[idx+len(pattern):]

			// Remove trailing stuff like ": Moneyline", "O/U", etc.
			team2 := m.cleanTeamName(m.trimAfterColonOrSpecial(rest))

			teams[0] = m.normalizeTeamName(team1)
			teams[1] = m.normalizeTeamName(team2)
			return teams
		}
	}

	// Try "@" pattern
	if idx := strings.Index(text, " @ "); idx > 0 {
		team1 := m.cleanTeamName(text[:idx])
		rest := text[idx+3:]
		team2 := m.cleanTeamName(m.trimAfterColonOrSpecial(rest))

		teams[0] = m.normalizeTeamName(team1)
		teams[1] = m.normalizeTeamName(team2)
		return teams
	}

	return teams
}

// cleanTeamName removes common prefixes/suffixes
func (m *Matcher) cleanTeamName(name string) string {
	name = strings.TrimSpace(name)

	// Remove common prefixes
	prefixes := []string{"will ", "can ", "the "}
	for _, p := range prefixes {
		if strings.HasPrefix(strings.ToLower(name), p) {
			name = name[len(p):]
		}
	}

	// Remove "beat", "win", etc.
	suffixes := []string{" win", " beat", " defeat"}
	for _, s := range suffixes {
		if idx := strings.Index(strings.ToLower(name), s); idx > 0 {
			name = name[:idx]
		}
	}

	return strings.TrimSpace(name)
}

// trimAfterColonOrSpecial removes text after colons or special markers
func (m *Matcher) trimAfterColonOrSpecial(text string) string {
	markers := []string{":", " - ", " | ", "?"}
	result := text

	for _, marker := range markers {
		if idx := strings.Index(result, marker); idx > 0 {
			result = result[:idx]
		}
	}

	return strings.TrimSpace(result)
}

// normalizeTeamName converts a team name to canonical form
func (m *Matcher) normalizeTeamName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))

	// Remove special characters
	re := regexp.MustCompile(`[^a-z0-9\s]`)
	name = re.ReplaceAllString(name, "")
	name = strings.TrimSpace(name)

	// Check aliases
	m.mu.RLock()
	if canonical, ok := m.teamAliases[name]; ok {
		m.mu.RUnlock()
		return canonical
	}
	m.mu.RUnlock()

	return name
}

// calculateMatchScore calculates similarity between two team pairs
func (m *Matcher) calculateMatchScore(teams1, teams2 [2]string) float64 {
	// Perfect match: both teams match exactly
	if (teams1[0] == teams2[0] && teams1[1] == teams2[1]) ||
		(teams1[0] == teams2[1] && teams1[1] == teams2[0]) {
		return 1.0
	}

	// Partial match: check Levenshtein distance for each pair
	score1 := (m.stringSimilarity(teams1[0], teams2[0]) + m.stringSimilarity(teams1[1], teams2[1])) / 2
	score2 := (m.stringSimilarity(teams1[0], teams2[1]) + m.stringSimilarity(teams1[1], teams2[0])) / 2

	if score1 > score2 {
		return score1
	}
	return score2
}

// stringSimilarity calculates similarity between two strings (0-1)
func (m *Matcher) stringSimilarity(s1, s2 string) float64 {
	if s1 == s2 {
		return 1.0
	}

	// Check if one contains the other
	if strings.Contains(s1, s2) || strings.Contains(s2, s1) {
		longer := len(s1)
		if len(s2) > longer {
			longer = len(s2)
		}
		shorter := len(s1)
		if len(s2) < shorter {
			shorter = len(s2)
		}
		return float64(shorter) / float64(longer)
	}

	// Simple Levenshtein distance based similarity
	distance := m.levenshtein(s1, s2)
	maxLen := len(s1)
	if len(s2) > maxLen {
		maxLen = len(s2)
	}

	if maxLen == 0 {
		return 0
	}

	return 1.0 - float64(distance)/float64(maxLen)
}

// levenshtein calculates the Levenshtein distance between two strings
func (m *Matcher) levenshtein(s1, s2 string) int {
	if len(s1) == 0 {
		return len(s2)
	}
	if len(s2) == 0 {
		return len(s1)
	}

	// Create matrix
	matrix := make([][]int, len(s1)+1)
	for i := range matrix {
		matrix[i] = make([]int, len(s2)+1)
	}

	// Initialize first row and column
	for i := 0; i <= len(s1); i++ {
		matrix[i][0] = i
	}
	for j := 0; j <= len(s2); j++ {
		matrix[0][j] = j
	}

	// Fill matrix
	for i := 1; i <= len(s1); i++ {
		for j := 1; j <= len(s2); j++ {
			cost := 1
			if s1[i-1] == s2[j-1] {
				cost = 0
			}

			matrix[i][j] = min(
				matrix[i-1][j]+1,      // deletion
				matrix[i][j-1]+1,      // insertion
				matrix[i-1][j-1]+cost, // substitution
			)
		}
	}

	return matrix[len(s1)][len(s2)]
}

// getMatchReason returns a human-readable match reason
func (m *Matcher) getMatchReason(score float64) string {
	if score >= 0.95 {
		return "exact_match"
	} else if score >= 0.85 {
		return "high_confidence"
	} else if score >= 0.75 {
		return "fuzzy_match"
	}
	return "low_confidence"
}

func min(a, b, c int) int {
	if a < b {
		if a < c {
			return a
		}
		return c
	}
	if b < c {
		return b
	}
	return c
}
