package contribution_setting

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

const (
	// SettingName is the registered config module name; every DB option key of this
	// module is "<SettingName>.<json tag>".
	SettingName = "contribution_setting"
	// CatalogOptionKey stores the admin catalog as a JSON array string. List
	// settings are stored as JSON strings rather than native columns, following
	// console_setting.
	CatalogOptionKey = SettingName + ".catalog"
	// EnabledOptionKey stores the global switch of the whole feature.
	EnabledOptionKey = SettingName + ".enabled"

	// AgreementText is the consent statement a contributor must accept before
	// submitting an upstream key. It is served by the backend so the wording a
	// user agreed to is versioned server-side instead of shipping with the client.
	AgreementText = "I understand that this upstream key will be added to a shared channel and used for requests from users other than me, and that only a masked preview of it is kept for display."
)

// Catalog invariants the admin API has to satisfy before anything is stored.
var (
	ErrChannelTypeRequired = errors.New("channel type is required")
	ErrNameRequired        = errors.New("entry name is required")
	ErrEntryIdDuplicated   = errors.New("entry id is duplicated")
	ErrChannelTypeTaken    = errors.New("another enabled entry already uses this channel type")
)

// ContributionEntry is one contributable upstream an admin has configured.
type ContributionEntry struct {
	Id             int    `json:"id"`
	ChannelType    int    `json:"channel_type"`
	Name           string `json:"name"`
	RegisterURL    string `json:"register_url"`
	KeyPlaceholder string `json:"key_placeholder"`
	Enabled        bool   `json:"enabled"`
	HostChannelId  int    `json:"host_channel_id"`
	PlanId         int    `json:"plan_id"`
}

// ContributionSetting is the registered config module behind the options table.
type ContributionSetting struct {
	Enabled bool   `json:"enabled"`
	Catalog string `json:"catalog"`
}

var contributionSetting = ContributionSetting{
	Enabled: false,
	Catalog: "[]",
}

func init() {
	config.GlobalConfig.Register(SettingName, &contributionSetting)
}

// optionWriter persists one option row. controller/contribution.go installs
// model.UpdateOption here because this package cannot import model: model imports
// setting, so the reverse import would be an import cycle.
var optionWriter func(key, value string) error

// SetOptionWriter wires the persistence backend used by SaveEntries and
// SetGlobalEnabled.
func SetOptionWriter(writer func(key, value string) error) {
	optionWriter = writer
}

// GlobalEnabled reports the global switch; when it is off the whole feature is hidden.
func GlobalEnabled() bool {
	return contributionSetting.Enabled
}

// AllEntries returns the admin catalog, enabled and disabled entries alike.
// Unreadable stored JSON degrades to an empty catalog instead of panicking.
func AllEntries() []ContributionEntry {
	return parseCatalog(contributionSetting.Catalog)
}

// EnabledEntries returns the entries users may contribute to. The global switch
// gates the feature, so it yields none while the switch is off.
func EnabledEntries() []ContributionEntry {
	if !GlobalEnabled() {
		return []ContributionEntry{}
	}
	entries := AllEntries()
	enabled := make([]ContributionEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Enabled {
			enabled = append(enabled, entry)
		}
	}
	return enabled
}

// EntryByChannelType resolves the entry that owns a channel type.
func EntryByChannelType(channelType int) (ContributionEntry, bool) {
	for _, entry := range AllEntries() {
		if entry.ChannelType == channelType {
			return entry, true
		}
	}
	return ContributionEntry{}, false
}

// SaveEntries validates and persists the whole catalog.
func SaveEntries(entries []ContributionEntry) error {
	if err := validateEntries(entries); err != nil {
		return err
	}
	encoded, err := common.Marshal(entries)
	if err != nil {
		return err
	}
	if err := persistOption(CatalogOptionKey, string(encoded)); err != nil {
		return err
	}
	contributionSetting.Catalog = string(encoded)
	return nil
}

// SetGlobalEnabled persists the global switch.
func SetGlobalEnabled(enabled bool) error {
	value := strconv.FormatBool(enabled)
	if err := persistOption(EnabledOptionKey, value); err != nil {
		return err
	}
	contributionSetting.Enabled = enabled
	return nil
}

func persistOption(key, value string) error {
	if optionWriter == nil {
		return errors.New("contribution setting option writer is not configured")
	}
	return optionWriter(key, value)
}

func parseCatalog(catalog string) []ContributionEntry {
	entries := []ContributionEntry{}
	if strings.TrimSpace(catalog) == "" {
		return entries
	}
	if err := common.UnmarshalJsonStr(catalog, &entries); err != nil {
		return []ContributionEntry{}
	}
	if entries == nil {
		return []ContributionEntry{}
	}
	return entries
}

func validateEntries(entries []ContributionEntry) error {
	ids := make(map[int]bool, len(entries))
	enabledChannelTypes := make(map[int]bool, len(entries))
	for _, entry := range entries {
		if entry.ChannelType <= 0 {
			return fmt.Errorf("entry %d: %w", entry.Id, ErrChannelTypeRequired)
		}
		if strings.TrimSpace(entry.Name) == "" {
			return fmt.Errorf("entry %d: %w", entry.Id, ErrNameRequired)
		}
		if entry.Id <= 0 || ids[entry.Id] {
			return fmt.Errorf("entry id %d: %w", entry.Id, ErrEntryIdDuplicated)
		}
		ids[entry.Id] = true
		if !entry.Enabled {
			continue
		}
		if enabledChannelTypes[entry.ChannelType] {
			return fmt.Errorf("channel type %d: %w", entry.ChannelType, ErrChannelTypeTaken)
		}
		enabledChannelTypes[entry.ChannelType] = true
	}
	return nil
}
