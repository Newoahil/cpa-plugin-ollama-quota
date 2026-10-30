package ollama

import (
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The settings page is parsed without an HTML package on purpose: this module
// keeps a zero-dependency go.mod, and the page is small, regular markup rather
// than arbitrary HTML. The flattener below strips scripts, styles and tags at
// block boundaries so the regexes only ever see plain text lines.

var (
	reScript   = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	reStyle    = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	reDataTime = regexp.MustCompile(`(?is)<(?:time|local-time)\b[^>]*?\bdata-time\s*=\s*"([^"]*)"[^>]*>`)
	reDateTime = regexp.MustCompile(`(?is)<(?:time|local-time)\b[^>]*?\bdatetime\s*=\s*"([^"]*)"[^>]*>`)
	reBlock    = regexp.MustCompile(`(?is)</?(?:address|article|aside|blockquote|br|dd|div|dl|dt|fieldset|figcaption|figure|footer|form|h[1-6]|header|hr|li|main|nav|ol|p|pre|section|table|td|th|tr|ul)\b[^>]*>`)
	reAnyTag   = regexp.MustCompile(`(?s)<[^>]*>`)

	// reStamp matches the machine-readable reset instant the flattener writes.
	reStamp = regexp.MustCompile(`\[ts:([^\]]+)\]`)
	// rePercent matches a usage share with the optional sign kept, so "-1%"
	// is refused rather than read as 1%.
	rePercent = regexp.MustCompile(`(-?\d+(?:\.\d+)?)\s*%`)
	// reReset reads a "Resets in 3h" / "Resets at <time>" hint.
	reReset = regexp.MustCompile(`(?i)resets?\s+(?:in|on|at)?\s*(.+)`)
	// "Session usage" is the current wording for the five-hour window; the
	// alternatives cover older labels.
	reLabel5h = regexp.MustCompile(`(?i)(session usage|session limit|rolling|5[\s-]?hour|five[\s-]?hour|hourly)`)
	reLabel7d = regexp.MustCompile(`(?i)(weekly|7[\s-]?day|seven[\s-]?day)`)
	reDur     = regexp.MustCompile(`(?i)(\d+)\s*([a-z]+)`)
	rePlanLn  = regexp.MustCompile(`(?i)\bplan\b[:\s]+([A-Za-z][\w -]{0,20})`)
	reMoney   = regexp.MustCompile(`([$€£])\s?(\d[\d,]*(?:\.\d+)?)`)
	reCredit  = regexp.MustCompile(`(?i)(?:balance|credit)[^\d$€£]{0,12}([$€£]?\s?\d[\d,]*(?:\.\d+)?)`)

	// loginMarkers identify a sign-in page served where the settings page was
	// expected, which means the cookie expired rather than the page changed.
	loginMarkers = []string{
		`type="password"`,
		"sign in to ollama",
		"sign in with",
		"<title>sign in",
		"log in to ollama",
	}
)

// windowSpecs lists the windows the parser looks for, in display order.
var windowSpecs = []struct {
	name  string
	label *regexp.Regexp
	title string
}{
	{WindowFiveHour, reLabel5h, "5-hour session"},
	{WindowWeekly, reLabel7d, "Weekly"},
}

// ParsedSettings is the result of parsing a settings page: the quota windows
// that were found and, when the page happened to state them, the plan and
// balance.
type ParsedSettings struct {
	Plan    string
	Balance *Balance
	Windows []Window
}

// ParseSettings extracts the quota view from a settings page. now anchors
// relative reset hints ("Resets in 3h") to an absolute instant.
//
// It returns a KindParse error, and an empty result, when no window's usage
// could be read. It never fills a missing figure with a zero or an estimate:
// a page whose markup changed must surface as a failure, not as a plausible
// number (SPEC 4.3). A page carrying only a plan and a balance is not a quota
// reading and is refused for the same reason.
//
// ParseSettings is a pure function of its inputs; all network access lives in
// Client.
func ParseSettings(document string, now time.Time) (ParsedSettings, error) {
	if strings.TrimSpace(document) == "" {
		return ParsedSettings{}, newError(KindParse, "settings", 0, "the Ollama settings page was empty")
	}
	lines := flattenToLines(document)
	parsed := ParsedSettings{
		Plan:    extractPlan(lines),
		Balance: extractBalance(lines),
	}
	// Locate every window label once so a percentage can be attributed to the
	// nearest label. Without this a percentage up to three lines from one label
	// could be stolen by the other window and reported as a wrong number.
	anchors := make(map[string]int, len(windowSpecs))
	for _, spec := range windowSpecs {
		if index := indexLabel(lines, spec.label); index >= 0 {
			anchors[spec.name] = index
		}
	}
	used := make(map[int]bool)
	for _, spec := range windowSpecs {
		index, ok := anchors[spec.name]
		if !ok {
			continue
		}
		window := Window{Name: spec.name, Label: spec.title}
		percent, percentIndex := findPercent(lines, index, anchors, used)
		if percentIndex < 0 {
			continue
		}
		used[percentIndex] = true
		value, ok := normalizePercent(percent, isRemainingShare(lines[index]+" "+lines[percentIndex]))
		if !ok {
			// A percentage that is not a usage share (out of range, negative)
			// is left unreported rather than clamped into a believable value.
			continue
		}
		window.UsedPercent = value
		if at, ok := findStamp(lines, index, anchors); ok {
			window.ResetAt = &at
		} else if reset := findReset(lines, index, anchors); reset != "" {
			if at, derived, ok := resolveReset(reset, now); ok {
				window.ResetAt = &at
				window.ResetDerived = derived
			} else {
				window.ResetText = reset
			}
		}
		parsed.Windows = append(parsed.Windows, window)
	}
	if len(parsed.Windows) == 0 {
		return ParsedSettings{}, newError(KindParse, "settings", 0,
			"no quota window could be read from the Ollama settings page; the page layout may have changed")
	}
	return parsed, nil
}

// LooksLikeLoginPage reports whether a settings response is really the sign-in
// page, which means the cookie expired rather than the markup changed.
func LooksLikeLoginPage(document string) bool {
	lowered := strings.ToLower(document)
	for _, marker := range loginMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// flattenToLines strips markup and returns whitespace-normalised text lines,
// breaking at block boundaries and inlining machine-readable timestamps.
func flattenToLines(document string) []string {
	body := reScript.ReplaceAllString(document, " ")
	body = reStyle.ReplaceAllString(body, " ")
	body = reDataTime.ReplaceAllString(body, " [ts:$1] ")
	body = reDateTime.ReplaceAllString(body, " [ts:$1] ")
	body = reBlock.ReplaceAllString(body, "\n")
	body = reAnyTag.ReplaceAllString(body, " ")
	body = html.UnescapeString(body)

	var lines []string
	for _, raw := range strings.Split(body, "\n") {
		if line := strings.Join(strings.Fields(raw), " "); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func indexLabel(lines []string, re *regexp.Regexp) int {
	for index, line := range lines {
		if re.MatchString(line) {
			return index
		}
	}
	return -1
}

// findPercent looks for a percentage on the label line first, then nearby
// lines. A neighbour belongs to this window only if no other window's label is
// at least as close, so a percentage sitting next to both labels is never
// claimed by the wrong one — that would report a wrong number, which SPEC 4.3
// forbids.
func findPercent(lines []string, index int, anchors map[string]int, used map[int]bool) (float64, int) {
	if match := rePercent.FindStringSubmatch(lines[index]); match != nil {
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			return value, index
		}
	}
	for _, offset := range []int{1, -1, 2, -2, 3, -3} {
		neighbour := index + offset
		if neighbour < 0 || neighbour >= len(lines) || used[neighbour] {
			continue
		}
		if !nearestTo(index, neighbour, anchors) {
			continue
		}
		match := rePercent.FindStringSubmatch(lines[neighbour])
		if match == nil {
			continue
		}
		if value, err := strconv.ParseFloat(match[1], 64); err == nil {
			return value, neighbour
		}
	}
	return 0, -1
}

// nearestTo reports whether line neighbour is tied to anchor index rather than
// to any other label: no other anchor may be strictly closer.
func nearestTo(index, neighbour int, anchors map[string]int) bool {
	distance := abs(neighbour - index)
	for _, other := range anchors {
		if other == index {
			continue
		}
		if abs(neighbour-other) < distance {
			return false
		}
	}
	return true
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// normalizePercent validates a scraped share and converts a "remaining" figure
// to the "used" convention the Window type uses. A value outside [0,100] or a
// non-finite value is refused rather than clamped: clamping 101 to 100 would
// fabricate a maxed reading the page never stated.
func normalizePercent(value float64, remaining bool) (float64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
		return 0, false
	}
	if remaining {
		return 100 - value, true
	}
	return value, true
}

// isRemainingShare reports whether nearby text says the percentage is what is
// left rather than what is used.
func isRemainingShare(context string) bool {
	lowered := strings.ToLower(context)
	left := strings.Contains(lowered, "remaining") || strings.Contains(lowered, " left")
	return left && !strings.Contains(lowered, "used")
}

// findStamp reads a machine-readable reset instant near the label. Only lines
// that belong to this window are inspected, so the other window's stamp is not
// read as this one's.
func findStamp(lines []string, index int, anchors map[string]int) (time.Time, bool) {
	for _, offset := range []int{0, 1, 2, 3} {
		neighbour := index + offset
		if neighbour < 0 || neighbour >= len(lines) {
			continue
		}
		if !nearestTo(index, neighbour, anchors) {
			continue
		}
		if match := reStamp.FindStringSubmatch(lines[neighbour]); match != nil {
			for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
				if at, err := time.Parse(layout, strings.TrimSpace(match[1])); err == nil {
					return at, true
				}
			}
		}
	}
	return time.Time{}, false
}

func findReset(lines []string, index int, anchors map[string]int) string {
	for _, offset := range []int{0, 1, 2, -1, 3} {
		neighbour := index + offset
		if neighbour < 0 || neighbour >= len(lines) {
			continue
		}
		if !nearestTo(index, neighbour, anchors) {
			continue
		}
		if match := reReset.FindStringSubmatch(lines[neighbour]); match != nil {
			if text := strings.TrimSpace(reStamp.ReplaceAllString(match[1], "")); text != "" {
				return text
			}
		}
	}
	return ""
}

// resolveReset turns a reset hint into an absolute time. Relative text such as
// "3h" is reported as derived; an absolute instant is not. An unrecognised
// string yields ok false so it is kept verbatim instead of guessed at.
func resolveReset(text string, now time.Time) (at time.Time, derived bool, ok bool) {
	value := strings.TrimSpace(text)
	if value == "" {
		return time.Time{}, false, false
	}
	if duration, parsed := parseDurationText(value); parsed {
		return now.Add(duration), true, true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, false, true
		}
	}
	return time.Time{}, false, false
}

// parseDurationText sums "3h", "6 days" and "2 hours 30 minutes" style text.
func parseDurationText(value string) (time.Duration, bool) {
	matches := reDur.FindAllStringSubmatch(value, -1)
	if len(matches) == 0 {
		return 0, false
	}
	var total time.Duration
	for _, match := range matches {
		amount, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		// Units arrive singular, plural or abbreviated, so fold the plural away.
		unit := strings.TrimSuffix(strings.ToLower(match[2]), "s")
		switch unit {
		case "d", "day":
			total += time.Duration(amount) * 24 * time.Hour
		case "h", "hr", "hour":
			total += time.Duration(amount) * time.Hour
		case "m", "min", "minute":
			total += time.Duration(amount) * time.Minute
		case "w", "week":
			total += time.Duration(amount) * 7 * 24 * time.Hour
		case "mo", "month":
			total += time.Duration(amount) * 30 * 24 * time.Hour
		}
	}
	if total <= 0 {
		return 0, false
	}
	return total, true
}

func extractPlan(lines []string) string {
	for _, line := range lines {
		if match := rePlanLn.FindStringSubmatch(line); match != nil {
			if plan := strings.TrimSpace(match[1]); plan != "" {
				return canonicalPlan(plan)
			}
		}
	}
	for _, line := range lines {
		lowered := strings.ToLower(line)
		for _, plan := range []string{"enterprise", "team", "pro", "max", "free", "trial"} {
			if containsWord(lowered, plan) {
				return canonicalPlan(plan)
			}
		}
	}
	return ""
}

// containsWord avoids matching "pro" inside "profile" and "free" inside
// "freely".
func containsWord(value, word string) bool {
	at := strings.Index(value, word)
	if at < 0 {
		return false
	}
	before := at == 0 || !isLetter(rune(value[at-1]))
	after := at+len(word) == len(value) || !isLetter(rune(value[at+len(word)]))
	return before && after
}

func isLetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// extractBalance reads a dollar figure from a balance or credit line. A bare
// amount on its own line is only used when it is next to a balance word, so a
// price or an unrelated figure elsewhere on the page is not mistaken for it.
func extractBalance(lines []string) *Balance {
	for _, line := range lines {
		lowered := strings.ToLower(line)
		if !strings.Contains(lowered, "balance") && !strings.Contains(lowered, "credit") {
			continue
		}
		if match := reCredit.FindStringSubmatch(line); match != nil {
			if balance, ok := parseMoney(match[1]); ok {
				return balance
			}
		}
		if match := reMoney.FindStringSubmatch(line); match != nil {
			if balance, ok := parseMoney(match[0]); ok {
				return balance
			}
		}
	}
	return nil
}

// parseMoney converts "$12.40" or "12.40" into a Balance. A figure that cannot
// be read cleanly is refused; a balance is never defaulted to zero.
func parseMoney(raw string) (*Balance, bool) {
	trimmed := strings.TrimSpace(raw)
	currency := currencyFor("$")
	if trimmed != "" {
		if symbol := trimmed[:1]; symbol == "$" || symbol == "€" || symbol == "£" {
			currency = currencyFor(symbol)
			trimmed = strings.TrimSpace(trimmed[1:])
		}
	}
	trimmed = strings.ReplaceAll(trimmed, ",", "")
	if trimmed == "" {
		return nil, false
	}
	amount, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return nil, false
	}
	return &Balance{Amount: amount, Currency: currency}, true
}
