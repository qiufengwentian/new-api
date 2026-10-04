package controller

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestGetDeepSeekBalanceUSD(t *testing.T) {
	tests := []struct {
		name            string
		responseJSON    string
		usdExchangeRate float64
		want            float64
		wantErrContains string
	}{
		{
			name:            "prefers USD when USD precedes CNY",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"12.50"},{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: 7.3,
			want:            12.5,
		},
		{
			name:            "prefers USD when CNY precedes USD",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"},{"currency":"USD","total_balance":"12.50"}]}`,
			usdExchangeRate: 7.3,
			want:            12.5,
		},
		{
			name:            "converts CNY when USD is absent",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: 7.3,
			want:            10,
		},
		{
			name:            "returns error when USD and CNY are absent",
			responseJSON:    `{"balance_infos":[{"currency":"EUR","total_balance":"10.00"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "currency USD or CNY not found",
		},
		{
			name:            "returns USD parse error instead of falling back to CNY",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"invalid"},{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "invalid syntax",
		},
		{
			name:            "rejects NaN USD balance",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"NaN"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "USD balance must be finite",
		},
		{
			name:            "rejects negative USD balance",
			responseJSON:    `{"balance_infos":[{"currency":"USD","total_balance":"-1.00"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "USD balance must be non-negative",
		},
		{
			name:            "rejects positive infinity CNY balance",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"+Inf"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "CNY balance must be finite",
		},
		{
			name:            "rejects negative CNY balance",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"-7.30"}]}`,
			usdExchangeRate: 7.3,
			wantErrContains: "CNY balance must be non-negative",
		},
		{
			name:            "returns error for non-positive CNY exchange rate",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: 0,
			wantErrContains: "USD exchange rate must be greater than zero",
		},
		{
			name:            "rejects NaN CNY exchange rate",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: math.NaN(),
			wantErrContains: "USD exchange rate must be finite",
		},
		{
			name:            "rejects positive infinity CNY exchange rate",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"73.00"}]}`,
			usdExchangeRate: math.Inf(1),
			wantErrContains: "USD exchange rate must be finite",
		},
		{
			name:            "rejects CNY conversion overflow",
			responseJSON:    `{"balance_infos":[{"currency":"CNY","total_balance":"1.7976931348623157e+308"}]}`,
			usdExchangeRate: math.SmallestNonzeroFloat64,
			wantErrContains: "converted USD balance must be finite",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var response DeepSeekUsageResponse
			require.NoError(t, common.Unmarshal([]byte(test.responseJSON), &response))

			balance, err := getDeepSeekBalanceUSD(response, test.usdExchangeRate)
			if test.wantErrContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), test.wantErrContains)
				return
			}

			require.NoError(t, err)
			assert.InDelta(t, test.want, balance, 1e-12)
		})
	}
}

// The contribution probe classifies a key by the HTTP status its upstream
// balance endpoint returned, so the status must survive the transport helper as
// a typed error and not only as the historical "status code: N" message.
func TestGetResponseBodyExposesUpstreamStatus(t *testing.T) {
	const okBody = `{"object":"credit_summary","total_available":10}`

	tests := []struct {
		name       string
		statusCode int
	}{
		{name: "200 returns the body", statusCode: http.StatusOK},
		{name: "401 carries the status code", statusCode: http.StatusUnauthorized},
		{name: "403 carries the status code", statusCode: http.StatusForbidden},
		{name: "500 carries the status code", statusCode: http.StatusInternalServerError},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.statusCode)
				_, _ = w.Write([]byte(okBody))
			}))
			t.Cleanup(server.Close)

			channel := &model.Channel{BaseURL: &server.URL}
			body, err := GetResponseBody(http.MethodGet, server.URL+"/v1/dashboard/billing/subscription", channel, nil)

			if test.statusCode == http.StatusOK {
				require.NoError(t, err)
				assert.JSONEq(t, okBody, string(body))
				return
			}

			require.Error(t, err)
			assert.Equal(t, fmt.Sprintf("status code: %d", test.statusCode), err.Error(),
				"the message must stay identical so existing string-matching callers keep working")
			var statusErr *UpstreamStatusError
			require.ErrorAs(t, err, &statusErr)
			assert.Equal(t, test.statusCode, statusErr.StatusCode)
		})
	}
}

// contributionProbeTestDB installs a throwaway SQLite database as model.DB so
// the probe's successful balance path can run Channel.UpdateBalance without
// touching any real database.
func contributionProbeTestDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}))
	previousDB := model.DB
	model.DB = db
	t.Cleanup(func() {
		model.DB = previousDB
		require.NoError(t, sqlDB.Close())
	})
}

