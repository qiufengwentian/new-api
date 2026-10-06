package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// setupMultiKeyTestChannelDB opens the isolated database shared by the multi-key channel tests.
func setupMultiKeyTestChannelDB(t *testing.T) (*gorm.DB, *model.User) {
	t.Helper()
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousMaster, previousCache, previousRedis, previousSQLite := common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousType, previousLogType)
		common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled, common.SQLitePath = previousMaster, previousCache, previousRedis, previousSQLite
	})
	t.Setenv("SQL_DSN", os.Getenv("TEST_CHANNEL_SQL_DSN"))
	t.Setenv("LOG_SQL_DSN", "")
	common.IsMasterNode, common.MemoryCacheEnabled, common.RedisEnabled = false, false, false
	common.SQLitePath = filepath.Join(t.TempDir(), "channel.db")
	require.NoError(t, model.InitDB())
	database := model.DB
	sqlDB, err := database.DB()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	model.LOG_DB = database
	common.SetLogDatabaseType(common.MainDatabaseType())
	require.NoError(t, database.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.User{}, &model.Log{}, &model.AuditLog{}))
	root := &model.User{Username: "multi-key-review-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled}
	require.NoError(t, database.Create(root).Error)
	t.Cleanup(func() { require.NoError(t, database.Unscoped().Delete(root).Error) })
	versionQuery := "SELECT VERSION()"
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		versionQuery = "SELECT sqlite_version()"
	}
	var version string
	require.NoError(t, database.Raw(versionQuery).Scan(&version).Error)
	t.Logf("database=%s version=%s", common.MainDatabaseType(), version)
	return database, root
}

// newMultiKeyTestChannel inserts a multi-key channel and removes it again after the test.
func newMultiKeyTestChannel(t *testing.T, key string, info model.ChannelInfo) *model.Channel {
	t.Helper()
	channel := &model.Channel{Name: t.Name(), Type: 1, Key: key, Status: common.ChannelStatusEnabled, Models: "test-model", Group: "default", ChannelInfo: info}
	require.NoError(t, channel.Insert())
	t.Cleanup(func() {
		require.NoError(t, channel.Delete())
		model.InitChannelCache()
	})
	return channel
}

// callMultiKeyManage drives the admin multi-key handler and returns the response envelope.
func callMultiKeyManage(t *testing.T, root *model.User, channelId int, action string, keyIndex *int) (bool, string) {
	t.Helper()
	payload, err := common.Marshal(MultiKeyManageRequest{ChannelId: channelId, Action: action, KeyIndex: keyIndex})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", root.Id)
	c.Set("role", common.RoleRootUser)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/channel/multi_key", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	ManageMultiKeys(c)
	var result struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
	return result.Success, result.Message
}

