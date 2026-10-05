package contribution_setting

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
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

// entryCodeAlphabet is the human-friendly alphabet for auto-assigned upstream
// codes: the 23 uppercase letters left of the RFC 4648 Base32 alphabet after the
// confusable characters I, O and L are removed (0 and 1 are not Base32 letters
// and therefore already absent). Every symbol reads back without guessing whether
// a character is a digit or a letter.
const entryCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ"

// entryCodeLength is how many symbols a generated upstream code carries. 23^8
// distinct codes is far beyond any catalog an operator can maintain by hand, so
// an accidental collision is not a realistic event; the controller still guards
// against it by rejecting a generated code that already exists.
const entryCodeLength = 8

// Catalog invariants the admin API has to satisfy before anything is stored.
var (
	ErrNameRequired        = errors.New("entry name is required")
	ErrEntryIdDuplicated   = errors.New("entry id is duplicated")
	ErrEntryCodeRequired   = errors.New("upstream code is required")
	ErrEntryCodeInvalid    = errors.New("upstream code is invalid")
	ErrEntryCodeDuplicated = errors.New("another entry already uses this upstream code")
)

// NewEntryCode returns a fresh 8-character upstream code drawn uniformly from
// entryCodeAlphabet. It is a helper because the code is a durable business
// identity - never regenerated, never reused after an entry is deleted - and
// every creation site must generate codes the same way.
func NewEntryCode() (string, error) {
	alphabetLen := big.NewInt(int64(len(entryCodeAlphabet)))
	buffer := make([]byte, entryCodeLength)
	for i := range entryCodeLength {
		n, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", err
		}
		buffer[i] = entryCodeAlphabet[n.Int64()]
	}
	return string(buffer), nil
}

// ContributionEntry is one contributable upstream an admin has configured.
//
// Code is the durable user-facing identity of the upstream: an 8-character
// human-friendly code assigned once at creation, never regenerated and never
// reused after the entry is deleted. The internal Id is an auto-incrementing
// number for admin-page display and sorting only and must not be used anywhere
// downstream of "which upstream is this". ChannelType is still stored because
// the entry is bound to a host channel of a concrete provider type, but it is
// no longer the identifier used for user-facing resolution or reward de-dup.
type ContributionEntry struct {
	Id             int    `json:"id"`
	Code           string `json:"code"`
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

// EntryByCode resolves the ENABLED entry that owns an upstream code. The code is
// the durable user-facing identity of a contributable upstream: a disabled entry
// is an admin-side draft and must not resolve a submission, and a deleted entry
// has no row at all, so its code never resolves anything. Callers that need the
// whole catalog read AllEntries.
func EntryByCode(code string) (ContributionEntry, bool) {
	for _, entry := range AllEntries() {
		if entry.Code == code && entry.Enabled {
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
	codes := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if strings.TrimSpace(entry.Name) == "" {
			return fmt.Errorf("entry %d: %w", entry.Id, ErrNameRequired)
		}
		if entry.Id <= 0 || ids[entry.Id] {
			return fmt.Errorf("entry id %d: %w", entry.Id, ErrEntryIdDuplicated)
		}
		ids[entry.Id] = true
		// The code is the durable user-facing identity, so it must be a well-formed
		// 8-symbol code and unique across the whole catalog, enabled or not: a
		// deleted entry's code is never reused elsewhere, and no two live entries
		// may share one. Channel type is no longer an identity here - two enabled
		// entries may deliberately share one provider channel type.
		if strings.TrimSpace(entry.Code) == "" {
			return fmt.Errorf("entry %d: %w", entry.Id, ErrEntryCodeRequired)
		}
		if len(entry.Code) != entryCodeLength || strings.IndexFunc(entry.Code, func(r rune) bool {
			return !strings.ContainsRune(entryCodeAlphabet, r)
		}) >= 0 {
			return fmt.Errorf("entry %d: %w", entry.Id, ErrEntryCodeInvalid)
		}
		if codes[entry.Code] {
			return fmt.Errorf("entry code %s: %w", entry.Code, ErrEntryCodeDuplicated)
		}
		codes[entry.Code] = true
	}
	return nil
}
