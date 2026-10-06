/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
package controller

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/cachex"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/contribution_setting"
	"github.com/samber/hot"
	"gorm.io/gorm"

	"github.com/gin-gonic/gin"
)

// Machine-readable rejection codes of the contributable catalog admin API. The
// frontend maps known codes to localized copy and falls back to the message.
const (
	contributionCodeInvalidEntry      = "contribution_invalid_entry"
	contributionCodeEntryNotFound     = "contribution_entry_not_found"
	contributionCodeHostChannelAbsent = "contribution_host_channel_not_found"
	contributionCodeChannelNotMulti   = "contribution_channel_not_multi_key"
	contributionCodePlanAbsent        = "contribution_plan_not_found"

	// Rejection codes of the user-side submission.
	contributionCodeConsentRequired  = "contribution_consent_required"
	contributionCodeGlobalDisabled   = "contribution_global_disabled"
	contributionCodeEntryUnknown     = "contribution_entry_unknown"
	contributionCodeRateLimited      = "contribution_rate_limited"
	contributionCodeKeyInvalid       = "contribution_key_invalid"
	contributionCodeKeyDead          = "contribution_key_dead"
	contributionCodeFingerprintTaken = "contribution_fingerprint_taken"
	contributionCodeAlreadyPooled    = "contribution_key_already_pooled"
	contributionCodePoolingFailed    = "contribution_pooling_failed"
	contributionCodeRewardFailed     = "contribution_reward_failed"

	// Rejection codes of the user-side withdrawal.
	contributionCodeRevokeNotFound = "contribution_not_found"
	contributionCodeDeadIsFinal    = "contribution_dead_is_final"
)

// The catalog lives in setting/contribution_setting, which cannot import model
// (model imports setting), so the option write path is injected from here.
func init() {
	contribution_setting.SetOptionWriter(model.UpdateOption)
}

// contributionReject answers with a readable, machine-identifiable failure.
func contributionReject(c *gin.Context, code string, message string) {
	c.JSON(http.StatusOK, gin.H{"success": false, "code": code, "message": message})
}

// contributionCatalogError maps catalog invariant violations to rejection codes.
func contributionCatalogError(err error) (string, string) {
	switch {
	case errors.Is(err, contribution_setting.ErrNameRequired),
		errors.Is(err, contribution_setting.ErrEntryIdDuplicated),
		errors.Is(err, contribution_setting.ErrEntryCodeRequired),
		errors.Is(err, contribution_setting.ErrEntryCodeInvalid),
		errors.Is(err, contribution_setting.ErrEntryCodeDuplicated):
		return contributionCodeInvalidEntry, err.Error()
	default:
		return "", err.Error()
	}
}

// saveContributionEntries persists the catalog or answers with the reason it failed.
func saveContributionEntries(c *gin.Context, entries []contribution_setting.ContributionEntry) bool {
	err := contribution_setting.SaveEntries(entries)
	if err == nil {
		return true
	}
	if code, message := contributionCatalogError(err); code != "" {
		contributionReject(c, code, message)
	} else {
		common.ApiError(c, err)
	}
	return false
}

// validateContributionEntryForEnable enforces the enable-time prerequisites: the
// host channel must be an existing multi-key channel and the reward plan must
// exist. There is no channel-type uniqueness anymore: two entries may
// deliberately back the same provider channel type as two distinct upstreams.
func validateContributionEntryForEnable(entry contribution_setting.ContributionEntry) (string, string) {
	hostChannel, err := model.GetChannelById(entry.HostChannelId, false)
	if err != nil {
		return contributionCodeHostChannelAbsent, fmt.Sprintf("host channel %d does not exist", entry.HostChannelId)
	}
	if !hostChannel.ChannelInfo.IsMultiKey {
		return contributionCodeChannelNotMulti, fmt.Sprintf("host channel %d (%s) is not a multi-key channel", entry.HostChannelId, hostChannel.Name)
	}
	if _, err := model.GetSubscriptionPlanById(entry.PlanId); err != nil {
		return contributionCodePlanAbsent, fmt.Sprintf("subscription plan %d does not exist", entry.PlanId)
	}
	return "", ""
}

// deriveContributionChannelType backfills the entry's provider channel type from
// its host channel when that channel is readable. The admin form no longer picks
// a channel type - the backend owns the binding - so a created or updated entry
// must carry the host channel's type, or every contribution it stamps would
// persist channel type 0 and user-facing names would degrade to "channel type 0".
// A disabled draft with an unreadable host channel keeps the request value (or 0);
// it cannot be enabled until its binding is fixed, and the next edit derives again.
func deriveContributionChannelType(entry contribution_setting.ContributionEntry) contribution_setting.ContributionEntry {
	hostChannel, err := model.GetChannelById(entry.HostChannelId, false)
	if err != nil {
		return entry
	}
	entry.ChannelType = hostChannel.Type
	return entry
}

