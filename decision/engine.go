package decision

import (
	"encoding/json"
	"fmt"
	"log"
	"nofx/market"
	"nofx/mcp"
	"nofx/pool"
	"strings"
	"time"
)

// PositionInfo Position information
type PositionInfo struct {
	Symbol           string  `json:"symbol"`
	Side             string  `json:"side"` // "long" or "short"
	EntryPrice       float64 `json:"entry_price"`
	MarkPrice        float64 `json:"mark_price"`
	Quantity         float64 `json:"quantity"`
	Leverage         int     `json:"leverage"`
	UnrealizedPnL    float64 `json:"unrealized_pnl"`
	UnrealizedPnLPct float64 `json:"unrealized_pnl_pct"`
	LiquidationPrice float64 `json:"liquidation_price"`
	MarginUsed       float64 `json:"margin_used"`
	UpdateTime       int64   `json:"update_time"` // Position update timestamp (milliseconds)
}

// AccountInfo Account information
type AccountInfo struct {
	TotalEquity      float64 `json:"total_equity"`      // Account equity
	AvailableBalance float64 `json:"available_balance"` // Available balance
	TotalPnL         float64 `json:"total_pnl"`         // Total P&L
	TotalPnLPct      float64 `json:"total_pnl_pct"`     // Total P&L percentage
	MarginUsed       float64 `json:"margin_used"`       // Margin used
	MarginUsedPct    float64 `json:"margin_used_pct"`   // Margin usage rate
	PositionCount    int     `json:"position_count"`    // Position count
}

// CandidateCoin Candidate coin (from coin pool)
type CandidateCoin struct {
	Symbol  string   `json:"symbol"`
	Sources []string `json:"sources"` // Sources: "ai500" and/or "oi_top"
}

// OITopData Open interest growth Top data (for AI decision reference)
type OITopData struct {
	Rank              int     // OI Top ranking
	OIDeltaPercent    float64 // Open interest change percentage (1 hour)
	OIDeltaValue      float64 // Open interest change value
	PriceDeltaPercent float64 // Price change percentage
	NetLong           float64 // Net long position
	NetShort          float64 // Net short position
}

// Context Trading context (complete information passed to AI)
type Context struct {
	CurrentTime     string                  `json:"current_time"`
	RuntimeMinutes  int                     `json:"runtime_minutes"`
	CallCount       int                     `json:"call_count"`
	Account         AccountInfo             `json:"account"`
	Positions       []PositionInfo          `json:"positions"`
	CandidateCoins  []CandidateCoin         `json:"candidate_coins"`
	MarketDataMap   map[string]*market.Data `json:"-"` // Not serialized, but used internally
	OITopDataMap    map[string]*OITopData   `json:"-"` // OI Top data mapping
	Performance     interface{}             `json:"-"` // Historical performance analysis (logger.PerformanceAnalysis)
	BTCETHLeverage  int                     `json:"-"` // BTC/ETH leverage multiplier (read from config)
	AltcoinLeverage int                     `json:"-"` // Altcoin leverage multiplier (read from config)
}

// Decision AI trading decision
type Decision struct {
	Symbol          string  `json:"symbol"`
	Action          string  `json:"action"` // "open_long", "open_short", "close_long", "close_short", "hold", "wait"
	Leverage        int     `json:"leverage,omitempty"`
	PositionSizeUSD float64 `json:"position_size_usd,omitempty"`
	StopLoss        float64 `json:"stop_loss,omitempty"`
	TakeProfit      float64 `json:"take_profit,omitempty"`
	Confidence      int     `json:"confidence,omitempty"` // Confidence level (0-100)
	RiskUSD         float64 `json:"risk_usd,omitempty"`   // Maximum USD risk
	Reasoning       string  `json:"reasoning"`
}

