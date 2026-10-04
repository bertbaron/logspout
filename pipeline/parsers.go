package pipeline

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/gliderlabs/logspout/router"
)

// parseResult is what a parser extracts. Level is normalized and never empty.
type parseResult struct {
	level  string
	fields [][2]string
}

// parserFunc gets the ANSI-stripped message. It returns false when the line
// does not have the parser's format.
type parserFunc func(text string) (parseResult, bool)

var parsers = map[string]parserFunc{
	"homeassistant": parseHomeAssistant,
	"bashio":        parseBashio,
	"logfmt":        parseLogfmt,
	"json":          parseJSON,
	"bracket":       parseBracket,
	"generic":       parseGeneric,
}

// ParserNames returns the names usable in `parse:`, sorted.
func ParserNames() []string {
	names := make([]string, 0, len(parsers))
	for n := range parsers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

var (
	// Supervisor logs use a two digit year.
	haRe = regexp.MustCompile(`^\d{2,4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:[.,]\d+)? ([A-Za-z]+)(?: \(([^)]*)\))?(?: \[([^\]]*)\])?`)

	bashioRe = regexp.MustCompile(`^\[(?:\d{4}-\d{2}-\d{2}[ T])?\d{2}:\d{2}:\d{2}(?:[.,]\d+)?\] ([A-Za-z]+):`)

	logfmtStartRe = regexp.MustCompile(`^[A-Za-z_][\w.-]*=`)

	bracketRe = regexp.MustCompile(`^\[([A-Za-z]+)\]`)

	genericRe = regexp.MustCompile(`(?i)\b(trace|debug|info|notice|warn|warning|error|fatal|critical)\b`)
)

const genericWindow = 40

func result(levelWord string, fields ...string) (parseResult, bool) {
	level, ok := router.NormalizeLevel(levelWord)
	if !ok {
		return parseResult{}, false
	}
	r := parseResult{level: level}
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i+1] != "" {
			r.fields = append(r.fields, [2]string{fields[i], fields[i+1]})
		}
	}
	return r, true
}

func parseHomeAssistant(text string) (parseResult, bool) {
	m := haRe.FindStringSubmatch(text)
	if m == nil {
		return parseResult{}, false
	}
	return result(m[1], "thread", m[2], "logger", m[3])
}

func parseBashio(text string) (parseResult, bool) {
	m := bashioRe.FindStringSubmatch(text)
	if m == nil {
		return parseResult{}, false
	}
	return result(m[1])
}

func parseLogfmt(text string) (parseResult, bool) {
	if !logfmtStartRe.MatchString(text) {
		return parseResult{}, false
	}
	// Walk the key=value pairs so that "level=" inside a quoted value is ignored.
	for i := 0; i < len(text); {
		for i < len(text) && text[i] == ' ' {
			i++
		}
		start := i
		for i < len(text) && text[i] != ' ' && text[i] != '=' {
			i++
		}
		key := text[start:i]
		if i >= len(text) || text[i] != '=' {
			continue // bare word
		}
		i++
		var val string
		if i < len(text) && text[i] == '"' {
			i++
			vs := i
			for i < len(text) && text[i] != '"' {
				if text[i] == '\\' {
					i++
				}
				i++
			}
			end := min(i, len(text))
			val = text[vs:end]
			i++
		} else {
			vs := i
			for i < len(text) && text[i] != ' ' {
				i++
			}
			val = text[vs:i]
		}
		if key == "level" || key == "lvl" {
			return result(val)
		}
	}
	return parseResult{}, false
}

func parseJSON(text string) (parseResult, bool) {
	if !strings.HasPrefix(text, "{") {
		return parseResult{}, false
	}
	var obj struct {
		Level, Severity, Logger any
	}
	if err := json.Unmarshal([]byte(text), &obj); err != nil {
		return parseResult{}, false
	}
	word, _ := obj.Level.(string)
	if _, ok := router.NormalizeLevel(word); !ok {
		// Some loggers put a non-level in `level` and the real one in `severity`.
		word, _ = obj.Severity.(string)
	}
	logger, _ := obj.Logger.(string)
	return result(word, "logger", logger)
}

func parseBracket(text string) (parseResult, bool) {
	m := bracketRe.FindStringSubmatch(text)
	if m == nil {
		return parseResult{}, false
	}
	return result(m[1])
}

func parseGeneric(text string) (parseResult, bool) {
	// The keyword must start in the window. Look a bit beyond it so the word boundary is checked on real text.
	if n := genericWindow + len("critical") + 1; len(text) > n {
		text = text[:n]
	}
	loc := genericRe.FindStringIndex(text)
	if loc == nil || loc[0] >= genericWindow {
		return parseResult{}, false
	}
	return result(text[loc[0]:loc[1]])
}