// GetContributionCatalogAdmin returns the full catalog, disabled entries included.
func GetContributionCatalogAdmin(c *gin.Context) {
	common.ApiSuccess(c, gin.H{
		"enabled": contribution_setting.GlobalEnabled(),
		"entries": contribution_setting.AllEntries(),
	})
}

// CreateContributionCatalogEntry adds one entry: it assigns the next internal id
// (max+1, for admin-page display and sorting only) and an 8-character upstream
// code that is never regenerated and never reused. The request body cannot pick
// the code - the backend owns the code namespace, so a client cannot collide two
// entries on one code.
func CreateContributionCatalogEntry(c *gin.Context) {
	entry := contribution_setting.ContributionEntry{}
	if err := c.ShouldBindJSON(&entry); err != nil {
		contributionReject(c, contributionCodeInvalidEntry, "invalid request body: "+err.Error())
		return
	}
	entries := contribution_setting.AllEntries()
	maxId := 0
	for _, existing := range entries {
		maxId = max(maxId, existing.Id)
	}
	entry.Id = maxId + 1
	// The code namespace is independent of the id namespace: a freshly generated
	// code must not collide with any existing entry, enabled or not, so a deleted
	// entry's code is never silently reused.
	used := make(map[string]bool, len(entries))
	for _, existing := range entries {
		used[existing.Code] = true
	}
	for range 100 {
		generated, err := contribution_setting.NewEntryCode()
		if err != nil {
			contributionReject(c, contributionCodeInvalidEntry, "failed to generate an upstream code: "+err.Error())
			return
		}
		if !used[generated] {
			entry.Code = generated
			break
		}
	}
	if entry.Code == "" {
		contributionReject(c, contributionCodeInvalidEntry, "failed to generate a unique upstream code")
		return
	}
	// The channel type is owned by the host-channel binding, not by the admin
	// form: backfill it whenever the host channel is readable, so an entry never
	// persists channel type 0 and every contribution it later stamps carries the
	// type its host channel really is.
	entry = deriveContributionChannelType(entry)
	if entry.Enabled {
		if code, message := validateContributionEntryForEnable(entry); code != "" {
			contributionReject(c, code, message)
			return
		}
	}
	if !saveContributionEntries(c, append(entries, entry)) {
		return
	}
	common.ApiSuccess(c, gin.H{"entry": entry, "entries": contribution_setting.AllEntries()})
}

// UpdateContributionCatalogEntry replaces one entry, keeping its id.
func UpdateContributionCatalogEntry(c *gin.Context) {
	entry := contribution_setting.ContributionEntry{}
	if err := c.ShouldBindJSON(&entry); err != nil {
		contributionReject(c, contributionCodeInvalidEntry, "invalid request body: "+err.Error())
		return
	}
	entries := contribution_setting.AllEntries()
	index := slices.IndexFunc(entries, func(existing contribution_setting.ContributionEntry) bool {
		return existing.Id == entry.Id
	})
	if index < 0 {
		contributionReject(c, contributionCodeEntryNotFound, fmt.Sprintf("contribution entry %d does not exist", entry.Id))
		return
	}
	// The channel type follows the host-channel binding exactly as in create: an
	// update may not leave a stale or zero channel type behind.
	entry = deriveContributionChannelType(entry)
	if entry.Enabled {
		if code, message := validateContributionEntryForEnable(entry); code != "" {
			contributionReject(c, code, message)
			return
		}
	}
	// The code is permanent: an update may edit the entry's name, binding or
	// switches, but never its upstream identity selection.
	entry.Code = entries[index].Code
	entries[index] = entry
	if !saveContributionEntries(c, entries) {
		return
	}
	common.ApiSuccess(c, gin.H{"entry": entry, "entries": contribution_setting.AllEntries()})
}

// DeleteContributionCatalogEntry removes one entry by id.
func DeleteContributionCatalogEntry(c *gin.Context) {
	id, err := strconv.Atoi(strings.TrimSpace(c.Query("id")))
	if err != nil {
		contributionReject(c, contributionCodeInvalidEntry, "id must be an integer")
		return
	}
	entries := contribution_setting.AllEntries()
	before := len(entries)
	remaining := slices.DeleteFunc(entries, func(existing contribution_setting.ContributionEntry) bool {
		return existing.Id == id
	})
	if len(remaining) == before {
		contributionReject(c, contributionCodeEntryNotFound, fmt.Sprintf("contribution entry %d does not exist", id))
		return
	}
	if !saveContributionEntries(c, remaining) {
		return
	}
	common.ApiSuccess(c, gin.H{"entries": contribution_setting.AllEntries()})
}

// UpdateContributionGlobalEnabled flips the global switch of the feature.
func UpdateContributionGlobalEnabled(c *gin.Context) {
	request := struct {
		Enabled bool `json:"enabled"`
	}{}
	if err := c.ShouldBindJSON(&request); err != nil {
		contributionReject(c, contributionCodeInvalidEntry, "invalid request body: "+err.Error())
		return
	}
	if err := contribution_setting.SetGlobalEnabled(request.Enabled); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"enabled": contribution_setting.GlobalEnabled()})
}