func TestMultiKeyEnableRestoresOnlyExhaustedChannels(t *testing.T) {
	database, root := setupMultiKeyTestChannelDB(t)

	for _, cacheEnabled := range []bool{false, true} {
		for _, action := range []string{"enable_key", "enable_all_keys"} {
			for _, tc := range []struct {
				name           string
				initialStatus  int
				manualOverride string
				wantStatus     int
			}{
				{name: "key exhaustion restores", initialStatus: common.ChannelStatusEnabled, wantStatus: common.ChannelStatusEnabled},
				{name: "manual disable is preserved", initialStatus: common.ChannelStatusManuallyDisabled, wantStatus: common.ChannelStatusManuallyDisabled},
				{name: "manual disable after exhaustion is preserved", initialStatus: common.ChannelStatusEnabled, manualOverride: "status", wantStatus: common.ChannelStatusManuallyDisabled},
				{name: "tag disable after exhaustion is preserved", initialStatus: common.ChannelStatusEnabled, manualOverride: "tag", wantStatus: common.ChannelStatusManuallyDisabled},
			} {
				t.Run(fmt.Sprintf("cache=%t/%s/%s", cacheEnabled, action, tc.name), func(t *testing.T) {
					common.MemoryCacheEnabled = cacheEnabled
					tag := t.Name()
					channel := &model.Channel{Name: t.Name(), Type: 1, Key: "key-one\nkey-two", Status: tc.initialStatus, Models: "test-model", Group: "default", Tag: &tag,
						ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2, MultiKeyStatusList: map[int]int{1: common.ChannelStatusManuallyDisabled}},
					}
					require.NoError(t, channel.Insert())
					t.Cleanup(func() {
						require.NoError(t, channel.Delete())
						model.InitChannelCache()
					})
					for _, operation := range []string{"disable_key", action} {
						if operation == action {
							if tc.manualOverride == "status" {
								model.UpdateChannelStatus(channel.Id, "", common.ChannelStatusManuallyDisabled, "manual operation")
							} else if tc.manualOverride == "tag" {
								require.NoError(t, model.DisableChannelByTag(tag))
							}
						}
						payload, err := common.Marshal(MultiKeyManageRequest{ChannelId: channel.Id, Action: operation, KeyIndex: common.GetPointer(0)})
						require.NoError(t, err)
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Set("id", root.Id)
						c.Set("role", common.RoleRootUser)
						c.Request = httptest.NewRequest(http.MethodPost, "/api/channel/multi_key", bytes.NewReader(payload))
						c.Request.Header.Set("Content-Type", "application/json")
						ManageMultiKeys(c)
						var result struct {
							Success bool `json:"success"`
						}
						require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &result))
						require.True(t, result.Success, recorder.Body.String())
					}
					loaded, err := model.GetChannelById(channel.Id, true)
					require.NoError(t, err)
					assert.Equal(t, tc.wantStatus, loaded.Status)
					assert.NotContains(t, loaded.ChannelInfo.MultiKeyStatusList, 0)
					assert.NotContains(t, loaded.ChannelInfo.MultiKeyDisabledReason, 0)
					assert.NotContains(t, loaded.ChannelInfo.MultiKeyDisabledTime, 0)
					var ability model.Ability
					require.NoError(t, database.Where("channel_id = ?", channel.Id).First(&ability).Error)
					assert.Equal(t, tc.wantStatus == common.ChannelStatusEnabled, ability.Enabled)
				})
			}
		}
	}
}

