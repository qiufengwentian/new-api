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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/contribution_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newContributionCatalogTestRouter mirrors the production /api/contribution routes
// so admin writes keep flowing through RootAuth's audit fallback.
func newContributionCatalogTestRouter() *gin.Engine {
	router := gin.New()
	router.Use(middleware.RequestId())
	api := router.Group("/api")
	userRoute := api.Group("/contribution", middleware.UserAuth())
	userRoute.GET("/catalog", GetContributionCatalog)
	adminRoute := api.Group("/contribution/admin", middleware.RootAuth())
	adminRoute.GET("/catalog", GetContributionCatalogAdmin)
	adminRoute.POST("/catalog", CreateContributionCatalogEntry)
	adminRoute.PUT("/catalog", UpdateContributionCatalogEntry)
	adminRoute.DELETE("/catalog", DeleteContributionCatalogEntry)
	adminRoute.PUT("/global", UpdateContributionGlobalEnabled)
	return router
}

func setupContributionCatalogTest(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis := common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.User{}, &model.Option{}, &model.Channel{}, &model.SubscriptionPlan{},
		&model.Log{}, &model.AuditLog{}, &model.CasbinRule{}, &model.AuthzRole{},
	))
	previousOptionMap := common.OptionMap
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	require.NoError(t, authz.Init(db))
	require.NoError(t, model.EnsureLegacyAccessTokenRetireAt(time.Now().Unix()))
	model.InitOptionMap()

	resetContributionCatalog(t)
	t.Cleanup(func() {
		resetContributionCatalog(t)
		// Plans are cached in-process for 5 minutes; drop the seeded ids so a later
		// case cannot observe this database's plans.
		for id := 1; id <= 8; id++ {
			model.InvalidateSubscriptionPlanCache(id)
		}
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled = previousRedis
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptionMap
		common.OptionMapRWMutex.Unlock()
	})

	pat := "contribution-catalog-test-token"
	operator := model.User{
		Username:    "contribution-root",
		Role:        common.RoleRootUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AccessToken: &pat,
		AuthVersion: 1,
		AffCode:     "contribution-root",
	}
	require.NoError(t, db.Create(&operator).Error)
	return db, pat
}

// resetContributionCatalog restores the registered option module to its default
// so each case starts from an empty catalog with the switch off.
func resetContributionCatalog(t *testing.T) {
	t.Helper()
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		contribution_setting.CatalogOptionKey: "[]",
		contribution_setting.EnabledOptionKey: "false",
	}))
}

func seedContributionHostChannel(t *testing.T, db *gorm.DB, id int, multiKey bool, key string) {
	t.Helper()
	channel := model.Channel{
		Id:     id,
		Type:   1,
		Name:   fmt.Sprintf("host-%d", id),
		Key:    key,
		Status: common.ChannelStatusEnabled,
	}
	if multiKey {
		keys := strings.Split(key, "\n")
		channel.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeySize: len(keys)}
	}
	require.NoError(t, db.Create(&channel).Error)
}

func seedContributionPlan(t *testing.T, db *gorm.DB, id int) {
	t.Helper()
	plan := model.SubscriptionPlan{Id: id, Title: fmt.Sprintf("Reward %d", id), Enabled: true, DurationUnit: "month", DurationValue: 1}
	require.NoError(t, db.Create(&plan).Error)
	model.InvalidateSubscriptionPlanCache(id)
}