// FullDecision AI's complete decision (includes chain of thought)
type FullDecision struct {
	UserPrompt string     `json:"user_prompt"` // Input prompt sent to AI
	CoTTrace   string     `json:"cot_trace"`   // Chain of thought analysis (AI output)
	Decisions  []Decision `json:"decisions"`   // Specific decision list
	Timestamp  time.Time  `json:"timestamp"`
}

// GetFullDecision Get AI's complete trading decision (batch analyze all symbols and positions)
func GetFullDecision(ctx *Context, mcpClient *mcp.Client) (*FullDecision, error) {
	// 1. Fetch market data for all symbols
	if err := fetchMarketDataForContext(ctx); err != nil {
		return nil, fmt.Errorf("failed to fetch market data: %w", err)
	}

	// 2. Build System Prompt (fixed rules, can be cached) and User Prompt (dynamic data)
	systemPrompt := buildSystemPrompt(ctx.BTCETHLeverage, ctx.AltcoinLeverage)
	userPrompt := buildUserPrompt(ctx)

	// 3. Call AI API (using system + user prompt)
	aiResponse, err := mcpClient.CallWithMessages(systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("failed to call AI API: %w", err)
	}

	// 4. Parse AI response
	decision, err := parseFullDecisionResponse(aiResponse, ctx.Account.TotalEquity, ctx.BTCETHLeverage, ctx.AltcoinLeverage)
	if err != nil {
		return nil, fmt.Errorf("failed to parse AI response: %w", err)
	}

	decision.Timestamp = time.Now()
	decision.UserPrompt = userPrompt // Save input prompt
	return decision, nil
}

// fetchMarketDataForContext Fetch market data and OI data for all symbols in context
func fetchMarketDataForContext(ctx *Context) error {
	ctx.MarketDataMap = make(map[string]*market.Data)
	ctx.OITopDataMap = make(map[string]*OITopData)

	// Collect all symbols that need data
	symbolSet := make(map[string]bool)

	// 1. Prioritize fetching data for position symbols (required)
	for _, pos := range ctx.Positions {
		symbolSet[pos.Symbol] = true
	}

	// 2. Candidate coin count dynamically adjusted based on account status
	maxCandidates := calculateMaxCandidates(ctx)
	for i, coin := range ctx.CandidateCoins {
		if i >= maxCandidates {
			break
		}
		symbolSet[coin.Symbol] = true
	}

	// Concurrently fetch market data
	// Position symbol set (used to determine whether to skip OI check)
	positionSymbols := make(map[string]bool)
	for _, pos := range ctx.Positions {
		positionSymbols[pos.Symbol] = true
	}

	for symbol := range symbolSet {
		data, err := market.Get(symbol)
		if err != nil {
			// Single symbol failure doesn't affect overall, just log error
			continue
		}

		// ⚠️ Liquidity filter: Skip symbols with open interest value below 15M USD (no long or short)
		// Open interest value = Open interest × Current price
		// But existing positions must be kept (need to decide whether to close)
		isExistingPosition := positionSymbols[symbol]
		if !isExistingPosition && data.OpenInterest != nil && data.CurrentPrice > 0 {
			// Calculate open interest value (USD) = Open interest × Current price
			oiValue := data.OpenInterest.Latest * data.CurrentPrice
			oiValueInMillions := oiValue / 1_000_000 // Convert to millions USD
			if oiValueInMillions < 15 {
				log.Printf("⚠️  %s open interest value too low (%.2fM USD < 15M), skipping symbol [OI:%.0f × Price:%.4f]",
					symbol, oiValueInMillions, data.OpenInterest.Latest, data.CurrentPrice)
				continue
			}
		}

		ctx.MarketDataMap[symbol] = data
	}

	// Load OI Top data (doesn't affect main flow)
	oiPositions, err := pool.GetOITopPositions()
	if err == nil {
		for _, pos := range oiPositions {
			// Normalize symbol matching
			symbol := pos.Symbol
			ctx.OITopDataMap[symbol] = &OITopData{
				Rank:              pos.Rank,
				OIDeltaPercent:    pos.OIDeltaPercent,
				OIDeltaValue:      pos.OIDeltaValue,
				PriceDeltaPercent: pos.PriceDeltaPercent,
				NetLong:           pos.NetLong,
				NetShort:          pos.NetShort,
			}
		}
	}

	return nil
}

