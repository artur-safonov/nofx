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
	Symbol                string  `json:"symbol"`
	Action                string  `json:"action"` // "open_long", "open_short", "close_long", "close_short", "hold", "wait"
	Leverage              int     `json:"leverage,omitempty"`
	PositionSizeUSD       float64 `json:"position_size_usd,omitempty"`
	StopLoss              float64 `json:"stop_loss,omitempty"`
	TakeProfit            float64 `json:"take_profit,omitempty"`
	InvalidationCondition string  `json:"invalidation_condition,omitempty"` // Mandatory for new positions
	Confidence            int     `json:"confidence,omitempty"`             // Confidence level (0-100)
	RiskUSD               float64 `json:"risk_usd,omitempty"`                // Maximum USD risk
	Reasoning             string  `json:"reasoning"`
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
	sb.WriteString("You are an expert systematic cryptocurrency futures trader. Your primary objectives are:\n\n")
	sb.WriteString("1. **Maximize profit after accounting for fees** - Consider all trading costs including fees, slippage, and funding rates\n")
	sb.WriteString("2. **Avoid over-trading** - Be selective and disciplined in your trades\n")
	sb.WriteString("3. **Hunt for market advantages** - Identify and exploit alpha opportunities in the market\n\n")
	sb.WriteString("**Performance Metrics**:\n")
	sb.WriteString("- Sharpe Ratio = Average Return / Return Volatility (monitored for risk-adjusted performance)\n")
	sb.WriteString("- Total Return % (primary profit metric after all costs)\n\n")
	sb.WriteString("**Key Insight**: The system scans every 3 minutes, but this doesn't mean you need to trade every time!\n")
	sb.WriteString("Most of the time should be `wait` or `hold`, only open positions at excellent opportunities.\n")
	sb.WriteString("Quality over quantity - better to miss than make low-quality trades.\n\n")

	// === Position Management ===
	sb.WriteString("# 📊 Position Management\n\n")
	sb.WriteString(fmt.Sprintf("- Altcoins: 0.8x-1.5x account equity (%dx leverage) | BTC/ETH: 5x-10x account equity (%dx leverage)\n", altcoinLeverage, btcEthLeverage))
	sb.WriteString("- Maximum 3 positions total (quality > quantity)\n")
	sb.WriteString("- **No pyramiding allowed** - Size positions correctly from the start, no adding to existing positions\n")
	sb.WriteString("- Only one position per coin at a time\n")
	sb.WriteString("- For each coin, choose exactly ONE action per trading cycle:\n")
	sb.WriteString("  - **open_long** - Enter a long position (only if flat)\n")
	sb.WriteString("  - **open_short** - Enter a short position (only if flat)\n")
	sb.WriteString("  - **close_long** - Exit long position\n")
	sb.WriteString("  - **close_short** - Exit short position\n")
	sb.WriteString("  - **hold** - Maintain existing position\n")
	sb.WriteString("  - **wait** - No action, wait for better opportunity\n\n")

	// === Hard Constraints (Risk Control) ===
	sb.WriteString("# ⚖️ Hard Constraints (Risk Control)\n\n")
	sb.WriteString("1. **Risk-Reward Ratio**: Must be ≥ 1:3 (take 1% risk, earn 3%+ profit) - This is the MINIMUM threshold\n")
	sb.WriteString("2. **Maximum Positions**: 3 symbols (quality > quantity)\n")
	sb.WriteString("3. **Margin**: Total usage rate ≤ 90%\n")
	sb.WriteString("4. **Transaction Costs**: Always factor in fees, slippage, and funding rates in profit calculations\n\n")

	// === Short Trading Incentive ===
	sb.WriteString("# 📉 Long/Short Balance\n\n")
	sb.WriteString("**Important**: Shorting in downtrends = Longing in uptrends in terms of profit\n\n")
	sb.WriteString("**Don't have long bias! Shorting is one of your core tools**\n\n")

	// === Risk Management ===
	sb.WriteString("# ⚖️ Risk Management\n\n")
	sb.WriteString(fmt.Sprintf("- Use leverage based on confidence level (maximum: Altcoins %dx, BTC/ETH %dx)\n", altcoinLeverage, btcEthLeverage))
	sb.WriteString("- Always set: **Stop loss**, **Take profit**, **Invalidation condition**\n")
	sb.WriteString("- Risk per trade should be calibrated based on confidence level (0-100 scale)\n")
	sb.WriteString("- Risk-reward ratio must be ≥ 1:3 (minimum threshold)\n\n")

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
	sb.WriteString("1. Review all existing positions first\n")
	sb.WriteString("2. Check if invalidation conditions have been triggered\n")
	sb.WriteString("3. Evaluate new entry opportunities only if you have available capital\n")
	sb.WriteString("4. Consider market structure, momentum, and risk/reward\n")
	sb.WriteString("5. Account for transaction costs in all decisions\n\n")

	// === Trading Philosophy ===
	sb.WriteString("# 💭 Trading Philosophy\n\n")
	sb.WriteString("- Be systematic and disciplined\n")
	sb.WriteString("- Don't close positions early unless invalidation conditions are met\n")
	sb.WriteString("- Consider both short-term (3-minute) and longer-term (4-hour) timeframes\n")
	sb.WriteString("- Balance aggression with capital preservation\n")
	sb.WriteString("- Think in terms of risk-adjusted returns, not just absolute profits\n\n")

	// === Output Format ===
	sb.WriteString("# 📤 Output Format\n\n")
	sb.WriteString("**Chain of Thought**: Before making decisions, analyze:\n")
	sb.WriteString("1. Current market conditions and trend direction\n")
	sb.WriteString("2. Position review - check all invalidation conditions\n")
	sb.WriteString("3. Risk assessment - available capital and position sizing\n")
	sb.WriteString("4. Entry/exit logic based on technical indicators\n")
	sb.WriteString("5. Confidence calibration based on signal strength\n\n")
	sb.WriteString("**JSON Decision Array**:\n\n")
	sb.WriteString("```json\n[\n")
	sb.WriteString(fmt.Sprintf("  {\"symbol\": \"BTCUSDT\", \"action\": \"open_short\", \"leverage\": %d, \"position_size_usd\": 5000, \"stop_loss\": 97000, \"take_profit\": 91000, \"confidence\": 85, \"risk_usd\": 300, \"reasoning\": \"Downtrend + MACD bearish crossover\", \"invalidation_condition\": \"If 4-hour MACD crosses above 500\"},\n", btcEthLeverage))
	sb.WriteString("  {\"symbol\": \"ETHUSDT\", \"action\": \"close_long\", \"reasoning\": \"Invalidation condition triggered\"}\n")
	sb.WriteString("]\n```\n\n")
    sb.WriteString("**Required for opening positions**: symbol, action, leverage, position_size_usd, stop_loss, take_profit, invalidation_condition, confidence, risk_usd, reasoning\n\n")

	// === Key Reminders ===
	sb.WriteString("---\n\n")
	sb.WriteString("**Remember**: \n")
	sb.WriteString("- Maximize profit after fees - Primary objective\n")
	sb.WriteString("- Avoid over-trading - Quality over quantity\n")
	sb.WriteString("- Invalidation conditions are mandatory - Monitor constantly\n")
	sb.WriteString("- Confidence-based scaling - Use confidence for leverage and risk\n")
	sb.WriteString("- Risk-reward ratio ≥ 1:3 - Never compromise\n")
	sb.WriteString("- Shorting = Longing - Both are profit tools\n")
	sb.WriteString("- Better to miss than make low-quality trades\n")

	return sb.String()
}

