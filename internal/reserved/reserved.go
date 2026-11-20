// Package reserved holds the SSH alias names that a Reach target may never
// claim. reachd (slug validation) and reach-mac-agent (rendering the
// operator's SSH config) both use this one list so the two trust boundaries
// cannot drift apart (astra P2-10).
package reserved

import (
	"path"
	"strings"
)

// Patterns use path.Match syntax.
var Patterns = []string{
	"jason*", "*-public", "pi", "pi-*", "localhost", "localhost-*",
	"reach", "reach-hub", "hub", "hub-*", "github*", "gitlab*", "bitbucket*",
}

// Match reports whether name matches a built-in or extra reserved pattern.
func Match(name string, extra []string) bool {
	for _, list := range [][]string{Patterns, extra} {
		for _, p := range list {
			p = strings.ToLower(strings.TrimSpace(p))
			if p == "" {
				continue
			}
			if ok, _ := path.Match(p, name); ok {
				return true
			}
		}
	}
	return false
}