// calculateMaxCandidates Calculate the number of candidate coins to analyze based on account status
func calculateMaxCandidates(ctx *Context) int {
	// Directly return the total number of coins in candidate pool
	// Because candidate pool has already been filtered in auto_trader.go
	// Fixed to analyze top 20 highest-scored coins (from AI500)
	return len(ctx.CandidateCoins)
}

// buildSystemPrompt Build System Prompt (fixed rules, can be cached)
// Note: accountEquity is NOT included here to enable prompt caching - it changes during runtime
func buildSystemPrompt(btcEthLeverage, altcoinLeverage int) string {
	var sb strings.Builder

	// === Core Mission ===
	sb.WriteString("You are a professional cryptocurrency trading AI, conducting autonomous trading on Binance futures market.\n\n")
	sb.WriteString("# 🎯 Core Objective\n\n")
	sb.WriteString("**Maximize Sharpe Ratio**\n\n")
	sb.WriteString("Sharpe Ratio = Average Return / Return Volatility\n\n")
	sb.WriteString("**This means**:\n")
	sb.WriteString("- ✅ High-quality trades (high win rate, large profit factor) → improve Sharpe\n")
	sb.WriteString("- ✅ Stable returns, control drawdowns → improve Sharpe\n")
	sb.WriteString("- ✅ Patient holding, let profits run → improve Sharpe\n")
	sb.WriteString("- ❌ Frequent trading, small wins/losses → increase volatility, severely reduce Sharpe\n")
	sb.WriteString("- ❌ Overtrading, fee erosion → direct losses\n")
	sb.WriteString("- ❌ Premature exits, frequent entries/exits → miss major trends\n\n")
	sb.WriteString("**Key Insight**: The system scans every 3 minutes, but this doesn't mean you need to trade every time!\n")
	sb.WriteString("Most of the time should be `wait` or `hold`, only open positions at excellent opportunities.\n\n")

	// === Hard Constraints (Risk Control) ===
	sb.WriteString("# ⚖️ Hard Constraints (Risk Control)\n\n")
	sb.WriteString("1. **Risk-Reward Ratio**: Must be ≥ 1:3 (take 1% risk, earn 3%+ profit)\n")
	sb.WriteString("2. **Maximum Positions**: 3 symbols (quality > quantity)\n")
	sb.WriteString(fmt.Sprintf("3. **Single Symbol Position**: Altcoins 0.8x-1.5x account equity (%dx leverage) | BTC/ETH 5x-10x account equity (%dx leverage)\n",
		altcoinLeverage, btcEthLeverage))
	sb.WriteString("4. **Margin**: Total usage rate ≤ 90%\n\n")

	// === Short Trading Incentive ===
	sb.WriteString("# 📉 Long/Short Balance\n\n")
	sb.WriteString("**Important**: Shorting in downtrends = Longing in uptrends in terms of profit\n\n")
	sb.WriteString("- Uptrend → Go long\n")
	sb.WriteString("- Downtrend → Go short\n")
	sb.WriteString("- Sideways market → Wait\n\n")
	sb.WriteString("**Don't have long bias! Shorting is one of your core tools**\n\n")

	// === Trading Frequency Awareness ===
	sb.WriteString("# ⏱️ Trading Frequency Awareness\n\n")
	sb.WriteString("**Quantitative Standards**:\n")
	sb.WriteString("- Excellent traders: 2-4 trades per day = 0.1-0.2 trades per hour\n")
	sb.WriteString("- Overtrading: >2 trades per hour = serious problem\n")
	sb.WriteString("- Optimal rhythm: Hold positions for at least 30-60 minutes after opening\n\n")
	sb.WriteString("**Self-Check**:\n")
	sb.WriteString("If you find yourself trading every cycle → your standards are too low\n")
	sb.WriteString("If you find yourself closing positions <30 minutes → you're too impatient\n\n")

	// === Entry Signal Strength ===
	sb.WriteString("# 🎯 Entry Criteria (Strict)\n\n")
	sb.WriteString("Only open positions on **strong signals**, wait if uncertain.\n\n")
	sb.WriteString("**Complete data you have access to**:\n")
	sb.WriteString("- 📊 **Raw sequences**: 3-minute price sequence (MidPrices array) + 4-hour candlestick sequence\n")
	sb.WriteString("- 📈 **Technical sequences**: EMA20 sequence, MACD sequence, RSI7 sequence, RSI14 sequence\n")
	sb.WriteString("- 💰 **Capital sequences**: Volume sequence, Open Interest (OI) sequence, funding rate\n")
	sb.WriteString("- 🎯 **Filter tags**: AI500 score / OI_Top ranking (if annotated)\n\n")
	sb.WriteString("**Analysis methods** (completely up to you):\n")
	sb.WriteString("- Freely use sequence data, you can do but not limited to: trend analysis, pattern recognition, support/resistance, technical resistance levels, Fibonacci, volatility band calculations\n")
	sb.WriteString("- Multi-dimensional cross-validation (price + volume + OI + indicators + sequence patterns)\n")
	sb.WriteString("- Use whatever method you think is most effective to identify high-probability opportunities\n")
	sb.WriteString("- Only open positions when comprehensive confidence ≥ 75\n\n")
	sb.WriteString("**Avoid low-quality signals**:\n")
	sb.WriteString("- Single dimension (only looking at one indicator)\n")
	sb.WriteString("- Contradictory (price up but volume declining)\n")
	sb.WriteString("- Sideways consolidation\n")
	sb.WriteString("- Just closed position recently (<15 minutes)\n\n")

	// === Sharpe Ratio Self-Evolution ===
	sb.WriteString("# 🧬 Sharpe Ratio Self-Evolution\n\n")
	sb.WriteString("You will receive **Sharpe Ratio** as performance feedback each cycle:\n\n")
	sb.WriteString("**Sharpe Ratio < -0.5** (Continuous losses):\n")
	sb.WriteString("  → 🛑 Stop trading, wait and observe for at least 6 cycles (18 minutes)\n")
	sb.WriteString("  → 🔍 Deep reflection:\n")
	sb.WriteString("     • Trading frequency too high? (>2 trades per hour is excessive)\n")
	sb.WriteString("     • Holding time too short? (<30 minutes is premature exit)\n")
	sb.WriteString("     • Signal strength insufficient? (confidence <75)\n")
	sb.WriteString("     • Are you shorting? (one-sided long bias is wrong)\n\n")
	sb.WriteString("**Sharpe Ratio -0.5 ~ 0** (Slight losses):\n")
	sb.WriteString("  → ⚠️ Strict control: only trade with confidence >80\n")
	sb.WriteString("  → Reduce trading frequency: maximum 1 new position per hour\n")
	sb.WriteString("  → Patient holding: hold for at least 30 minutes\n\n")
	sb.WriteString("**Sharpe Ratio 0 ~ 0.7** (Positive returns):\n")
	sb.WriteString("  → ✅ Maintain current strategy\n\n")
	sb.WriteString("**Sharpe Ratio > 0.7** (Excellent performance):\n")
	sb.WriteString("  → 🚀 Can moderately increase position size\n\n")
	sb.WriteString("**Key**: Sharpe Ratio is the only metric, it naturally penalizes frequent trading and excessive entry/exit.\n\n")

	// === Decision Process ===
	sb.WriteString("# 📋 Decision Process\n\n")
	sb.WriteString("1. **Analyze Sharpe Ratio**: Is current strategy effective? Need adjustment?\n")
	sb.WriteString("2. **Evaluate Positions**: Has trend changed? Should take profit/stop loss?\n")
	sb.WriteString("3. **Look for New Opportunities**: Any strong signals? Long/short opportunities?\n")
	sb.WriteString("4. **Output Decision**: Chain of thought analysis + JSON\n\n")

	// === Output Format ===
	sb.WriteString("# 📤 Output Format\n\n")
	sb.WriteString("**Step 1: Chain of Thought (Plain Text)**\n")
	sb.WriteString("Briefly analyze your thought process\n\n")
	sb.WriteString("**Step 2: JSON Decision Array**\n\n")
	sb.WriteString("```json\n[\n")
	sb.WriteString(fmt.Sprintf("  {\"symbol\": \"BTCUSDT\", \"action\": \"open_short\", \"leverage\": %d, \"position_size_usd\": 5000, \"stop_loss\": 97000, \"take_profit\": 91000, \"confidence\": 85, \"risk_usd\": 300, \"reasoning\": \"Downtrend + MACD bearish crossover\"},\n", btcEthLeverage))
	sb.WriteString("  {\"symbol\": \"ETHUSDT\", \"action\": \"close_long\", \"reasoning\": \"Take profit exit\"}\n")
	sb.WriteString("]\n```\n\n")
	sb.WriteString("**Field Descriptions**:\n")
	sb.WriteString("- `action`: open_long | open_short | close_long | close_short | hold | wait\n")
	sb.WriteString("- `confidence`: 0-100 (recommend ≥75 for opening positions)\n")
	sb.WriteString("- Required for opening positions: leverage, position_size_usd, stop_loss, take_profit, confidence, risk_usd, reasoning\n\n")

	// === Key Reminders ===
	sb.WriteString("---\n\n")
	sb.WriteString("**Remember**: \n")
	sb.WriteString("- Target is Sharpe Ratio, not trading frequency\n")
	sb.WriteString("- Shorting = Longing, both are profit tools\n")
	sb.WriteString("- Better to miss than make low-quality trades\n")
	sb.WriteString("- Risk-reward ratio of 1:3 is the minimum\n")

	return sb.String()
}

