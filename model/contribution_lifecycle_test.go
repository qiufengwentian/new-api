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

// MaskContributionKey is the only way a contributed key may ever be displayed.
// Upstream keys are live credentials handed over by a third party, so the mask
// must not carry a prefix, a suffix or the length of the plaintext.
func TestContributionKeyMaskNeverRevealsTheKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"typical provider key", "sk-proj-abcdefghijklmnopqrstuvwxyz0123456789"},
		{"short key", "sk-1"},
		{"empty key", ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mask := MaskContributionKey(test.key)
			if test.key == "" {
				assert.Empty(t, mask)
				return
			}
			assert.NotContains(t, mask, test.key)
			for _, fragment := range []string{"sk-", "proj", "abcdefghij", "6789"} {
				assert.NotContains(t, mask, fragment)
			}
		})
	}
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
			ChannelType:    1,
			HostChannelId:  host.Id,
			KeyFingerprint: common.GetPointer(fingerprint),
			KeyMask:        MaskContributionKey(submittedKey),
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
			KeyMask:        MaskContributionKey(submittedKey),
			Status:         ContributionStatusActive,
			RewardGranted:  true,
		}
		require.Error(t, second.Create())
		require.Zero(t, second.Id)

		stored, err := GetContributionByFingerprint(fingerprint)
		require.NoError(t, err)
		assert.Equal(t, first.Id, stored.Id)
		assert.Equal(t, 101, stored.UserId)

		active, err := GetActiveContributionsByUserAndType(101, 1)
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
		KeyMask:        MaskContributionKey(contributedKey),
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
		KeyMask:        MaskContributionKey(contributedKey),
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
		KeyMask:        MaskContributionKey(contributedKey),
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
		KeyMask:        MaskContributionKey(contributedKey),
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

// "One upstream counts once" is a catalog invariant, not a submission rule: an
// administrator must not be able to enable two entries of the same channel type.
func TestContributionCatalogAllowsAtMostOneEnabledEntryPerChannelType(t *testing.T) {
	// The catalog is normally persisted into the options table by the controller
	// package, which owns the option writer. This test binary has no options
	// table, so persistence is captured here and an existing catalog cannot leak
	// in: only the in-memory module and its validation rules are under test.
	contribution_setting.SetOptionWriter(func(string, string) error { return nil })
	t.Cleanup(func() {
		require.NoError(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{}))
	})

	require.NoError(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{
		{Id: 1, ChannelType: 1, Name: "OpenAI", Enabled: true, HostChannelId: 1, PlanId: 1},
	}))

	duplicated := []contribution_setting.ContributionEntry{
		{Id: 1, ChannelType: 1, Name: "OpenAI", Enabled: true, HostChannelId: 1, PlanId: 1},
		{Id: 2, ChannelType: 1, Name: "OpenAI reseller", Enabled: true, HostChannelId: 2, PlanId: 2},
	}
	require.ErrorIs(t, contribution_setting.SaveEntries(duplicated), contribution_setting.ErrChannelTypeTaken)
	assert.Len(t, contribution_setting.AllEntries(), 1, "the rejected catalog must not be stored")

	// A disabled duplicate is allowed: only enabled entries claim a channel type.
	require.NoError(t, contribution_setting.SaveEntries([]contribution_setting.ContributionEntry{
		{Id: 1, ChannelType: 1, Name: "OpenAI", Enabled: true, HostChannelId: 1, PlanId: 1},
		{Id: 3, ChannelType: 1, Name: "OpenAI (disabled)", Enabled: false, HostChannelId: 2, PlanId: 2},
	}))

	entry, found := contribution_setting.EntryByChannelType(1)
	require.True(t, found)
	assert.Equal(t, "OpenAI", entry.Name)
	assert.Equal(t, 2, len(contribution_setting.AllEntries()))
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
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), contributedKey)),
		KeyMask:        MaskContributionKey(contributedKey),
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
	hasActive, err := HasActiveContributionForType(contributor.Id, 1, 0)
	require.NoError(t, err)
	assert.True(t, hasActive, "the channel type already holds an active contribution")

	redundant := &Contribution{
		UserId:         contributor.Id,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(ContributionKeyFingerprint(host.GetBaseURL(), "sk-contribution-redundant")),
		KeyMask:        MaskContributionKey("sk-contribution-redundant"),
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
		KeyMask:        MaskContributionKey(contributedKey),
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
		KeyMask:        MaskContributionKey(reclaimedKey),
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
		KeyMask:        MaskContributionKey(revokedKey),
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
		KeyMask:        MaskContributionKey(revokedKey),
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
