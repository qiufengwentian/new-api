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
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
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
	userRoute.GET("/mine", GetMyContributions)
	userRoute.POST("/submit", SubmitContribution)
	userRoute.POST("/revoke", RevokeContribution)
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
	previousSessionSecret := common.SessionSecret
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.User{}, &model.Option{}, &model.Channel{}, &model.SubscriptionPlan{},
		&model.UserSubscription{}, &model.Contribution{}, &model.Ability{},
		&model.Log{}, &model.AuditLog{}, &model.CasbinRule{}, &model.AuthzRole{},
		&model.UserSession{},
	))
	previousOptionMap := common.OptionMap
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	// The handlers select reserved-word columns (users.group, tokens.key) whose
	// quoting depends on the active dialect; InitLogDB with no separate log DSN
	// installs those column names for this database.
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	common.RedisEnabled = false
	// Submitting or withdrawing a contribution is a browser-session action (it
	// hands a third-party credential into a shared channel), so the cases
	// authenticate exactly like the product: with a dashboard login session.
	common.SessionSecret = "contribution-catalog-test-session-secret"
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
		common.SessionSecret = previousSessionSecret
		common.OptionMapRWMutex.Lock()
		common.OptionMap = previousOptionMap
		common.OptionMapRWMutex.Unlock()
	})

	operator := model.User{
		Username:    "contribution-root",
		Role:        common.RoleRootUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
		AffCode:     "contribution-root",
	}
	require.NoError(t, db.Create(&operator).Error)
	bundle, err := service.CreateLoginSession(operator.Id, "password", "127.0.0.1", "contribution-catalog-test")
	require.NoError(t, err)
	// The submit cooldown lives in a process-wide cache that outlives this
	// database, so a case must not inherit another case's failed validations.
	t.Cleanup(func() { clearContributionValidationFailures(operator.Id) })
	return db, bundle.AccessToken
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
	plan := model.SubscriptionPlan{Id: id, Title: fmt.Sprintf("Reward %d", id), Enabled: true, DurationUnit: "month", DurationValue: 1, TotalAmount: 5000}
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

// The internal id and the upstream code are deliberately decoupled: after an entry
// is deleted, the next entry reuses the freed id range (max+1 over the remaining
// catalog) but the code namespace never reuses a deleted entry's code.
func TestContributionCatalogDeleteReusesIdButNeverTheCode(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	seedContributionHostChannel(t, db, 2, true, "sk-multi-a\nsk-multi-b")
	seedContributionPlan(t, db, 1)
	router := newContributionCatalogTestRouter()

	entry := func(channelType int, name string) string {
		return fmt.Sprintf(`{"channel_type":%d,"name":"%s","register_url":"https://upstream.example/signup","key_placeholder":"sk-...","enabled":true,"host_channel_id":2,"plan_id":1}`, channelType, name)
	}

	create := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", entry(1, "First")))
	require.Equal(t, true, create["success"], "body: %+v", create)
	firstData := create["data"].(map[string]any)
	firstEntry := firstData["entry"].(map[string]any)
	firstCode := firstEntry["code"].(string)
	assert.Equal(t, float64(1), firstEntry["id"])

	// Delete the only entry, so the next internal id is 1 again.
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodDelete, "/api/contribution/admin/catalog?id=1", ""))["success"])
	assert.Empty(t, contribution_setting.AllEntries())

	// The fresh entry reuses the freed internal id, but must carry a brand-new code.
	recreate := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", entry(1, "Second")))
	require.Equal(t, true, recreate["success"], "body: %+v", recreate)
	recreatedData := recreate["data"].(map[string]any)
	recreatedEntry := recreatedData["entry"].(map[string]any)
	assert.Equal(t, float64(1), recreatedEntry["id"], "the internal id is max+1 over the remaining catalog")
	recreatedCode := recreatedEntry["code"].(string)
	assert.NotEqual(t, firstCode, recreatedCode, "a deleted entry's code is never reused")

	// The fresh code is well-formed: 8 symbols from the unambiguous alphabet.
	assert.Len(t, recreatedCode, 8)
	for _, r := range recreatedCode {
		require.NotContains(t, "01IOL", string(r), "code %s uses an ambiguous character", recreatedCode)
	}
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

	// Two enabled entries may deliberately share the same provider channel type:
	// the code is the only identity that stays unique, so the second entry is
	// accepted and receives its own auto-assigned code.
	second := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", entry(3, 2, 1, true)))
	require.Equal(t, true, second["success"], "body: %+v", second)
	data, ok := second["data"].(map[string]any)
	require.True(t, ok)
	secondEntry, ok := data["entry"].(map[string]any)
	require.True(t, ok)
	assert.NotEmpty(t, secondEntry["code"], "the second entry carries its own upstream code")
	firstEntry, ok := data["entries"].([]any)
	require.True(t, ok)
	require.Len(t, firstEntry, 2)
	firstRaw, ok := firstEntry[0].(map[string]any)
	require.True(t, ok)
	assert.NotEqual(t, firstRaw["code"], secondEntry["code"], "two entries never share a code")

	// A disabled entry skips the host-channel and plan checks.
	disabled := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", entry(4, 999, 999, false)))
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

// The admin catalog read resolves each entry's display data server-side: the bound
// host channel's name, the bound plan's title, and how many contributed keys are
// still active under the entry's upstream code. Dead and revoked keys never count,
// and a binding whose channel or plan no longer exists degrades its cells to
// empty names instead of failing the request.
func TestContributionCatalogAdminResolvesNamesAndKeyCount(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	seedContributionHostChannel(t, db, 1, true, "sk-multi-a\nsk-multi-b")
	seedContributionPlan(t, db, 1)
	router := newContributionCatalogTestRouter()

	create := `{"channel_type":1,"name":"OpenAI","register_url":"https://platform.openai.com/signup","key_placeholder":"sk-...","enabled":true,"host_channel_id":1,"plan_id":1}`
	created := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", create))
	require.Equal(t, true, created["success"], "body: %+v", created)
	entryCode, ok := created["data"].(map[string]any)["entry"].(map[string]any)["code"].(string)
	require.True(t, ok, "the created entry carries its upstream code")

	// Four live keys, one dead and one revoked under the same code: the count
	// must be exactly four.
	for i := range 4 {
		require.NoError(t, db.Create(&model.Contribution{
			UserId:        100 + i,
			EntryCode:     entryCode,
			ChannelType:   1,
			HostChannelId: 1,
			KeyMask:       model.ContributionKeyMask,
			Status:        model.ContributionStatusActive,
		}).Error)
	}
	require.NoError(t, db.Create(&model.Contribution{
		UserId: 424, EntryCode: entryCode, ChannelType: 1, HostChannelId: 1,
		KeyMask: model.ContributionKeyMask, Status: model.ContributionStatusDead,
	}).Error)
	require.NoError(t, db.Create(&model.Contribution{
		UserId: 425, EntryCode: entryCode, ChannelType: 1, HostChannelId: 1,
		KeyMask: model.ContributionKeyMask, Status: model.ContributionStatusRevoked,
	}).Error)

	// A disabled entry may point at a channel or plan that no longer exists; the
	// read must degrade its cells to empty names, not refuse the request.
	broken := `{"channel_type":1,"name":"Broken","register_url":"https://upstream.example/signup","key_placeholder":"sk-...","enabled":false,"host_channel_id":999,"plan_id":999}`
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", broken))["success"])

	response := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodGet, "/api/contribution/admin/catalog", ""))
	require.Equal(t, true, response["success"], "body: %+v", response)
	data, ok := response["data"].(map[string]any)
	require.True(t, ok)
	entries, ok := data["entries"].([]any)
	require.True(t, ok)
	require.Len(t, entries, 2)

	byId := map[float64]map[string]any{}
	for _, raw := range entries {
		item, itemOk := raw.(map[string]any)
		require.True(t, itemOk)
		byId[item["id"].(float64)] = item
	}

	first, ok := byId[1]
	require.True(t, ok)
	assert.Equal(t, "host-1", first["host_channel_name"])
	assert.Equal(t, "Reward 1", first["plan_title"])
	assert.EqualValues(t, 4, first["contributed_keys"], "only active records count; dead and revoked are excluded")
	// The id fields stay in the payload, so the edit form keeps working.
	assert.EqualValues(t, 1, first["host_channel_id"])
	assert.EqualValues(t, 1, first["plan_id"])

	brokenEntry, ok := byId[2]
	require.True(t, ok)
	assert.Empty(t, brokenEntry["host_channel_name"], "a since-deleted channel degrades to an empty name")
	assert.Empty(t, brokenEntry["plan_title"], "a since-deleted plan degrades to an empty title")
	assert.EqualValues(t, 0, brokenEntry["contributed_keys"], "a read failure or absent code degrades to zero")
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