// buildUserPrompt Build User Prompt (dynamic data)
func buildUserPrompt(ctx *Context) string {
	var sb strings.Builder

	// System status
	sb.WriteString(fmt.Sprintf("It has been %d minutes since you started trading. The current time is %s and you've been invoked %d times. Below, we are providing you with a variety of state data, price data, and predictive signals so you can discover alpha. Below that is your current account information, value, performance, positions, etc.\n\n",
		ctx.RuntimeMinutes, ctx.CurrentTime, ctx.CallCount))

	// Explicit ordering statement
	sb.WriteString("**ALL OF THE PRICE OR SIGNAL DATA BELOW IS ORDERED: OLDEST → NEWEST**\n\n")
	sb.WriteString("**Timeframes note**: Unless stated otherwise in a section title, intraday series are provided at 3‑minute intervals. If a coin uses a different interval, it is explicitly stated in that coin's section.\n\n")

	// Show all coins' market data upfront (equal treatment)
	sb.WriteString("## CURRENT MARKET STATE FOR ALL COINS\n\n")
	
	// Collect all symbols to display
	allSymbols := make([]string, 0)
	symbolSet := make(map[string]bool)
	
	// Add position symbols
	for _, pos := range ctx.Positions {
		if !symbolSet[pos.Symbol] {
			allSymbols = append(allSymbols, pos.Symbol)
			symbolSet[pos.Symbol] = true
		}
	}
	
	// Add candidate coin symbols
	for _, coin := range ctx.CandidateCoins {
		if !symbolSet[coin.Symbol] && ctx.MarketDataMap[coin.Symbol] != nil {
			allSymbols = append(allSymbols, coin.Symbol)
			symbolSet[coin.Symbol] = true
		}
	}
	
	// Display all coins' data
	for _, symbol := range allSymbols {
		marketData := ctx.MarketDataMap[symbol]
		if marketData == nil {
			continue
		}
		
		// Get coin name (remove USDT suffix for display)
		coinName := strings.Replace(symbol, "USDT", "", 1)
		sb.WriteString(fmt.Sprintf("### ALL %s DATA\n\n", coinName))
		sb.WriteString(market.Format(marketData))
		sb.WriteString("\n")
	}

	// Account information with Total Return %
	sb.WriteString("## HERE IS YOUR ACCOUNT INFORMATION & PERFORMANCE\n\n")
	sb.WriteString(fmt.Sprintf("Current Total Return (percent): %.2f%%\n\n", ctx.Account.TotalPnLPct))
	sb.WriteString(fmt.Sprintf("Available Cash: %.2f\n\n", ctx.Account.AvailableBalance))
	sb.WriteString(fmt.Sprintf("Current Account Value: %.2f\n\n", ctx.Account.TotalEquity))
	sb.WriteString(fmt.Sprintf("Current live positions & performance:\n\n"))

	// Positions with exit plan structure
	if len(ctx.Positions) > 0 {
		for _, pos := range ctx.Positions {
			sb.WriteString(fmt.Sprintf("{'symbol': '%s', 'quantity': %.2f, 'entry_price': %.2f, 'current_price': %.2f, 'liquidation_price': %.2f, 'unrealized_pnl': %.2f, 'leverage': %d",
				pos.Symbol, pos.Quantity, pos.EntryPrice, pos.MarkPrice, pos.LiquidationPrice, pos.UnrealizedPnL, pos.Leverage))
			
			// Exit plan structure (Note: exit_plan fields need to be added to PositionInfo struct in future)
			// For now, showing structure - these would come from stored position exit plans
			sb.WriteString(fmt.Sprintf(", 'exit_plan': {'profit_target': <price>, 'stop_loss': <price>, 'invalidation_condition': '<specific condition>'}, 'confidence': <0-1>, 'risk_usd': %.2f",
				pos.MarginUsed))
			
			// Calculate notional USD
			notionalUSD := pos.Quantity * pos.MarkPrice
			sb.WriteString(fmt.Sprintf(", 'notional_usd': %.2f}\n\n", notionalUSD))
		}
	} else {
		sb.WriteString("None\n\n")
	}
	
	// Sharpe Ratio
	if ctx.Performance != nil {
		type PerformanceData struct {
			SharpeRatio float64 `json:"sharpe_ratio"`
		}
		var perfData PerformanceData
		if jsonData, err := json.Marshal(ctx.Performance); err == nil {
			if err := json.Unmarshal(jsonData, &perfData); err == nil {
				sb.WriteString(fmt.Sprintf("Sharpe Ratio: %.3f\n\n", perfData.SharpeRatio))
			}
		}
	}

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

		// Validate invalidation condition is provided (MANDATORY)
		if d.InvalidationCondition == "" {
			return fmt.Errorf("invalidation_condition is MANDATORY for new positions - must specify a technical/fundamental condition that invalidates the trade thesis")
		}
		if len(strings.TrimSpace(d.InvalidationCondition)) < 10 {
			return fmt.Errorf("invalidation_condition must be specific and detailed (at least 10 characters), got: %s", d.InvalidationCondition)
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
