package model

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	// ContributionStatusActive is a contribution whose key is pooled and whose
	// reward (if any) is live. Dead and revoked are terminal.
	ContributionStatusActive  = "active"
	ContributionStatusDead    = "dead"
	ContributionStatusRevoked = "revoked"

	// Machine-readable terminal reasons; the frontend maps them to localized
	// text instead of rendering a server-side sentence.
	ContributionReasonUpstreamUnauthorized = "upstream_unauthorized"
	ContributionReasonUserRevoked          = "user_revoked"

	// ContributionRewardSource tags the subscription instance a contribution
	// grants, so the record distinguishes a reward from an administrator's manual
	// grant ("admin") or a purchase ("order").
	ContributionRewardSource = "contribution"

	// contributionKeyMask is what a contributed key looks like once stored. It
	// carries nothing of the plaintext - see MaskContributionKey.
	contributionKeyMask = "************"
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

// ResolveContributedKey recovers the plaintext key a contribution owns inside its
// host channel by recomputing the fingerprint over each stored key. The record is
// addressed by fingerprint and never by index, because removing one key renumbers
// every later key.
//
// It is the one implementation shared by the liveness probe and the user-facing
// revoke endpoint, so both agree on which stored key a record owns. A false result
// means the key is no longer in the channel - an administrator reclaimed it with the
// existing cleanup action, or the host channel base URL changed - and the caller
// must not invent one: the release pipeline accepts an empty key and skips the
// already moot key-disabling step.
func ResolveContributedKey(contribution *Contribution, hostChannel *Channel) (string, bool) {
	if contribution == nil || hostChannel == nil {
		return "", false
	}
	if contribution.KeyFingerprint == nil || *contribution.KeyFingerprint == "" {
		return "", false
	}
	baseURL := hostChannel.GetBaseURL()
	for _, storedKey := range hostChannel.GetKeys() {
		storedKey = strings.TrimSpace(storedKey)
		if storedKey == "" {
			continue
		}
		if ContributionKeyFingerprint(baseURL, storedKey) == *contribution.KeyFingerprint {
			return storedKey, true
		}
	}
	return "", false
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

// MaskContributionKey returns the display-only mask stored with a contribution
// record.
//
// Upstream keys are live credentials handed over by a third party, so the mask
// reveals no character of the key at all - unlike model.MaskTokenKey, which
// keeps a first/last preview. Contribution rows are told apart by id and
// creation time, never by a key fragment that could be replayed from a
// screenshot, a support ticket or a log line. One shared helper keeps every
// surface (submit response, contribution list, admin views) redacting alike.
func MaskContributionKey(key string) string {
	if key == "" {
		return ""
	}
	return contributionKeyMask
}

// Contribution is one accepted contribution of an upstream key: who handed it
// over, which channel type and host channel it feeds, and which reward instance
// (if any) it produced.
//
// It is tracked by key fingerprint and never by the key's index inside the host
// channel: deleting one key renumbers every later key, the fingerprint does not.
type Contribution struct {
	Id            int `json:"id" gorm:"primaryKey"`
	UserId        int `json:"user_id" gorm:"index"`
	ChannelType   int `json:"channel_type" gorm:"index"`
	HostChannelId int `json:"host_channel_id"`
	// KeyFingerprint is nil once the fingerprint is released (revoke). NULLs
	// never collide in a unique index on SQLite, MySQL or PostgreSQL, so a
	// released fingerprint becomes submittable again while the row is retained
	// for audit. json:"-" keeps it out of every response.
	KeyFingerprint *string `json:"-" gorm:"uniqueIndex;size:64"`
	KeyMask        string  `json:"key_mask" gorm:"size:32"`
	SubscriptionId int     `json:"subscription_id"`
	Status         string  `json:"status" gorm:"size:16;index"`
	// RewardGranted is false when the submission was accepted as redundant: the
	// user already holds an active contribution for this channel type.
	RewardGranted bool   `json:"reward_granted"`
	Reason        string `json:"reason" gorm:"size:64"`
	ReasonTime    int64  `json:"reason_time"`
	CreatedTime   int64  `json:"created_time"`
	UpdatedTime   int64  `json:"updated_time"`
}

func (Contribution) TableName() string {
	return "contributed_keys"
}

// Create inserts the record, stamping its timestamps. The unique fingerprint
// index is the authority on "first come, first served": a concurrent second
// submission fails here instead of silently creating a duplicate.
func (c *Contribution) Create() error {
	now := common.GetTimestamp()
	if c.CreatedTime == 0 {
		c.CreatedTime = now
	}
	c.UpdatedTime = now
	return DB.Create(c).Error
}

// GetContributionByFingerprint returns the record that holds fingerprint, in any
// status. gorm.ErrRecordNotFound means the fingerprint is free.
func GetContributionByFingerprint(fingerprint string) (*Contribution, error) {
	if fingerprint == "" {
		return nil, errors.New("contribution fingerprint is empty")
	}
	var contribution Contribution
	if err := DB.Where("key_fingerprint = ?", fingerprint).First(&contribution).Error; err != nil {
		return nil, err
	}
	return &contribution, nil
}

// GetContributionById returns one contribution record in any status.
// gorm.ErrRecordNotFound means the id does not exist. Ownership is the caller's
// check: the user-facing withdrawal answers a foreign record exactly like a missing
// one, so that endpoint cannot reveal whose contribution exists.
func GetContributionById(id int) (*Contribution, error) {
	if id <= 0 {
		return nil, errors.New("invalid contribution id")
	}
	var contribution Contribution
	if err := DB.Where("id = ?", id).First(&contribution).Error; err != nil {
		return nil, err
	}
	return &contribution, nil
}

// LockAndGetActiveContributionByFingerprint locks and reads the active record
// that owns fingerprint, so a caller inside a transaction can transition it
// without racing another writer. gorm.ErrRecordNotFound means no active record
// holds that fingerprint.
func LockAndGetActiveContributionByFingerprint(tx *gorm.DB, fingerprint string) (*Contribution, error) {
	if tx == nil {
		return nil, errors.New("tx is nil")
	}
	if fingerprint == "" {
		return nil, errors.New("contribution fingerprint is empty")
	}
	var contribution Contribution
	if err := lockForUpdate(tx).
		Where("key_fingerprint = ? AND status = ?", fingerprint, ContributionStatusActive).
		First(&contribution).Error; err != nil {
		return nil, err
	}
	return &contribution, nil
}

// GetContributionsByUser returns a user's contributions, newest first.
func GetContributionsByUser(userId int) ([]Contribution, error) {
	contributions := make([]Contribution, 0)
	if err := DB.Where("user_id = ?", userId).Order("id desc").Find(&contributions).Error; err != nil {
		return nil, err
	}
	return contributions, nil
}

// GetActiveContributionsByUserAndType returns the user's live contributions for
// one channel type. "One upstream counts once" means at most one reward is
// granted per channel type, so a submission that finds a row here is redundant.
func GetActiveContributionsByUserAndType(userId, channelType int) ([]Contribution, error) {
	contributions := make([]Contribution, 0)
	if err := DB.Where("user_id = ? AND channel_type = ? AND status = ?", userId, channelType, ContributionStatusActive).
		Find(&contributions).Error; err != nil {
		return nil, err
	}
	return contributions, nil
}

// HasActiveContributionForType reports whether the user already holds a live
// contribution for this channel type. "One upstream counts once" makes such a
// submission redundant: the key still widens the pool, but no second reward is
// granted. excludeId lets a caller ignore the record it is about to judge; pass 0
// when the caller is deciding whether a brand new submission is redundant. Dead
// and revoked contributions are not active, so they never block a fresh grant.
func HasActiveContributionForType(userId, channelType, excludeId int) (bool, error) {
	if userId <= 0 {
		return false, errors.New("invalid user id")
	}
	if channelType <= 0 {
		return false, errors.New("invalid channel type")
	}
	query := DB.Model(&Contribution{}).
		Where("user_id = ? AND channel_type = ? AND status = ?", userId, channelType, ContributionStatusActive)
	if excludeId > 0 {
		query = query.Where("id <> ?", excludeId)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// CountActiveContributionChannelTypes counts the distinct channel types a user
// currently holds a live contribution for: "how many upstreams this account has
// already brought in".
//
// It counts distinct channel types rather than records because one upstream counts
// once: several live keys of the same type are one reward and therefore one
// upstream. Dead and revoked contributions are excluded, so the number falls back
// when a contribution ends.
func CountActiveContributionChannelTypes(userId int) (int, error) {
	if userId <= 0 {
		return 0, errors.New("invalid user id")
	}
	var count int64
	if err := DB.Model(&Contribution{}).
		Where("user_id = ? AND status = ?", userId, ContributionStatusActive).
		Distinct("channel_type").
		Count(&count).Error; err != nil {
		return 0, err
	}
	return int(count), nil
}

// GrantContributionReward issues the accepted contribution's reward: one
// subscription instance built from the catalog entry's plan, tagged with the
// "contribution" source and the plan's own quota/reset rules.
//
// It is idempotent per record - a record that already carries a subscription id
// returns that instance and creates nothing new - and it never cancels, expires
// or deletes anything: withdrawal and death belong to the release pipeline, which
// calls AdminInvalidateUserSubscription.
//
// The purchase cap is deliberately not enforced (enforcePurchaseCap = false):
// MaxPurchasePerUser limits purchases, and a reward that failed on it would leave
// a pooled key without the reward the contributor was promised.
func GrantContributionReward(contribution *Contribution, planId int) (*UserSubscription, error) {
	if contribution == nil || contribution.Id <= 0 {
		return nil, errors.New("invalid contribution")
	}
	if planId <= 0 {
		return nil, errors.New("invalid plan id")
	}
	if contribution.SubscriptionId > 0 {
		return GetUserSubscriptionById(contribution.SubscriptionId)
	}
	subscription, _, err := bindSubscriptionWithSource(contribution.UserId, planId, ContributionRewardSource, false)
	if err != nil {
		return nil, err
	}
	// Point the record at the instance it produced, so the reward can be rendered
	// and later cancelled from the record.
	if err := DB.Model(&Contribution{}).
		Where("id = ?", contribution.Id).
		Updates(map[string]any{
			"subscription_id": subscription.Id,
			"updated_time":    common.GetTimestamp(),
		}).Error; err != nil {
		return nil, err
	}
	contribution.SubscriptionId = subscription.Id
	return subscription, nil
}

// GetAllActiveContributions returns every live contribution: the input of the
// periodic per-key liveness probe.
func GetAllActiveContributions() ([]Contribution, error) {
	contributions := make([]Contribution, 0)
	if err := DB.Where("status = ?", ContributionStatusActive).Order("id").Find(&contributions).Error; err != nil {
		return nil, err
	}
	return contributions, nil
}

// MarkContributionDead moves an active contribution to the dead terminal status.
// It is a compare-and-set on the active status, so a repeated kill is a no-op
// and reports false.
func MarkContributionDead(id int, reason string) (bool, error) {
	return markContributionTerminal(id, ContributionStatusDead, reason)
}

// MarkContributionRevoked moves an active contribution to the revoked terminal
// status. Like MarkContributionDead it is a compare-and-set, so a repeated
// revoke is a no-op. The fingerprint is released separately by
// ReleaseContributionFingerprint, because death keeps it forever.
func MarkContributionRevoked(id int, reason string) (bool, error) {
	return markContributionTerminal(id, ContributionStatusRevoked, reason)
}

func markContributionTerminal(id int, status string, reason string) (bool, error) {
	if id <= 0 {
		return false, errors.New("invalid contribution id")
	}
	now := common.GetTimestamp()
	result := DB.Model(&Contribution{}).
		Where("id = ? AND status = ?", id, ContributionStatusActive).
		Updates(map[string]any{
			"status":       status,
			"reason":       reason,
			"reason_time":  now,
			"updated_time": now,
		})
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// HasActiveContributions reports whether any contributed key is still pooled, so
// the scheduled liveness probe creates no task row - and issues no upstream call
// - while the pool is empty. A read failure reports false for the same reason:
// the next pass retries instead of the probe failing forever on a transient
// database error.
func HasActiveContributions() bool {
	var id int
	err := DB.Model(&Contribution{}).
		Where("status = ?", ContributionStatusActive).
		Limit(1).
		Pluck("id", &id).Error
	return err == nil && id != 0
}

// ReleaseContribution is the single terminal transition of a contribution. It
// auto-disables the contributed key in the host channel with the reason, cancels
// the reward subscription, and - for a revoke only - releases the key fingerprint.
// The record then becomes terminal.
//
// The ordering is deliberate and fail-safe: the side effects run BEFORE the record
// is marked terminal, so a failure in the middle leaves the record active and the
// next liveness pass retries it. The expensive failure is a dead key that keeps
// serving traffic, never a record that is dead on paper while its key still does.
// Every step is idempotent - re-disabling a key, re-cancelling a cancelled
// subscription, clearing an already cleared fingerprint - so a revoke racing the
// probe finishes safely and only the final compare-and-set decides which status the
// record keeps. A record already in a terminal status is a no-op that leaves the
// caller's contribution struct untouched, so the caller can tell "I performed the
// transition" from "somebody else already did" by looking at contribution.Status
// and skip its own audit and notification.
//
// A key that is no longer present in the host channel, or a host channel or
// subscription an administrator already removed, is not an error: the key is
// already unselectable and nothing is left to cancel, so the release logs it and
// continues instead of retrying forever.
//
// Nothing is ever deleted: the record stays for audit, the key text stays in the
// channel until an administrator reclaims it with the existing cleanup action, and
// already-granted subscription quota is not clawed back.
//
// plainKey is the plaintext key the caller resolved from the host channel's key
// list; death and revoke are both addressed by key string because deleting one
// key renumbers the indexes of every later key. An empty plainKey means the key is
// already gone from the channel, so the key-disabling step is skipped as moot.
func ReleaseContribution(contribution *Contribution, plainKey string, status string, reason string, releaseFingerprint bool) error {
	if contribution == nil || contribution.Id <= 0 {
		return errors.New("invalid contribution")
	}

	var current Contribution
	if err := DB.Where("id = ?", contribution.Id).First(&current).Error; err != nil {
		return err
	}
	if current.Status != ContributionStatusActive {
		// Already terminal: the transition happened once and its side effects
		// must not be repeated.
		return nil
	}

	if plainKey == "" {
		// An administrator already reclaimed the key with the existing cleanup
		// action, so there is nothing left to disable: the caller resolved no
		// plaintext key. Skipping is not a failure - the reward still has to be
		// cancelled and the fingerprint still has to be freed.
		common.SysLog(fmt.Sprintf("contribution %d: its key is already gone from channel %d, continuing its release", contribution.Id, contribution.HostChannelId))
	} else if err := SetChannelKeyStatus(contribution.HostChannelId, plainKey, common.ChannelStatusAutoDisabled, reason); err != nil {
		if !errors.Is(err, ErrChannelKeyNotFound) && !errors.Is(err, gorm.ErrRecordNotFound) {
			common.SysError(fmt.Sprintf("failed to auto-disable the key of contribution %d in channel %d: %v", contribution.Id, contribution.HostChannelId, err))
			return err
		}
		common.SysLog(fmt.Sprintf("contribution %d: its host channel or key is already gone, continuing its release", contribution.Id))
	}

	if contribution.SubscriptionId > 0 {
		if _, err := AdminInvalidateUserSubscription(contribution.SubscriptionId); err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				common.SysError(fmt.Sprintf("failed to cancel subscription %d of contribution %d: %v", contribution.SubscriptionId, contribution.Id, err))
				return err
			}
			common.SysLog(fmt.Sprintf("contribution %d: subscription %d is already gone, continuing its release", contribution.Id, contribution.SubscriptionId))
		}
	}

	if releaseFingerprint {
		if err := ReleaseContributionFingerprint(contribution.Id); err != nil {
			common.SysError(fmt.Sprintf("failed to release the fingerprint of contribution %d: %v", contribution.Id, err))
			return err
		}
		contribution.KeyFingerprint = nil
	}

	moved, err := markContributionTerminal(contribution.Id, status, reason)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to move contribution %d to status %s: %v", contribution.Id, status, err))
		return err
	}
	if !moved {
		return nil
	}
	contribution.Status = status
	contribution.Reason = reason
	contribution.ReasonTime = common.GetTimestamp()
	return nil
}

// ReleaseContributionFingerprint sets the fingerprint to NULL, freeing the key
// for a later submission while the record stays for audit. Only revoke releases
// the fingerprint; a dead key must never be submittable again.
func ReleaseContributionFingerprint(id int) error {
	if id <= 0 {
		return errors.New("invalid contribution id")
	}
	return DB.Model(&Contribution{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"key_fingerprint": nil,
			"updated_time":    common.GetTimestamp(),
		}).Error
}
