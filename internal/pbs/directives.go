package pbs

import (
	"fmt"
	"strings"
	"unicode"
)

func Directives(script string) (Options, error) {
	out := NewOptions()
	for number, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			break
		}
		if !strings.HasPrefix(line, "#PBS") || (len(line) > 4 && !unicode.IsSpace(rune(line[4]))) {
			continue
		}
		args, err := tokenize(strings.TrimSpace(line[4:]))
		if err != nil {
			return out, fmt.Errorf("#PBS line %d: %w", number+1, err)
		}
		o, operands, err := Parse(args, "NqloejVvJWh")
		if err != nil {
			return out, fmt.Errorf("#PBS line %d: %w", number+1, err)
		}
		if len(operands) != 0 {
			return out, fmt.Errorf("#PBS line %d: unexpected argument %q", number+1, operands[0])
		}
		out.Merge(o)
	}
	return out, nil
}

// tokenize handles literal argument quoting without invoking a shell or expansion.
func tokenize(line string) ([]string, error) {
	var args []string
	var word strings.Builder
	var quote rune
	active, escaped := false, false
	for _, r := range line {
		if escaped {
			word.WriteRune(r)
			active = true
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			active = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			active = true
			continue
		}
		if unicode.IsSpace(r) {
			if active {
				args = append(args, word.String())
				word.Reset()
				active = false
			}
			continue
		}
		word.WriteRune(r)
		active = true
	}
	if escaped {
		return nil, fmt.Errorf("multiline continuations are unsupported")
	}
	if quote != 0 {
		return nil, fmt.Errorf("unmatched quote")
	}
	if active {
		args = append(args, word.String())
	}
	return args, nil
}