// buildUserPrompt Build User Prompt (dynamic data)
func buildUserPrompt(ctx *Context) string {
	var sb strings.Builder

	// System status
	sb.WriteString(fmt.Sprintf("**Time**: %s | **Cycle**: #%d | **Runtime**: %d minutes\n\n",
		ctx.CurrentTime, ctx.CallCount, ctx.RuntimeMinutes))

	// BTC market
	if btcData, hasBTC := ctx.MarketDataMap["BTCUSDT"]; hasBTC {
		sb.WriteString(fmt.Sprintf("**BTC**: %.2f (1h: %+.2f%%, 4h: %+.2f%%) | MACD: %.4f | RSI: %.2f\n\n",
			btcData.CurrentPrice, btcData.PriceChange1h, btcData.PriceChange4h,
			btcData.CurrentMACD, btcData.CurrentRSI7))
	}

	// Account
	sb.WriteString(fmt.Sprintf("**Account**: Equity %.2f | Balance %.2f (%.1f%%) | P&L %+.2f%% | Margin %.1f%% | Positions %d\n\n",
		ctx.Account.TotalEquity,
		ctx.Account.AvailableBalance,
		(ctx.Account.AvailableBalance/ctx.Account.TotalEquity)*100,
		ctx.Account.TotalPnLPct,
		ctx.Account.MarginUsedPct,
		ctx.Account.PositionCount))
	
	// Position size limits (based on current account equity)
	sb.WriteString(fmt.Sprintf("**Position Size Limits**: Altcoins %.0f-%.0f USDT (%dx leverage) | BTC/ETH %.0f-%.0f USDT (%dx leverage)\n\n",
		ctx.Account.TotalEquity*0.8, ctx.Account.TotalEquity*1.5, ctx.AltcoinLeverage,
		ctx.Account.TotalEquity*5, ctx.Account.TotalEquity*10, ctx.BTCETHLeverage))

	// Positions (complete market data)
	if len(ctx.Positions) > 0 {
		sb.WriteString("## Current Positions\n")
		for i, pos := range ctx.Positions {
			// Calculate holding duration
			holdingDuration := ""
			if pos.UpdateTime > 0 {
				durationMs := time.Now().UnixMilli() - pos.UpdateTime
				durationMin := durationMs / (1000 * 60) // Convert to minutes
				if durationMin < 60 {
					holdingDuration = fmt.Sprintf(" | Holding duration: %d minutes", durationMin)
				} else {
					durationHour := durationMin / 60
					durationMinRemainder := durationMin % 60
					holdingDuration = fmt.Sprintf(" | Holding duration: %d hours %d minutes", durationHour, durationMinRemainder)
				}
			}

			sb.WriteString(fmt.Sprintf("%d. %s %s | Entry %.4f Current %.4f | P&L %+.2f%% | Leverage %dx | Margin %.0f | Liq %.4f%s\n\n",
				i+1, pos.Symbol, strings.ToUpper(pos.Side),
				pos.EntryPrice, pos.MarkPrice, pos.UnrealizedPnLPct,
				pos.Leverage, pos.MarginUsed, pos.LiquidationPrice, holdingDuration))

			// Use FormatMarketData to output complete market data
			if marketData, ok := ctx.MarketDataMap[pos.Symbol]; ok {
				sb.WriteString(market.Format(marketData))
				sb.WriteString("\n")
			}
		}
	} else {
		sb.WriteString("**Current Positions**: None\n\n")
	}

	// Candidate coins (complete market data)
	sb.WriteString(fmt.Sprintf("## Candidate Coins (%d)\n\n", len(ctx.MarketDataMap)))
	displayedCount := 0
	for _, coin := range ctx.CandidateCoins {
		marketData, hasData := ctx.MarketDataMap[coin.Symbol]
		if !hasData {
			continue
		}
		displayedCount++

		sourceTags := ""
		if len(coin.Sources) > 1 {
			sourceTags = " (AI500+OI_Top dual signal)"
		} else if len(coin.Sources) == 1 && coin.Sources[0] == "oi_top" {
			sourceTags = " (OI_Top open interest growth)"
		}

		// Use FormatMarketData to output complete market data
		sb.WriteString(fmt.Sprintf("### %d. %s%s\n\n", displayedCount, coin.Symbol, sourceTags))
		sb.WriteString(market.Format(marketData))
		sb.WriteString("\n")
	}
	sb.WriteString("\n")

	// Sharpe Ratio (pass value directly, no complex formatting)
	if ctx.Performance != nil {
		// Directly extract SharpeRatio from interface{}
		type PerformanceData struct {
			SharpeRatio float64 `json:"sharpe_ratio"`
		}
		var perfData PerformanceData
		if jsonData, err := json.Marshal(ctx.Performance); err == nil {
			if err := json.Unmarshal(jsonData, &perfData); err == nil {
				sb.WriteString(fmt.Sprintf("## 📊 Sharpe Ratio: %.2f\n\n", perfData.SharpeRatio))
			}
		}
	}

	sb.WriteString("---\n\n")
	sb.WriteString("Now please analyze and output your decision (Chain of Thought + JSON)\n")

	return sb.String()
}