func TestMultiKeyDeleteKeyKeepsExistingBehavior(t *testing.T) {
	_, root := setupMultiKeyTestChannelDB(t)

	t.Run("removes one key and re-indexes the status maps", func(t *testing.T) {
		channel := newMultiKeyTestChannel(t, "key-one\nkey-two\nkey-three", model.ChannelInfo{
			IsMultiKey:             true,
			MultiKeySize:           3,
			MultiKeyStatusList:     map[int]int{2: common.ChannelStatusManuallyDisabled},
			MultiKeyDisabledReason: map[int]string{2: "manual operation"},
			MultiKeyDisabledTime:   map[int]int64{2: 222},
		})

		success, message := callMultiKeyManage(t, root, channel.Id, "delete_key", common.GetPointer(1))
		require.True(t, success, message)

		loaded, err := model.GetChannelById(channel.Id, true)
		require.NoError(t, err)
		assert.Equal(t, []string{"key-one", "key-three"}, loaded.GetKeys())
		assert.Equal(t, 2, loaded.ChannelInfo.MultiKeySize)
		assert.Equal(t, map[int]int{1: common.ChannelStatusManuallyDisabled}, loaded.ChannelInfo.MultiKeyStatusList)
		assert.Equal(t, map[int]string{1: "manual operation"}, loaded.ChannelInfo.MultiKeyDisabledReason)
		assert.Equal(t, map[int]int64{1: 222}, loaded.ChannelInfo.MultiKeyDisabledTime)
		assert.Equal(t, common.ChannelStatusEnabled, loaded.Status)
	})

	t.Run("keeps reason and time recorded for surviving keys", func(t *testing.T) {
		channel := newMultiKeyTestChannel(t, "key-one\nkey-two\nkey-three", model.ChannelInfo{
			IsMultiKey:             true,
			MultiKeySize:           3,
			MultiKeyDisabledReason: map[int]string{2: "stale reason"},
			MultiKeyDisabledTime:   map[int]int64{2: 333},
		})

		success, message := callMultiKeyManage(t, root, channel.Id, "delete_key", common.GetPointer(0))
		require.True(t, success, message)

		loaded, err := model.GetChannelById(channel.Id, true)
		require.NoError(t, err)
		assert.Equal(t, []string{"key-two", "key-three"}, loaded.GetKeys())
		assert.Empty(t, loaded.ChannelInfo.MultiKeyStatusList)
		assert.Equal(t, map[int]string{1: "stale reason"}, loaded.ChannelInfo.MultiKeyDisabledReason)
		assert.Equal(t, map[int]int64{1: 333}, loaded.ChannelInfo.MultiKeyDisabledTime)
	})

	t.Run("disables the channel when the last enabled key goes away", func(t *testing.T) {
		channel := newMultiKeyTestChannel(t, "key-one\nkey-two", model.ChannelInfo{
			IsMultiKey:             true,
			MultiKeySize:           2,
			MultiKeyStatusList:     map[int]int{1: common.ChannelStatusAutoDisabled},
			MultiKeyDisabledReason: map[int]string{1: "upstream 401"},
			MultiKeyDisabledTime:   map[int]int64{1: 111},
		})

		success, message := callMultiKeyManage(t, root, channel.Id, "delete_key", common.GetPointer(0))
		require.True(t, success, message)

		loaded, err := model.GetChannelById(channel.Id, true)
		require.NoError(t, err)
		assert.Equal(t, []string{"key-two"}, loaded.GetKeys())
		assert.Equal(t, 1, loaded.ChannelInfo.MultiKeySize)
		assert.Equal(t, map[int]int{0: common.ChannelStatusAutoDisabled}, loaded.ChannelInfo.MultiKeyStatusList)
		assert.Equal(t, common.ChannelStatusManuallyDisabled, loaded.Status)
		assert.Equal(t, model.ChannelStatusReasonAllKeysDisabled, loaded.GetOtherInfo()["status_reason"])
	})

	t.Run("refuses to delete the last key", func(t *testing.T) {
		channel := newMultiKeyTestChannel(t, "only-key", model.ChannelInfo{IsMultiKey: true, MultiKeySize: 1})

		success, message := callMultiKeyManage(t, root, channel.Id, "delete_key", common.GetPointer(0))
		assert.False(t, success)
		assert.Equal(t, "不能删除最后一个密钥", message)

		loaded, err := model.GetChannelById(channel.Id, true)
		require.NoError(t, err)
		assert.Equal(t, []string{"only-key"}, loaded.GetKeys())
		assert.Equal(t, 1, loaded.ChannelInfo.MultiKeySize)
	})
}