// GetContributionCatalog returns the contributable entries a signed-in user may
// pick from, the consent statement the backend version-controls, and the account's
// contribution summary.
func GetContributionCatalog(c *gin.Context) {
	entries := contribution_setting.EnabledEntries()
	public := make([]gin.H, 0, len(entries))
	for _, entry := range entries {
		public = append(public, gin.H{
			// The user-facing identity of the upstream is its 8-character code, not
			// the internal id nor the host channel's provider type number.
			"entry_id":        entry.Code,
			"name":            entry.Name,
			"register_url":    entry.RegisterURL,
			"key_placeholder": entry.KeyPlaceholder,
		})
	}
	common.ApiSuccess(c, gin.H{
		"enabled":   contribution_setting.GlobalEnabled(),
		"entries":   public,
		"agreement": contribution_setting.AgreementText,
		"summary":   contributionAccountSummary(c.GetInt("id")),
	})
}

// ---------------------------------------------------------------------------
// GET /api/contribution/mine
// ---------------------------------------------------------------------------

// GetMyContributions lists the signed-in user's own contributions, newest first,
// together with the account's contribution summary. Each record is reported with
// its display mask only: the plaintext key and the key fingerprint never leave the
// database.
func GetMyContributions(c *gin.Context) {
	userId := c.GetInt("id")
	contributions, err := model.GetContributionsByUser(userId)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to read the contributions of user %d: %v", userId, err))
		common.ApiError(c, errors.New("the contributions could not be read"))
		return
	}

	// A record points at the reward instance it produced; caching the lookup keeps a
	// long list from re-reading the same subscription. The plan titles are cached the
	// same way, so a page of rewards from one catalog plan does not re-read that plan
	// once per row.
	subscriptions := make(map[int]*model.UserSubscription, len(contributions))
	planTitles := make(map[int]string)
	items := make([]gin.H, 0, len(contributions))
	for i := range contributions {
		contribution := &contributions[i]
		var subscription *model.UserSubscription
		if contribution.SubscriptionId > 0 {
			cached, seen := subscriptions[contribution.SubscriptionId]
			if !seen {
				cached, err = model.GetUserSubscriptionById(contribution.SubscriptionId)
				if err != nil {
					// An administrator may have deleted the instance outright; the
					// record then reports no reward instead of failing the list.
					common.SysLog(fmt.Sprintf("failed to read subscription %d of contribution %d: %v",
						contribution.SubscriptionId, contribution.Id, err))
					cached = nil
				}
				subscriptions[contribution.SubscriptionId] = cached
			}
			subscription = cached
		}
		items = append(items, contributionSummary(c, contribution, subscription, planTitles))
	}

	common.ApiSuccess(c, gin.H{
		"items":   items,
		"summary": contributionAccountSummary(userId),
	})
}

// contributionAccountSummary is the account's contribution tally: how many
// distinct catalog entries (upstream codes) still reward it, rendered as
// "upstreams you have already brought in". A failed count is logged and reported
// as zero rather than blanking the catalog or the list around it; the next read
// retries.
func contributionAccountSummary(userId int) gin.H {
	count, err := model.CountActiveContributionEntryCodes(userId)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to count the active contribution entry codes of user %d: %v", userId, err))
		count = 0
	}
	return gin.H{"entry_code_count": count}
}

// ---------------------------------------------------------------------------
// POST /api/contribution/submit
// ---------------------------------------------------------------------------

// contributionSubmitRequest is the body of POST /api/contribution/submit.
type contributionSubmitRequest struct {
	// EntryId is the 8-character upstream code of the catalog entry the user picked.
	// It resolves to the enabled entry that names the host channel and the reward
	// plan; the provider channel-type number is no longer a submission input.
	EntryId string `json:"entry_id"`
	Key     string `json:"key"`
	// Agreed is the server-side half of the consent control. The checkbox is only
	// the user-facing gate; consent is enforced here so a crafted request cannot
	// skip it.
	Agreed bool `json:"agreed"`
}

// Every rejected validation costs a real HTTP call to a third-party upstream, so
// the threshold is deliberately small and the cooldown is a hard stop that runs
// before any network work.
const (
	// contributionSubmitFailureLimit is how many consecutive failed validations a
	// user may spend before the cooldown starts.
	contributionSubmitFailureLimit = 3
	// contributionSubmitCooldown is how long a cooled-down user is refused for. It
	// is also the lifetime of the failure counter, so an idle user starts clean.
	contributionSubmitCooldown = 10 * time.Minute
)

// contributionThrottleNamespace isolates the throttle keys from every other
// cache user.
const contributionThrottleNamespace = "contribution:submit:throttle"

// contributionSubmitThrottle counts one user's consecutive failed validations.
type contributionSubmitThrottle struct {
	Failures      int   `json:"failures"`
	CooldownUntil int64 `json:"cooldown_until"`
}

var (
	contributionThrottleOnce  sync.Once
	contributionThrottleCache *cachex.HybridCache[contributionSubmitThrottle]
)

