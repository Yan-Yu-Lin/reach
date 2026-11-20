package reserved

import "testing"

func TestMatch(t *testing.T) {
	for _, n := range []string{"jason", "jason-ts", "x-public", "pi", "localhost-1", "reach", "hub-2", "bitbucket"} {
		if !Match(n, nil) {
			t.Fatalf("%q not reserved", n)
		}
	}
	for _, n := range []string{"omarchy", "jack", "pix", "reachable", "myhub"} {
		if Match(n, nil) {
			t.Fatalf("%q wrongly reserved", n)
		}
	}
	if !Match("lab-1", []string{"LAB-*"}) {
		t.Fatal("extra pattern ignored")
	}
}
