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
	"github.com/QuantumNous/new-api/relaykit/dto"
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
	contributionCodeChannelTypeTaken  = "contribution_channel_type_taken"

	// Rejection codes of the user-side submission.
	contributionCodeConsentRequired  = "contribution_consent_required"
	contributionCodeGlobalDisabled   = "contribution_global_disabled"
	contributionCodeTypeUnknown      = "contribution_channel_type_unknown"
	contributionCodeRateLimited      = "contribution_rate_limited"
	contributionCodeKeyInvalid       = "contribution_key_invalid"
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
	case errors.Is(err, contribution_setting.ErrChannelTypeTaken):
		return contributionCodeChannelTypeTaken, err.Error()
	case errors.Is(err, contribution_setting.ErrChannelTypeRequired),
		errors.Is(err, contribution_setting.ErrNameRequired),
		errors.Is(err, contribution_setting.ErrEntryIdDuplicated):
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
// host channel must be an existing multi-key channel, the reward plan must exist,
// and no other enabled entry may claim the same channel type.
func validateContributionEntryForEnable(entry contribution_setting.ContributionEntry, siblings []contribution_setting.ContributionEntry) (string, string) {
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
	for _, sibling := range siblings {
		if sibling.Id != entry.Id && sibling.Enabled && sibling.ChannelType == entry.ChannelType {
			return contributionCodeChannelTypeTaken, fmt.Sprintf("channel type %d already has an enabled entry (%s)", entry.ChannelType, sibling.Name)
		}
	}
	return "", ""
}

// GetContributionCatalogAdmin returns the full catalog, disabled entries included.
func GetContributionCatalogAdmin(c *gin.Context) {
	common.ApiSuccess(c, gin.H{
		"enabled": contribution_setting.GlobalEnabled(),
		"entries": contribution_setting.AllEntries(),
	})
}

