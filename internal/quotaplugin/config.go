package quotaplugin

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// defaultIntervalSeconds is the collection cadence from SPEC 3.3. #1 only
// parses it; #2 hands it to the collector.
const defaultIntervalSeconds = 900

// Config is the plugins.configs.ollama-quota block passed at register time.
// The host also writes enabled/priority into that block; this parser only
// reads the plugin-owned keys.
type Config struct {
	IntervalSeconds int
	StateDir        string
}

// ParseConfig reads interval_seconds out of the config block CPA sends.
//
// The block is a YAML mapping that may also hold nested objects and lists the
// plugin does not own, so lines that are not a flat `key: value` pair are
// skipped rather than treated as errors — only an interval_seconds value that
// will not parse is worth reporting.
func ParseConfig(raw []byte) (Config, error) {
	cfg := Config{IntervalSeconds: defaultIntervalSeconds}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)

	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := stripComment(scanner.Text())
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			// A list item or a bare key of a nested block.
			continue
		}
		if strings.TrimSpace(key) == "state_dir" {
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, "\"") {
				parsed, err := strconv.Unquote(value)
				if err != nil {
					return cfg, fmt.Errorf("config line %d: state_dir must be a string", lineNo)
				}
				cfg.StateDir = parsed
			} else if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
				cfg.StateDir = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
			} else {
				cfg.StateDir = value
			}
			continue
		}
		if strings.TrimSpace(key) != "interval_seconds" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if value == "" {
			continue
		}
		seconds, okInteger := parsePositiveInt(value)
		if !okInteger {
			// Deliberately not wrapping an *strconv.NumError: it carries the
			// offending value, and this message reaches the registration
			// response and the management UI. Only the field name, line and
			// expected type are reported (SPEC 4.2).
			return cfg, fmt.Errorf("config line %d: interval_seconds must be a positive integer", lineNo)
		}
		cfg.IntervalSeconds = seconds
	}
	if errScan := scanner.Err(); errScan != nil {
		return cfg, fmt.Errorf("read config: %w", errScan)
	}
	return cfg, nil
}

// parsePositiveInt reports whether value is a base-10 integer greater than
// zero. It exists so the caller never sees the strconv error, which embeds the
// offending input.
func parsePositiveInt(value string) (int, bool) {
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return 0, false
		}
	}
	seconds, errParse := strconv.Atoi(value)
	if errParse != nil {
		// Out of range for int.
		return 0, false
	}
	if seconds <= 0 {
		return 0, false
	}
	return seconds, true
}

// stripComment removes a trailing `#` comment that is not inside quotes.
func stripComment(line string) string {
	var quote rune
	for index, char := range line {
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
			}
		case char == '"' || char == '\'':
			quote = char
		case char == '#':
			return strings.TrimSpace(line[:index])
		}
	}
	return strings.TrimSpace(line)
}
