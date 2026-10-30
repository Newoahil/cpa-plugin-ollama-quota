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
		if strings.TrimSpace(key) != "interval_seconds" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if value == "" {
			continue
		}
		seconds, errParse := strconv.Atoi(value)
		if errParse != nil {
			return cfg, fmt.Errorf("config line %d: interval_seconds must be an integer: %w", lineNo, errParse)
		}
		if seconds > 0 {
			cfg.IntervalSeconds = seconds
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return cfg, fmt.Errorf("read config: %w", errScan)
	}
	return cfg, nil
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
