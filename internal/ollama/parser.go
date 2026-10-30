package ollama

import (
	"fmt"
	"html"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The settings page is parsed with a small built-in tokeniser rather than an
// HTML package: this module keeps a zero-dependency go.mod. The tokeniser
// produces a real element tree, so a percentage is only attributed to a window
// when its label sits in the same local structure, and text inside an attribute
// value is never mistaken for page text. Flattening the page to bare text lines
// lost both, which produced wrong numbers rather than failures (SPEC 4.3).

var (
	// reResetClause strips a "Resets ..." clause before the window labels are
	// looked for, so "Resets in 7 days" is not read as a weekly label.
	reResetClause = regexp.MustCompile(`(?is)\bresets?\b.*$`)
	// rePercent matches a usage share with the optional sign kept, so "-1%" is
	// refused rather than read as 1%.
	rePercent = regexp.MustCompile(`(-?\d+(?:\.\d+)?)\s*%`)
	// reReset reads a "Resets in 3h" / "Resets at <time>" hint.
	reReset = regexp.MustCompile(`(?i)resets?\s+(?:in|on|at)?\s*(.+)`)
	// "Session usage" is the current wording for the five-hour window; the
	// alternatives cover older labels.
	reLabel5h = regexp.MustCompile(`(?i)(session usage|session limit|rolling|5[\s-]?hour|five[\s-]?hour|hourly)`)
	reLabel7d = regexp.MustCompile(`(?i)(weekly|7[\s-]?day|seven[\s-]?day)`)
	// reDurToken matches one "3h" / "6 days" token anchored at the start of the
	// remaining text. parseDurationText consumes a whole string token by token,
	// so a fragment it does not understand cannot be skipped quietly.
	reDurToken = regexp.MustCompile(`(?i)^(\d{1,9})\s*(months?|minutes?|mins?|hours?|hrs?|days?|weeks?|mo|min|hr|d|h|m|w)\b`)
	rePlanLn   = regexp.MustCompile(`(?i)\bplan\b[:\s]+([A-Za-z][\w -]{0,20})`)
	reMoney    = regexp.MustCompile(`([$€£])\s?(\d[\d,]*(?:\.\d+)?)`)
	reCredit   = regexp.MustCompile(`(?i)(?:balance|credit)[^\d$€£]{0,12}([$€£]?\s?\d[\d,]*(?:\.\d+)?)`)

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
// It returns a KindParse error, and an empty result, unless every window the
// page names could be read. A page that names a window and then shows a figure
// that cannot be attributed to it is a failure, not a partial reading: a window
// silently dropped, or a percentage claimed by the wrong label, would surface
// as a real-looking number the page never stated (SPEC 4.3).
//
// ParseSettings is a pure function of its inputs; all network access lives in
// Client.
func ParseSettings(document string, now time.Time) (ParsedSettings, error) {
	if strings.TrimSpace(document) == "" {
		return ParsedSettings{}, newError(KindParse, "settings", 0, "the Ollama settings page was empty")
	}
	root := parseHTMLTree(document)
	lines := flattenTree(root)
	parsed := ParsedSettings{
		Plan:    extractPlan(lines),
		Balance: extractBalance(lines),
	}

	parser := newSettingsParser(root)
	if len(parser.anchors) == 0 {
		return ParsedSettings{}, newError(KindParse, "settings", 0,
			"no quota window could be read from the Ollama settings page; the page layout may have changed")
	}

	// Resolve every named window first, in display order, so an error always
	// names the same window for the same page.
	resolved := make(map[string]percentReading, len(parser.anchors))
	for _, spec := range windowSpecs {
		labels, declared := parser.anchors[spec.name]
		if !declared {
			continue
		}
		reading, err := parser.resolve(spec.name, labels)
		if err != nil {
			return ParsedSettings{}, err
		}
		if parser.usedPercent[reading.text] {
			return ParsedSettings{}, newError(KindParse, "settings", 0,
				fmt.Sprintf("the Ollama settings page ties one percentage to more than one quota window (%s); the reading was refused", spec.title))
		}
		parser.usedPercent[reading.text] = true
		resolved[spec.name] = reading
	}

	for _, spec := range windowSpecs {
		reading, ok := resolved[spec.name]
		if !ok {
			continue
		}
		context := normalizeSpace(reading.label.text) + " " + normalizeSpace(reading.text.text)
		value, ok := normalizePercent(reading.value, isRemainingShare(context))
		if !ok {
			return ParsedSettings{}, newError(KindParse, "settings", 0,
				fmt.Sprintf("the Ollama settings page reported an impossible figure for the %s window; the reading was refused", spec.title))
		}
		window := Window{Name: spec.name, Label: spec.title, UsedPercent: value}
		parser.applyReset(&window, reading.label, spec.name, now)
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

// --- element tree ---

type nodeKind int

const (
	kindDocument nodeKind = iota
	kindElement
	kindText
)

// node is one element or text node of the settings page.
type node struct {
	kind     nodeKind
	tag      string            // lowercased element name, empty for text
	attrs    map[string]string // attribute values, entity-decoded
	text     string            // entity-decoded text, for kindText
	parent   *node
	children []*node
}

// parseHTMLTree tokenises the page into a tree. It is deliberately forgiving:
// stray end tags are ignored, unclosed elements are closed at their parent, and
// anything that does not start a tag is kept as text. What it will not do is
// let an attribute value leak into the text, which is what a "<[^>]*>" strip
// does when a quoted value contains '>'.
func parseHTMLTree(source string) *node {
	root := &node{kind: kindDocument}
	current := root
	for index := 0; index < len(source); {
		offset := strings.IndexByte(source[index:], '<')
		if offset < 0 {
			appendText(current, source[index:])
			break
		}
		if offset > 0 {
			appendText(current, source[index:index+offset])
		}
		index += offset
		rest := source[index:]

		switch {
		case strings.HasPrefix(rest, "<!--"):
			if end := strings.Index(rest[4:], "-->"); end >= 0 {
				index += 4 + end + 3
			} else {
				index = len(source)
			}
		case strings.HasPrefix(rest, "<!") || strings.HasPrefix(rest, "<?"):
			if end := strings.IndexByte(rest, '>'); end >= 0 {
				index += end + 1
			} else {
				index = len(source)
			}
		case len(rest) > 1 && rest[1] == '/':
			end := strings.IndexByte(rest, '>')
			if end < 0 {
				index = len(source)
				continue
			}
			current = closeElement(current, strings.ToLower(strings.TrimSpace(rest[2:end])))
			index += end + 1
		case len(rest) > 1 && isTagNameByte(rest[1]):
			tag, attrs, selfClosing, consumed, ok := parseStartTag(rest)
			if !ok {
				appendText(current, "<")
				index++
				continue
			}
			index += consumed
			element := &node{kind: kindElement, tag: tag, attrs: attrs, parent: current}
			current.children = append(current.children, element)
			switch {
			case selfClosing || isVoidTag(tag):
				// nothing to descend into
			case isRawTextTag(tag):
				// Script and style bodies are not page text; skip them entirely.
				lowered := strings.ToLower(source[index:])
				if end := strings.Index(lowered, "</"+tag); end >= 0 {
					index += end
					if close2 := strings.IndexByte(source[index:], '>'); close2 >= 0 {
						index += close2 + 1
					} else {
						index = len(source)
					}
				} else {
					index = len(source)
				}
			default:
				current = element
			}
		default:
			appendText(current, "<")
			index++
		}
	}
	return root
}

func appendText(parent *node, raw string) {
	if raw == "" {
		return
	}
	text := html.UnescapeString(raw)
	if len(parent.children) > 0 {
		last := parent.children[len(parent.children)-1]
		if last.kind == kindText {
			last.text += text
			return
		}
	}
	parent.children = append(parent.children, &node{kind: kindText, text: text, parent: parent})
}

func closeElement(current *node, name string) *node {
	if name == "" {
		return current
	}
	for walker := current; walker != nil; walker = walker.parent {
		if walker.kind == kindElement && walker.tag == name {
			return walker.parent
		}
	}
	return current
}

// parseStartTag reads one start tag from the beginning of source. Attribute
// values are read with quote awareness, so a '>' inside a quoted value does not
// end the tag.
func parseStartTag(source string) (tag string, attrs map[string]string, selfClosing bool, consumed int, ok bool) {
	if len(source) < 2 || source[0] != '<' {
		return "", nil, false, 0, false
	}
	index := 1
	for index < len(source) && !isSpaceByte(source[index]) && source[index] != '/' && source[index] != '>' {
		index++
	}
	if index == 1 {
		return "", nil, false, 0, false
	}
	tag = strings.ToLower(source[1:index])
	attrs = map[string]string{}

	for {
		for index < len(source) && isSpaceByte(source[index]) {
			index++
		}
		if index >= len(source) {
			return "", nil, false, 0, false
		}
		if source[index] == '>' {
			return tag, attrs, false, index + 1, true
		}
		if source[index] == '/' {
			index++
			if index < len(source) && source[index] == '>' {
				return tag, attrs, true, index + 1, true
			}
			continue
		}
		start := index
		for index < len(source) &&
			!isSpaceByte(source[index]) && source[index] != '=' && source[index] != '>' && source[index] != '/' {
			index++
		}
		name := strings.ToLower(source[start:index])
		for index < len(source) && isSpaceByte(source[index]) {
			index++
		}
		value := ""
		if index < len(source) && source[index] == '=' {
			index++
			for index < len(source) && isSpaceByte(source[index]) {
				index++
			}
			if index < len(source) && (source[index] == '"' || source[index] == '\'') {
				quote := source[index]
				index++
				valueStart := index
				for index < len(source) && source[index] != quote {
					index++
				}
				value = source[valueStart:index]
				if index < len(source) {
					index++
				}
			} else {
				valueStart := index
				for index < len(source) && !isSpaceByte(source[index]) && source[index] != '>' {
					index++
				}
				value = source[valueStart:index]
			}
		}
		if name != "" {
			attrs[name] = html.UnescapeString(value)
		}
	}
}

func isSpaceByte(value byte) bool {
	return value == ' ' || value == '\t' || value == '\n' || value == '\r' || value == '\f'
}

func isTagNameByte(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}

// voidTags never have children, so a start tag for one must not become the
// current element.
var voidTags = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"param": true, "source": true, "track": true, "wbr": true,
}

func isVoidTag(tag string) bool { return voidTags[tag] }

func isRawTextTag(tag string) bool { return tag == "script" || tag == "style" }

// blockTags start a new text line when flattened, so "Plan: Pro" and "Balance
// $12.40" stay on separate lines instead of running together.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"dd": true, "div": true, "dl": true, "dt": true, "fieldset": true,
	"figcaption": true, "figure": true, "footer": true, "form": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"header": true, "hr": true, "li": true, "main": true, "nav": true,
	"ol": true, "p": true, "pre": true, "section": true, "table": true,
	"td": true, "th": true, "tr": true, "ul": true,
}

func isBlockTag(tag string) bool { return blockTags[tag] }

// flattenTree returns whitespace-normalised text lines in document order.
// Attribute values are absent by construction, and script/style bodies were
// never tokenised, so only text the page actually displays reaches the
// plan and balance patterns.
func flattenTree(root *node) []string {
	var lines []string
	var builder strings.Builder
	flush := func() {
		if line := normalizeSpace(builder.String()); line != "" {
			lines = append(lines, line)
		}
		builder.Reset()
	}
	var walk func(*node)
	walk = func(current *node) {
		for _, child := range current.children {
			switch child.kind {
			case kindText:
				builder.WriteString(child.text)
				builder.WriteByte(' ')
			case kindElement:
				if child.tag == "br" {
					flush()
					continue
				}
				block := isBlockTag(child.tag)
				if block {
					flush()
				}
				walk(child)
				if block {
					flush()
				}
			}
		}
	}
	walk(root)
	flush()
	return lines
}

func normalizeSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// --- window attribution ---

// percentReading is one window's resolved usage share: the text node the
// percentage came from and the label text node it belongs to.
type percentReading struct {
	value float64
	text  *node
	label *node
}

// settingsParser holds the state one ParseSettings call needs: which text nodes
// carry a window label, plus the bookkeeping that keeps one percentage or one
// reset from being handed to two windows.
type settingsParser struct {
	anchors     map[string][]*node
	labelMemo   map[*node]map[string]bool
	usedPercent map[*node]bool
	usedReset   map[*node]bool
}

func newSettingsParser(root *node) *settingsParser {
	parser := &settingsParser{
		anchors:     map[string][]*node{},
		labelMemo:   map[*node]map[string]bool{},
		usedPercent: map[*node]bool{},
		usedReset:   map[*node]bool{},
	}
	walkTextNodes(root, func(text *node) {
		// A reset clause is not a label: "Resets in 7 days" must not register as
		// the weekly window.
		labelZone := reResetClause.ReplaceAllString(normalizeSpace(text.text), "")
		for _, spec := range windowSpecs {
			if spec.label.MatchString(labelZone) {
				parser.anchors[spec.name] = append(parser.anchors[spec.name], text)
			}
		}
	})
	return parser
}

func walkTextNodes(root *node, fn func(*node)) {
	for _, child := range root.children {
		switch child.kind {
		case kindText:
			fn(child)
		case kindElement:
			walkTextNodes(child, fn)
		}
	}
}

// labelsIn reports which windows have a label text node inside n. It is memoised
// because the same subtrees are queried once per candidate.
func (p *settingsParser) labelsIn(n *node) map[string]bool {
	if cached, ok := p.labelMemo[n]; ok {
		return cached
	}
	found := map[string]bool{}
	if n.kind == kindText {
		for name, anchors := range p.anchors {
			for _, anchor := range anchors {
				if anchor == n {
					found[name] = true
				}
			}
		}
	} else {
		for _, child := range n.children {
			for name := range p.labelsIn(child) {
				found[name] = true
			}
		}
	}
	p.labelMemo[n] = found
	return found
}

// hasForeignLabel reports whether n holds another window's label.
func (p *settingsParser) hasForeignLabel(n *node, window string) bool {
	for name := range p.labelsIn(n) {
		if name != window {
			return true
		}
	}
	return false
}

// resolve turns every occurrence of one window's label into a reading, or fails.
// The readings from several label occurrences must agree; a page that says
// different things about the same window is refused rather than picked from.
func (p *settingsParser) resolve(window string, labels []*node) (percentReading, error) {
	var readings []percentReading
	ambiguous := false
	for _, label := range labels {
		reading, found, amb := p.readingForLabel(window, label)
		if amb {
			ambiguous = true
			continue
		}
		if found {
			readings = append(readings, reading)
		}
	}
	title := windowTitle(window)
	if len(readings) == 0 {
		if ambiguous {
			return percentReading{}, newError(KindParse, "settings", 0,
				fmt.Sprintf("the Ollama settings page shows more than one percentage beside the %s label; the reading was refused rather than guessed", title))
		}
		return percentReading{}, newError(KindParse, "settings", 0,
			fmt.Sprintf("the Ollama settings page names the %s window but its usage could not be read; no partial reading is reported", title))
	}
	for _, other := range readings[1:] {
		if other.value != readings[0].value {
			return percentReading{}, newError(KindParse, "settings", 0,
				fmt.Sprintf("the Ollama settings page states different figures for the %s window; the reading was refused", title))
		}
	}
	return readings[0], nil
}

// readingForLabel finds the percentage that belongs to one label. It takes the
// tightest structure that carries a single percentage:
//
//  1. Walk out from the label to the innermost container that holds no other
//     window's label and exactly one percentage. A container holding more than
//     one is ambiguous, and the reading is refused rather than guessed (SPEC 4.3).
//  2. In a flat layout where each window is a label and a value side by side in
//     one container ("<span>Session usage</span><span>20% used</span>"), the
//     container is shared, so the value is taken from the label's neighbours in
//     that container, stopping at the other window's label.
func (p *settingsParser) readingForLabel(window string, label *node) (percentReading, bool, bool) {
	for ancestor := label.parent; ancestor != nil; ancestor = ancestor.parent {
		if p.hasForeignLabel(ancestor, window) {
			break
		}
		candidates, ambiguous := percentagesIn(ancestor)
		switch {
		case ambiguous || len(candidates) > 1:
			return percentReading{}, false, true
		case len(candidates) == 1:
			return percentReading{value: candidates[0].value, text: candidates[0].text, label: label}, true, false
		}
	}
	return p.readingFromSiblings(window, label)
}

// readingFromSiblings resolves the value from the label's siblings, moving
// outward on each side until the other window's label or a percentage is
// reached. Two percentages on the way make the reading ambiguous.
//
// The label is usually a text node inside its own element, so the carrier is
// first lifted out of any wrapping elements that hold nothing but the label
// ("<div><span>Session usage</span><span>20% used</span></div>").
func (p *settingsParser) readingFromSiblings(window string, label *node) (percentReading, bool, bool) {
	carrier := label
	for carrier.parent != nil && len(carrier.parent.children) == 1 && carrier.parent.kind == kindElement {
		carrier = carrier.parent
	}
	if carrier.parent == nil {
		return percentReading{}, false, false
	}
	siblings := carrier.parent.children
	position := -1
	for index, sibling := range siblings {
		if sibling == carrier {
			position = index
			break
		}
	}
	if position < 0 {
		return percentReading{}, false, false
	}

	var found *percentReading
	for _, step := range []int{1, -1} {
		for index := position + step; index >= 0 && index < len(siblings); index += step {
			sibling := siblings[index]
			if p.hasForeignLabel(sibling, window) {
				break
			}
			candidates, ambiguous := percentagesIn(sibling)
			if ambiguous {
				return percentReading{}, false, true
			}
			for _, candidate := range candidates {
				if found != nil {
					return percentReading{}, false, true
				}
				reading := percentReading{value: candidate.value, text: candidate.text, label: label}
				found = &reading
			}
			if found != nil {
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		return percentReading{}, false, false
	}
	return *found, true, false
}

// percentCandidate is one percentage occurrence inside a subtree.
type percentCandidate struct {
	text  *node
	value float64
}

// percentagesIn collects the percentages of a subtree in document order. A text
// node carrying more than one percentage is reported as ambiguous: taking the
// first would be a guess about which figure is the window's.
func percentagesIn(scope *node) ([]percentCandidate, bool) {
	var candidates []percentCandidate
	ambiguous := false
	walkTextNodes(scope, func(text *node) {
		if ambiguous {
			return
		}
		matches := rePercent.FindAllStringSubmatch(text.text, -1)
		switch len(matches) {
		case 0:
			return
		case 1:
		default:
			ambiguous = true
			return
		}
		value, err := strconv.ParseFloat(matches[0][1], 64)
		if err != nil {
			ambiguous = true
			return
		}
		candidates = append(candidates, percentCandidate{text: text, value: value})
	})
	return candidates, ambiguous
}

// applyReset attaches the reset hint that belongs to one window. The search
// stops at the first container that also mentions the other window, so a reset
// stated once cannot be handed to both, and a window with no reset of its own
// gets none (SPEC 4.3: no invented reset).
func (p *settingsParser) applyReset(window *Window, label *node, name string, now time.Time) {
	for scope := label.parent; scope != nil; scope = scope.parent {
		if p.hasForeignLabel(scope, name) {
			return
		}
		if element, at, ok := findTimeElement(scope); ok {
			if p.usedReset[element] {
				return
			}
			p.usedReset[element] = true
			window.ResetAt = &at
			return
		}
		if text, raw, ok := findResetTextNode(scope); ok {
			if p.usedReset[text] {
				return
			}
			p.usedReset[text] = true
			if at, derived, resolved := resolveReset(raw, now); resolved {
				window.ResetAt = &at
				window.ResetDerived = derived
			} else {
				window.ResetText = raw
			}
			return
		}
	}
}

// findTimeElement returns the first <time>/<local-time> element in document
// order that carries a machine-readable instant.
func findTimeElement(scope *node) (*node, time.Time, bool) {
	for _, child := range scope.children {
		if child.kind != kindElement {
			continue
		}
		if child.tag == "time" || child.tag == "local-time" {
			for _, key := range []string{"datetime", "data-time"} {
				raw := strings.TrimSpace(child.attrs[key])
				if raw == "" {
					continue
				}
				for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
					if at, err := time.Parse(layout, raw); err == nil {
						return child, at, true
					}
				}
			}
		}
		if element, at, ok := findTimeElement(child); ok {
			return element, at, ok
		}
	}
	return nil, time.Time{}, false
}

// findResetTextNode returns the first "Resets ..." text node in document order.
// The body of a <time> element is skipped: it is the human rendering of the
// machine-readable instant that findTimeElement already read.
func findResetTextNode(scope *node) (*node, string, bool) {
	var source *node
	var text string
	found := false
	walkTextNodes(scope, func(candidate *node) {
		if found || candidate.parent == nil {
			return
		}
		if tag := candidate.parent.tag; tag == "time" || tag == "local-time" {
			return
		}
		match := reReset.FindStringSubmatch(normalizeSpace(candidate.text))
		if match == nil {
			return
		}
		value := strings.TrimSpace(match[1])
		if value == "" {
			return
		}
		source, text, found = candidate, value, true
	})
	return source, text, found
}

func windowTitle(name string) string {
	for _, spec := range windowSpecs {
		if spec.name == name {
			return spec.title
		}
	}
	return name
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

// bounds for a relative reset hint. A window that resets more than ten years
// out, or a figure larger than a hundred thousand, is refused rather than
// multiplied into an overflow.
const (
	maxResetAmount = 100_000
	maxResetSpan   = 10 * 365 * 24 * time.Hour
)

// parseDurationText sums "3h", "6 days" and "2 hours 30 minutes" style text.
// The whole string must be consumed: a leading sign, a decimal or any leftover
// word makes the parse fail, so "1.5 hours" is never read as "5 hours" and
// "-3h" is never read as a positive three hours.
func parseDurationText(value string) (time.Duration, bool) {
	rest := trimResetNoise(value)
	if rest == "" {
		return 0, false
	}
	var total time.Duration
	tokens := 0
	for rest != "" {
		match := reDurToken.FindStringSubmatch(rest)
		if match == nil {
			return 0, false
		}
		amount, err := strconv.Atoi(match[1])
		if err != nil || amount <= 0 || amount > maxResetAmount {
			return 0, false
		}
		unit, ok := unitDuration(match[2])
		if !ok {
			return 0, false
		}
		span := time.Duration(amount) * unit
		if span <= 0 || span > maxResetSpan {
			return 0, false
		}
		total += span
		if total > maxResetSpan {
			return 0, false
		}
		tokens++
		rest = trimResetNoise(rest[len(match[0]):])
	}
	if tokens == 0 || total <= 0 {
		return 0, false
	}
	return total, true
}

// trimResetNoise drops the punctuation and the conjunction that separate one
// duration token from the next ("2 hours, 30 minutes", "1 day and 6 hours").
// Anything else stops the trim, so a word the parser does not understand is left
// in place for reDurToken to refuse rather than skipped quietly.
func trimResetNoise(value string) string {
	trimmed := strings.TrimLeft(value, " \t\r\n,.;:")
	for {
		if len(trimmed) < 3 || !strings.EqualFold(trimmed[:3], "and") {
			return trimmed
		}
		if len(trimmed) > 3 && trimmed[3] != ' ' && trimmed[3] != '\t' {
			return trimmed
		}
		trimmed = strings.TrimLeft(trimmed[3:], " \t\r\n,.;:")
	}
}

func unitDuration(unit string) (time.Duration, bool) {
	switch strings.ToLower(unit) {
	case "d", "day", "days":
		return 24 * time.Hour, true
	case "h", "hr", "hrs", "hour", "hours":
		return time.Hour, true
	case "m", "min", "mins", "minute", "minutes":
		return time.Minute, true
	case "w", "week", "weeks":
		return 7 * 24 * time.Hour, true
	case "mo", "month", "months":
		return 30 * 24 * time.Hour, true
	}
	return 0, false
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

// extractBalance reads a dollar figure from a balance or credit line. A negative
// figure is refused outright: reading "-$12.40" as "$12.40" would invert the
// page's meaning, and there is no safe substitute.
func extractBalance(lines []string) *Balance {
	for _, line := range lines {
		lowered := strings.ToLower(line)
		if !strings.Contains(lowered, "balance") && !strings.Contains(lowered, "credit") {
			continue
		}
		if match := reCredit.FindStringSubmatchIndex(line); match != nil {
			if precededByMinus(line, match[2]) {
				continue
			}
			if balance, ok := parseMoney(line[match[2]:match[3]]); ok {
				return balance
			}
			continue
		}
		if match := reMoney.FindStringSubmatchIndex(line); match != nil {
			if precededByMinus(line, match[0]) {
				continue
			}
			if balance, ok := parseMoney(line[match[0]:match[1]]); ok {
				return balance
			}
		}
	}
	return nil
}

// precededByMinus reports whether the amount starting at start is written with
// a minus sign, allowing spaces between the sign and the symbol.
func precededByMinus(line string, start int) bool {
	for index := start - 1; index >= 0; {
		r, size := utf8.DecodeLastRuneInString(line[:index+1])
		switch r {
		case ' ', '\t':
			index -= size
			continue
		case '-', '−', '–':
			return true
		default:
			return false
		}
	}
	return false
}

// parseMoney converts "$12.40" or "12.40" into a Balance. A figure that cannot
// be read cleanly is refused; a balance is never defaulted to zero.
func parseMoney(raw string) (*Balance, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed != "" {
		if sign, _ := utf8.DecodeRuneInString(trimmed); sign == '-' || sign == '−' || sign == '–' {
			return nil, false
		}
	}
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