func callContributionCatalog(t *testing.T, router http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func decodeContributionResponse(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	response := map[string]any{}
	require.NoErrorf(t, common.Unmarshal(recorder.Body.Bytes(), &response), "body: %s", recorder.Body.String())
	return response
}

func contributionEntriesOf(t *testing.T, response map[string]any) []any {
	t.Helper()
	if response["data"] == nil {
		return nil
	}
	data, ok := response["data"].(map[string]any)
	require.True(t, ok)
	entries, ok := data["entries"].([]any)
	require.True(t, ok)
	return entries
}

func TestContributionCatalogAdminLifecyclePersistsEntries(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	seedContributionHostChannel(t, db, 1, true, "sk-multi-a\nsk-multi-b")
	seedContributionPlan(t, db, 1)
	router := newContributionCatalogTestRouter()

	create := `{"channel_type":1,"name":"OpenAI","register_url":"https://platform.openai.com/signup","key_placeholder":"sk-...","enabled":true,"host_channel_id":1,"plan_id":1}`
	response := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", create))
	require.Equal(t, true, response["success"], "body: %+v", response)
	data, ok := response["data"].(map[string]any)
	require.True(t, ok)
	entry, ok := data["entry"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 1, entry["id"])

	var option model.Option
	require.NoError(t, db.Where("key = ?", contribution_setting.CatalogOptionKey).First(&option).Error)
	assert.Contains(t, option.Value, "platform.openai.com")
	assert.Contains(t, option.Value, "host_channel_id")

	// A restart replays the stored option through the registered config module.
	resetContributionCatalog(t)
	require.Empty(t, contribution_setting.AllEntries())
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{contribution_setting.CatalogOptionKey: option.Value}))
	persisted := contribution_setting.AllEntries()
	require.Len(t, persisted, 1)
	assert.Equal(t, "OpenAI", persisted[0].Name)
	assert.Equal(t, 1, persisted[0].HostChannelId)

	update := `{"id":1,"channel_type":1,"name":"OpenAI Official","register_url":"https://platform.openai.com/signup","key_placeholder":"sk-...","enabled":true,"host_channel_id":1,"plan_id":1}`
	response = decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPut, "/api/contribution/admin/catalog", update))
	require.Equal(t, true, response["success"], "body: %+v", response)
	require.Len(t, contribution_setting.AllEntries(), 1)
	assert.Equal(t, "OpenAI Official", contribution_setting.AllEntries()[0].Name)

	disable := `{"id":1,"channel_type":1,"name":"OpenAI Official","register_url":"","key_placeholder":"","enabled":false,"host_channel_id":1,"plan_id":1}`
	response = decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPut, "/api/contribution/admin/catalog", disable))
	require.Equal(t, true, response["success"], "body: %+v", response)
	require.Len(t, contribution_setting.AllEntries(), 1)
	assert.False(t, contribution_setting.AllEntries()[0].Enabled)

	response = decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodDelete, "/api/contribution/admin/catalog?id=1", ""))
	require.Equal(t, true, response["success"], "body: %+v", response)
	assert.Empty(t, contribution_setting.AllEntries())
}

func TestContributionCatalogAdminRejectsInvalidEnable(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	seedContributionHostChannel(t, db, 1, false, "sk-single")
	seedContributionHostChannel(t, db, 2, true, "sk-multi-a\nsk-multi-b")
	seedContributionPlan(t, db, 1)
	router := newContributionCatalogTestRouter()

	entry := func(channelType, hostChannelId, planId int, enabled bool) string {
		return fmt.Sprintf(`{"channel_type":%d,"name":"Entry %d","register_url":"https://upstream.example/signup","key_placeholder":"sk-...","enabled":%t,"host_channel_id":%d,"plan_id":%d}`,
			channelType, channelType, enabled, hostChannelId, planId)
	}

	for _, tc := range []struct {
		name string
		body string
		code string
	}{
		{name: "host channel is not multi-key", body: entry(1, 1, 1, true), code: "contribution_channel_not_multi_key"},
		{name: "host channel does not exist", body: entry(1, 999, 1, true), code: "contribution_host_channel_not_found"},
		{name: "plan does not exist", body: entry(1, 2, 999, true), code: "contribution_plan_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", tc.body))
			assert.Equal(t, false, response["success"])
			assert.Equal(t, tc.code, response["code"])
			assert.NotEmpty(t, response["message"])
			assert.Empty(t, contribution_setting.AllEntries(), "a rejected entry must not be stored")
		})
	}

	enabled := entry(3, 2, 1, true)
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", enabled))["success"])
	duplicate := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", entry(3, 2, 1, true)))
	assert.Equal(t, false, duplicate["success"])
	assert.Equal(t, "contribution_channel_type_taken", duplicate["code"])
	require.Len(t, contribution_setting.AllEntries(), 1)

	// A disabled entry skips the host-channel and plan checks.
	disabled := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", entry(3, 999, 999, false)))
	require.Equal(t, true, disabled["success"], "body: %+v", disabled)

	missing := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPut, "/api/contribution/admin/catalog", `{"id":9999,"channel_type":4,"name":"Missing","enabled":false}`))
	assert.Equal(t, false, missing["success"])
	assert.Equal(t, "contribution_entry_not_found", missing["code"])
}