// parseFullDecisionResponse Parse AI's complete decision response
func parseFullDecisionResponse(aiResponse string, accountEquity float64, btcEthLeverage, altcoinLeverage int) (*FullDecision, error) {
	// 1. Extract chain of thought
	cotTrace := extractCoTTrace(aiResponse)

	// 2. Extract JSON decision list
	decisions, err := extractDecisions(aiResponse)
	if err != nil {
		return &FullDecision{
			CoTTrace:  cotTrace,
			Decisions: []Decision{},
		}, fmt.Errorf("failed to extract decisions: %w\n\n=== AI Chain of Thought ===\n%s", err, cotTrace)
	}

	// 3. Validate decisions
	if err := validateDecisions(decisions, accountEquity, btcEthLeverage, altcoinLeverage); err != nil {
		return &FullDecision{
			CoTTrace:  cotTrace,
			Decisions: decisions,
		}, fmt.Errorf("decision validation failed: %w\n\n=== AI Chain of Thought ===\n%s", err, cotTrace)
	}

	return &FullDecision{
		CoTTrace:  cotTrace,
		Decisions: decisions,
	}, nil
}

// extractCoTTrace 提取思维链分析
func extractCoTTrace(response string) string {
	// 查找JSON数组的开始位置
	jsonStart := strings.Index(response, "[")

	if jsonStart > 0 {
		// 思维链是JSON数组之前的内容
		return strings.TrimSpace(response[:jsonStart])
	}

	// 如果找不到JSON，整个响应都是思维链
	return strings.TrimSpace(response)
}

