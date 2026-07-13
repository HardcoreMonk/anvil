// Package sni provides a pure-Go TLS ClientHello SNI parser and an allow-list
// matcher. Nothing here touches netlink or the kernel — it is unit-testable
// without root so the fail-closed decision logic can be validated cheaply.
package sni

import (
	"fmt"
	"strings"
)

type Matcher struct {
	exact    map[string]struct{}
	wildcard []string // stored as ".example.com" (the suffix a "*." pattern must match on a label boundary)
}

func NewMatcher(patterns []string) (*Matcher, error) {
	m := &Matcher{exact: make(map[string]struct{})}
	for _, raw := range patterns {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			return nil, fmt.Errorf("empty allow_sni pattern")
		}
		if strings.HasPrefix(p, "*.") {
			rest := p[2:]
			if rest == "" || strings.ContainsRune(rest, '*') {
				return nil, fmt.Errorf("invalid wildcard allow_sni %q", raw)
			}
			m.wildcard = append(m.wildcard, "."+rest)
			continue
		}
		if strings.ContainsRune(p, '*') {
			return nil, fmt.Errorf("allow_sni %q: '*' only allowed as a leading %q label", raw, "*.")
		}
		m.exact[p] = struct{}{}
	}
	return m, nil
}

func (m *Matcher) Empty() bool { return m == nil || (len(m.exact) == 0 && len(m.wildcard) == 0) }

func (m *Matcher) Match(serverName string) bool {
	if m == nil {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(serverName))
	if name == "" {
		return false
	}
	if _, ok := m.exact[name]; ok {
		return true
	}
	for _, suffix := range m.wildcard {
		// "*.example.com" -> suffix ".example.com"; require >=1 left label so
		// the match lands on a label boundary and "example.com" itself is excluded.
		if strings.HasSuffix(name, suffix) && len(name) > len(suffix) {
			return true
		}
	}
	return false
}
