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
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/cachex"
	"github.com/QuantumNous/new-api/setting/contribution_setting"
	"github.com/samber/hot"

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
	// before the upstream is bothered; ticket 05 turns it into an instance.
	if _, err := model.GetSubscriptionPlanById(entry.PlanId); err != nil {
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
	activeContributions, err := model.GetActiveContributionsByUserAndType(userId, entry.ChannelType)
	if err == nil && len(activeContributions) > 0 {
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
		"contribution": contributionSummary(contribution, entry.Name),
		"reward":       nil,
		"redundant":    !rewardGranted,
	})
}

// contributionSummary is the user-visible shape of one contribution record. It
// carries the mask only: the plaintext key and the fingerprint never leave the
// database.
func contributionSummary(contribution *model.Contribution, channelTypeName string) gin.H {
	return gin.H{
		"id":                contribution.Id,
		"channel_type":      contribution.ChannelType,
		"channel_type_name": channelTypeName,
		"status":            contribution.Status,
		"reason":            contribution.Reason,
		"reason_time":       contribution.ReasonTime,
		"key_mask":          contribution.KeyMask,
		"subscription_id":   contribution.SubscriptionId,
		"reward_granted":    contribution.RewardGranted,
		"created_time":      contribution.CreatedTime,
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
