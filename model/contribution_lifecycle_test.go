package model

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/contribution_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

// A key an upstream ever judged dead is dead for the key itself, not for the
// channel type or the host channel base URL it happened to be submitted under. The
// indexed key hash is what blocks every later submission - by any user, for any
// enabled channel type - because the per-record fingerprint changes with the base
// URL and cannot express that rule. Withdrawal deliberately does not count.
func TestContributionDeadKeyHashIsFinalAcrossChannelTypes(t *testing.T) {
	openContributionRewardTestDB(t)
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })

	const deadKey = "sk-contribution-globally-dead"
	host := seedContributionHostChannel(t, "host-key-one", 1)
	require.NoError(t, AppendOrEnableChannelKey(host.Id, deadKey))

	keyHash := ContributionKeyHash(deadKey)
	dead := &Contribution{
		UserId:         801,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), deadKey)),
		KeyHash:        keyHash,
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  true,
	}
	require.NoError(t, dead.Create())

	hasDead, err := HasDeadContributionForKeyHash(keyHash)
	require.NoError(t, err)
	assert.False(t, hasDead, "a live record is not a death")

	require.NoError(t, ReleaseContribution(dead, deadKey, ContributionStatusDead, ContributionReasonUpstreamUnauthorized, false))

	hasDead, err = HasDeadContributionForKeyHash(keyHash)
	require.NoError(t, err)
	assert.True(t, hasDead, "death is recorded against the key hash itself")

	// The hole the hash closes: the same key submitted for a channel type whose host
	// channel has a different base URL produces a fingerprint that has never been
	// recorded, so the fingerprint lookup alone would accept it.
	otherBaseFingerprint := ContributionKeyFingerprint("https://type-b.example.com", deadKey)
	assert.NotEqual(t, *dead.KeyFingerprint, otherBaseFingerprint)
	_, err = GetContributionByFingerprint(otherBaseFingerprint)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound, "a different base URL yields an unrecorded fingerprint")
	assert.True(t, hasDead, "the key hash still blocks every channel type")

	// Withdrawal frees the key: only dead is final.
	const revokedKey = "sk-contribution-globally-revoked"
	revoked := &Contribution{
		UserId:         802,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), revokedKey)),
		KeyHash:        ContributionKeyHash(revokedKey),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
	}
	require.NoError(t, revoked.Create())
	require.NoError(t, ReleaseContribution(revoked, revokedKey, ContributionStatusRevoked, ContributionReasonUserRevoked, true))

	hasDead, err = HasDeadContributionForKeyHash(ContributionKeyHash(revokedKey))
	require.NoError(t, err)
	assert.False(t, hasDead, "a revoked key is not dead and may be contributed again")
}

// The知情同意 is not just enforced, it is recorded: the exact consent statement the
// contributor accepted at submission time is stored on the row (and audited), so
// the wording they agreed to survives a later change of the served agreement.
func TestContributionRecordsTheAcceptedAgreement(t *testing.T) {
	truncateTables(t)

	contribution := &Contribution{
		UserId:         811,
		ChannelType:    1,
		HostChannelId:  1,
		KeyFingerprint: common.GetPointer("fp-agreement-recorded"),
		KeyHash:        ContributionKeyHash("sk-agreement-recorded"),
		KeyMask:        ContributionKeyMask,
		Agreement:      contribution_setting.AgreementText,
		Status:         ContributionStatusActive,
	}
	require.NoError(t, contribution.Create())

	var stored Contribution
	require.NoError(t, DB.Where("id = ?", contribution.Id).First(&stored).Error)
	assert.NotEmpty(t, stored.Agreement)
	assert.Equal(t, contribution_setting.AgreementText, stored.Agreement,
		"the accepted consent statement is recorded verbatim on the contribution")
}

// "One upstream counts once" is judged on a reward that actually exists. An active
// record whose grant failed (flagged rewarded, but pointing at no subscription)
// must not make a later submission redundant, or the contributor would be locked
// out of the reward forever.
func TestContributionRedundancyRequiresAGrantedReward(t *testing.T) {
	truncateTables(t)

	failed := &Contribution{
		UserId:         901,
		EntryCode:      "ABCDEFGH",
		ChannelType:    1,
		HostChannelId:  1,
		KeyFingerprint: common.GetPointer("fp-unrewarded"),
		KeyHash:        ContributionKeyHash("sk-unrewarded"),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  true,
		SubscriptionId: 0,
	}
	require.NoError(t, failed.Create())

	hasRewarded, err := HasRewardedActiveContributionForCode(901, "ABCDEFGH")
	require.NoError(t, err)
	assert.False(t, hasRewarded, "an unrewarded active record must not make a later submission redundant")

	// Attaching the instance the failed grant should have produced makes the entry
	// actually rewarded, which is the state the rule keys on.
	require.NoError(t, DB.Model(&Contribution{}).Where("id = ?", failed.Id).Update("subscription_id", 4242).Error)
	hasRewarded, err = HasRewardedActiveContributionForCode(901, "ABCDEFGH")
	require.NoError(t, err)
	assert.True(t, hasRewarded)

	// A redundant record was never promised a reward, so it does not count either,
	// and another entry is judged on its own.
	redundant := &Contribution{
		UserId:         901,
		EntryCode:      "ABCDEFGH",
		ChannelType:    1,
		HostChannelId:  1,
		KeyFingerprint: common.GetPointer("fp-redundant"),
		KeyHash:        ContributionKeyHash("sk-redundant"),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  false,
	}
	require.NoError(t, redundant.Create())
	hasRewarded, err = HasRewardedActiveContributionForCode(901, "JKMNPRST")
	require.NoError(t, err)
	assert.False(t, hasRewarded, "another entry is judged separately")
}

