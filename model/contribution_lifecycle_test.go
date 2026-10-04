package model

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The contribution liveness predicate is the only death criterion of the whole
// feature: the submit-time probe, the periodic probe task and the release
// pipeline all branch on this single function, so every status class it must
// classify is pinned here.
func TestIsContributionKeyDead(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantDead   bool
	}{
		{"upstream 401 unauthorized is dead", http.StatusUnauthorized, true},
		{"upstream 403 forbidden is alive", http.StatusForbidden, false},
		{"upstream 429 rate limited is alive", http.StatusTooManyRequests, false},
		{"upstream 500 server error is alive", http.StatusInternalServerError, false},
		{"upstream 503 service unavailable is alive", http.StatusServiceUnavailable, false},
		{"successful query with zero balance is alive", http.StatusOK, false},
		{"no HTTP response (timeout or transport failure) is alive", 0, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.wantDead, IsContributionKeyDead(test.statusCode))
		})
	}
}

func TestContributionKeyFingerprint(t *testing.T) {
	const baseURL = "https://upstream.example.com"
	const key = "sk-contribution-secret"

	fingerprint := ContributionKeyFingerprint(baseURL, key)

	assert.NotEmpty(t, fingerprint)
	assert.NotContains(t, fingerprint, key, "the fingerprint must never leak the plaintext key")
	assert.Equal(t, fingerprint, ContributionKeyFingerprint(baseURL, key), "the fingerprint must be stable across submit and probe")
	assert.NotEqual(t, fingerprint, ContributionKeyFingerprint("https://other.example.com", key))
	assert.NotEqual(t, fingerprint, ContributionKeyFingerprint(baseURL, key+"x"))

	// The base URL and the key are joined with a separator, so moving the
	// boundary between them produces a different fingerprint instead of the
	// same naive concatenation.
	assert.NotEqual(t,
		ContributionKeyFingerprint("https://upstream.example.com", "bsecret"),
		ContributionKeyFingerprint("https://upstream.example.comb", "secret"),
	)
}