func TestMultiKeyDeleteDisabledKeysKeepsExistingBehavior(t *testing.T) {
	_, root := setupMultiKeyTestChannelDB(t)

	t.Run("removes only auto-disabled keys and re-indexes the rest", func(t *testing.T) {
		channel := newMultiKeyTestChannel(t, "key-one\nkey-two\nkey-three", model.ChannelInfo{
			IsMultiKey:             true,
			MultiKeySize:           3,
			MultiKeyStatusList:     map[int]int{0: common.ChannelStatusAutoDisabled, 2: common.ChannelStatusManuallyDisabled},
			MultiKeyDisabledReason: map[int]string{0: "upstream 401", 2: "manual operation"},
			MultiKeyDisabledTime:   map[int]int64{0: 111, 2: 222},
		})

		success, message := callMultiKeyManage(t, root, channel.Id, "delete_disabled_keys", nil)
		require.True(t, success, message)
		assert.Equal(t, "已删除 1 个自动禁用的密钥", message)

		loaded, err := model.GetChannelById(channel.Id, true)
		require.NoError(t, err)
		assert.Equal(t, []string{"key-two", "key-three"}, loaded.GetKeys())
		assert.Equal(t, 2, loaded.ChannelInfo.MultiKeySize)
		assert.Equal(t, map[int]int{1: common.ChannelStatusManuallyDisabled}, loaded.ChannelInfo.MultiKeyStatusList)
		assert.Equal(t, map[int]string{1: "manual operation"}, loaded.ChannelInfo.MultiKeyDisabledReason)
		assert.Equal(t, map[int]int64{1: 222}, loaded.ChannelInfo.MultiKeyDisabledTime)
		assert.Equal(t, common.ChannelStatusEnabled, loaded.Status)
	})

	t.Run("drops reason and time that belong to a non-disabled key", func(t *testing.T) {
		channel := newMultiKeyTestChannel(t, "key-one\nkey-two\nkey-three", model.ChannelInfo{
			IsMultiKey:             true,
			MultiKeySize:           3,
			MultiKeyStatusList:     map[int]int{0: common.ChannelStatusAutoDisabled},
			MultiKeyDisabledReason: map[int]string{2: "stale reason"},
			MultiKeyDisabledTime:   map[int]int64{2: 333},
		})

		success, message := callMultiKeyManage(t, root, channel.Id, "delete_disabled_keys", nil)
		require.True(t, success, message)

		loaded, err := model.GetChannelById(channel.Id, true)
		require.NoError(t, err)
		assert.Equal(t, []string{"key-two", "key-three"}, loaded.GetKeys())
		assert.Empty(t, loaded.ChannelInfo.MultiKeyStatusList)
		assert.Empty(t, loaded.ChannelInfo.MultiKeyDisabledReason)
		assert.Empty(t, loaded.ChannelInfo.MultiKeyDisabledTime)
	})

	t.Run("refuses when no key is auto-disabled", func(t *testing.T) {
		channel := newMultiKeyTestChannel(t, "key-one\nkey-two", model.ChannelInfo{
			IsMultiKey:             true,
			MultiKeySize:           2,
			MultiKeyStatusList:     map[int]int{0: common.ChannelStatusManuallyDisabled},
			MultiKeyDisabledReason: map[int]string{0: "manual operation"},
			MultiKeyDisabledTime:   map[int]int64{0: 111},
		})

		success, message := callMultiKeyManage(t, root, channel.Id, "delete_disabled_keys", nil)
		assert.False(t, success)
		assert.Equal(t, "没有需要删除的自动禁用密钥", message)

		loaded, err := model.GetChannelById(channel.Id, true)
		require.NoError(t, err)
		assert.Equal(t, []string{"key-one", "key-two"}, loaded.GetKeys())
		assert.Equal(t, map[int]int{0: common.ChannelStatusManuallyDisabled}, loaded.ChannelInfo.MultiKeyStatusList)
		assert.Equal(t, map[int]string{0: "manual operation"}, loaded.ChannelInfo.MultiKeyDisabledReason)
	})
}

func TestMultiKeyUpdateChannelStatusDisablesOnlyTargetedKey(t *testing.T) {
	setupMultiKeyTestChannelDB(t)

	channel := newMultiKeyTestChannel(t, "key-one\nkey-two\nkey-three", model.ChannelInfo{
		IsMultiKey:             true,
		MultiKeySize:           3,
		MultiKeyStatusList:     map[int]int{0: common.ChannelStatusManuallyDisabled},
		MultiKeyDisabledReason: map[int]string{0: "manual operation"},
		MultiKeyDisabledTime:   map[int]int64{0: 100},
	})

	require.True(t, model.UpdateChannelStatus(channel.Id, "key-three", common.ChannelStatusAutoDisabled, "upstream 401"))

	loaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, map[int]int{0: common.ChannelStatusManuallyDisabled, 2: common.ChannelStatusAutoDisabled}, loaded.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, "manual operation", loaded.ChannelInfo.MultiKeyDisabledReason[0])
	assert.Equal(t, int64(100), loaded.ChannelInfo.MultiKeyDisabledTime[0])
	assert.Equal(t, "upstream 401", loaded.ChannelInfo.MultiKeyDisabledReason[2])
	assert.NotZero(t, loaded.ChannelInfo.MultiKeyDisabledTime[2])
	assert.NotContains(t, loaded.ChannelInfo.MultiKeyStatusList, 1)
	assert.Equal(t, common.ChannelStatusEnabled, loaded.Status)
}

