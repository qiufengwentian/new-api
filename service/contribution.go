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
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/contribution_setting"
)

// ContributionKeyProbeFunc probes one contributed key and reports the HTTP status
// the upstream returned for it; 0 means no HTTP response was observed at all (a
// transport failure, a timeout, or a channel type with no balance query).
//
// It is injected by the controller package, which owns the channel-type balance
// dispatch, because the reverse direction is an import cycle: controller imports
// service. The probe runs against a throwaway single-key copy of the host channel
// (Id = 0), so nothing it observes can reach the production row or the shared
// channel cache.
var ContributionKeyProbeFunc func(key string, hostChannel *model.Channel) (statusCode int, err error)

// ProbeContributionLiveness runs one per-key liveness pass over every pooled
// contribution. For each record it resolves the plaintext key inside the host
// channel and probes exactly that key; only model.IsContributionKeyDead judges
// the outcome, so 403, timeouts, 5xx and a zero balance all leave the record
// active.
//
// A record whose host channel or key cannot be resolved is skipped and stays
// active. Such an orphan is an administrative accident - a changed host channel
// base URL, a key reclaimed by hand - and silently killing it would cancel a
// reward nobody proved dead, so the skip is logged instead.
func ProbeContributionLiveness(ctx context.Context) error {
	if ContributionKeyProbeFunc == nil {
		return errors.New("contribution key probe is not wired")
	}
	contributions, err := model.GetAllActiveContributions()
	if err != nil {
		return err
	}
	for i := range contributions {
		if err := ctx.Err(); err != nil {
			return err
		}
		contribution := &contributions[i]

		hostChannel, err := model.GetChannelById(contribution.HostChannelId, true)
		if err != nil {
			common.SysError(fmt.Sprintf("contribution %d: host channel %d is unreadable, leaving it active: %v", contribution.Id, contribution.HostChannelId, err))
			continue
		}
		key, found := resolveContributedKey(contribution, hostChannel)
		if !found {
			common.SysError(fmt.Sprintf("contribution %d: its key is no longer present in host channel %d, leaving it active", contribution.Id, contribution.HostChannelId))
			continue
		}

		statusCode, probeErr := ContributionKeyProbeFunc(key, hostChannel)
		if probeErr != nil {
			// A probe error carries no verdict on its own: the status decides, and a
			// status of 0 stays alive.
			common.SysLog(fmt.Sprintf("contribution %d: probing channel %d reported %v (status %d)", contribution.Id, hostChannel.Id, probeErr, statusCode))
		}
		if !model.IsContributionKeyDead(statusCode) {
			continue
		}

		if err := model.ReleaseContribution(contribution, key, model.ContributionStatusDead, model.ContributionReasonUpstreamUnauthorized, false); err != nil {
			common.SysError(fmt.Sprintf("failed to release dead contribution %d: %v", contribution.Id, err))
			continue
		}
		// ReleaseContribution is idempotent and leaves the struct untouched when it
		// performed no transition, so a revoke racing this pass is never audited or
		// announced as a death.
		if contribution.Status != model.ContributionStatusDead {
			continue
		}
		recordContributionKillAudit(contribution)
		notifyContributionContributor(contribution)
	}
	return nil
}

// resolveContributedKey recovers the plaintext key a contribution owns inside its
// host channel by recomputing the fingerprint over each stored key. The record is
// addressed by fingerprint and never by index, because removing one key renumbers
// every later key.
func resolveContributedKey(contribution *model.Contribution, hostChannel *model.Channel) (string, bool) {
	if contribution.KeyFingerprint == nil || *contribution.KeyFingerprint == "" {
		return "", false
	}
	baseURL := hostChannel.GetBaseURL()
	for _, storedKey := range hostChannel.GetKeys() {
		storedKey = strings.TrimSpace(storedKey)
		if storedKey == "" {
			continue
		}
		if model.ContributionKeyFingerprint(baseURL, storedKey) == *contribution.KeyFingerprint {
			return storedKey, true
		}
	}
	return "", false
}

// recordContributionKillAudit writes the audit row of one death. The contribution,
// its channel type and its host channel identify it for an administrator; the key
// appears nowhere, so the audit trail is not a place to recover a credential.
func recordContributionKillAudit(contribution *model.Contribution) {
	model.RecordLogWithAdminInfo(contribution.UserId, model.LogTypeManage,
		fmt.Sprintf("Disabled the contributed upstream key of channel type %d after the upstream rejected it", contribution.ChannelType),
		nil, &model.AuditOperation{
			Action: "contribution.kill",
			Params: model.AuditFields{
				"contribution_id": contribution.Id,
				"channel_type":    contribution.ChannelType,
				"host_channel_id": contribution.HostChannelId,
			},
		})
}

// notifyContributionContributor tells the contributor that their key died, through
// the existing per-user notification channel (email, webhook, Bark or Gotify).
// There is no per-user in-site inbox to fall back on.
//
// The notice is best-effort: the key is already disabled and the reward already
// cancelled, so a refused notification - the limit gate, an unreachable webhook -
// is logged and never fails the kill.
func notifyContributionContributor(contribution *model.Contribution) {
	contributor, err := model.GetUserById(contribution.UserId, false)
	if err != nil {
		common.SysError(fmt.Sprintf("failed to load contributor %d to notify about contribution %d: %v", contribution.UserId, contribution.Id, err))
		return
	}
	setting := contributor.GetSetting()
	upstream := fmt.Sprintf("channel type %d", contribution.ChannelType)
	if entry, found := contribution_setting.EntryByChannelType(contribution.ChannelType); found && entry.Name != "" {
		upstream = entry.Name
	}
	notice := dto.NewNotify(dto.NotifyTypeChannelUpdate,
		i18n.Translate(setting.Language, i18n.MsgContributionKeyDeadTitle),
		i18n.Translate(setting.Language, i18n.MsgContributionKeyDeadContent, map[string]any{"Upstream": upstream}),
		nil)
	if err := NotifyUser(contributor.Id, contributor.Email, setting, notice); err != nil {
		common.SysLog(fmt.Sprintf("failed to notify contributor %d about the dead contribution %d: %v", contributor.Id, contribution.Id, err))
	}
}