// getContributionThrottleCache uses the shared hybrid cache, so the counter is
// consistent across instances when Redis is configured and degrades to an
// in-memory TTL cache otherwise.
func getContributionThrottleCache() *cachex.HybridCache[contributionSubmitThrottle] {
	contributionThrottleOnce.Do(func() {
		contributionThrottleCache = cachex.NewHybridCache[contributionSubmitThrottle](cachex.HybridCacheConfig[contributionSubmitThrottle]{
			Namespace: cachex.Namespace(contributionThrottleNamespace),
			Redis:     common.RDB,
			RedisEnabled: func() bool {
				return common.RedisEnabled && common.RDB != nil
			},
			RedisCodec: cachex.JSONCodec[contributionSubmitThrottle]{},
			Memory: func() *hot.HotCache[string, contributionSubmitThrottle] {
				return hot.NewHotCache[string, contributionSubmitThrottle](hot.LRU, 4096).
					WithTTL(contributionSubmitCooldown).
					WithJanitor().
					Build()
			},
		})
	})
	return contributionThrottleCache
}

func readContributionThrottle(userId int) contributionSubmitThrottle {
	state, found, err := getContributionThrottleCache().Get(strconv.Itoa(userId))
	if err != nil {
		// A cache failure must never lock a contributor out: the upstream
		// validation still runs and the request limiters still apply.
		common.SysError(fmt.Sprintf("failed to read contribution submit throttle for user %d: %v", userId, err))
		return contributionSubmitThrottle{}
	}
	if !found {
		return contributionSubmitThrottle{}
	}
	return state
}

// contributionCooldownRemainingSeconds reports how long a user is still refused
// for; zero means the submission may proceed.
func contributionCooldownRemainingSeconds(userId int) int64 {
	if userId <= 0 {
		return 0
	}
	remaining := readContributionThrottle(userId).CooldownUntil - common.GetTimestamp()
	return max(remaining, 0)
}

// recordContributionValidationFailure adds one failed validation and arms the
// cooldown once the failure limit is reached.
func recordContributionValidationFailure(userId int) {
	if userId <= 0 {
		return
	}
	state := readContributionThrottle(userId)
	state.Failures++
	if state.Failures >= contributionSubmitFailureLimit {
		state.CooldownUntil = common.GetTimestamp() + int64(contributionSubmitCooldown/time.Second)
	}
	if err := getContributionThrottleCache().SetWithTTL(strconv.Itoa(userId), state, contributionSubmitCooldown); err != nil {
		common.SysError(fmt.Sprintf("failed to record contribution validation failure for user %d: %v", userId, err))
	}
}

// clearContributionValidationFailures restarts the counter after a key passed
// validation, so only consecutive failures accumulate.
func clearContributionValidationFailures(userId int) {
	if userId <= 0 {
		return
	}
	if _, err := getContributionThrottleCache().DeleteMany([]string{strconv.Itoa(userId)}); err != nil {
		common.SysError(fmt.Sprintf("failed to clear contribution validation failures for user %d: %v", userId, err))
	}
}

// redactContributionKey removes the plaintext key from a message built out of a
// database or transport error, because such errors quote the statement or URL
// they failed on. The key must never reach a log or a response.
func redactContributionKey(message string, key string) string {
	if key == "" {
		return message
	}
	return strings.ReplaceAll(message, key, model.ContributionKeyMask)
}

// contributionRejectRateLimited answers a cooled-down user with the wait time so
// the client can say exactly how long to wait instead of inviting a retry storm.
func contributionRejectRateLimited(c *gin.Context, retryAfterSeconds int64) {
	c.JSON(http.StatusOK, gin.H{
		"success":             false,
		"code":                contributionCodeRateLimited,
		"message":             fmt.Sprintf("too many failed validations, retry in %d seconds", retryAfterSeconds),
		"retry_after_seconds": retryAfterSeconds,
	})
}