func TestMultiKeyAppendOrEnableChannelKeyAppendsNewKey(t *testing.T) {
	setupMultiKeyTestChannelDB(t)

	channel := newMultiKeyTestChannel(t, "key-one\nkey-two", model.ChannelInfo{
		IsMultiKey:             true,
		MultiKeySize:           2,
		MultiKeyStatusList:     map[int]int{1: common.ChannelStatusAutoDisabled},
		MultiKeyDisabledReason: map[int]string{1: "upstream 401"},
		MultiKeyDisabledTime:   map[int]int64{1: 111},
	})

	require.NoError(t, model.AppendOrEnableChannelKey(channel.Id, "key-three"))

	loaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"key-one", "key-two", "key-three"}, loaded.GetKeys())
	assert.Equal(t, 3, loaded.ChannelInfo.MultiKeySize)
	assert.Equal(t, map[int]int{1: common.ChannelStatusAutoDisabled}, loaded.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, map[int]string{1: "upstream 401"}, loaded.ChannelInfo.MultiKeyDisabledReason)
	assert.Equal(t, map[int]int64{1: 111}, loaded.ChannelInfo.MultiKeyDisabledTime)
	assert.Equal(t, common.ChannelStatusEnabled, loaded.Status)
}

func TestMultiKeyAppendOrEnableChannelKeyReenablesDisabledKey(t *testing.T) {
	setupMultiKeyTestChannelDB(t)

	channel := newMultiKeyTestChannel(t, "key-one\nkey-two\nkey-three", model.ChannelInfo{
		IsMultiKey:             true,
		MultiKeySize:           3,
		MultiKeyStatusList:     map[int]int{1: common.ChannelStatusAutoDisabled},
		MultiKeyDisabledReason: map[int]string{1: "upstream 401"},
		MultiKeyDisabledTime:   map[int]int64{1: 222},
	})

	require.NoError(t, model.AppendOrEnableChannelKey(channel.Id, "key-two"))

	loaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"key-one", "key-two", "key-three"}, loaded.GetKeys())
	assert.Equal(t, 3, loaded.ChannelInfo.MultiKeySize)
	assert.NotContains(t, loaded.ChannelInfo.MultiKeyStatusList, 1)
	assert.NotContains(t, loaded.ChannelInfo.MultiKeyDisabledReason, 1)
	assert.NotContains(t, loaded.ChannelInfo.MultiKeyDisabledTime, 1)
	assert.Equal(t, common.ChannelStatusEnabled, loaded.Status)
}

func TestMultiKeyAppendOrEnableChannelKeyRejectsEnabledKey(t *testing.T) {
	setupMultiKeyTestChannelDB(t)

	channel := newMultiKeyTestChannel(t, "key-one\nkey-two", model.ChannelInfo{
		IsMultiKey:             true,
		MultiKeySize:           2,
		MultiKeyStatusList:     map[int]int{1: common.ChannelStatusManuallyDisabled},
		MultiKeyDisabledReason: map[int]string{1: "manual operation"},
		MultiKeyDisabledTime:   map[int]int64{1: 222},
	})

	err := model.AppendOrEnableChannelKey(channel.Id, "key-one")
	require.ErrorIs(t, err, model.ErrChannelKeyAlreadyEnabled)

	loaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"key-one", "key-two"}, loaded.GetKeys())
	assert.Equal(t, 2, loaded.ChannelInfo.MultiKeySize)
	assert.Equal(t, map[int]int{1: common.ChannelStatusManuallyDisabled}, loaded.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, map[int]string{1: "manual operation"}, loaded.ChannelInfo.MultiKeyDisabledReason)
	assert.Equal(t, map[int]int64{1: 222}, loaded.ChannelInfo.MultiKeyDisabledTime)
}

func TestMultiKeyAppendOrEnableChannelKeyRestoresAllKeysDisabledChannel(t *testing.T) {
	setupMultiKeyTestChannelDB(t)

	channel := newMultiKeyTestChannel(t, "key-one", model.ChannelInfo{
		IsMultiKey:             true,
		MultiKeySize:           1,
		MultiKeyStatusList:     map[int]int{0: common.ChannelStatusAutoDisabled},
		MultiKeyDisabledReason: map[int]string{0: "upstream 401"},
		MultiKeyDisabledTime:   map[int]int64{0: 111},
	})
	channel.Status = common.ChannelStatusAutoDisabled
	info := channel.GetOtherInfo()
	info["status_reason"] = model.ChannelStatusReasonAllKeysDisabled
	channel.SetOtherInfo(info)
	require.NoError(t, channel.Update())

	require.NoError(t, model.AppendOrEnableChannelKey(channel.Id, "key-two"))

	loaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"key-one", "key-two"}, loaded.GetKeys())
	assert.Equal(t, common.ChannelStatusEnabled, loaded.Status)
	assert.Empty(t, loaded.GetOtherInfo()["status_reason"])
}

