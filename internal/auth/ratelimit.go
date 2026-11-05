package auth

import "github.com/silopost/silo-server/internal/ratelimit"

// Limit describes a token bucket: it holds up to Burst tokens and refills one
// token every Every (see package ratelimit, which holds the implementation).
type Limit = ratelimit.Limit