// SubmitContribution accepts one upstream key from a signed-in user: it validates
// the key against the live upstream, appends or re-enables it in the host channel
// of the entry's channel type, and records the contribution by key fingerprint.
//
// The plaintext key never leaves this function: it enters the host channel and
// nothing else - not the response, not the record, not a log.
func SubmitContribution(c *gin.Context) {
	request := contributionSubmitRequest{}
	if err := c.ShouldBindJSON(&request); err != nil {
		contributionReject(c, contributionCodeInvalidEntry, "invalid request body: "+err.Error())
		return
	}
	if !request.Agreed {
		contributionReject(c, contributionCodeConsentRequired, "the contribution statement must be accepted before submitting a key")
		return
	}

	userId := c.GetInt("id")
	if !contribution_setting.GlobalEnabled() {
		contributionReject(c, contributionCodeGlobalDisabled, "the contribution feature is disabled")
		return
	}
	// The enabled entry of this upstream code: a disabled or deleted entry never
	// owns a submission, so an admin who disabled A and enabled B has B resolve
	// here.
	entry, found := contribution_setting.EntryByCode(request.EntryId)
	if !found {
		contributionReject(c, contributionCodeEntryUnknown, fmt.Sprintf("entry %s is not open for contribution", request.EntryId))
		return
	}

	hostChannel, err := model.GetChannelById(entry.HostChannelId, true)
	if err != nil {
		contributionReject(c, contributionCodeHostChannelAbsent, fmt.Sprintf("host channel %d does not exist", entry.HostChannelId))
		return
	}
	if !hostChannel.ChannelInfo.IsMultiKey {
		contributionReject(c, contributionCodeChannelNotMulti, fmt.Sprintf("host channel %d (%s) is not a multi-key channel", hostChannel.Id, hostChannel.Name))
		return
	}
	// The reward plan is resolved now so a broken catalog entry is reported
	// before the upstream is bothered, and so the granted instance can be named in
	// the response without another lookup.
	plan, err := model.GetSubscriptionPlanById(entry.PlanId)
	if err != nil {
		contributionReject(c, contributionCodePlanAbsent, fmt.Sprintf("subscription plan %d does not exist", entry.PlanId))
		return
	}

	if remaining := contributionCooldownRemainingSeconds(userId); remaining > 0 {
		contributionRejectRateLimited(c, remaining)
		return
	}

	key := strings.TrimSpace(request.Key)
	if key == "" {
		contributionReject(c, contributionCodeKeyInvalid, "the upstream key is empty")
		return
	}

	// Death is final for the key itself, not for the record's host channel. The
	// keyed hash of the plaintext key is checked first, so a key an upstream ever
	// rejected cannot be submitted again by anyone, for any channel type - including
	// one whose host channel base URL would produce a different fingerprint. A
	// revoked record never counts (the query only matches dead), so withdrawal
	// still frees the key.
	keyHash := model.ContributionKeyHash(key)
	if hasDead, deadErr := model.HasDeadContributionForKeyHash(keyHash); deadErr != nil {
		// A failed lookup is logged, not guessed at: the submission proceeds and the
		// failure leaves a trace, instead of turning a transient read error into a
		// permanent refusal.
		common.SysError(fmt.Sprintf("failed to read dead contributions for a submitted key: %v", deadErr))
	} else if hasDead {
		contributionReject(c, contributionCodeKeyDead, "this upstream key was judged dead by the upstream and can no longer be contributed")
		return
	}

	// The fingerprint is the per-record identity of a contributed key. A record
	// that still holds it - active or dead - blocks the submission; a revoked record
	// has a NULL fingerprint and does not.
	fingerprint := model.ContributionKeyFingerprint(hostChannel.GetBaseURL(), key)
	if existing, lookupErr := model.GetContributionByFingerprint(fingerprint); lookupErr == nil && existing != nil {
		// Idempotent recovery: the contributor's own record is live and was promised
		// a reward whose grant failed - it is flagged rewarded but points at no
		// subscription. The key is already recorded and pooled, so only the missing
		// grant is retried here and the normal success payload is answered. Any other
		// record - someone else's, a redundant one, or one already rewarded - is a
		// taken fingerprint.
		if existing.UserId == userId && existing.Status == model.ContributionStatusActive &&
			existing.RewardGranted && existing.SubscriptionId == 0 {
			subscription, grantErr := model.GrantContributionReward(existing, plan.Id)
			if grantErr != nil {
				common.SysError(redactContributionKey(fmt.Sprintf(
					"failed to recover the contribution reward of contribution %d (plan %d) for user %d: %v",
					existing.Id, plan.Id, userId, grantErr), key))
				contributionReject(c, contributionCodeRewardFailed,
					"the contribution was recorded but its reward subscription could not be issued; please contact your administrator")
				return
			}
			recordContributionGrantAudit(c, existing, plan.Id, subscription.Id)
			recordContributionSubmitAudit(c, existing, true)
			common.ApiSuccess(c, gin.H{
				"contribution": contributionSummary(c, existing, subscription, map[int]string{}),
				"reward":       contributionRewardSummary(subscription, entry.Code, plan.Title),
				"redundant":    false,
			})
			return
		}
		contributionReject(c, contributionCodeFingerprintTaken, "this upstream key has already been contributed")
		return
	}

	if !validateContributionKey(c, hostChannel, key) {
		recordContributionValidationFailure(userId)
		contributionReject(c, contributionCodeKeyInvalid, "the upstream key failed validation")
		return
	}
	clearContributionValidationFailures(userId)

	if err := model.AppendOrEnableChannelKey(hostChannel.Id, key); err != nil {
		if errors.Is(err, model.ErrChannelKeyAlreadyEnabled) {
			contributionReject(c, contributionCodeAlreadyPooled, "this upstream key is already pooled in the host channel")
			return
		}
		common.SysError(redactContributionKey(fmt.Sprintf("failed to pool contributed key in channel %d: %v", hostChannel.Id, err), key))
		contributionReject(c, contributionCodePoolingFailed, "the upstream key could not be pooled in the host channel")
		return
	}

	// "One upstream counts once": a user who already holds a live contribution for
	// this catalog entry (upstream code) that was actually rewarded gets no second
	// reward, but the key still widens the pool. The query requires the reward to
	// exist (reward_granted and a subscription instance), so an active record whose
	// grant failed never makes a later submission redundant; a dead or revoked
	// predecessor does not block a fresh grant either. Two entries that happen to
	// back the same provider channel type are two upstreams and reward separately.
	rewardGranted := true
	if hasRewarded, err := model.HasRewardedActiveContributionForCode(userId, entry.Code); err != nil {
		// A failed lookup is logged, not guessed at: the submission still grants
		// (the grant itself is idempotent per record), and the failure leaves a
		// trace instead of silently changing the reward decision.
		common.SysError(fmt.Sprintf("failed to read rewarded contributions of user %d for entry %s: %v", userId, entry.Code, err))
	} else if hasRewarded {
		rewardGranted = false
	}

	contribution := &model.Contribution{
		UserId:         userId,
		EntryCode:      entry.Code,
		ChannelType:    entry.ChannelType,
		HostChannelId:  hostChannel.Id,
		KeyFingerprint: common.GetPointer(fingerprint),
		KeyHash:        keyHash,
		KeyMask:        model.ContributionKeyMask,
		Agreement:      contribution_setting.AgreementText,
		Status:         model.ContributionStatusActive,
		RewardGranted:  rewardGranted,
	}
	if err := contribution.Create(); err != nil {
		// The unique fingerprint index decides the race: if a row appeared between
		// the check above and this insert, the other submission won.
		if _, lookupErr := model.GetContributionByFingerprint(fingerprint); lookupErr == nil {
			contributionReject(c, contributionCodeFingerprintTaken, "this upstream key has already been contributed")
			return
		}
		common.SysError(redactContributionKey(fmt.Sprintf("failed to record contribution for user %d: %v", userId, err), key))
		common.ApiError(c, errors.New("the contribution could not be recorded"))
		return
	}

	// The reward is the second half of an accepted contribution. It is granted
	// from the catalog entry's plan with the existing subscription machinery, and a
	// failure is reported loudly: a pooled key without its reward is a broken
	// promise, not a state to hide behind a successful response.
	var rewardSubscription *model.UserSubscription
	if rewardGranted {
		rewardSubscription, err = model.GrantContributionReward(contribution, plan.Id)
		if err != nil {
			common.SysError(redactContributionKey(fmt.Sprintf(
				"failed to grant the contribution reward of contribution %d (plan %d) to user %d: %v",
				contribution.Id, plan.Id, userId, err), key))
			contributionReject(c, contributionCodeRewardFailed,
				"the contribution was recorded but its reward subscription could not be issued; please contact your administrator")
			return
		}
		// The grant is audited with the plan and instance it produced - never the
		// key, which this whole request keeps out of every durable surface.
		recordContributionGrantAudit(c, contribution, plan.Id, rewardSubscription.Id)
	}

	recordContributionSubmitAudit(c, contribution, rewardGranted)

	common.ApiSuccess(c, gin.H{
		"contribution": contributionSummary(c, contribution, rewardSubscription, map[int]string{}),
		"reward":       contributionRewardSummary(rewardSubscription, entry.Code, plan.Title),
		"redundant":    !rewardGranted,
	})
}