func TestMultiKeyAppendOrEnableChannelKeyRejectsIneligibleChannel(t *testing.T) {
	setupMultiKeyTestChannelDB(t)

	channel := newMultiKeyTestChannel(t, "key-one", model.ChannelInfo{})

	require.Error(t, model.AppendOrEnableChannelKey(channel.Id, "key-two"))
	require.Error(t, model.AppendOrEnableChannelKey(channel.Id+9999, "key-two"))

	loaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "key-one", loaded.Key)
}

// TestMultiKeySetChannelKeyStatusForcesPerKeyWrite pins the difference between the channel-level
// status writer and the per-key writer. When the channel already carries the target status,
// UpdateChannelStatus short-circuits and drops the per-key write, so a key that was judged dead
// would come back to life the next time the channel is enabled. SetChannelKeyStatus must persist it.
func TestMultiKeySetChannelKeyStatusForcesPerKeyWrite(t *testing.T) {
	setupMultiKeyTestChannelDB(t)

	channel := newMultiKeyTestChannel(t, "key-one\nkey-two", model.ChannelInfo{
		IsMultiKey:             true,
		MultiKeySize:           2,
		MultiKeyStatusList:     map[int]int{1: common.ChannelStatusAutoDisabled},
		MultiKeyDisabledReason: map[int]string{1: "upstream 401"},
		MultiKeyDisabledTime:   map[int]int64{1: 111},
	})
	channel.Status = common.ChannelStatusAutoDisabled
	info := channel.GetOtherInfo()
	info["status_reason"] = model.ChannelStatusReasonAllKeysDisabled
	channel.SetOtherInfo(info)
	require.NoError(t, channel.Update())

	assert.False(t, model.UpdateChannelStatus(channel.Id, "key-one", common.ChannelStatusAutoDisabled, "upstream 401"))
	loaded, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.NotContains(t, loaded.ChannelInfo.MultiKeyStatusList, 0)

	require.NoError(t, model.SetChannelKeyStatus(channel.Id, "key-one", common.ChannelStatusAutoDisabled, "upstream 401"))

	loaded, err = model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, map[int]int{0: common.ChannelStatusAutoDisabled, 1: common.ChannelStatusAutoDisabled}, loaded.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, "upstream 401", loaded.ChannelInfo.MultiKeyDisabledReason[0])
	assert.NotZero(t, loaded.ChannelInfo.MultiKeyDisabledTime[0])
	assert.Equal(t, "upstream 401", loaded.ChannelInfo.MultiKeyDisabledReason[1])
	assert.Equal(t, int64(111), loaded.ChannelInfo.MultiKeyDisabledTime[1])

	_, _, keyErr := loaded.GetNextEnabledKey()
	require.NotNil(t, keyErr)
}

func TestMultiKeySetChannelKeyStatusRejectsUnknownKey(t *testing.T) {
	setupMultiKeyTestChannelDB(t)

	channel := newMultiKeyTestChannel(t, "key-one", model.ChannelInfo{IsMultiKey: true, MultiKeySize: 1})
	require.ErrorIs(t, model.SetChannelKeyStatus(channel.Id, "key-missing", common.ChannelStatusAutoDisabled, "upstream 401"), model.ErrChannelKeyNotFound)
	require.Error(t, model.SetChannelKeyStatus(channel.Id+9999, "key-one", common.ChannelStatusAutoDisabled, "upstream 401"))

	nonMultiKey := newMultiKeyTestChannel(t, "key-one", model.ChannelInfo{})
	require.ErrorIs(t, model.SetChannelKeyStatus(nonMultiKey.Id, "key-one", common.ChannelStatusAutoDisabled, "upstream 401"), model.ErrChannelNotMultiKey)
}

// keyProbeUpstream stands in for the channel's upstream in the key-probe
// pipeline test: it records the credentials and payload the pipeline sends and
// answers the minimal chat request with a valid completion.
type keyProbeUpstream struct {
	server *httptest.Server

	mu             sync.Mutex
	authorizations []string
	bodies         []string
}