// ContributionKeyMask is the only shape a contributed key may ever be displayed
// in. Upstream keys are live credentials handed over by a third party, so the mask
// is a fixed constant: it cannot carry a prefix, a suffix, a length or any other
// fragment of the plaintext, because it never sees the plaintext at all.
func TestContributionKeyMaskNeverRevealsTheKey(t *testing.T) {
	assert.NotEmpty(t, ContributionKeyMask)
	for _, fragment := range []string{"sk-", "proj", "abcdefghij", "6789"} {
		assert.NotContains(t, ContributionKeyMask, fragment)
	}
	// The mask is a constant rather than a function of the key, so every
	// contributed key displays identically and no surface can leak a key fragment.
	assert.Equal(t, "************", ContributionKeyMask)
}

// seedContributionHostChannel inserts the real multi-key host channel a
// contribution appends its key to. Polling mode makes key selection
// deterministic, so "the key is selectable" is asserted instead of assumed.
func seedContributionHostChannel(t *testing.T, key string, size int) *Channel {
	t.Helper()
	channel := &Channel{
		Name:   t.Name(),
		Type:   1,
		Key:    key,
		Status: common.ChannelStatusEnabled,
		Models: "gpt-4o-mini",
		Group:  "default",
		ChannelInfo: ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       size,
			MultiKeyMode:       constant.MultiKeyModePolling,
			MultiKeyStatusList: map[int]int{},
		},
	}
	require.NoError(t, DB.Create(channel).Error)
	t.Cleanup(func() { InitChannelCache() })
	return channel
}

func reloadContributionChannel(t *testing.T, id int) *Channel {
	t.Helper()
	channel, err := GetChannelById(id, true)
	require.NoError(t, err)
	return channel
}

// TestContributionPoolingLifecycle is the main seam of the feature: it asserts
// what a submission does to the host channel and to the contribution table,
// through the public model API only.
func TestContributionPoolingLifecycle(t *testing.T) {
	truncateTables(t)
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })

	rewardPlan := &SubscriptionPlan{
		Id:               9301,
		Title:            "Contribution reward",
		Enabled:          true,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		TotalAmount:      1000,
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	require.NoError(t, DB.Create(rewardPlan).Error)

	host := seedContributionHostChannel(t, "host-key-one", 1)

	t.Run("an appended key lands at the end, enabled and selectable", func(t *testing.T) {
		require.NoError(t, AppendOrEnableChannelKey(host.Id, "contrib-key-a"))

		channel := reloadContributionChannel(t, host.Id)
		assert.Equal(t, "host-key-one\ncontrib-key-a", channel.Key, "the new key is appended at the end")
		assert.Equal(t, 2, channel.ChannelInfo.MultiKeySize)
		assert.Equal(t, common.ChannelStatusEnabled, channel.GetMultiKeyStatus(1), "an appended key is enabled by default")

		// Polling reaches every enabled key in order, so the second pick proves
		// the appended key really became selectable for relay traffic.
		// GetNextEnabledKey reports through a typed pointer, so a nil check is the
		// only assertion that cannot be fooled by a typed nil error interface.
		first, _, firstErr := channel.GetNextEnabledKey()
		require.Nil(t, firstErr)
		assert.Equal(t, "host-key-one", first)
		second, secondIndex, secondErr := channel.GetNextEnabledKey()
		require.Nil(t, secondErr)
		assert.Equal(t, "contrib-key-a", second)
		assert.Equal(t, 1, secondIndex)
	})

	t.Run("a key already in the channel but disabled is re-enabled, never duplicated", func(t *testing.T) {
		require.NoError(t, SetChannelKeyStatus(host.Id, "host-key-one", common.ChannelStatusAutoDisabled, "upstream rejected it"))
		require.NoError(t, SetChannelKeyStatus(host.Id, "contrib-key-a", common.ChannelStatusAutoDisabled, "upstream rejected it"))

		disabled := reloadContributionChannel(t, host.Id)
		require.Equal(t, common.ChannelStatusAutoDisabled, disabled.GetMultiKeyStatus(0))
		require.Equal(t, common.ChannelStatusAutoDisabled, disabled.GetMultiKeyStatus(1))
		require.Equal(t, common.ChannelStatusAutoDisabled, disabled.Status, "a channel with every key disabled stops serving")

		require.NoError(t, AppendOrEnableChannelKey(host.Id, "host-key-one"))

		channel := reloadContributionChannel(t, host.Id)
		assert.Equal(t, "host-key-one\ncontrib-key-a", channel.Key, "the key must not be appended a second time")
		assert.Equal(t, 2, channel.ChannelInfo.MultiKeySize)
		assert.Equal(t, common.ChannelStatusEnabled, channel.GetMultiKeyStatus(0))
		assert.Equal(t, common.ChannelStatusEnabled, channel.Status, "the host channel serves traffic again")
		_, hasReason := channel.ChannelInfo.MultiKeyDisabledReason[0]
		assert.False(t, hasReason)
		_, hasTime := channel.ChannelInfo.MultiKeyDisabledTime[0]
		assert.False(t, hasTime, "re-enabling clears the disable bookkeeping")
	})

	t.Run("a key that is already pooled and enabled is refused with the channel untouched", func(t *testing.T) {
		before := reloadContributionChannel(t, host.Id)

		err := AppendOrEnableChannelKey(host.Id, "host-key-one")

		require.ErrorIs(t, err, ErrChannelKeyAlreadyEnabled)
		after := reloadContributionChannel(t, host.Id)
		assert.Equal(t, before.Key, after.Key)
		assert.Equal(t, before.ChannelInfo.MultiKeySize, after.ChannelInfo.MultiKeySize)
		assert.Equal(t, before.Status, after.Status)
	})

	t.Run("the fingerprint is first-come-first-served", func(t *testing.T) {
		const submittedKey = "sk-contribution-submitted"
		fingerprint := ContributionKeyFingerprint(host.GetBaseURL(), submittedKey)

		first := &Contribution{
			UserId:         101,
			EntryCode:      "ABCDEFGH",
			ChannelType:    1,
			HostChannelId:  host.Id,
			KeyFingerprint: common.GetPointer(fingerprint),
			KeyMask:        ContributionKeyMask,
			Status:         ContributionStatusActive,
			RewardGranted:  true,
		}
		require.NoError(t, first.Create())
		require.NotZero(t, first.Id)
		require.NotZero(t, first.CreatedTime)
		assert.NotContains(t, first.KeyMask, submittedKey)

		// A second submission of the same key - by this user or any other - has to
		// fail: the unique fingerprint index is what makes that race-safe.
		second := &Contribution{
			UserId:         102,
			ChannelType:    1,
			HostChannelId:  host.Id,
			KeyFingerprint: common.GetPointer(fingerprint),
			KeyMask:        ContributionKeyMask,
			Status:         ContributionStatusActive,
			RewardGranted:  true,
		}
		require.Error(t, second.Create())
		require.Zero(t, second.Id)

		stored, err := GetContributionByFingerprint(fingerprint)
		require.NoError(t, err)
		assert.Equal(t, first.Id, stored.Id)
		assert.Equal(t, 101, stored.UserId)

		active, err := GetActiveContributionsByUserAndEntryCode(101, "ABCDEFGH")
		require.NoError(t, err)
		require.Len(t, active, 1)
		assert.Equal(t, first.Id, active[0].Id)

		all, err := GetAllActiveContributions()
		require.NoError(t, err)
		require.Len(t, all, 1)

		mine, err := GetContributionsByUser(101)
		require.NoError(t, err)
		require.Len(t, mine, 1)
	})

}

