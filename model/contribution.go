package model

import (
	"errors"
	"net/http"

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
