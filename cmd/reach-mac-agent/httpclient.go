package main

import "reach/internal/safehttp"

// No redirects for admin API calls; no client timeout because the event
// stream is long-lived (requests carry their own contexts).
var apiClient = safehttp.API(0)