// CreateContributionCatalogEntry adds one entry and assigns it a stable id.
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
	if entry.Enabled {
		if code, message := validateContributionEntryForEnable(entry, entries); code != "" {
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
	if entry.Enabled {
		if code, message := validateContributionEntryForEnable(entry, entries); code != "" {
			contributionReject(c, code, message)
			return
		}
	}
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
// pick from, plus the consent statement the backend version-controls.
func GetContributionCatalog(c *gin.Context) {
	entries := contribution_setting.EnabledEntries()
	public := make([]gin.H, 0, len(entries))
	for _, entry := range entries {
		public = append(public, gin.H{
			"channel_type":    entry.ChannelType,
			"name":            entry.Name,
			"register_url":    entry.RegisterURL,
			"key_placeholder": entry.KeyPlaceholder,
		})
	}
	common.ApiSuccess(c, gin.H{
		"enabled":   contribution_setting.GlobalEnabled(),
		"entries":   public,
		"agreement": contribution_setting.AgreementText,
	})
}

// ---------------------------------------------------------------------------
// POST /api/contribution/submit
// ---------------------------------------------------------------------------

// contributionSubmitRequest is the body of POST /api/contribution/submit.
type contributionSubmitRequest struct {
	ChannelType int    `json:"channel_type"`
	Key         string `json:"key"`
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
	return strings.ReplaceAll(message, key, model.MaskContributionKey(key))
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
	entry, found := contribution_setting.EntryByChannelType(request.ChannelType)
	if !found || !entry.Enabled {
		contributionReject(c, contributionCodeTypeUnknown, fmt.Sprintf("channel type %d is not open for contribution", request.ChannelType))
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

	// The fingerprint is the identity of a contributed key. A record that still
	// holds it - active or dead - blocks the submission; a revoked record has a
	// NULL fingerprint and does not.
	fingerprint := model.ContributionKeyFingerprint(hostChannel.GetBaseURL(), key)
	if existing, lookupErr := model.GetContributionByFingerprint(fingerprint); lookupErr == nil && existing != nil {
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

	// "One upstream counts once": a user who already holds an active contribution
	// for this channel type gets no second reward, but the key still widens the
	// pool. A dead or revoked predecessor does not block a fresh grant.
	rewardGranted := true
	if hasActive, err := model.HasActiveContributionForType(userId, entry.ChannelType, 0); err != nil {
		// A failed lookup is logged, not guessed at: the submission still grants
		// (the grant itself is idempotent per record), and the failure leaves a
		// trace instead of silently changing the reward decision.
		common.SysError(fmt.Sprintf("failed to read active contributions of user %d for channel type %d: %v", userId, entry.ChannelType, err))
	} else if hasActive {
		rewardGranted = false
	}

	contribution := &model.Contribution{
		UserId:         userId,
		ChannelType:    entry.ChannelType,
		HostChannelId:  hostChannel.Id,
		KeyFingerprint: common.GetPointer(fingerprint),
		KeyMask:        model.MaskContributionKey(key),
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
		model.RecordLogWithAdminInfo(userId, model.LogTypeManage,
			fmt.Sprintf("Granted the contribution reward for channel type %d", entry.ChannelType), auditOperatorInfo(c), &model.AuditOperation{
				Action: "contribution.grant",
				Params: model.AuditFields{
					"contribution_id": contribution.Id,
					"channel_type":    entry.ChannelType,
					"plan_id":         plan.Id,
					"subscription_id": rewardSubscription.Id,
				},
			}, c)
	}

	model.RecordLogWithAdminInfo(userId, model.LogTypeManage,
		fmt.Sprintf("Submitted an upstream key for channel type %d", entry.ChannelType), auditOperatorInfo(c), &model.AuditOperation{
			Action: "contribution.submit",
			Params: model.AuditFields{
				"contribution_id": contribution.Id,
				"channel_type":    entry.ChannelType,
				"host_channel_id": hostChannel.Id,
				"reward_granted":  rewardGranted,
			},
		}, c)

	common.ApiSuccess(c, gin.H{
		"contribution": contributionSummary(contribution, entry.Name, rewardSubscription),
		"reward":       contributionRewardSummary(rewardSubscription, entry.ChannelType, plan.Title),
		"redundant":    !rewardGranted,
	})
}

// contributionRewardSummary is the user-visible shape of the subscription a
// contribution just produced: which channel type earned it, which plan, how much
// of it is left, and until when. It is nil when nothing was granted, so the
// frontend renders no reward instead of an invented one.
func contributionRewardSummary(subscription *model.UserSubscription, channelType int, planTitle string) gin.H {
	if subscription == nil {
		return nil
	}
	return gin.H{
		"channel_type":    channelType,
		"subscription_id": subscription.Id,
		"plan_title":      planTitle,
		"amount_total":    subscription.AmountTotal,
		"amount_used":     subscription.AmountUsed,
		"end_time":        subscription.EndTime,
		"status":          subscription.Status,
	}
}

// contributionSummary is the user-visible shape of one contribution record. It
// carries the mask only: the plaintext key and the fingerprint never leave the
// database. subscription is the reward instance the record points at, or nil when it
// granted none or the instance is gone, and it contributes only its status.
func contributionSummary(contribution *model.Contribution, channelTypeName string, subscription *model.UserSubscription) gin.H {
	subscriptionStatus := ""
	if subscription != nil {
		subscriptionStatus = subscription.Status
	}
	return gin.H{
		"id":                  contribution.Id,
		"channel_type":        contribution.ChannelType,
		"channel_type_name":   channelTypeName,
		"status":              contribution.Status,
		"reason":              contribution.Reason,
		"reason_time":         contribution.ReasonTime,
		"key_mask":            contribution.KeyMask,
		"subscription_id":     contribution.SubscriptionId,
		"subscription_status": subscriptionStatus,
		"reward_granted":      contribution.RewardGranted,
		"created_time":        contribution.CreatedTime,
	}
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
		notifyContributionRevoked(contribution)
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
	channelTypeName := ""
	if entry, found := contribution_setting.EntryByChannelType(contribution.ChannelType); found {
		channelTypeName = entry.Name
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
	common.ApiSuccess(c, gin.H{"contribution": contributionSummary(contribution, channelTypeName, subscription)})
}

// recordContributionRevokeAudit writes the audit row of one withdrawal. The
// contribution, its channel type and its host channel identify it for an
// administrator; the key appears nowhere, so the audit trail is not a place to
// recover a credential.
func recordContributionRevokeAudit(c *gin.Context, contribution *model.Contribution) {
	model.RecordLogWithAdminInfo(contribution.UserId, model.LogTypeManage,
		fmt.Sprintf("Revoked the contributed upstream key of channel type %d", contribution.ChannelType),
		auditOperatorInfo(c), &model.AuditOperation{
			Action: "contribution.revoke",
			Params: model.AuditFields{
				"contribution_id": contribution.Id,
				"channel_type":    contribution.ChannelType,
				"host_channel_id": contribution.HostChannelId,
			},
		}, c)
}

// notifyContributionRevoked tells the contributor their withdrawal went through,
// through the existing per-user notification channel (email, webhook, Bark or
// Gotify) - the repo has no per-user in-site inbox to fall back on.
//
// The notice is best-effort: the key is already out of the pool and the reward
// already cancelled, so a refused notification - the limit gate, an unreachable
// webhook - is logged and never fails the withdrawal.
func notifyContributionRevoked(contribution *model.Contribution) {
	contributor, err := model.GetUserById(contribution.UserId, false)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to load contributor %d to notify about the revoked contribution %d: %v",
			contribution.UserId, contribution.Id, err))
		return
	}
	setting := contributor.GetSetting()
	upstream := fmt.Sprintf("channel type %d", contribution.ChannelType)
	if entry, found := contribution_setting.EntryByChannelType(contribution.ChannelType); found && entry.Name != "" {
		upstream = entry.Name
	}
	notice := dto.NewNotify(dto.NotifyTypeChannelUpdate,
		i18n.Translate(setting.Language, i18n.MsgContributionKeyRevokedTitle),
		i18n.Translate(setting.Language, i18n.MsgContributionKeyRevokedContent, map[string]any{"Upstream": upstream}),
		nil)
	if err := service.NotifyUser(contributor.Id, contributor.Email, setting, notice); err != nil {
		common.SysLog(fmt.Sprintf("failed to notify contributor %d about the revoked contribution %d: %v",
			contributor.Id, contribution.Id, err))
	}
}