// TestContributionReleaseFreesFingerprintOnlyWhenAsked pins the caller-controlled
// half of the terminal transition: death keeps the fingerprint forever, so only an
// explicit release (the user-initiated revoke) may free it.
func TestContributionReleaseFreesFingerprintOnlyWhenAsked(t *testing.T) {
	openContributionRewardTestDB(t)
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })

	host := seedContributionHostChannel(t, "host-key-one", 1)
	const contributedKey = "sk-contribution-released"
	require.NoError(t, AppendOrEnableChannelKey(host.Id, contributedKey))

	contribution := &Contribution{
		UserId:         401,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), contributedKey)),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
	}
	require.NoError(t, contribution.Create())
	fingerprint := *contribution.KeyFingerprint

	require.NoError(t, ReleaseContribution(contribution, contributedKey, ContributionStatusRevoked, ContributionReasonUserRevoked, true))

	// The record is retained for audit, but its fingerprint is NULL: exactly the
	// shape that lets the same key be submitted again.
	_, err := GetContributionByFingerprint(fingerprint)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound, "a released fingerprint no longer identifies the record")
	var released Contribution
	require.NoError(t, DB.Where("id = ?", contribution.Id).First(&released).Error)
	assert.Equal(t, ContributionStatusRevoked, released.Status)
	assert.Nil(t, released.KeyFingerprint)

	resubmitted := &Contribution{
		UserId:         402,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(fingerprint),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
	}
	require.NoError(t, resubmitted.Create(), "release makes the same key submittable again")
	assert.NotEqual(t, contribution.Id, resubmitted.Id)
}

