package expr

import (
	_ "embed"
	"strings"
)

//go:embed reserved.txt
var reservedRaw string

// reserved holds DynamoDB's reserved words, lower-cased for the
// case-insensitive check. A bare attribute name matching one of these is
// rejected in any expression.
var reserved = func() map[string]bool {
	m := make(map[string]bool)
	for _, line := range strings.Split(reservedRaw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m[strings.ToLower(line)] = true
	}
	return m
}()
