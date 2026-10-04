package model

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/contribution_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	// The terminal half of the lifecycle - a probe judging the key dead, the
	// release pipeline disabling it in the host channel and cancelling the reward
	// subscription, and the user-initiated revoke that frees the fingerprint - is
	// owned by tickets 06 and 07. They extend this file.
	// TODO(ticket 06/07): dead/revoked terminal assertions. Nothing here fakes
	// them: MarkContributionDead, MarkContributionRevoked and
	// ReleaseContributionFingerprint only become meaningful once the release
	// pipeline that calls them exists.
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