// TestContributionDeathIsTerminal is the model-layer main seam of the release
// pipeline: one call must disable the key in the host channel, cancel the reward
// subscription and move the record to its terminal status, and each of those
// effects is asserted through the public model API so the model/service import
// cycle cannot hide a missing step.
func TestContributionDeathIsTerminal(t *testing.T) {
	openContributionRewardTestDB(t)
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })

	plan := &SubscriptionPlan{
		Id:               9502,
		Title:            "Contribution reward",
		Enabled:          true,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		TotalAmount:      1000,
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	require.NoError(t, DB.Create(plan).Error)

	contributor := &User{
		Id:          401,
		Username:    "contribution-death",
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
	}
	require.NoError(t, DB.Create(contributor).Error)

	const contributedKey = "sk-contribution-dead"
	host := seedContributionHostChannel(t, "host-key-one", 1)
	require.NoError(t, AppendOrEnableChannelKey(host.Id, contributedKey))

	contribution := &Contribution{
		UserId:         contributor.Id,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), contributedKey)),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  true,
	}
	require.NoError(t, contribution.Create())
	subscription, err := GrantContributionReward(contribution, plan.Id)
	require.NoError(t, err)
	require.NotNil(t, subscription)
	fingerprint := *contribution.KeyFingerprint

	require.NoError(t, ReleaseContribution(contribution, contributedKey, ContributionStatusDead, ContributionReasonUpstreamUnauthorized, false))

	// The record survives - never deleted - carrying the terminal status and the
	// machine-readable reason the frontend localizes, and it leaves the probe's
	// input set so a repeated probe cannot see it again.
	stored, err := GetContributionByFingerprint(fingerprint)
	require.NoError(t, err)
	assert.Equal(t, ContributionStatusDead, stored.Status)
	assert.Equal(t, ContributionReasonUpstreamUnauthorized, stored.Reason)
	assert.NotZero(t, stored.ReasonTime)
	active, err := GetAllActiveContributions()
	require.NoError(t, err)
	assert.Empty(t, active, "a dead contribution is no longer probed")

	// The key is still physically present in the host channel - an administrator
	// reclaims it with the existing cleanup action, not with this transition - but
	// it is auto-disabled with the reason recorded against its index.
	channel := reloadContributionChannel(t, host.Id)
	assert.Equal(t, "host-key-one\n"+contributedKey, channel.Key, "death never deletes the key")
	assert.Equal(t, common.ChannelStatusAutoDisabled, channel.GetMultiKeyStatus(1))
	assert.Equal(t, ContributionReasonUpstreamUnauthorized, channel.ChannelInfo.MultiKeyDisabledReason[1])

	// ... and it is no longer selectable for relay traffic: polling reaches every
	// enabled key in order, so three picks that all land on the surviving key prove
	// the dead one was skipped rather than merely not picked first.
	for pick := range 3 {
		key, _, pickErr := channel.GetNextEnabledKey()
		require.Nil(t, pickErr, "pick %d", pick)
		assert.Equal(t, "host-key-one", key, "a dead key must never be selected")
	}

	// The reward is cancelled, not deleted, and its granted quota is not clawed
	// back: the record of what the contributor was owed survives for the audit.
	cancelled, err := GetUserSubscriptionById(subscription.Id)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", cancelled.Status)
	assert.Less(t, cancelled.EndTime, subscription.EndTime, "the reward stops at the moment of death")
	assert.LessOrEqual(t, cancelled.EndTime, common.GetTimestamp())
	assert.EqualValues(t, subscription.AmountTotal, cancelled.AmountTotal)
	assert.EqualValues(t, subscription.AmountUsed, cancelled.AmountUsed)

	// The fingerprint stays occupied forever: neither the contributor nor anyone
	// else may submit that key again, and no active record holds it any more.
	second := &Contribution{
		UserId:         402,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(fingerprint),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  true,
	}
	require.Error(t, second.Create(), "a dead fingerprint is blocked forever")
	_, err = LockAndGetActiveContributionByFingerprint(DB, fingerprint)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)

	// A repeated kill is a no-op. The key is re-enabled and the subscription's end
	// time is moved to a sentinel first, so a second release would be visible as a
	// second disable and a second cancellation instead of passing by construction.
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", subscription.Id).
		Update("end_time", int64(12345)).Error)
	require.NoError(t, AppendOrEnableChannelKey(host.Id, contributedKey))

	require.NoError(t, ReleaseContribution(contribution, contributedKey, ContributionStatusDead, ContributionReasonUpstreamUnauthorized, false))

	untouched := reloadContributionChannel(t, host.Id)
	assert.Equal(t, common.ChannelStatusEnabled, untouched.GetMultiKeyStatus(1), "a repeated kill must not disable the key again")
	stillCancelled, err := GetUserSubscriptionById(subscription.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 12345, stillCancelled.EndTime, "a repeated kill must not cancel the subscription again")
	movedAgain, err := MarkContributionDead(contribution.Id, ContributionReasonUpstreamUnauthorized)
	require.NoError(t, err)
	assert.False(t, movedAgain, "the terminal transition is a compare-and-set on the active status")

	// A no-op leaves the caller's struct untouched, which is how the probe task
	// tells "I performed the transition" from "a revoke or an earlier pass already
	// did" and skips its own audit and notification. The struct is reset to the
	// stale value a caller would have read before the first kill.
	contribution.Status = ContributionStatusActive
	require.NoError(t, ReleaseContribution(contribution, contributedKey, ContributionStatusDead, ContributionReasonUpstreamUnauthorized, false))
	assert.Equal(t, ContributionStatusActive, contribution.Status, "a no-op must not report a transition it did not perform")
}

// The contribution table is created by AutoMigrate on every supported database,
// and fingerprint uniqueness is enforced by an index rather than by application
// code. This environment has no MySQL or PostgreSQL server, so the assertion is
// SQLite-only - reported as a verification gap, not as cross-database proof.
func TestContributionTableIsMigratedWithUniqueFingerprint(t *testing.T) {
	require.True(t, DB.Migrator().HasTable("contributed_keys"))
	indexName := DB.NamingStrategy.IndexName("contributed_keys", "key_fingerprint")
	require.NotEmpty(t, indexName)
	assert.True(t, DB.Migrator().HasIndex("contributed_keys", indexName), "expected unique index %s", indexName)
}

// "One upstream counts once" is keyed per catalog entry (per code), not per
// provider channel type: an administrator may open two enabled entries that both
// back the same provider channel type, and each of them is its own upstream for
// contribution purposes.
func TestContributionCatalogAllowsTwoEnabledEntriesPerChannelType(t *testing.T) {
	// The catalog is normally persisted into the options table by the controller
	// package, which owns the option writer. This test binary has no options
	// table, so persistence is captured here and an existing catalog cannot leak
	// in: only the in-memory module and its validation rules are under test.
	contribution_setting.SetOptionWriter(func(string, string) error { return nil })
	t.Cleanup(func() {
		require.NoError(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{}))
	})

	require.NoError(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{
		{Id: 1, Code: "ABCDEFGH", ChannelType: 1, Name: "OpenAI", Enabled: true, HostChannelId: 1, PlanId: 1},
		{Id: 2, Code: "JKMNPRST", ChannelType: 1, Name: "OpenAI reseller", Enabled: true, HostChannelId: 2, PlanId: 2},
	}), "two enabled entries may share one provider channel type")

	first, found := contribution_setting.EntryByCode("ABCDEFGH")
	require.True(t, found)
	assert.Equal(t, "OpenAI", first.Name)
	second, found := contribution_setting.EntryByCode("JKMNPRST")
	require.True(t, found)
	assert.Equal(t, "OpenAI reseller", second.Name)
	assert.Len(t, contribution_setting.AllEntries(), 2)

	// The code is the only identity that stays unique: a second entry claiming an
	// existing code is refused, while a duplicate internal id stays refused too.
	require.ErrorIs(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{
		{Id: 1, Code: "ABCDEFGH", ChannelType: 1, Name: "OpenAI", Enabled: true, HostChannelId: 1, PlanId: 1},
		{Id: 3, Code: "ABCDEFGH", ChannelType: 2, Name: "Clone", Enabled: true, HostChannelId: 2, PlanId: 2},
	}), contribution_setting.ErrEntryCodeDuplicated)
	assert.Len(t, contribution_setting.AllEntries(), 2, "the rejected catalog must not be stored")
	require.ErrorIs(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{
		{Id: 1, Code: "ABCDEFGH", ChannelType: 1, Name: "OpenAI", Enabled: true, HostChannelId: 1, PlanId: 1},
		{Id: 1, Code: "JKMNPRST", ChannelType: 2, Name: "Clone", Enabled: true, HostChannelId: 2, PlanId: 2},
	}), contribution_setting.ErrEntryIdDuplicated)
}

