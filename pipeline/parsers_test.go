package pipeline

import (
	"bufio"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParserFixtures(t *testing.T) {
	for _, name := range ParserNames() {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", "parsers", name+".txt"))
			if err != nil {
				t.Fatalf("every parser needs a fixture file: %v", err)
			}
			defer f.Close()
			n := 0
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				line := sc.Text()
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				cols := strings.Split(line, "\t")
				if len(cols) != 3 {
					t.Fatalf("bad fixture line %q", line)
				}
				input := strings.ReplaceAll(cols[0], `\e`, "\x1b")
				wantLevel := cols[1]
				var wantFields map[string]string
				if cols[2] != "" {
					wantFields = map[string]string{}
					for _, kv := range strings.Split(cols[2], ";") {
						k, v, _ := strings.Cut(kv, "=")
						wantFields[k] = v
					}
				}
				n++
				res, ok := parsers[name](stripANSI(input))
				if wantLevel == "-" {
					if ok {
						t.Errorf("%q: want no match, got %q", cols[0], res.level)
					}
					continue
				}
				if !ok || res.level != wantLevel {
					t.Errorf("%q: got level %q (ok=%v), want %q", cols[0], res.level, ok, wantLevel)
					continue
				}
				var gotFields map[string]string
				for _, kv := range res.fields {
					if gotFields == nil {
						gotFields = map[string]string{}
					}
					gotFields[kv[0]] = kv[1]
				}
				if !reflect.DeepEqual(gotFields, wantFields) {
					t.Errorf("%q: fields %v, want %v", cols[0], gotFields, wantFields)
				}
			}
			if n == 0 {
				t.Error("empty fixture")
			}
		})
	}
}

func TestParserEdgeCases(t *testing.T) {
	tests := []struct {
		parser, line, want string // want "" means no match
	}{
		{"generic", "1234567890123456789012345678901234 errors", ""},
		{"generic", "1234567890123456789012345678901234 error", "error"},
		{"logfmt", `msg="saw level=error in input" level=info`, "info"},
		{"logfmt", `msg="a \" level=error" level=warn`, "warning"},
		{"logfmt", `msg="only level=error inside"`, ""},
	}
	for _, tt := range tests {
		res, ok := parsers[tt.parser](tt.line)
		if tt.want == "" && ok || tt.want != "" && (!ok || res.level != tt.want) {
			t.Errorf("%s(%q) = %q ok=%v, want %q", tt.parser, tt.line, res.level, ok, tt.want)
		}
	}
}