// extractDecisions 提取JSON决策列表
func extractDecisions(response string) ([]Decision, error) {
	// 直接查找JSON数组 - 找第一个完整的JSON数组
	arrayStart := strings.Index(response, "[")
	if arrayStart == -1 {
		return nil, fmt.Errorf("无法找到JSON数组起始")
	}

	// 从 [ 开始，匹配括号找到对应的 ]
	arrayEnd := findMatchingBracket(response, arrayStart)
	if arrayEnd == -1 {
		return nil, fmt.Errorf("无法找到JSON数组结束")
	}

	jsonContent := strings.TrimSpace(response[arrayStart : arrayEnd+1])

	// 🔧 修复常见的JSON格式错误：缺少引号的字段值
	// 匹配: "reasoning": 内容"}  或  "reasoning": 内容}  (没有引号)
	// 修复为: "reasoning": "内容"}
	// 使用简单的字符串扫描而不是正则表达式
	jsonContent = fixMissingQuotes(jsonContent)

	// 解析JSON
	var decisions []Decision
	if err := json.Unmarshal([]byte(jsonContent), &decisions); err != nil {
		return nil, fmt.Errorf("JSON解析失败: %w\nJSON内容: %s", err, jsonContent)
	}

	return decisions, nil
}

// fixMissingQuotes Replace Chinese quotes with English quotes (avoid IME auto-conversion)
func fixMissingQuotes(jsonStr string) string {
	jsonStr = strings.ReplaceAll(jsonStr, "\u201c", "\"") // "
	jsonStr = strings.ReplaceAll(jsonStr, "\u201d", "\"") // "
	jsonStr = strings.ReplaceAll(jsonStr, "\u2018", "'")  // '
	jsonStr = strings.ReplaceAll(jsonStr, "\u2019", "'")  // '
	return jsonStr
}