// A submission is resolved by the entry's code, and resolution has to follow the
// enabled entry only: a disabled entry is an admin-side draft, so its code stops
// resolving and submit refuses it instead of silently pooling into a disabled
// upstream.
func TestContributionEntryByCodeResolvesOnlyEnabledEntries(t *testing.T) {
	contribution_setting.SetOptionWriter(func(string, string) error { return nil })
	t.Cleanup(func() {
		require.NoError(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{}))
	})

	require.NoError(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{
		{Id: 1, Code: "AAAABBBB", ChannelType: 4, Name: "Disabled A", Enabled: false, HostChannelId: 1, PlanId: 1},
		{Id: 2, Code: "CCCCDDDD", ChannelType: 4, Name: "Enabled B", Enabled: true, HostChannelId: 2, PlanId: 2},
	}))

	resolved, found := contribution_setting.EntryByCode("CCCCDDDD")
	require.True(t, found)
	assert.Equal(t, "Enabled B", resolved.Name, "the enabled entry resolves, not the disabled first entry")
	assert.Equal(t, 2, resolved.HostChannelId)

	// With no enabled entry the code does not resolve at all, so submit refuses it
	// and the notice falls back to the channel type name.
	require.NoError(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{
		{Id: 1, Code: "AAAABBBB", ChannelType: 4, Name: "Disabled A", Enabled: false, HostChannelId: 1, PlanId: 1},
	}))
	_, found = contribution_setting.EntryByCode("CCCCDDDD")
	assert.False(t, found, "a removed entry's code stops resolving")
}

// The entry code is the durable, user-facing identity of an upstream: 8
// characters drawn from the distinguishable Base32 alphabet (no 0/1/I/O/L), so a
// user can read a code back without confusing a digit for a letter. Every draw is
// fresh, so the code namespace is independent of the internal auto-increment id.
func TestContributionEntryCodeIsGeneratedDistinctAndUnambiguous(t *testing.T) {
	seen := make(map[string]bool, 100)
	for range 100 {
		code, err := contribution_setting.NewEntryCode()
		require.NoError(t, err)
		require.Len(t, code, 8)
		for _, r := range code {
			require.True(t, (r >= 'A' && r <= 'Z') || (r >= '2' && r <= '9'),
				"unexpected character %q in code %s", r, code)
			require.NotContains(t, "01IOL", string(r), "code %s uses an ambiguous character", code)
		}
		require.False(t, seen[code], "a generated code must be unique within a draw")
		seen[code] = true
	}
}

// openContributionRewardTestDB hands one test its own shared-cache in-memory
// database with a real connection pool.
//
// A reward grant runs AdminBindSubscription's transaction, which also reads the
// database clock through DB while that transaction holds its connection. The
// package harness keeps exactly one connection (SetMaxOpenConns(1) on a private
// ":memory:" database), where that read would block forever, so the grant needs
// the shared-cache fixture subscription_auth_test.go already uses.
func openContributionRewardTestDB(t *testing.T) {
	t.Helper()
	previousDB, previousLogDB := DB, LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	testDB, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	DB, LOG_DB = testDB, testDB
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, testDB.AutoMigrate(
		&User{}, &Channel{}, &Ability{}, &SubscriptionPlan{}, &UserSubscription{}, &Contribution{},
	))
	sqlDB, err := testDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(4)
	t.Cleanup(func() {
		DB, LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		_ = sqlDB.Close()
		InitChannelCache()
	})
}

