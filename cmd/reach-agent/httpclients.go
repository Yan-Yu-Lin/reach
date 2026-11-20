package main

import (
	"time"

	"reach/internal/safehttp"
)

// Control-plane requests never follow redirects; downloads revalidate every
// hop and never downgrade from https (daybreak round 3 P2-B).
var (
	apiClient      = safehttp.API(60 * time.Second)
	downloadClient = safehttp.Download(5 * time.Minute)
)