func TestContributionCatalogUserViewFollowsGlobalSwitch(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	seedContributionHostChannel(t, db, 2, true, "sk-multi-a\nsk-multi-b")
	seedContributionPlan(t, db, 1)
	router := newContributionCatalogTestRouter()

	entry := func(channelType int, name string, enabled bool) string {
		return fmt.Sprintf(`{"channel_type":%d,"name":"%s","register_url":"https://upstream-%d.example/signup","key_placeholder":"sk-...","enabled":%t,"host_channel_id":2,"plan_id":1}`,
			channelType, name, channelType, enabled)
	}
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", entry(1, "OpenAI", true)))["success"])
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", entry(3, "Azure", true)))["success"])
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", entry(5, "Hidden", false)))["success"])

	off := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodGet, "/api/contribution/catalog", ""))
	offData := off["data"].(map[string]any)
	assert.Equal(t, false, offData["enabled"])
	assert.Empty(t, contributionEntriesOf(t, off))
	assert.NotEmpty(t, offData["agreement"])

	switched := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPut, "/api/contribution/admin/global", `{"enabled":true}`))
	require.Equal(t, true, switched["success"], "body: %+v", switched)
	assert.Equal(t, true, switched["data"].(map[string]any)["enabled"])

	on := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodGet, "/api/contribution/catalog", ""))
	assert.Equal(t, true, on["data"].(map[string]any)["enabled"])
	entries := contributionEntriesOf(t, on)
	require.Len(t, entries, 2)
	names := []string{}
	for _, rawEntry := range entries {
		item := rawEntry.(map[string]any)
		names = append(names, item["name"].(string))
		assert.NotEmpty(t, item["register_url"], "each listed entry carries its upstream registration link")
	}
	assert.ElementsMatch(t, []string{"OpenAI", "Azure"}, names)

	// The admin view keeps disabled entries so they can be edited again.
	admin := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodGet, "/api/contribution/admin/catalog", ""))
	assert.Len(t, contributionEntriesOf(t, admin), 3)
}

func TestContributionCatalogAdminMutationsAreAudited(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	seedContributionHostChannel(t, db, 2, true, "sk-multi-a\nsk-multi-b")
	seedContributionPlan(t, db, 1)
	router := newContributionCatalogTestRouter()

	create := `{"channel_type":1,"name":"OpenAI","register_url":"https://platform.openai.com/signup","key_placeholder":"sk-...","enabled":true,"host_channel_id":2,"plan_id":1}`
	update := `{"id":1,"channel_type":1,"name":"OpenAI Official","register_url":"https://platform.openai.com/signup","key_placeholder":"sk-...","enabled":true,"host_channel_id":2,"plan_id":1}`

	for _, tc := range []struct {
		method string
		path   string
		body   string
		action string
	}{
		{method: http.MethodPost, path: "/api/contribution/admin/catalog", body: create, action: "contribution.catalog_create"},
		{method: http.MethodPut, path: "/api/contribution/admin/catalog", body: update, action: "contribution.catalog_update"},
		{method: http.MethodPut, path: "/api/contribution/admin/global", body: `{"enabled":true}`, action: "contribution.global_update"},
		{method: http.MethodDelete, path: "/api/contribution/admin/catalog?id=1", action: "contribution.catalog_delete"},
	} {
		recorder := callContributionCatalog(t, router, token, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusOK, recorder.Code, "body: %s", recorder.Body.String())
		requestId := recorder.Header().Get(common.RequestIdKey)
		require.NotEmpty(t, requestId)
		var audits []model.AuditLog
		require.NoError(t, db.Where("request_id = ? AND category = ?", requestId, model.AuditCategoryOperation).Find(&audits).Error)
		require.Lenf(t, audits, 1, "expected one operation audit for %s %s", tc.method, tc.path)
		assert.Equal(t, tc.action, audits[0].Action)
		assert.True(t, audits[0].Success)
		assert.Equal(t, common.RoleRootUser, audits[0].ActorRole)
	}
}