// TestContributionRewardGrantLifecycle is the model-layer main seam of the
// reward: in one place it asserts both side effects of a successful grant - the
// contributed key is pooled in the host channel and selectable there, and the
// contributor holds a subscription built from the catalog entry's plan - and then
// the "one upstream counts once" rule and the reset schedule the subscription
// inherits from its plan.
func TestContributionRewardGrantLifecycle(t *testing.T) {
	openContributionRewardTestDB(t)
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })

	plan := &SubscriptionPlan{
		Id:               9401,
		Title:            "Contribution reward",
		Enabled:          true,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		TotalAmount:      1000,
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	require.NoError(t, DB.Create(plan).Error)

	// A reward grant locks the contributor's user row, so the contributor must
	// exist before an instance can be issued.
	contributor := &User{
		Id:          201,
		Username:    "contribution-contributor",
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
	}
	require.NoError(t, DB.Create(contributor).Error)

	const contributedKey = "sk-contribution-rewarded"
	host := seedContributionHostChannel(t, "host-key-one", 1)
	require.NoError(t, AppendOrEnableChannelKey(host.Id, contributedKey))

	contribution := &Contribution{
		UserId:         contributor.Id,
		EntryCode:      "ABCDEFGH",
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), contributedKey)),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  true,
	}
	require.NoError(t, contribution.Create())

	subscription, err := GrantContributionReward(contribution, plan.Id)
	require.NoError(t, err)
	require.NotNil(t, subscription)

	// Side effect 1: the contributed key is pooled, enabled, and therefore selected
	// by relay traffic. Polling reaches every enabled key in order, so the second
	// pick proves the contributed key really became selectable.
	channel := reloadContributionChannel(t, host.Id)
	assert.Equal(t, "host-key-one\n"+contributedKey, channel.Key)
	assert.Equal(t, common.ChannelStatusEnabled, channel.GetMultiKeyStatus(1))
	first, _, firstErr := channel.GetNextEnabledKey()
	require.Nil(t, firstErr)
	assert.Equal(t, "host-key-one", first)
	second, secondIndex, secondErr := channel.GetNextEnabledKey()
	require.Nil(t, secondErr)
	assert.Equal(t, contributedKey, second)
	assert.Equal(t, 1, secondIndex)

	// Side effect 2: the contributor holds the plan's subscription, and only that.
	stored, err := GetUserSubscriptionById(subscription.Id)
	require.NoError(t, err)
	assert.Equal(t, contributor.Id, stored.UserId)
	assert.Equal(t, plan.Id, stored.PlanId)
	assert.EqualValues(t, plan.TotalAmount, stored.AmountTotal)
	assert.EqualValues(t, 0, stored.AmountUsed)
	assert.Equal(t, "active", stored.Status)
	assert.Equal(t, "contribution", stored.Source, "a reward stays distinguishable from an admin grant")

	var record Contribution
	require.NoError(t, DB.Where("id = ?", contribution.Id).First(&record).Error)
	assert.Equal(t, subscription.Id, record.SubscriptionId, "the record points at the instance it produced")

	// Granting again for the same record is a no-op: idempotency lives on the record.
	again, err := GrantContributionReward(&record, plan.Id)
	require.NoError(t, err)
	assert.Equal(t, subscription.Id, again.Id)

	// "One upstream counts once": a second contribution of the same channel type is
	// redundant and grants no second reward.
	hasActive, err := HasActiveContributionForCode(contributor.Id, "ABCDEFGH", 0)
	require.NoError(t, err)
	assert.True(t, hasActive, "the channel type already holds an active contribution")

	redundant := &Contribution{
		UserId:         contributor.Id,
		EntryCode:      "ABCDEFGH",
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), "sk-contribution-redundant")),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  false,
	}
	require.NoError(t, redundant.Create())
	assert.Zero(t, redundant.SubscriptionId)
	var subscriptionCount int64
	require.NoError(t, DB.Model(&UserSubscription{}).
		Where("user_id = ?", contributor.Id).Count(&subscriptionCount).Error)
	assert.EqualValues(t, 1, subscriptionCount, "the same channel type never grants a second reward")

	// The daily rollover belongs to the plan and the existing reset task
	// (service/subscription_reset_task.go): the granted instance inherits the
	// plan's schedule instead of a day-rollover invented by this feature. Asserting
	// the boundary itself - not a re-derivation of the formula - keeps that honest.
	require.NotZero(t, stored.NextResetTime)
	assert.Greater(t, stored.NextResetTime, stored.StartTime)
	assert.LessOrEqual(t, stored.NextResetTime-stored.StartTime, int64(24*time.Hour))
	next := time.Unix(stored.NextResetTime, 0)
	assert.Equal(t, 0, next.Hour())
	assert.Equal(t, 0, next.Minute())
	assert.Equal(t, 0, next.Second())
	assert.Equal(t, stored.StartTime, stored.LastResetTime)

	// The rollover itself belongs to the existing machine, and the granted
	// instance participates in it: an instance that is due is reset by
	// ResetDueSubscriptions, not by anything this feature added.
	twoDaysAgo := stored.StartTime - 2*24*3600
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", stored.Id).
		Updates(map[string]any{
			"amount_used":     40,
			"last_reset_time": twoDaysAgo,
			"next_reset_time": twoDaysAgo + 24*3600,
		}).Error)
	resetCount, err := ResetDueSubscriptions(10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, resetCount)
	rolled, err := GetUserSubscriptionById(stored.Id)
	require.NoError(t, err)
	assert.Zero(t, rolled.AmountUsed, "the existing reset machine performs the daily rollover")
	assert.Greater(t, rolled.LastResetTime, twoDaysAgo, "the reset moved the base past the elapsed period")
	assert.Greater(t, rolled.NextResetTime, common.GetTimestamp(), "the rollover scheduled the next daily reset")
}

// TestContributionReleaseStaysRetryableWhenTheKeyCannotBeDisabled pins the ordering
// invariant of the terminal transition: the side effects run before the record becomes
// terminal, so a release that cannot disable the key leaves the record active for the
// next liveness pass instead of reporting a death whose key keeps serving traffic.
func TestContributionReleaseStaysRetryableWhenTheKeyCannotBeDisabled(t *testing.T) {
	truncateTables(t)
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })

	const contributedKey = "sk-contribution-unreleasable"
	host := seedContributionHostChannel(t, contributedKey, 1)
	// A host channel that is no longer a multi-key channel cannot address one of its
	// keys: this is the hard failure the release must not swallow.
	host.ChannelInfo.IsMultiKey = false
	require.NoError(t, host.Update())

	contribution := &Contribution{
		UserId:         501,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), contributedKey)),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  true,
	}
	require.NoError(t, contribution.Create())

	require.Error(t, ReleaseContribution(contribution, contributedKey, ContributionStatusDead, ContributionReasonUpstreamUnauthorized, false))

	mine, err := GetContributionsByUser(501)
	require.NoError(t, err)
	require.Len(t, mine, 1)
	assert.Equal(t, ContributionStatusActive, mine[0].Status, "a release that could not disable the key stays retryable")
	assert.Empty(t, mine[0].Reason)
	assert.Zero(t, mine[0].ReasonTime)
}