// contributionRewardSummary is the user-visible shape of the subscription a
// contribution just produced: which channel type earned it, which plan, how much
// of it is left, and until when. It is nil when nothing was granted, so the
// frontend renders no reward instead of an invented one.
func contributionRewardSummary(subscription *model.UserSubscription, entryCode string, planTitle string) gin.H {
	summary := contributionSubscriptionSummary(subscription, planTitle)
	if summary == nil {
		return nil
	}
	summary["entry_code"] = entryCode
	summary["subscription_id"] = subscription.Id
	return summary
}

// contributionSubscriptionSummary is the nested shape of the reward instance a
// contribution points at: which plan it came from, how much of it is left, until
// when, and whether it is still live. It is nil when the record granted no reward
// or the instance is gone, and it never carries the instance's id - the record
// around it already does.
func contributionSubscriptionSummary(subscription *model.UserSubscription, planTitle string) gin.H {
	if subscription == nil {
		return nil
	}
	return gin.H{
		"plan_title":   planTitle,
		"amount_total": subscription.AmountTotal,
		"amount_used":  subscription.AmountUsed,
		"end_time":     subscription.EndTime,
		"status":       subscription.Status,
	}
}

// contributionSummary is the one user-visible shape of a contribution record,
// shared by the submit response, the withdrawal response and the contribution
// list. It carries the mask only: the plaintext key and the fingerprint never
// leave the database. The channel type is named by the shared service resolver,
// and subscription is the reward instance the record points at, or nil when it
// granted none or the instance is gone.
//
// planTitles is the request-scoped plan-title cache: the list endpoint shares one
// map across its rows, so a page of contributions that were all rewarded from the
// same catalog plan reads that plan once instead of once per row. A single-record
// caller passes a fresh map.
func contributionSummary(c *gin.Context, contribution *model.Contribution, subscription *model.UserSubscription, planTitles map[int]string) gin.H {
	planTitle := ""
	subscriptionStatus := ""
	if subscription != nil {
		subscriptionStatus = subscription.Status
		if subscription.PlanId > 0 {
			title, cached := planTitles[subscription.PlanId]
			if !cached {
				if plan, err := model.GetSubscriptionPlanById(subscription.PlanId); err == nil {
					title = plan.Title
				} else {
					common.SysLog(fmt.Sprintf("failed to read plan %d of subscription %d: %v", subscription.PlanId, subscription.Id, err))
				}
				planTitles[subscription.PlanId] = title
			}
			planTitle = title
		}
	}
	return gin.H{
		"id":                  contribution.Id,
		"entry_code":          contribution.EntryCode,
		"channel_type":        contribution.ChannelType,
		"channel_type_name":   service.ContributionUpstreamName(contribution, i18n.GetLangFromContext(c)),
		"status":              contribution.Status,
		"reason":              contribution.Reason,
		"reason_time":         contribution.ReasonTime,
		"key_mask":            contribution.KeyMask,
		"subscription_id":     contribution.SubscriptionId,
		"subscription_status": subscriptionStatus,
		"subscription":        contributionSubscriptionSummary(subscription, planTitle),
		"reward_granted":      contribution.RewardGranted,
		"created_time":        contribution.CreatedTime,
	}
}

