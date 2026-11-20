// Package reserved holds the SSH alias names that a Reach target may never
// claim. reachd (slug validation) and reach-mac-agent (rendering the
// operator's SSH config) both use this one list so the two trust boundaries
// cannot drift apart (astra P2-10).
package reserved

import (
	"fmt"
	"path"
	"strings"
)

// Patterns use path.Match syntax.
var Patterns = []string{
	"jason*", "*-public", "pi", "pi-*", "localhost", "localhost-*",
	"reach", "reach-hub", "hub", "hub-*", "github*", "gitlab*", "bitbucket*",
}

// Validate reports the first pattern that is not valid path.Match syntax.
// Config loading and the Mac agent call it so a typo cannot silently
// reserve nothing (daybreak round 4 P3-02).
func Validate(patterns []string) error {
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("invalid reserved alias pattern %q: %w", p, err)
		}
	}
	return nil
}

// Match reports whether name matches a built-in or extra reserved pattern.
// A malformed pattern matches everything (fail closed).
func Match(name string, extra []string) bool {
	for _, list := range [][]string{Patterns, extra} {
		for _, p := range list {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == "" {
				continue
			}
			ok, err := path.Match(p, name)
			if ok || err != nil {
				return true
			}
		}
	}
	return false
}