// TestContributionRevokeCompletesWhenTheKeyIsAlreadyGone pins the failure-tolerant
// half of the revoke contract: an administrator may have reclaimed the contributed
// key with the existing cleanup action before its owner withdrew it. The
// key-disabling step is then already moot and must be skipped, while the rest of the
// release - cancelling the reward and freeing the fingerprint - still runs.
func TestContributionRevokeCompletesWhenTheKeyIsAlreadyGone(t *testing.T) {
	openContributionRewardTestDB(t)
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })

	plan := &SubscriptionPlan{
		Id:               9601,
		Title:            "Contribution reward",
		Enabled:          true,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		TotalAmount:      1000,
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	require.NoError(t, DB.Create(plan).Error)

	contributor := &User{
		Id:          601,
		Username:    "contribution-reclaimed",
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
	}
	require.NoError(t, DB.Create(contributor).Error)

	// The host channel no longer carries the contributed key: an administrator
	// reclaimed it, so nothing in the channel resolves the record's fingerprint.
	host := seedContributionHostChannel(t, "host-key-one", 1)

	const reclaimedKey = "sk-contribution-reclaimed"
	contribution := &Contribution{
		UserId:         contributor.Id,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), reclaimedKey)),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  true,
	}
	require.NoError(t, contribution.Create())
	subscription, err := GrantContributionReward(contribution, plan.Id)
	require.NoError(t, err)
	require.NotNil(t, subscription)
	fingerprint := *contribution.KeyFingerprint

	// Nothing in the channel resolves the record's fingerprint any more, which is
	// exactly how the caller knows to hand the release an empty plaintext key.
	pooled := reloadContributionChannel(t, host.Id)
	_, found := ResolveContributedKey(contribution, pooled)
	assert.False(t, found, "a reclaimed key is no longer resolvable from the record's fingerprint")

	require.NoError(t, ReleaseContribution(contribution, "", ContributionStatusRevoked, ContributionReasonUserRevoked, true))

	_, err = GetContributionByFingerprint(fingerprint)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound, "the fingerprint is freed even though the key was already gone")
	var stored Contribution
	require.NoError(t, DB.Where("id = ?", contribution.Id).First(&stored).Error)
	assert.Equal(t, ContributionStatusRevoked, stored.Status)
	assert.Equal(t, ContributionReasonUserRevoked, stored.Reason)
	assert.NotZero(t, stored.ReasonTime)
	assert.Nil(t, stored.KeyFingerprint)

	cancelled, err := GetUserSubscriptionById(subscription.Id)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", cancelled.Status, "the reward is cancelled even though the key was already gone")

	active, err := GetAllActiveContributions()
	require.NoError(t, err)
	assert.Empty(t, active)
}