// The probe is the only gate that decides whether a contributed key is taken
// into the pool, so every upstream outcome it can observe is pinned here: only
// 401 is fatal, and the balance it reports is display-only.
func TestProbeContributionKeyClassifiesUpstreamStatus(t *testing.T) {
	tests := []struct {
		name           string
		handler        http.HandlerFunc
		unreachable    bool
		channelType    int
		wantStatusCode int
		wantBalance    float64
		wantHasBalance bool
		wantDead       bool
	}{
		{
			name: "401 marks the key dead and reports the status",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			}),
			channelType:    constant.ChannelTypeCustom,
			wantStatusCode: http.StatusUnauthorized,
			wantDead:       true,
		},
		{
			name: "403 keeps the key alive",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusForbidden)
			}),
			channelType:    constant.ChannelTypeCustom,
			wantStatusCode: http.StatusForbidden,
		},
		{
			name: "500 keeps the key alive",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			}),
			channelType:    constant.ChannelTypeCustom,
			wantStatusCode: http.StatusInternalServerError,
		},
		{
			name:           "a transport failure keeps the key alive and reports no status",
			unreachable:    true,
			channelType:    constant.ChannelTypeCustom,
			wantStatusCode: 0,
		},
		{
			name: "a successful query with zero balance stays alive and still reports the balance",
			handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/subscription") {
					_, _ = w.Write([]byte(`{"has_payment_method":true,"hard_limit_usd":0}`))
					return
				}
				_, _ = w.Write([]byte(`{"total_usage":0}`))
			}),
			channelType:    constant.ChannelTypeCustom,
			wantStatusCode: http.StatusOK,
			wantBalance:    0,
			wantHasBalance: true,
		},
		{
			name:           "a channel type without a balance query reports no balance and stays alive",
			channelType:    constant.ChannelTypeAzure,
			wantStatusCode: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contributionProbeTestDB(t)

			baseURL := "http://127.0.0.1:1"
			if test.handler != nil {
				server := httptest.NewServer(test.handler)
				t.Cleanup(server.Close)
				baseURL = server.URL
			} else if test.unreachable {
				closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				baseURL = closed.URL
				closed.Close()
			}

			hostChannel := &model.Channel{
				Id:      4242,
				Type:    test.channelType,
				Key:     "host-key",
				BaseURL: &baseURL,
			}

			result := probeContributionKey("contributed-key", hostChannel)

			assert.Equal(t, test.wantStatusCode, result.StatusCode)
			assert.Equal(t, test.wantHasBalance, result.HasBalance)
			assert.InDelta(t, test.wantBalance, result.Balance, 1e-12)
			assert.Equal(t, test.wantDead, model.IsContributionKeyDead(result.StatusCode))
			if test.wantStatusCode == 0 {
				require.Error(t, result.Err, "an outcome without an HTTP status must still explain itself")
			}
		})
	}
}

// A contributed key must be probed on its own: the host channel is a multi-key
// channel, which the channel-level balance entry point refuses outright. The
// probe therefore works on a copy, sends only the probed key upstream, and never
// mutates the host channel it was handed.
func TestProbeContributionKeyUsesOnlyTheGivenKeyAndLeavesTheHostChannelUntouched(t *testing.T) {
	contributionProbeTestDB(t)

	var gotAuthHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		if strings.HasSuffix(r.URL.Path, "/subscription") {
			_, _ = w.Write([]byte(`{"has_payment_method":true,"hard_limit_usd":10}`))
			return
		}
		_, _ = w.Write([]byte(`{"total_usage":0}`))
	}))
	t.Cleanup(server.Close)

	hostKeys := "host-key-a" + "\n" + "host-key-b"
	hostChannel := &model.Channel{
		Id:      4242,
		Type:    constant.ChannelTypeCustom,
		Name:    "host",
		Key:     hostKeys,
		BaseURL: &server.URL,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         2,
			MultiKeyStatusList:   map[int]int{0: common.ChannelStatusAutoDisabled},
			MultiKeyPollingIndex: 1,
		},
		Keys: []string{"host-key-a", "host-key-b"},
	}

	result := probeContributionKey("contributed-key", hostChannel)

	require.NoError(t, result.Err)
	assert.Equal(t, http.StatusOK, result.StatusCode)
	assert.True(t, result.HasBalance)
	assert.InDelta(t, 10, result.Balance, 1e-12)
	assert.False(t, model.IsContributionKeyDead(result.StatusCode))

	assert.Equal(t, "Bearer contributed-key", gotAuthHeader, "only the probed key may be sent upstream")
	assert.Equal(t, hostKeys, hostChannel.Key)
	assert.Equal(t, []string{"host-key-a", "host-key-b"}, hostChannel.Keys)
	assert.True(t, hostChannel.ChannelInfo.IsMultiKey)
	assert.Equal(t, 2, hostChannel.ChannelInfo.MultiKeySize)
	assert.Equal(t, map[int]int{0: common.ChannelStatusAutoDisabled}, hostChannel.ChannelInfo.MultiKeyStatusList)
	assert.Equal(t, 1, hostChannel.ChannelInfo.MultiKeyPollingIndex)
	assert.Equal(t, 4242, hostChannel.Id)
	assert.Zero(t, hostChannel.Balance)
	assert.Zero(t, hostChannel.BalanceUpdatedTime)
}
