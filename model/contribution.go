package model

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
)

// ContributionKeyFingerprint is a keyed HMAC (common.GenerateHMAC, keyed by
// common.CryptoSecret) over the host channel base URL and the raw upstream key.
// The plaintext key is never stored: only this fingerprint and a display-only
// mask enter the contributed_keys table, responses and logs.
//
// The base URL MUST be derived identically at submit time and at probe time:
// hostChannel.GetBaseURL(), which falls back to
// constant.GetChannelBaseURL(hostChannel.Type) when BaseURL is empty.
//
// Consequence: if an admin later changes the host channel base URL, fingerprints
// recorded earlier no longer match any key in that channel. Such records become
// probe orphans (the probe skips them, they stay active) and the same key can be
// submitted again as a new record. This is accepted, not a bug: the fingerprint
// only has to identify a key uniquely within one host channel configuration.
func ContributionKeyFingerprint(hostBaseURL string, key string) string {
	return common.GenerateHMAC(hostBaseURL + "\n" + key)
}

// IsContributionKeyDead reports whether a probe outcome is fatal for a
// contributed key. Only HTTP 401 is fatal: 403, timeouts, 5xx and a successful
// balance query that happens to return zero balance all count as alive.
//
// A probe that observed no HTTP response at all (transport error or timeout)
// reports status code 0 and is therefore alive as well: liveness must never be
// revoked on an ambiguous outcome. Balance is display-only and is never an input
// to this predicate.
func IsContributionKeyDead(httpStatusCode int) bool {
	return httpStatusCode == http.StatusUnauthorized
}