// TestContributionRevokeIsTerminal is the model-layer seam of the user-initiated
// revoke: one call disables the key in the host channel, cancels the reward and
// frees the fingerprint, and the same key then re-enters the pool through the
// re-enable branch instead of being appended a second time.
func TestContributionRevokeIsTerminal(t *testing.T) {
	openContributionRewardTestDB(t)
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() { common.MemoryCacheEnabled = previousCache })

	plan := &SubscriptionPlan{
		Id:               9701,
		Title:            "Contribution reward",
		Enabled:          true,
		DurationUnit:     SubscriptionDurationMonth,
		DurationValue:    1,
		TotalAmount:      1000,
		QuotaResetPeriod: SubscriptionResetDaily,
	}
	require.NoError(t, DB.Create(plan).Error)

	contributor := &User{
		Id:          701,
		Username:    "contribution-withdrawn",
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
	}
	require.NoError(t, DB.Create(contributor).Error)

	const revokedKey = "sk-contribution-withdrawn"
	host := seedContributionHostChannel(t, "host-key-one", 1)
	require.NoError(t, AppendOrEnableChannelKey(host.Id, revokedKey))

	contribution := &Contribution{
		UserId:         contributor.Id,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), revokedKey)),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
		RewardGranted:  true,
	}
	require.NoError(t, contribution.Create())
	subscription, err := GrantContributionReward(contribution, plan.Id)
	require.NoError(t, err)
	fingerprint := *contribution.KeyFingerprint

	// The plaintext key is recovered through the shared resolver the liveness probe
	// and the revoke endpoint both call, never through the key's index.
	pooled := reloadContributionChannel(t, host.Id)
	resolved, found := ResolveContributedKey(contribution, pooled)
	require.True(t, found)
	assert.Equal(t, revokedKey, resolved)

	require.NoError(t, ReleaseContribution(contribution, resolved, ContributionStatusRevoked, ContributionReasonUserRevoked, true))

	// The record survives with the terminal status, the machine-readable reason and a
	// NULL fingerprint: the shape that frees the key for a later submission.
	var stored Contribution
	require.NoError(t, DB.Where("id = ?", contribution.Id).First(&stored).Error)
	assert.Equal(t, ContributionStatusRevoked, stored.Status)
	assert.Equal(t, ContributionReasonUserRevoked, stored.Reason)
	assert.NotZero(t, stored.ReasonTime)
	assert.Nil(t, stored.KeyFingerprint)
	_, err = GetContributionByFingerprint(fingerprint)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	active, err := GetAllActiveContributions()
	require.NoError(t, err)
	assert.Empty(t, active, "a revoked contribution is no longer probed")

	// The key is still physically present - revoke never deletes it - but it is
	// auto-disabled with the reason recorded against its index, and polling never
	// selects it again.
	channel := reloadContributionChannel(t, host.Id)
	assert.Equal(t, "host-key-one\n"+revokedKey, channel.Key, "revoke never deletes the key")
	assert.Equal(t, common.ChannelStatusAutoDisabled, channel.GetMultiKeyStatus(1))
	assert.Equal(t, ContributionReasonUserRevoked, channel.ChannelInfo.MultiKeyDisabledReason[1])
	for pick := range 3 {
		key, _, pickErr := channel.GetNextEnabledKey()
		require.Nil(t, pickErr, "pick %d", pick)
		assert.Equal(t, "host-key-one", key, "a revoked key must never be selected")
	}

	// The reward is cancelled, not deleted, and its granted quota is not clawed back.
	cancelled, err := GetUserSubscriptionById(subscription.Id)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", cancelled.Status)
	assert.Less(t, cancelled.EndTime, subscription.EndTime)
	assert.EqualValues(t, subscription.AmountTotal, cancelled.AmountTotal)

	// The freed fingerprint can be claimed by anyone.
	resubmitted := &Contribution{
		UserId:         702,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(fingerprint),
		KeyMask:        ContributionKeyMask,
		Status:         ContributionStatusActive,
	}
	require.NoError(t, resubmitted.Create(), "a revoked fingerprint is free again")

	// Resubmitting the same key re-enables it in place: the key list does not grow a
	// second copy of it.
	require.NoError(t, AppendOrEnableChannelKey(host.Id, revokedKey))
	reclaimed := reloadContributionChannel(t, host.Id)
	assert.Equal(t, "host-key-one\n"+revokedKey, reclaimed.Key, "re-enabling must not append a duplicate key")
	assert.Equal(t, 2, reclaimed.ChannelInfo.MultiKeySize)
	assert.Equal(t, common.ChannelStatusEnabled, reclaimed.GetMultiKeyStatus(1))
	_, hasReason := reclaimed.ChannelInfo.MultiKeyDisabledReason[1]
	assert.False(t, hasReason)
	_, hasTime := reclaimed.ChannelInfo.MultiKeyDisabledTime[1]
	assert.False(t, hasTime, "re-enabling clears the disable bookkeeping")
	picked := make([]string, 0, 2)
	for range 2 {
		key, _, pickErr := reclaimed.GetNextEnabledKey()
		require.Nil(t, pickErr)
		picked = append(picked, key)
	}
	assert.ElementsMatch(t, []string{"host-key-one", revokedKey}, picked, "the re-enabled key is selectable again")

	// A repeated revoke is a no-op. The key is re-enabled and the subscription's end
	// time is moved to a sentinel first, so a second release would be visible as a
	// second disable and a second cancellation instead of passing by construction.
	require.NoError(t, DB.Model(&UserSubscription{}).Where("id = ?", subscription.Id).
		Update("end_time", int64(12345)).Error)
	require.NoError(t, ReleaseContribution(contribution, revokedKey, ContributionStatusRevoked, ContributionReasonUserRevoked, true))

	untouched := reloadContributionChannel(t, host.Id)
	assert.Equal(t, common.ChannelStatusEnabled, untouched.GetMultiKeyStatus(1), "a repeated revoke must not disable the key again")
	stillCancelled, err := GetUserSubscriptionById(subscription.Id)
	require.NoError(t, err)
	assert.EqualValues(t, 12345, stillCancelled.EndTime, "a repeated revoke must not cancel the subscription again")

	// A no-op leaves the caller's struct untouched, which is how the endpoint tells
	// "I performed the transition" from "somebody already did" before it audits and
	// notifies. The struct is reset to the stale value a caller would have read.
	contribution.Status = ContributionStatusActive
	require.NoError(t, ReleaseContribution(contribution, revokedKey, ContributionStatusRevoked, ContributionReasonUserRevoked, true))
	assert.Equal(t, ContributionStatusActive, contribution.Status, "a no-op must not report a transition it did not perform")
}

// The account tally counts distinct catalog entries (upstream codes), not records
// and not channel types: two active contributions behind different codes are two
// upstreams, two active contributions behind the same code are one, and a dead or
// revoked contribution never counts at all. The count is per account.
func TestContributionEntryCodeCountDistinguishesDistinctUpstreams(t *testing.T) {
	truncateTables(t)

	seed := func(userId int, entryCode string, status string, fingerprint string) *Contribution {
		record := &Contribution{
			UserId:         userId,
			EntryCode:      entryCode,
			ChannelType:    1,
			HostChannelId:  1,
			KeyFingerprint: common.GetPointer(fingerprint),
			KeyHash:        ContributionKeyHash("sk-" + fingerprint),
			KeyMask:        ContributionKeyMask,
			Status:         status,
			RewardGranted:  status == ContributionStatusActive,
		}
		require.NoError(t, record.Create())
		return record
	}

	// Two active contributions behind different codes - the same provider channel
	// type is fine, they are two distinct upstreams - tally as two.
	seed(901, "ABCDEFGH", ContributionStatusActive, "fp-count-a")
	seed(901, "JKMNPRST", ContributionStatusActive, "fp-count-b")
	count, err := CountActiveContributionEntryCodes(901)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "two different codes are two upstreams")

	// A second active key behind an already-counted code is still one upstream.
	seed(901, "ABCDEFGH", ContributionStatusActive, "fp-count-c")
	count, err = CountActiveContributionEntryCodes(901)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "the same code is one upstream no matter how many keys")

	// Terminal records never count: dead and revoked contributions behind two more
	// codes do not raise the tally.
	seed(901, "CCCCDDDD", ContributionStatusDead, "fp-count-dead")
	seed(901, "EEEEFFFF", ContributionStatusRevoked, "fp-count-revoked")
	count, err = CountActiveContributionEntryCodes(901)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "dead and revoked contributions are not upstreams")

	// The tally is per account.
	seed(902, "ABCDEFGH", ContributionStatusActive, "fp-count-other-user")
	count, err = CountActiveContributionEntryCodes(902)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}