// recordContributionGrantAudit writes the audit row of one reward grant. The
// contribution, its channel type, the plan and the instance identify it for an
// administrator; the key appears nowhere, so the audit trail is never a place to
// recover a credential.
func recordContributionGrantAudit(c *gin.Context, contribution *model.Contribution, planId, subscriptionId int) {
	model.RecordLogWithAdminInfo(contribution.UserId, model.LogTypeManage,
		fmt.Sprintf("Granted the contribution reward for entry %s", contribution.EntryCode), auditOperatorInfo(c), &model.AuditOperation{
			Action: "contribution.grant",
			Params: model.AuditFields{
				"contribution_id": contribution.Id,
				"entry_code":      contribution.EntryCode,
				"plan_id":         planId,
				"subscription_id": subscriptionId,
			},
		}, c)
}

// recordContributionSubmitAudit writes the audit row of one accepted submission,
// including the consent statement the contributor accepted at that moment.
func recordContributionSubmitAudit(c *gin.Context, contribution *model.Contribution, rewardGranted bool) {
	model.RecordLogWithAdminInfo(contribution.UserId, model.LogTypeManage,
		fmt.Sprintf("Submitted an upstream key for entry %s", contribution.EntryCode), auditOperatorInfo(c), &model.AuditOperation{
			Action: "contribution.submit",
			Params: model.AuditFields{
				"contribution_id": contribution.Id,
				"entry_code":      contribution.EntryCode,
				"host_channel_id": contribution.HostChannelId,
				"reward_granted":  rewardGranted,
				"agreement":       contribution.Agreement,
			},
		}, c)
}