// validateDecisions Validate all decisions (requires account info and leverage config)
func validateDecisions(decisions []Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int) error {
	for i, decision := range decisions {
		if err := validateDecision(&decision, accountEquity, btcEthLeverage, altcoinLeverage); err != nil {
			return fmt.Errorf("decision #%d validation failed: %w", i+1, err)
		}
	}
	return nil
}

// findMatchingBracket Find matching closing bracket
func findMatchingBracket(s string, start int) int {
	if start >= len(s) || s[start] != '[' {
		return -1
	}

	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return i
			}
		}
	}

	return -1
}

// validateDecision Validate single decision validity
func validateDecision(d *Decision, accountEquity float64, btcEthLeverage, altcoinLeverage int) error {
	// Validate action
	validActions := map[string]bool{
		"open_long":   true,
		"open_short":  true,
		"close_long":  true,
		"close_short": true,
		"hold":        true,
		"wait":        true,
	}

	if !validActions[d.Action] {
		return fmt.Errorf("invalid action: %s", d.Action)
	}

	// Opening positions must provide complete parameters
	if d.Action == "open_long" || d.Action == "open_short" {
		// Use configured leverage limit based on symbol
		maxLeverage := altcoinLeverage          // Altcoins use configured leverage
		maxPositionValue := accountEquity * 1.5 // Altcoins max 1.5x account equity
		if d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" {
			maxLeverage = btcEthLeverage          // BTC and ETH use configured leverage
			maxPositionValue = accountEquity * 10 // BTC/ETH max 10x account equity
		}

		if d.Leverage <= 0 || d.Leverage > maxLeverage {
			return fmt.Errorf("leverage must be between 1-%d (%s, current config limit %dx): %d", maxLeverage, d.Symbol, maxLeverage, d.Leverage)
		}
		if d.PositionSizeUSD <= 0 {
			return fmt.Errorf("position size must be greater than 0: %.2f", d.PositionSizeUSD)
		}
		// Validate position value limit (add 1% tolerance to avoid floating point precision issues)
		tolerance := maxPositionValue * 0.01 // 1% tolerance
		if d.PositionSizeUSD > maxPositionValue+tolerance {
			if d.Symbol == "BTCUSDT" || d.Symbol == "ETHUSDT" {
				return fmt.Errorf("BTC/ETH single symbol position value cannot exceed %.0f USDT (10x account equity), actual: %.0f", maxPositionValue, d.PositionSizeUSD)
			} else {
				return fmt.Errorf("altcoin single symbol position value cannot exceed %.0f USDT (1.5x account equity), actual: %.0f", maxPositionValue, d.PositionSizeUSD)
			}
		}
		if d.StopLoss <= 0 || d.TakeProfit <= 0 {
			return fmt.Errorf("stop loss and take profit must be greater than 0")
		}

		// Validate stop loss and take profit reasonableness
		if d.Action == "open_long" {
			if d.StopLoss >= d.TakeProfit {
				return fmt.Errorf("when going long, stop loss price must be less than take profit price")
			}
		} else {
			if d.StopLoss <= d.TakeProfit {
				return fmt.Errorf("when going short, stop loss price must be greater than take profit price")
			}
		}

		// Validate risk-reward ratio (must be ≥1:3)
		// Calculate entry price (assume current market price)
		var entryPrice float64
		if d.Action == "open_long" {
			// Long: entry price between stop loss and take profit
			entryPrice = d.StopLoss + (d.TakeProfit-d.StopLoss)*0.2 // Assume entry at 20% position
		} else {
			// Short: entry price between stop loss and take profit
			entryPrice = d.StopLoss - (d.StopLoss-d.TakeProfit)*0.2 // Assume entry at 20% position
		}

		var riskPercent, rewardPercent, riskRewardRatio float64
		if d.Action == "open_long" {
			riskPercent = (entryPrice - d.StopLoss) / entryPrice * 100
			rewardPercent = (d.TakeProfit - entryPrice) / entryPrice * 100
			if riskPercent > 0 {
				riskRewardRatio = rewardPercent / riskPercent
			}
		} else {
			riskPercent = (d.StopLoss - entryPrice) / entryPrice * 100
			rewardPercent = (entryPrice - d.TakeProfit) / entryPrice * 100
			if riskPercent > 0 {
				riskRewardRatio = rewardPercent / riskPercent
			}
		}

		// Hard constraint: risk-reward ratio must be ≥3.0
		if riskRewardRatio < 3.0 {
			return fmt.Errorf("risk-reward ratio too low (%.2f:1), must be ≥3.0:1 [Risk:%.2f%% Reward:%.2f%%] [Stop Loss:%.2f Take Profit:%.2f]",
				riskRewardRatio, riskPercent, rewardPercent, d.StopLoss, d.TakeProfit)
		}
	}

	return nil
}