// ---------------------------------------------------------------------------
// POST /api/contribution/submit
// ---------------------------------------------------------------------------

// contributionSubmitUpstream is a real OpenAI-compatible upstream standing in
// for the third party a user contributes from. The submit-time balance probe and
// the minimal inference both travel over HTTP to it, so the whole first-time
// validation runs for real instead of being stubbed out.
type contributionSubmitUpstream struct {
	server *httptest.Server

	mu              sync.Mutex
	balanceStatus   int
	inferenceStatus int
	balanceRequests int
	inferenceCalls  int
}

func newContributionSubmitUpstream(t *testing.T) *contributionSubmitUpstream {
	t.Helper()
	upstream := &contributionSubmitUpstream{}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()

		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/v1/chat/completions"):
			upstream.inferenceCalls++
			if upstream.inferenceStatus != 0 {
				writer.WriteHeader(upstream.inferenceStatus)
				_, _ = writer.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error"}}`))
				return
			}
			_, _ = writer.Write([]byte(`{"id":"chatcmpl-contribution","object":"chat.completion","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
		case strings.HasSuffix(request.URL.Path, "/billing/subscription"):
			upstream.balanceRequests++
			if upstream.balanceStatus != 0 {
				writer.WriteHeader(upstream.balanceStatus)
				_, _ = writer.Write([]byte(`{"error":{"message":"invalid api key"}}`))
				return
			}
			_, _ = writer.Write([]byte(`{"object":"billing_subscription","hard_limit_usd":100,"has_payment_method":true}`))
		case strings.HasSuffix(request.URL.Path, "/billing/usage"):
			_, _ = writer.Write([]byte(`{"object":"list","total_usage":0}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *contributionSubmitUpstream) rejectBalanceWith(statusCode int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.balanceStatus = statusCode
}

func (u *contributionSubmitUpstream) rejectInferenceWith(statusCode int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.inferenceStatus = statusCode
}

func (u *contributionSubmitUpstream) calls() (balanceRequests int, inferenceCalls int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.balanceRequests, u.inferenceCalls
}

// seedContributionSubmitSetup installs the real pieces a submission needs: a
// multi-key host channel that points at the fake upstream, a reward plan, and an
// enabled catalog entry bound to both.
func seedContributionSubmitSetup(t *testing.T, db *gorm.DB, handler http.Handler, token string, upstreamURL string) (int, string) {
	t.Helper()
	// The minimal inference runs through real relay billing, which refuses a model
	// without a configured price. Self-use mode is the repo's supported way to run
	// the relay path without a price table.
	withSelfUseModeEnabled(t)
	previousCache := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		common.MemoryCacheEnabled = previousCache
		model.InitChannelCache()
	})

	baseURL := upstreamURL
	hostChannel := model.Channel{
		Id:          1,
		Type:        1,
		Name:        "contribution-host",
		Key:         "sk-host-pooled",
		BaseURL:     &baseURL,
		Status:      common.ChannelStatusEnabled,
		Models:      "gpt-4o-mini",
		Group:       "default",
		ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 1, MultiKeyMode: constant.MultiKeyModePolling},
	}
	require.NoError(t, db.Create(&hostChannel).Error)
	seedContributionPlan(t, db, 1)

	entry := `{"channel_type":1,"name":"OpenAI","register_url":"https://upstream.example/signup","key_placeholder":"sk-...","enabled":true,"host_channel_id":1,"plan_id":1}`
	created := decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/admin/catalog", entry))
	require.Equal(t, true, created["success"], "body: %+v", created)
	data, ok := created["data"].(map[string]any)
	require.True(t, ok)
	createdEntry, ok := data["entry"].(map[string]any)
	require.True(t, ok)
	entryCode, ok := createdEntry["code"].(string)
	require.True(t, ok, "a created entry carries its auto-assigned upstream code")
	require.Len(t, entryCode, 8)
	switched := decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPut, "/api/contribution/admin/global", `{"enabled":true}`))
	require.Equal(t, true, switched["success"], "body: %+v", switched)
	return hostChannel.Id, entryCode
}