// validateContributionKey is the first-time liveness check of a submitted key:
// the channel-type balance probe, then one minimal real inference. Both run
// against a throwaway single-key copy of the host channel (Id = 0), so nothing
// the upstream reports can reach the production row or the shared channel cache.
//
// The probe only rejects on a status the upstream actually returned. A transport
// failure, a timeout, or a channel type with no balance query at all reports no
// HTTP status and says nothing about the key - the inference below decides, and
// it fails for those cases too.
func validateContributionKey(c *gin.Context, hostChannel *model.Channel, key string) bool {
	probe := probeContributionKey(key, hostChannel)
	if probe.Err != nil && probe.StatusCode != 0 {
		common.SysLog(fmt.Sprintf("contribution key rejected by the balance probe of channel %d with status %d", hostChannel.Id, probe.StatusCode))
		return false
	}

	testUserID, err := resolveChannelTestUserID(c)
	if err != nil {
		common.SysError(fmt.Sprintf("contribution key validation could not resolve a test user: %v", err))
		return false
	}
	result := testChannel(c.Request.Context(), singleKeyProbeChannel(hostChannel, key), testUserID, "", string(constant.EndpointTypeOpenAI), false)
	if result.localErr != nil || result.newAPIError != nil {
		// The upstream body never carries the key, but the key is redacted anyway:
		// an error text is not a place to discover a credential in.
		common.SysLog(redactContributionKey(fmt.Sprintf("contribution key rejected by the inference validation of channel %d: %v / %v",
			hostChannel.Id, result.localErr, result.newAPIError), key))
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// POST /api/contribution/revoke
// ---------------------------------------------------------------------------

// contributionRevokeRequest is the body of POST /api/contribution/revoke.
type contributionRevokeRequest struct {
	Id int `json:"id"`
}

// RevokeContribution withdraws one of the signed-in user's own contributions: the
// key is auto-disabled in its host channel, the reward subscription is cancelled and
// the key fingerprint is released, so the same key can be contributed again later.
// Nothing is deleted - the record stays for audit.
//
// Ownership is enforced as part of the lookup: a record that belongs to somebody else
// is answered exactly like an id that does not exist, so the endpoint cannot be used
// to discover that another user contributed a key.
//
// Only a live record can be withdrawn, with one deliberate exception. A dead record
// is refused, because the upstream killed that key and reporting a successful
// withdrawal would misrepresent its state. A record the user has already withdrawn is
// already in the state this request asks for, so the endpoint answers with the same
// revoked summary instead of an error: a double click or a retry after a timeout must
// not turn a successful withdrawal into a failure. Neither case performs a
// transition, so neither is audited or announced again.
func RevokeContribution(c *gin.Context) {
	request := contributionRevokeRequest{}
	if err := c.ShouldBindJSON(&request); err != nil {
		contributionReject(c, contributionCodeInvalidEntry, "invalid request body: "+err.Error())
		return
	}
	userId := c.GetInt("id")

	contribution, err := model.GetContributionById(request.Id)
	if err != nil || contribution.UserId != userId {
		// A lookup failure and a foreign record are answered identically so the
		// endpoint never reveals whether somebody else's contribution exists, but an
		// unexpected read failure still leaves a trace for the operator.
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			common.SysError(fmt.Sprintf("failed to read contribution %d for user %d: %v", request.Id, userId, err))
		}
		contributionReject(c, contributionCodeRevokeNotFound, "the contribution does not exist")
		return
	}
	if contribution.Status == model.ContributionStatusRevoked {
		respondWithRevokedContribution(c, contribution.Id)
		return
	}
	if contribution.Status != model.ContributionStatusActive {
		contributionReject(c, contributionCodeDeadIsFinal, "this contribution is already over and cannot be revoked")
		return
	}

	// The plaintext key is resolved through the same helper the liveness probe uses.
	// An empty result means an administrator already reclaimed the key, and the release
	// then skips the already moot key-disabling step instead of failing.
	plainKey := ""
	if hostChannel, hostErr := model.GetChannelById(contribution.HostChannelId, true); hostErr != nil {
		common.SysLog(fmt.Sprintf("contribution %d: its host channel %d is unreadable, revoking without a plaintext key: %v",
			contribution.Id, contribution.HostChannelId, hostErr))
	} else if resolved, found := model.ResolveContributedKey(contribution, hostChannel); found {
		plainKey = resolved
	}

	if err := model.ReleaseContribution(contribution, plainKey, model.ContributionStatusRevoked, model.ContributionReasonUserRevoked, true); err != nil {
		common.SysError(fmt.Sprintf("failed to revoke contribution %d: %v", contribution.Id, err))
		common.ApiError(c, errors.New("the contribution could not be revoked"))
		return
	}
	// ReleaseContribution is idempotent and leaves the struct untouched when it
	// performed no transition, so a withdrawal racing the liveness probe is never
	// audited or announced as this request's doing.
	if contribution.Status == model.ContributionStatusRevoked {
		recordContributionRevokeAudit(c, contribution)
		service.NotifyContributionContributor(contribution, i18n.MsgContributionKeyRevokedTitle, i18n.MsgContributionKeyRevokedContent)
	}
	respondWithRevokedContribution(c, contribution.Id)
}

// respondWithRevokedContribution answers with the record's refreshed summary, so the
// client renders what the database holds rather than what this request assumed, and
// so a repeated withdrawal gets the same answer as the first one.
func respondWithRevokedContribution(c *gin.Context, contributionId int) {
	contribution, err := model.GetContributionById(contributionId)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to read contribution %d back after its revoke: %v", contributionId, err))
		common.ApiError(c, errors.New("the contribution could not be read back"))
		return
	}
	var subscription *model.UserSubscription
	if contribution.SubscriptionId > 0 {
		subscription, err = model.GetUserSubscriptionById(contribution.SubscriptionId)
		if err != nil {
			// An administrator may have deleted the instance outright; the summary then
			// reports no subscription status instead of failing the response.
			common.SysLog(fmt.Sprintf("failed to read subscription %d of contribution %d: %v",
				contribution.SubscriptionId, contribution.Id, err))
			subscription = nil
		}
	}
	common.ApiSuccess(c, gin.H{"contribution": contributionSummary(c, contribution, subscription, map[int]string{})})
}

// recordContributionRevokeAudit writes the audit row of one withdrawal. The
// contribution, its channel type and its host channel identify it for an
// administrator; the key appears nowhere, so the audit trail is not a place to
// recover a credential.
func recordContributionRevokeAudit(c *gin.Context, contribution *model.Contribution) {
	model.RecordLogWithAdminInfo(contribution.UserId, model.LogTypeManage,
		fmt.Sprintf("Revoked the contributed upstream key of entry %s", contribution.EntryCode),
		auditOperatorInfo(c), &model.AuditOperation{
			Action: "contribution.revoke",
			Params: model.AuditFields{
				"contribution_id": contribution.Id,
				"entry_code":      contribution.EntryCode,
				"host_channel_id": contribution.HostChannelId,
			},
		}, c)
}