func newKeyProbeUpstream(t *testing.T) *keyProbeUpstream {
	t.Helper()
	upstream := &keyProbeUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.authorizations = append(upstream.authorizations, r.Header.Get("Authorization"))
		upstream.bodies = append(upstream.bodies, string(body))
		upstream.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-key-probe","object":"chat.completion","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *keyProbeUpstream) recorded() (authorizations, bodies []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.authorizations...), append([]string(nil), u.bodies...)
}

func (u *keyProbeUpstream) calls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.authorizations)
}

// The channel-test pipeline supports probing the Nth channel key by index:
// pinned to a key, the minimal real request carries exactly that key's
// credentials upstream (disabled keys included), an out-of-range index fails
// before the upstream is touched, and probing leaves no billing trace.
func TestChannelTestPipelineProbesKeyByIndex(t *testing.T) {
	database, root := setupMultiKeyTestChannelDB(t)
	// The probe runs the real relay path, which refuses a model without a
	// configured price; self-use mode is the repo's supported way to run it.
	withSelfUseModeEnabled(t)
	service.InitHttpClient()
	upstream := newKeyProbeUpstream(t)

	channel := &model.Channel{
		Name:    "key-probe-channel",
		Type:    constant.ChannelTypeOpenAI,
		Key:     "sk-key-a\nsk-key-b\nsk-key-c",
		BaseURL: &upstream.server.URL,
		Models:  "gpt-4o-mini",
		Group:   "default",
		Status:  common.ChannelStatusEnabled,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       3,
			MultiKeyStatusList: map[int]int{1: common.ChannelStatusManuallyDisabled},
		},
	}

	for _, tc := range []struct {
		name        string
		keyIndex    *int
		wantAuth    string
		wantPayload string
	}{
		{
			name:        "probes a disabled key by index",
			keyIndex:    lo.ToPtr(1),
			wantAuth:    "Bearer sk-key-b",
			wantPayload: `{"model":"gpt-4o-mini","stream":false,"messages":[{"role":"user","content":"hi"}],"max_tokens":16}`,
		},
		{
			name:        "probes the third key by index",
			keyIndex:    lo.ToPtr(2),
			wantAuth:    "Bearer sk-key-c",
			wantPayload: `{"model":"gpt-4o-mini","stream":false,"messages":[{"role":"user","content":"hi"}],"max_tokens":16}`,
		},
		{
			name:     "an out-of-range index fails before any upstream call",
			keyIndex: lo.ToPtr(5),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callsBefore := upstream.calls()
			result := testChannel(context.Background(), channel, root.Id, "", "", false, tc.keyIndex)
			if tc.wantAuth == "" {
				require.Error(t, result.localErr, "an out-of-range key index must fail the probe")
				require.NotNil(t, result.newAPIError)
				assert.Equal(t, types.ErrorCodeChannelNoAvailableKey, result.newAPIError.GetErrorCode())
				assert.Equal(t, callsBefore, upstream.calls(), "no upstream call may follow an out-of-range probe")
				return
			}
			require.NoError(t, result.localErr, "probe failed: %+v", result.newAPIError)
			require.Nil(t, result.newAPIError)

			authorizations, bodies := upstream.recorded()
			require.Greater(t, len(authorizations), callsBefore, "the pipeline must have called the upstream")
			assert.Equal(t, tc.wantAuth, authorizations[len(authorizations)-1], "only the probed key's credentials may reach the upstream")
			assert.JSONEq(t, tc.wantPayload, bodies[len(bodies)-1], "the minimal real request payload must stay unchanged under a pinned key")
		})
	}

	// Probing never bills the user: the successful probes record their
	// 模型测试 log rows but leave the user's quota untouched.
	var user model.User
	require.NoError(t, database.Select("quota, used_quota").First(&user, "id = ?", root.Id).Error)
	assert.Zero(t, user.Quota)
	assert.Zero(t, user.UsedQuota)

	var probeLogs int64
	require.NoError(t, database.Model(&model.Log{}).Where("user_id = ? AND token_name = ?", root.Id, "模型测试").Count(&probeLogs).Error)
	assert.EqualValues(t, 2, probeLogs, "each successful probe records one 模型测试 consume log row")
}