func submitContribution(t *testing.T, handler http.Handler, token string, entryId string, key string, agreed bool) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"entry_id":%q,"key":%q,"agreed":%t}`, entryId, key, agreed)
	recorder := callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/submit", body)
	return recorder, decodeContributionResponse(t, recorder)
}

func contributionDataOf(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	require.Equal(t, true, response["success"], "body: %+v", response)
	data, ok := response["data"].(map[string]any)
	require.True(t, ok)
	return data
}

func contributionRecordOf(t *testing.T, response map[string]any) map[string]any {
	t.Helper()
	record, ok := contributionDataOf(t, response)["contribution"].(map[string]any)
	require.True(t, ok)
	return record
}

// Consent is a server-side gate, not a frontend affordance: a request that does
// not carry agreed=true must be refused before anything is probed or pooled.
func TestContributionSubmitRequiresServerSideConsent(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	_, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	recorder, response := submitContribution(t, handler, token, entryCode, "sk-without-consent", false)

	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_consent_required", response["code"])
	assert.NotContains(t, recorder.Body.String(), "sk-without-consent")
	balanceRequests, inferenceCalls := upstream.calls()
	assert.Zero(t, balanceRequests, "consent is checked before the upstream is touched")
	assert.Zero(t, inferenceCalls)

	channel, err := model.GetChannelById(1, true)
	require.NoError(t, err)
	assert.Equal(t, "sk-host-pooled", channel.Key)
	var stored int64
	require.NoError(t, db.Model(&model.Contribution{}).Count(&stored).Error)
	assert.Zero(t, stored)
}

// The global switch and the per-entry switch decide whether a channel type can be
// contributed to at all, and each refusal is machine-readable.
func TestContributionSubmitRejectsUnavailableUpstream(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	_, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	_, response := submitContribution(t, handler, token, "ZZZZZZZZ", "sk-unknown-type", true)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_entry_unknown", response["code"])

	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPut, "/api/contribution/admin/global", `{"enabled":false}`))["success"])
	_, response = submitContribution(t, handler, token, entryCode, "sk-globally-disabled", true)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_global_disabled", response["code"])
}

// The happy path: the key is validated against the upstream, appended to the host
// channel, recorded by fingerprint, rewarded with the entry's plan, and never
// echoed back.
func TestContributionSubmitPoolsTheKeyAndRecordsIt(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	hostChannelId, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	const submittedKey = "sk-contribution-valid-key"
	recorder, response := submitContribution(t, handler, token, entryCode, submittedKey, true)
	data := contributionDataOf(t, response)

	// The plaintext key is nowhere in the response, in any shape.
	assert.NotContains(t, recorder.Body.String(), submittedKey)
	record := contributionRecordOf(t, response)
	assert.Equal(t, entryCode, record["entry_code"])
	assert.Equal(t, "active", record["status"])
	assert.NotEmpty(t, record["key_mask"])
	assert.NotContains(t, fmt.Sprint(record["key_mask"]), submittedKey)
	assert.Equal(t, false, data["redundant"])

	// The reward is the plan the catalog entry carries, granted as a real
	// subscription instance of this user.
	reward, ok := data["reward"].(map[string]any)
	require.True(t, ok, "an accepted, non-redundant submission carries its reward summary")
	assert.Equal(t, entryCode, reward["entry_code"])
	assert.Equal(t, "Reward 1", reward["plan_title"])
	assert.Equal(t, "active", reward["status"])
	assert.Equal(t, float64(5000), reward["amount_total"])
	assert.Equal(t, float64(0), reward["amount_used"])
	assert.NotZero(t, reward["end_time"])
	rewardSubscriptionId := int(reward["subscription_id"].(float64))
	require.NotZero(t, rewardSubscriptionId)

	// The key is pooled, enabled, and therefore selectable for relay traffic.
	channel, err := model.GetChannelById(hostChannelId, true)
	require.NoError(t, err)
	assert.Equal(t, "sk-host-pooled\n"+submittedKey, channel.Key)
	assert.Equal(t, common.ChannelStatusEnabled, channel.GetMultiKeyStatus(1))

	// The record tracks the fingerprint, not the key, and carries no plaintext.
	var stored model.Contribution
	require.NoError(t, db.Where("id = ?", int(record["id"].(float64))).First(&stored).Error)
	require.NotNil(t, stored.KeyFingerprint)
	assert.Equal(t, model.ContributionKeyFingerprint(channel.GetBaseURL(), submittedKey), *stored.KeyFingerprint)
	assert.Equal(t, model.ContributionStatusActive, stored.Status)
	assert.True(t, stored.RewardGranted)
	assert.Equal(t, rewardSubscriptionId, stored.SubscriptionId, "the record points at the reward it produced")
	assert.Empty(t, stored.Reason)

	// The granted instance is a subscription from the catalog plan, tagged as a
	// contribution reward rather than an admin grant or a purchase.
	var granted model.UserSubscription
	require.NoError(t, db.Where("id = ?", rewardSubscriptionId).First(&granted).Error)
	assert.Equal(t, 1, granted.PlanId)
	assert.Equal(t, "contribution", granted.Source)
	assert.Equal(t, "active", granted.Status)
	assert.EqualValues(t, 5000, granted.AmountTotal)

	var all []model.Contribution
	require.NoError(t, db.Find(&all).Error)
	encoded, err := common.Marshal(all)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), submittedKey, "no contribution row may carry the plaintext key")

	// The submission and its reward are audited, and neither audit carries key
	// material.
	requestId := recorder.Header().Get(common.RequestIdKey)
	require.NotEmpty(t, requestId)
	var audits []model.AuditLog
	require.NoError(t, db.Where("request_id = ? AND category = ?", requestId, model.AuditCategoryOperation).Find(&audits).Error)
	require.Len(t, audits, 2, "the submission and the granted reward are each audited")

	auditsByAction := map[string]model.AuditLog{}
	for _, audit := range audits {
		require.NotNil(t, audit.Other.Op)
		auditsByAction[audit.Other.Op.Action] = audit
		assert.NotContains(t, fmt.Sprint(audit.Other.Op.Params), submittedKey)
	}
	require.Contains(t, auditsByAction, "contribution.submit")
	grantAudit, ok := auditsByAction["contribution.grant"]
	require.True(t, ok, "the granted reward must be audited as contribution.grant")
	// AuditFields keeps each parameter as raw JSON, so the values are compared as
	// JSON rather than as decoded Go types.
	planIdParam, err := common.Marshal(grantAudit.Other.Op.Params["plan_id"])
	require.NoError(t, err)
	assert.JSONEq(t, "1", string(planIdParam))
	subscriptionIdParam, err := common.Marshal(grantAudit.Other.Op.Params["subscription_id"])
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprint(rewardSubscriptionId), string(subscriptionIdParam))
	contributionIdParam, err := common.Marshal(grantAudit.Other.Op.Params["contribution_id"])
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprint(stored.Id), string(contributionIdParam))
}

// A key that failed a previous check but is still present in the host channel is
// re-enabled rather than appended a second time.
func TestContributionSubmitReenablesAPooledButDisabledKey(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	hostChannelId, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	const submittedKey = "sk-contribution-reenabled"
	require.NoError(t, model.AppendOrEnableChannelKey(hostChannelId, submittedKey))
	require.NoError(t, model.SetChannelKeyStatus(hostChannelId, submittedKey, common.ChannelStatusAutoDisabled, model.ContributionReasonUpstreamUnauthorized))

	response := decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/submit",
		fmt.Sprintf(`{"entry_id":%q,"key":%q,"agreed":true}`, entryCode, submittedKey)))
	require.Equal(t, true, response["success"], "body: %+v", response)

	channel, err := model.GetChannelById(hostChannelId, true)
	require.NoError(t, err)
	assert.Equal(t, "sk-host-pooled\n"+submittedKey, channel.Key)
	assert.Equal(t, common.ChannelStatusEnabled, channel.GetMultiKeyStatus(1))
}

// Fingerprints are first-come-first-served: resubmitting a key that already has a
// record - by this user or any other - must not touch the host channel again.
func TestContributionSubmitRejectsAlreadySubmittedKey(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	hostChannelId, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	const submittedKey = "sk-contribution-first-come"
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/submit",
		fmt.Sprintf(`{"entry_id":%q,"key":%q,"agreed":true}`, entryCode, submittedKey)))["success"])

	recorder, response := submitContribution(t, handler, token, entryCode, submittedKey, true)

	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_fingerprint_taken", response["code"])
	assert.NotContains(t, recorder.Body.String(), submittedKey)

	channel, err := model.GetChannelById(hostChannelId, true)
	require.NoError(t, err)
	assert.Equal(t, "sk-host-pooled\n"+submittedKey, channel.Key, "the channel content must not change")
	assert.Equal(t, 2, channel.ChannelInfo.MultiKeySize)

	var stored int64
	require.NoError(t, db.Model(&model.Contribution{}).Count(&stored).Error)
	assert.EqualValues(t, 1, stored)
}

// Death is final for the key itself: once an upstream judged a key dead, nobody may
// submit it again for any channel type, even one whose host channel base URL would
// produce a different per-record fingerprint. Withdrawal is the exception and only
// frees the key, so a revoked record must not block the resubmission.
func TestContributionSubmitRejectsAKeyJudgedDeadForAnotherChannelType(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	_, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	// The death happened under channel type 2 and another host channel, long before
	// this submission: the fingerprint knows nothing about it, the key hash does.
	const deadKey = "sk-contribution-judged-dead"
	require.NoError(t, db.Create(&model.Contribution{
		UserId:         4242,
		ChannelType:    2,
		HostChannelId:  99,
		KeyFingerprint: common.GetPointer("fp-dead-elsewhere"),
		KeyHash:        model.ContributionKeyHash(deadKey),
		KeyMask:        model.ContributionKeyMask,
		Status:         model.ContributionStatusDead,
		Reason:         model.ContributionReasonUpstreamUnauthorized,
	}).Error)

	recorder, response := submitContribution(t, handler, token, entryCode, deadKey, true)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_key_dead", response["code"])
	assert.NotEmpty(t, response["message"])
	assert.NotContains(t, recorder.Body.String(), deadKey)

	balanceRequests, inferenceCalls := upstream.calls()
	assert.Zero(t, balanceRequests, "a globally dead key is refused before any upstream call")
	assert.Zero(t, inferenceCalls)

	var deadRecords int64
	require.NoError(t, db.Model(&model.Contribution{}).Where("key_hash = ?", model.ContributionKeyHash(deadKey)).Count(&deadRecords).Error)
	assert.EqualValues(t, 1, deadRecords, "a refused submission records nothing new")

	// Withdrawal frees the key for everyone, so a revoked record never blocks.
	const revokedKey = "sk-contribution-just-revoked"
	require.NoError(t, db.Create(&model.Contribution{
		UserId:         4243,
		ChannelType:    2,
		HostChannelId:  99,
		KeyFingerprint: common.GetPointer("fp-revoked-elsewhere"),
		KeyHash:        model.ContributionKeyHash(revokedKey),
		KeyMask:        model.ContributionKeyMask,
		Status:         model.ContributionStatusRevoked,
		Reason:         model.ContributionReasonUserRevoked,
	}).Error)

	_, response = submitContribution(t, handler, token, entryCode, revokedKey, true)
	require.Equal(t, true, response["success"], "a revoked key is free to be contributed again: %+v", response)
}

// A reward grant that failed leaves an active record flagged rewarded but pointing
// at no subscription. Resubmitting that same key must not answer "fingerprint
// taken": the key is already pooled and recorded, so only the missing grant is
// retried and the contributor gets the normal success payload.
func TestContributionSubmitRetriesAFailedRewardForItsOwnRecord(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	hostChannelId, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	var operator model.User
	require.NoError(t, db.Where("username = ?", "contribution-root").First(&operator).Error)

	const submittedKey = "sk-contribution-recover-reward"
	channel, err := model.GetChannelById(hostChannelId, true)
	require.NoError(t, err)
	require.NoError(t, model.AppendOrEnableChannelKey(hostChannelId, submittedKey))
	failed := &model.Contribution{
		UserId:         operator.Id,
		EntryCode:      entryCode,
		ChannelType:    1,
		HostChannelId:  hostChannelId,
		KeyFingerprint: common.GetPointer(model.ContributionKeyFingerprint(channel.GetBaseURL(), submittedKey)),
		KeyHash:        model.ContributionKeyHash(submittedKey),
		KeyMask:        model.ContributionKeyMask,
		Status:         model.ContributionStatusActive,
		RewardGranted:  true,
		SubscriptionId: 0,
	}
	require.NoError(t, failed.Create())

	recorder, response := submitContribution(t, handler, token, entryCode, submittedKey, true)
	data := contributionDataOf(t, response)
	assert.Equal(t, false, data["redundant"], "a recovered grant is a normal rewarded submission")
	reward, ok := data["reward"].(map[string]any)
	require.True(t, ok, "the retried grant answers with the reward it produced")
	subscriptionId := int(reward["subscription_id"].(float64))
	require.NotZero(t, subscriptionId)

	// The existing record is completed in place: no duplicate is recorded and the
	// key is not pooled a second time.
	var stored []model.Contribution
	require.NoError(t, db.Find(&stored).Error)
	require.Len(t, stored, 1)
	assert.Equal(t, failed.Id, stored[0].Id)
	assert.Equal(t, subscriptionId, stored[0].SubscriptionId, "the recovered record points at its reward")
	pooled, err := model.GetChannelById(hostChannelId, true)
	require.NoError(t, err)
	assert.Equal(t, "sk-host-pooled\n"+submittedKey, pooled.Key)

	requestId := recorder.Header().Get(common.RequestIdKey)
	require.NotEmpty(t, requestId)
	var audits []model.AuditLog
	require.NoError(t, db.Where("request_id = ? AND category = ?", requestId, model.AuditCategoryOperation).Find(&audits).Error)
	actions := map[string]bool{}
	for _, audit := range audits {
		if audit.Other.Op == nil {
			continue
		}
		actions[audit.Other.Op.Action] = true
	}
	assert.True(t, actions["contribution.grant"], "the recovered grant is audited")
	assert.True(t, actions["contribution.submit"], "the retry is audited as a submission")
}

// "One upstream counts once" is judged on a reward that actually exists: an active
// record whose grant failed must not make a later key of the same channel type
// redundant, or the contributor would never receive the promised reward.
func TestContributionSubmitRewardsAfterAnEarlierGrantNeverCompleted(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	hostChannelId, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	var operator model.User
	require.NoError(t, db.Where("username = ?", "contribution-root").First(&operator).Error)
	failed := &model.Contribution{
		UserId:         operator.Id,
		EntryCode:      entryCode,
		ChannelType:    1,
		HostChannelId:  hostChannelId,
		KeyFingerprint: common.GetPointer("fp-earlier-failed-reward"),
		KeyHash:        model.ContributionKeyHash("sk-earlier-failed-reward"),
		KeyMask:        model.ContributionKeyMask,
		Status:         model.ContributionStatusActive,
		RewardGranted:  true,
		SubscriptionId: 0,
	}
	require.NoError(t, failed.Create())

	_, response := submitContribution(t, handler, token, entryCode, "sk-contribution-after-failure", true)
	data := contributionDataOf(t, response)
	assert.Equal(t, false, data["redundant"], "an unrewarded active record does not make the new key redundant")
	require.NotNil(t, data["reward"], "the new submission is rewarded")

	var stored []model.Contribution
	require.NoError(t, db.Find(&stored).Error)
	require.Len(t, stored, 2)
	assert.Equal(t, failed.Id, stored[0].Id)
	assert.True(t, stored[1].RewardGranted)
	assert.NotZero(t, stored[1].SubscriptionId)
}

// A second key of a channel type the user already contributes to is still
// accepted into the pool, but it is marked as a redundant reward.
func TestContributionSubmitAcceptsRedundantKeyWithoutReward(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	hostChannelId, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	first := decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/submit",
		fmt.Sprintf(`{"entry_id":%q,"key":%q,"agreed":true}`, entryCode, "sk-contribution-first")))
	require.Equal(t, true, first["success"], "body: %+v", first)
	assert.Equal(t, false, contributionDataOf(t, first)["redundant"])

	_, response := submitContribution(t, handler, token, entryCode, "sk-contribution-second", true)
	data := contributionDataOf(t, response)

	assert.Equal(t, true, data["redundant"])
	assert.Nil(t, data["reward"])

	// "One upstream counts once": the redundant key still widens the pool, but the
	// channel type never grants a second subscription.
	var subscriptions []model.UserSubscription
	require.NoError(t, db.Find(&subscriptions).Error)
	require.Len(t, subscriptions, 1)
	assert.Equal(t, "contribution", subscriptions[0].Source)

	var stored []model.Contribution
	require.NoError(t, db.Order("id").Find(&stored).Error)
	require.Len(t, stored, 2)
	assert.True(t, stored[0].RewardGranted)
	assert.False(t, stored[1].RewardGranted, "the redundant contribution is marked on the record")

	channel, err := model.GetChannelById(hostChannelId, true)
	require.NoError(t, err)
	assert.Equal(t, "sk-host-pooled\nsk-contribution-first\nsk-contribution-second", channel.Key)
}

// The balance probe alone is not the validation: a key whose balance endpoint
// answers but whose inference call fails is refused and never pooled.
func TestContributionSubmitRequiresAWorkingInference(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	upstream.rejectInferenceWith(http.StatusUnauthorized)
	handler := newContributionCatalogTestRouter()
	hostChannelId, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	recorder, response := submitContribution(t, handler, token, entryCode, "sk-balance-only", true)

	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_key_invalid", response["code"])
	assert.NotContains(t, recorder.Body.String(), "sk-balance-only")

	balanceRequests, inferenceCalls := upstream.calls()
	assert.GreaterOrEqual(t, balanceRequests, 1, "the balance probe ran first")
	assert.Equal(t, 1, inferenceCalls, "the inference validation is the second gate")

	channel, err := model.GetChannelById(hostChannelId, true)
	require.NoError(t, err)
	assert.Equal(t, "sk-host-pooled", channel.Key, "a rejected key is never pooled")
	var stored int64
	require.NoError(t, db.Model(&model.Contribution{}).Count(&stored).Error)
	assert.Zero(t, stored)
}

// A rejected key costs an upstream call, so three consecutive failures put the
// user in a ten minute cooldown that is refused without touching the upstream.
func TestContributionSubmitCoolsDownAfterRepeatedValidationFailures(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	upstream.rejectBalanceWith(http.StatusUnauthorized)
	handler := newContributionCatalogTestRouter()
	_, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	for attempt := range 3 {
		_, response := submitContribution(t, handler, token, entryCode, fmt.Sprintf("sk-cooled-down-%d", attempt), true)
		assert.Equal(t, false, response["success"])
		assert.Equal(t, "contribution_key_invalid", response["code"], "attempt %d", attempt)
	}

	balanceRequests, _ := upstream.calls()
	require.EqualValues(t, 3, balanceRequests)

	_, response := submitContribution(t, handler, token, entryCode, "sk-cooled-down-4", true)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_rate_limited", response["code"])
	retryAfter, ok := response["retry_after_seconds"].(float64)
	require.True(t, ok, "the refusal must tell the user how long to wait")
	assert.Greater(t, retryAfter, float64(0))
	assert.LessOrEqual(t, retryAfter, float64(600))

	cooled, _ := upstream.calls()
	assert.EqualValues(t, balanceRequests, cooled, "the cooldown short-circuits before any upstream call")
}

// ---------------------------------------------------------------------------
// POST /api/contribution/revoke
// ---------------------------------------------------------------------------

// seedContributionRevokeUser creates an ordinary signed-in user with a dashboard
// login session, so a case can act as that user through UserAuth. A personal
// access token cannot perform a contribution write any more.
func seedContributionRevokeUser(t *testing.T, db *gorm.DB, username string) (int, string) {
	t.Helper()
	user := model.User{
		Username:    username,
		Password:    "unused-password-hash",
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       "default",
		AuthVersion: 1,
		AffCode:     username,
	}
	require.NoError(t, db.Create(&user).Error)
	bundle, err := service.CreateLoginSession(user.Id, "password", "127.0.0.1", username)
	require.NoError(t, err)
	return user.Id, bundle.AccessToken
}

// contributionRevokeFixture is one live contribution: the owner's key is pooled in
// the host channel and a reward subscription was granted for it.
type contributionRevokeFixture struct {
	ownerId        int
	ownerToken     string
	strangerToken  string
	hostChannelId  int
	contributionId int
	subscriptionId int
	fingerprint    string
	key            string
}

func seedContributionRevokeFixture(t *testing.T, db *gorm.DB, prefix string) *contributionRevokeFixture {
	t.Helper()
	ownerId, ownerToken := seedContributionRevokeUser(t, db, prefix+"-owner")
	_, strangerToken := seedContributionRevokeUser(t, db, prefix+"-stranger")
	seedContributionPlan(t, db, 1)
	seedContributionHostChannel(t, db, 1, true, "sk-host-pooled")

	fixture := &contributionRevokeFixture{
		ownerId:       ownerId,
		ownerToken:    ownerToken,
		strangerToken: strangerToken,
		hostChannelId: 1,
		key:           "sk-" + prefix + "-pooled",
	}
	require.NoError(t, model.AppendOrEnableChannelKey(fixture.hostChannelId, fixture.key))

	channel, err := model.GetChannelById(fixture.hostChannelId, true)
	require.NoError(t, err)
	fixture.fingerprint = model.ContributionKeyFingerprint(channel.GetBaseURL(), fixture.key)

	contribution := &model.Contribution{
		UserId:         ownerId,
		EntryCode:      "ABCDEFGH",
		ChannelType:    1,
		HostChannelId:  fixture.hostChannelId,
		KeyFingerprint: common.GetPointer(fixture.fingerprint),
		KeyMask:        model.ContributionKeyMask,
		Status:         model.ContributionStatusActive,
		RewardGranted:  true,
	}
	require.NoError(t, contribution.Create())
	fixture.contributionId = contribution.Id

	subscription, err := model.GrantContributionReward(contribution, 1)
	require.NoError(t, err)
	fixture.subscriptionId = subscription.Id
	return fixture
}

func revokeContribution(t *testing.T, handler http.Handler, token string, id int) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body := fmt.Sprintf("{\"id\":%d}", id)
	recorder := callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/revoke", body)
	return recorder, decodeContributionResponse(t, recorder)
}

// Only the owner may revoke: another user's record is answered exactly like an id
// that does not exist, so the endpoint cannot be used to discover that somebody else
// contributed a key.
func TestContributionRevokeOnlyAnswersForTheOwnersRecord(t *testing.T) {
	db, _ := setupContributionCatalogTest(t)
	fixture := seedContributionRevokeFixture(t, db, "ownership")
	handler := newContributionCatalogTestRouter()

	recorder, response := revokeContribution(t, handler, fixture.strangerToken, fixture.contributionId)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_not_found", response["code"])
	assert.NotEmpty(t, response["message"])
	assert.NotContains(t, recorder.Body.String(), fixture.key)

	_, missing := revokeContribution(t, handler, fixture.ownerToken, 999999)
	assert.Equal(t, false, missing["success"])
	assert.Equal(t, "contribution_not_found", missing["code"], "a foreign record and a missing id must be indistinguishable")

	// The rejected attempt changed nothing.
	var stored model.Contribution
	require.NoError(t, db.Where("id = ?", fixture.contributionId).First(&stored).Error)
	assert.Equal(t, model.ContributionStatusActive, stored.Status)
	require.NotNil(t, stored.KeyFingerprint)
	assert.Equal(t, fixture.fingerprint, *stored.KeyFingerprint)
	assert.Empty(t, stored.Reason)

	subscription, err := model.GetUserSubscriptionById(fixture.subscriptionId)
	require.NoError(t, err)
	assert.Equal(t, "active", subscription.Status)

	channel, err := model.GetChannelById(fixture.hostChannelId, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusEnabled, channel.GetMultiKeyStatus(1), "the key stays pooled and enabled")
}

// A dead record is terminal: the key was killed by the liveness probe, so the
// withdrawal is refused and nothing about the record changes.
func TestContributionRevokeRefusesADeadContribution(t *testing.T) {
	db, _ := setupContributionCatalogTest(t)
	fixture := seedContributionRevokeFixture(t, db, "terminal")
	handler := newContributionCatalogTestRouter()

	contribution, err := model.GetContributionById(fixture.contributionId)
	require.NoError(t, err)
	require.NoError(t, model.ReleaseContribution(contribution, fixture.key,
		model.ContributionStatusDead, model.ContributionReasonUpstreamUnauthorized, false))

	recorder, response := revokeContribution(t, handler, fixture.ownerToken, fixture.contributionId)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_dead_is_final", response["code"])
	assert.NotEmpty(t, response["message"])
	assert.NotContains(t, recorder.Body.String(), fixture.key)

	var stored model.Contribution
	require.NoError(t, db.Where("id = ?", fixture.contributionId).First(&stored).Error)
	assert.Equal(t, model.ContributionStatusDead, stored.Status)
	assert.Equal(t, model.ContributionReasonUpstreamUnauthorized, stored.Reason)
	require.NotNil(t, stored.KeyFingerprint, "death keeps the fingerprint forever")
	assert.Equal(t, fixture.fingerprint, *stored.KeyFingerprint)
	_, err = model.GetContributionByFingerprint(fixture.fingerprint)
	require.NoError(t, err)
}

// The happy path: the key leaves the pool, the reward is cancelled, the fingerprint
// is freed and the withdrawal is audited - and repeating it changes nothing.
func TestContributionRevokeDisablesTheKeyCancelsTheRewardAndAudits(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	fixture := seedContributionRevokeFixture(t, db, "withdraw")
	handler := newContributionCatalogTestRouter()
	entry := "{\"channel_type\":1,\"name\":\"OpenAI\",\"register_url\":\"https://upstream.example/signup\",\"key_placeholder\":\"sk-...\",\"enabled\":true,\"host_channel_id\":1,\"plan_id\":1}"
	created := decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/admin/catalog", entry))
	require.Equal(t, true, created["success"], "body: %+v", created)

	recorder, response := revokeContribution(t, handler, fixture.ownerToken, fixture.contributionId)
	require.Equal(t, true, response["success"], "body: %+v", response)
	record := contributionRecordOf(t, response)

	assert.Equal(t, float64(fixture.contributionId), record["id"])
	assert.Equal(t, "ABCDEFGH", record["entry_code"])
	assert.Equal(t, "OpenAI", record["channel_type_name"])
	assert.Equal(t, model.ContributionStatusRevoked, record["status"])
	assert.Equal(t, model.ContributionReasonUserRevoked, record["reason"])
	assert.NotZero(t, record["reason_time"])
	assert.NotEmpty(t, record["key_mask"])
	assert.NotContains(t, fmt.Sprint(record["key_mask"]), fixture.key)
	assert.Equal(t, "cancelled", record["subscription_status"], "the summary reports the reward's refreshed state")
	assert.NotContains(t, recorder.Body.String(), fixture.key)

	// The record survives for audit with a terminal status and no fingerprint.
	var stored model.Contribution
	require.NoError(t, db.Where("id = ?", fixture.contributionId).First(&stored).Error)
	assert.Equal(t, model.ContributionStatusRevoked, stored.Status)
	assert.Equal(t, model.ContributionReasonUserRevoked, stored.Reason)
	assert.Nil(t, stored.KeyFingerprint, "revoke frees the fingerprint")

	channel, err := model.GetChannelById(fixture.hostChannelId, true)
	require.NoError(t, err)
	assert.Equal(t, common.ChannelStatusAutoDisabled, channel.GetMultiKeyStatus(1))
	assert.Equal(t, model.ContributionReasonUserRevoked, channel.ChannelInfo.MultiKeyDisabledReason[1])

	subscription, err := model.GetUserSubscriptionById(fixture.subscriptionId)
	require.NoError(t, err)
	assert.Equal(t, "cancelled", subscription.Status)

	// The withdrawal is audited once, with the contribution, its channel type and its
	// host channel - and no key material anywhere in the row.
	requestId := recorder.Header().Get(common.RequestIdKey)
	require.NotEmpty(t, requestId)
	var audits []model.AuditLog
	require.NoError(t, db.Where("request_id = ? AND category = ?", requestId, model.AuditCategoryOperation).Find(&audits).Error)
	require.Len(t, audits, 1)
	require.NotNil(t, audits[0].Other.Op)
	assert.Equal(t, "contribution.revoke", audits[0].Other.Op.Action)
	assert.True(t, audits[0].Success)
	contributionIdParam, err := common.Marshal(audits[0].Other.Op.Params["contribution_id"])
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprint(fixture.contributionId), string(contributionIdParam))
	hostChannelParam, err := common.Marshal(audits[0].Other.Op.Params["host_channel_id"])
	require.NoError(t, err)
	assert.JSONEq(t, fmt.Sprint(fixture.hostChannelId), string(hostChannelParam))
	encodedAudits, err := common.Marshal(audits)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedAudits), fixture.key, "the audit trail must not carry key material")

	// Repeating the withdrawal answers with the same revoked summary and performs no
	// transition, so it audits nothing new.
	againRecorder, again := revokeContribution(t, handler, fixture.ownerToken, fixture.contributionId)
	require.Equal(t, true, again["success"], "body: %+v", again)
	againRecord := contributionRecordOf(t, again)
	assert.Equal(t, model.ContributionStatusRevoked, againRecord["status"])
	assert.Equal(t, model.ContributionReasonUserRevoked, againRecord["reason"])
	assert.Equal(t, "cancelled", againRecord["subscription_status"])
	againRequestId := againRecorder.Header().Get(common.RequestIdKey)
	require.NotEmpty(t, againRequestId)
	var againAudits []model.AuditLog
	require.NoError(t, db.Where("request_id = ?", againRequestId).Find(&againAudits).Error)
	// The generic middleware fallback still records that the endpoint was called, so
	// what must be absent is a second contribution.revoke transition.
	for _, audit := range againAudits {
		if audit.Other.Op == nil {
			continue
		}
		assert.NotEqual(t, "contribution.revoke", audit.Other.Op.Action, "a no-op withdrawal must not be audited as a transition")
	}
}

// A successful validation clears the failure counter, so failures that are not
// consecutive never accumulate into a cooldown.
func TestContributionSubmitSuccessClearsFailureCounter(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	_, entryCode := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	upstream.rejectBalanceWith(http.StatusUnauthorized)
	for attempt := range 2 {
		_, response := submitContribution(t, handler, token, entryCode, fmt.Sprintf("sk-flaky-%d", attempt), true)
		require.Equal(t, "contribution_key_invalid", response["code"])
	}

	upstream.rejectBalanceWith(0)
	_, response := submitContribution(t, handler, token, entryCode, "sk-flaky-recovered", true)
	require.Equal(t, true, response["success"], "body: %+v", response)

	upstream.rejectBalanceWith(http.StatusUnauthorized)
	for attempt := range 2 {
		_, response := submitContribution(t, handler, token, entryCode, fmt.Sprintf("sk-flaky-again-%d", attempt), true)
		assert.Equal(t, "contribution_key_invalid", response["code"], "the counter restarted after a success")
	}
}

// ---------------------------------------------------------------------------
// GET /api/contribution/mine
// ---------------------------------------------------------------------------

// contributionMineItemsById indexes the list by record id and keeps the order it
// was returned in, so a case can assert both "newest first" and per-record detail
// without depending on position.
func contributionMineItemsById(t *testing.T, response map[string]any) (map[int]map[string]any, []int) {
	t.Helper()
	raw, ok := contributionDataOf(t, response)["items"].([]any)
	require.True(t, ok, "the list endpoint answers with items")
	byId := make(map[int]map[string]any, len(raw))
	order := make([]int, 0, len(raw))
	for _, entry := range raw {
		item, ok := entry.(map[string]any)
		require.True(t, ok)
		id := int(item["id"].(float64))
		byId[id] = item
		order = append(order, id)
	}
	return byId, order
}

// The list is the contributor's own bookkeeping: the caller sees only their own
// records, every state renders truthfully with its reason and reward, and the
// summary counts the channel types that still reward the account.
func TestContributionMineListsOnlyTheCallersOwnContributionsInFullDetail(t *testing.T) {
	db, _ := setupContributionCatalogTest(t)
	handler := newContributionCatalogTestRouter()
	ownerId, ownerToken := seedContributionRevokeUser(t, db, "mine-owner")
	strangerId, _ := seedContributionRevokeUser(t, db, "mine-stranger")
	seedContributionPlan(t, db, 1)
	// Only channel type 1 has a catalog entry; the other types are named by their
	// built-in channel type name.
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{
		contribution_setting.CatalogOptionKey: `[{"id":1,"code":"ABCDEFGH","channel_type":1,"name":"OpenAI","register_url":"https://upstream.example/signup","key_placeholder":"sk-...","enabled":true,"host_channel_id":1,"plan_id":1}]`,
	}))

	const pooledKey = "sk-mine-plaintext-must-never-be-returned"
	seedContributionHostChannel(t, db, 1, true, "sk-host-pooled\n"+pooledKey)

	seed := func(userId, channelType int, entryCode string, status string, reason string, fingerprint string) *model.Contribution {
		contribution := &model.Contribution{
			UserId:        userId,
			EntryCode:     entryCode,
			ChannelType:   channelType,
			HostChannelId: 1,
			KeyMask:       model.ContributionKeyMask,
			Status:        status,
			RewardGranted: status == model.ContributionStatusActive,
		}
		if fingerprint != "" {
			contribution.KeyFingerprint = common.GetPointer(fingerprint)
		}
		if reason != "" {
			contribution.Reason = reason
			contribution.ReasonTime = common.GetTimestamp()
		}
		require.NoError(t, contribution.Create())
		return contribution
	}

	liveRewarded := seed(ownerId, 1, "ABCDEFGH", model.ContributionStatusActive, "", "fp-mine-1")
	liveRedundant := seed(ownerId, 1, "ABCDEFGH", model.ContributionStatusActive, "", "fp-mine-2")
	revoked := seed(ownerId, 1, "ABCDEFGH", model.ContributionStatusRevoked, model.ContributionReasonUserRevoked, "")
	liveOtherType := seed(ownerId, 3, "CCCCDDDD", model.ContributionStatusActive, "", "fp-mine-3")
	dead := seed(ownerId, 3, "CCCCDDDD", model.ContributionStatusDead, model.ContributionReasonUpstreamUnauthorized, "fp-mine-4")
	foreign := seed(strangerId, 1, "EEEEFFFF", model.ContributionStatusActive, "", "fp-mine-foreign")

	reward, err := model.GrantContributionReward(liveRewarded, 1)
	require.NoError(t, err)
	cancelledReward, err := model.GrantContributionReward(dead, 1)
	require.NoError(t, err)
	_, err = model.AdminInvalidateUserSubscription(cancelledReward.Id)
	require.NoError(t, err)

	recorder := callContributionCatalog(t, handler, ownerToken, http.MethodGet, "/api/contribution/mine", "")
	response := decodeContributionResponse(t, recorder)
	byId, order := contributionMineItemsById(t, response)

	require.Len(t, order, 5, "only the caller's own records are listed")
	assert.NotContains(t, order, foreign.Id)
	assert.Equal(t, []int{dead.Id, liveOtherType.Id, revoked.Id, liveRedundant.Id, liveRewarded.Id}, order, "newest first")

	liveItem := byId[liveRewarded.Id]
	assert.Equal(t, "ABCDEFGH", liveItem["entry_code"])
	assert.Equal(t, "OpenAI", liveItem["channel_type_name"])
	assert.Equal(t, model.ContributionStatusActive, liveItem["status"])
	assert.Empty(t, liveItem["reason"])
	assert.NotEmpty(t, liveItem["key_mask"])
	require.NotNil(t, liveItem["subscription"], "an active rewarded contribution reports its instance")
	rewardItem := liveItem["subscription"].(map[string]any)
	assert.Equal(t, "Reward 1", rewardItem["plan_title"])
	assert.Equal(t, float64(5000), rewardItem["amount_total"])
	assert.Equal(t, float64(0), rewardItem["amount_used"])
	assert.NotZero(t, rewardItem["end_time"])
	assert.Equal(t, "active", rewardItem["status"])
	assert.Equal(t, float64(reward.Id), liveItem["subscription_id"], "the summary names the instance the record points at")

	assert.Nil(t, byId[liveRedundant.Id]["subscription"], "a redundant contribution granted no reward")

	deadItem := byId[dead.Id]
	assert.Equal(t, model.ContributionStatusDead, deadItem["status"])
	assert.Equal(t, model.ContributionReasonUpstreamUnauthorized, deadItem["reason"])
	assert.NotZero(t, deadItem["reason_time"])
	require.NotNil(t, deadItem["subscription"], "a dead contribution still reports the reward it used to hold")
	assert.Equal(t, "cancelled", deadItem["subscription"].(map[string]any)["status"])

	revokedItem := byId[revoked.Id]
	assert.Equal(t, model.ContributionStatusRevoked, revokedItem["status"])
	assert.Equal(t, model.ContributionReasonUserRevoked, revokedItem["reason"])
	assert.NotZero(t, revokedItem["reason_time"])
	assert.Nil(t, revokedItem["subscription"])

	// The catalog entry names channel type 1; channel type 3 has none and falls back
	// to the built-in channel type name.
	assert.Equal(t, "OpenAI", byId[liveRedundant.Id]["channel_type_name"])
	assert.Equal(t, "Azure", byId[liveOtherType.Id]["channel_type_name"])
	assert.Equal(t, "Azure", deadItem["channel_type_name"])

	summary, ok := contributionDataOf(t, response)["summary"].(map[string]any)
	require.True(t, ok, "the list carries the account summary")
	assert.Equal(t, float64(2), summary["entry_code_count"], "distinct live entries: 1 and 2")

	assert.NotContains(t, recorder.Body.String(), pooledKey, "the list never echoes the plaintext key")
	assert.NotContains(t, recorder.Body.String(), "fp-mine", "the fingerprint never leaves the database")
}

// Submitting pools a third-party credential into a shared channel and withdrawing
// pulls it back out, so both are browser-session actions: a personal access token
// must not perform them, whatever scopes it carries. Scoped tokens could otherwise
// turn a wallet-scoped token into control over shared upstream credentials.
func TestContributionWritesRequireASessionNotAnAccessToken(t *testing.T) {
	for _, key := range []string{
		"POST /api/contribution/submit",
		"POST /api/contribution/revoke",
	} {
		rule, declared := middleware.AccessTokenRouteRule(key)
		require.True(t, declared, key)
		assert.Equal(t, "session", rule.Kind(), key)
		assert.Empty(t, rule.Scope(), key)
	}
}

// The catalog read reports the account's contribution summary as well, without
// losing any field the contribute panel already renders.
func TestContributionCatalogReportsTheAccountContributionSummary(t *testing.T) {
	db, adminToken := setupContributionCatalogTest(t)
	handler := newContributionCatalogTestRouter()
	ownerId, ownerToken := seedContributionRevokeUser(t, db, "catalog-summary-owner")
	seedContributionHostChannel(t, db, 1, true, "sk-host-pooled")
	seedContributionPlan(t, db, 1)

	entry := `{"channel_type":1,"name":"OpenAI","register_url":"https://upstream.example/signup","key_placeholder":"sk-...","enabled":true,"host_channel_id":1,"plan_id":1}`
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, handler, adminToken, http.MethodPost, "/api/contribution/admin/catalog", entry))["success"])
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, handler, adminToken, http.MethodPut, "/api/contribution/admin/global", `{"enabled":true}`))["success"])

	// Two live catalog entries plus one that already ended: only the live entries
	// count as upstreams this account brought in, keyed by the entry code.
	for index, seed := range []struct {
		channelType int
		entryCode   string
		status      string
	}{
		{channelType: 1, entryCode: "ABCDEFGH", status: model.ContributionStatusActive},
		{channelType: 3, entryCode: "JKMNPRST", status: model.ContributionStatusActive},
		{channelType: 3, entryCode: "JKMNPRST", status: model.ContributionStatusDead},
	} {
		contribution := &model.Contribution{
			UserId:         ownerId,
			EntryCode:      seed.entryCode,
			ChannelType:    seed.channelType,
			HostChannelId:  1,
			KeyFingerprint: common.GetPointer(fmt.Sprintf("fp-catalog-summary-%d", index)),
			KeyMask:        model.ContributionKeyMask,
			Status:         seed.status,
		}
		require.NoError(t, contribution.Create())
	}

	response := decodeContributionResponse(t, callContributionCatalog(t, handler, ownerToken, http.MethodGet, "/api/contribution/catalog", ""))
	data := contributionDataOf(t, response)
	assert.Equal(t, true, data["enabled"])
	assert.NotEmpty(t, contributionEntriesOf(t, response))
	assert.NotEmpty(t, data["agreement"])

	summary, ok := data["summary"].(map[string]any)
	require.True(t, ok, "the catalog read reports the account's contribution summary")
	assert.Equal(t, float64(2), summary["entry_code_count"])
}

// The admin form no longer sends channel_type - the backend derives it from the
// bound host channel. An entry created or updated without a channel type must
// persist the host channel's type, or every contribution it stamps would degrade
// to "channel type 0" in user-facing names.
func TestContributionCatalogDerivesChannelTypeFromHostChannel(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	// A non-default provider type proves the derivation really came from the
	// channel, not from the zero value coinciding with the request default.
	host := model.Channel{Id: 1, Type: 14, Name: "host-14", Key: "sk-multi-a\nsk-multi-b", Status: common.ChannelStatusEnabled}
	host.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2}
	require.NoError(t, db.Create(&host).Error)
	seedContributionPlan(t, db, 1)
	router := newContributionCatalogTestRouter()

	// A create body without a channel_type field, exactly like the dialog sends.
	create := `{"name":"Anthropic","register_url":"https://claude.ai/signup","key_placeholder":"sk-ant-...","enabled":true,"host_channel_id":1,"plan_id":1}`
	response := decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPost, "/api/contribution/admin/catalog", create))
	require.Equal(t, true, response["success"], "body: %+v", response)
	data, ok := response["data"].(map[string]any)
	require.True(t, ok)
	entry, ok := data["entry"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 14, entry["channel_type"], "the created entry carries its host channel's type")
	assert.Equal(t, float64(1), entry["id"])

	// An update without a channel_type field keeps the derivation in sync too.
	update := `{"id":1,"name":"Anthropic Official","register_url":"https://claude.ai/signup","key_placeholder":"sk-ant-...","enabled":true,"host_channel_id":1,"plan_id":1}`
	response = decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPut, "/api/contribution/admin/catalog", update))
	require.Equal(t, true, response["success"], "body: %+v", response)
	require.Len(t, contribution_setting.AllEntries(), 1)
	assert.Equal(t, 14, contribution_setting.AllEntries()[0].ChannelType, "the updated entry keeps its host channel's type")

	// A disabled draft with an unreadable host channel keeps the request value
	// instead of being rejected: it cannot be enabled until the binding is fixed.
	disabled := `{"id":1,"channel_type":7,"name":"Draft","enabled":false,"host_channel_id":999,"plan_id":999}`
	response = decodeContributionResponse(t, callContributionCatalog(t, router, token, http.MethodPut, "/api/contribution/admin/catalog", disabled))
	require.Equal(t, true, response["success"], "body: %+v", response)
	require.Len(t, contribution_setting.AllEntries(), 1)
	assert.Equal(t, 7, contribution_setting.AllEntries()[0].ChannelType, "an unreadable host channel keeps the request value")
}
