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

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/contribution_setting"

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
