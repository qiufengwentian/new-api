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
	userRoute.POST("/submit", SubmitContribution)
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
		&model.UserSubscription{}, &model.Contribution{}, &model.Ability{},
		&model.Log{}, &model.AuditLog{}, &model.CasbinRule{}, &model.AuthzRole{},
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
	// The submit cooldown lives in a process-wide cache that outlives this
	// database, so a case must not inherit another case's failed validations.
	t.Cleanup(func() { clearContributionValidationFailures(operator.Id) })
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
func seedContributionSubmitSetup(t *testing.T, db *gorm.DB, handler http.Handler, token string, upstreamURL string) int {
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
	switched := decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPut, "/api/contribution/admin/global", `{"enabled":true}`))
	require.Equal(t, true, switched["success"], "body: %+v", switched)
	return hostChannel.Id
}

func submitContribution(t *testing.T, handler http.Handler, token string, channelType int, key string, agreed bool) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"channel_type":%d,"key":%q,"agreed":%t}`, channelType, key, agreed)
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
	seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	recorder, response := submitContribution(t, handler, token, 1, "sk-without-consent", false)

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
	seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	_, response := submitContribution(t, handler, token, 7, "sk-unknown-type", true)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_channel_type_unknown", response["code"])

	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPut, "/api/contribution/admin/global", `{"enabled":false}`))["success"])
	_, response = submitContribution(t, handler, token, 1, "sk-globally-disabled", true)
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
	hostChannelId := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	const submittedKey = "sk-contribution-valid-key"
	recorder, response := submitContribution(t, handler, token, 1, submittedKey, true)
	data := contributionDataOf(t, response)

	// The plaintext key is nowhere in the response, in any shape.
	assert.NotContains(t, recorder.Body.String(), submittedKey)
	record := contributionRecordOf(t, response)
	assert.Equal(t, float64(1), record["channel_type"])
	assert.Equal(t, "active", record["status"])
	assert.NotEmpty(t, record["key_mask"])
	assert.NotContains(t, fmt.Sprint(record["key_mask"]), submittedKey)
	assert.Equal(t, false, data["redundant"])

	// The reward is the plan the catalog entry carries, granted as a real
	// subscription instance of this user.
	reward, ok := data["reward"].(map[string]any)
	require.True(t, ok, "an accepted, non-redundant submission carries its reward summary")
	assert.Equal(t, float64(1), reward["channel_type"])
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
	hostChannelId := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	const submittedKey = "sk-contribution-reenabled"
	require.NoError(t, model.AppendOrEnableChannelKey(hostChannelId, submittedKey))
	require.NoError(t, model.SetChannelKeyStatus(hostChannelId, submittedKey, common.ChannelStatusAutoDisabled, model.ContributionReasonUpstreamUnauthorized))

	response := decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/submit",
		fmt.Sprintf(`{"channel_type":1,"key":%q,"agreed":true}`, submittedKey)))
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
	hostChannelId := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	const submittedKey = "sk-contribution-first-come"
	require.Equal(t, true, decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/submit",
		fmt.Sprintf(`{"channel_type":1,"key":%q,"agreed":true}`, submittedKey)))["success"])

	recorder, response := submitContribution(t, handler, token, 1, submittedKey, true)

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

// A second key of a channel type the user already contributes to is still
// accepted into the pool, but it is marked as a redundant reward.
func TestContributionSubmitAcceptsRedundantKeyWithoutReward(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	hostChannelId := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	first := decodeContributionResponse(t, callContributionCatalog(t, handler, token, http.MethodPost, "/api/contribution/submit",
		fmt.Sprintf(`{"channel_type":1,"key":%q,"agreed":true}`, "sk-contribution-first")))
	require.Equal(t, true, first["success"], "body: %+v", first)
	assert.Equal(t, false, contributionDataOf(t, first)["redundant"])

	_, response := submitContribution(t, handler, token, 1, "sk-contribution-second", true)
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
	hostChannelId := seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	recorder, response := submitContribution(t, handler, token, 1, "sk-balance-only", true)

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
	seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	for attempt := range 3 {
		_, response := submitContribution(t, handler, token, 1, fmt.Sprintf("sk-cooled-down-%d", attempt), true)
		assert.Equal(t, false, response["success"])
		assert.Equal(t, "contribution_key_invalid", response["code"], "attempt %d", attempt)
	}

	balanceRequests, _ := upstream.calls()
	require.EqualValues(t, 3, balanceRequests)

	_, response := submitContribution(t, handler, token, 1, "sk-cooled-down-4", true)
	assert.Equal(t, false, response["success"])
	assert.Equal(t, "contribution_rate_limited", response["code"])
	retryAfter, ok := response["retry_after_seconds"].(float64)
	require.True(t, ok, "the refusal must tell the user how long to wait")
	assert.Greater(t, retryAfter, float64(0))
	assert.LessOrEqual(t, retryAfter, float64(600))

	cooled, _ := upstream.calls()
	assert.EqualValues(t, balanceRequests, cooled, "the cooldown short-circuits before any upstream call")
}

// A successful validation clears the failure counter, so failures that are not
// consecutive never accumulate into a cooldown.
func TestContributionSubmitSuccessClearsFailureCounter(t *testing.T) {
	db, token := setupContributionCatalogTest(t)
	upstream := newContributionSubmitUpstream(t)
	handler := newContributionCatalogTestRouter()
	seedContributionSubmitSetup(t, db, handler, token, upstream.server.URL)

	upstream.rejectBalanceWith(http.StatusUnauthorized)
	for attempt := range 2 {
		_, response := submitContribution(t, handler, token, 1, fmt.Sprintf("sk-flaky-%d", attempt), true)
		require.Equal(t, "contribution_key_invalid", response["code"])
	}

	upstream.rejectBalanceWith(0)
	_, response := submitContribution(t, handler, token, 1, "sk-flaky-recovered", true)
	require.Equal(t, true, response["success"], "body: %+v", response)

	upstream.rejectBalanceWith(http.StatusUnauthorized)
	for attempt := range 2 {
		_, response := submitContribution(t, handler, token, 1, fmt.Sprintf("sk-flaky-again-%d", attempt), true)
		assert.Equal(t, "contribution_key_invalid", response["code"], "the counter restarted after a success")
	}
}
