package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/system_setting"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSystemTaskRegistry swaps the package registry for the given handlers for
// the duration of a test and restores the original registry afterward.
func withSystemTaskRegistry(t *testing.T, handlers ...SystemTaskHandler) {
	t.Helper()
	systemTaskHandlersMu.Lock()
	saved := systemTaskHandlers
	systemTaskHandlers = map[string]SystemTaskHandler{}
	for _, h := range handlers {
		systemTaskHandlers[h.Type()] = h
	}
	systemTaskHandlersMu.Unlock()
	t.Cleanup(func() {
		systemTaskHandlersMu.Lock()
		systemTaskHandlers = saved
		systemTaskHandlersMu.Unlock()
	})
}

type stubScheduledHandler struct {
	taskType string
	enabled  bool
	interval time.Duration
	onRun    func(ctx context.Context, task *model.SystemTask, runnerID string)
}

type stubSystemTaskRunResult struct {
	taskID   string
	taskType string
	err      error
}

func (h *stubScheduledHandler) Type() string { return h.taskType }

func (h *stubScheduledHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	if h.onRun != nil {
		h.onRun(ctx, task, runnerID)
	}
}

func (h *stubScheduledHandler) Enabled() bool           { return h.enabled }
func (h *stubScheduledHandler) Interval() time.Duration { return h.interval }
func (h *stubScheduledHandler) NewPayload() any         { return nil }

func countSystemTasks(t *testing.T, taskType string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, model.DB.Model(&model.SystemTask{}).Where("type = ?", taskType).Count(&count).Error)
	return count
}

func TestSystemTaskSchedulerCreatesWhenDueAndDedups(t *testing.T) {
	truncate(t)

	handler := &stubScheduledHandler{taskType: "test_scheduled", enabled: true, interval: time.Minute}
	withSystemTaskRegistry(t, handler)

	runSystemTaskScheduler()
	require.Equal(t, int64(1), countSystemTasks(t, handler.taskType))

	// An active (pending) row already exists, so a second pass must not create
	// another row.
	runSystemTaskScheduler()
	require.Equal(t, int64(1), countSystemTasks(t, handler.taskType))

	// Finish the run; with a fresh updated_at the next run is not due yet.
	latest, err := model.GetLatestSystemTask(handler.taskType)
	require.NoError(t, err)
	require.NotNil(t, latest)
	_, claimed, err := model.ClaimSystemTask(latest.ID, handler.taskType, "runner-a", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, model.FinishSystemTask(latest.TaskID, "runner-a", model.SystemTaskStatusSucceeded, nil, ""))

	runSystemTaskScheduler()
	require.Equal(t, int64(1), countSystemTasks(t, handler.taskType))

	// Backdate the finished row beyond the interval -> the job becomes due again.
	require.NoError(t, model.DB.Model(&model.SystemTask{}).
		Where("task_id = ?", latest.TaskID).
		Update("updated_at", common.GetTimestamp()-120).Error)

	runSystemTaskScheduler()
	require.Equal(t, int64(2), countSystemTasks(t, handler.taskType))
}

func TestSystemTaskSchedulerSkipsDisabled(t *testing.T) {
	truncate(t)

	handler := &stubScheduledHandler{taskType: "test_disabled", enabled: false, interval: time.Minute}
	withSystemTaskRegistry(t, handler)

	runSystemTaskScheduler()
	assert.Equal(t, int64(0), countSystemTasks(t, handler.taskType))
}

func TestSystemTaskClaimPassDispatchesByType(t *testing.T) {
	truncate(t)

	ran := make(chan stubSystemTaskRunResult, 1)
	handler := &stubScheduledHandler{
		taskType: "test_dispatch",
		enabled:  true,
		interval: time.Minute,
		onRun: func(_ context.Context, task *model.SystemTask, runnerID string) {
			ran <- stubSystemTaskRunResult{
				taskType: task.Type,
				err:      model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, nil, ""),
			}
		},
	}
	withSystemTaskRegistry(t, handler)

	_, err := model.CreateSystemTask(handler.taskType, nil, nil)
	require.NoError(t, err)

	runSystemTaskClaimPass("runner-dispatch")

	select {
	case got := <-ran:
		require.NoError(t, got.err)
		assert.Equal(t, handler.taskType, got.taskType)
	case <-time.After(2 * time.Second):
		t.Fatal("claimed task was not dispatched to its handler")
	}

	require.Eventually(t, func() bool {
		latest, err := model.GetLatestSystemTask(handler.taskType)
		return err == nil && latest != nil && latest.Status == model.SystemTaskStatusSucceeded
	}, 2*time.Second, 20*time.Millisecond)
}

func TestSystemTaskClaimPassDispatchesEarliestPendingByType(t *testing.T) {
	truncate(t)

	ran := make(chan stubSystemTaskRunResult, 2)
	handlerA := &stubScheduledHandler{
		taskType: "test_dispatch_a",
		enabled:  true,
		interval: time.Minute,
		onRun: func(_ context.Context, task *model.SystemTask, runnerID string) {
			ran <- stubSystemTaskRunResult{
				taskID: task.TaskID,
				err:    model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, nil, ""),
			}
		},
	}
	handlerB := &stubScheduledHandler{
		taskType: "test_dispatch_b",
		enabled:  true,
		interval: time.Minute,
		onRun: func(_ context.Context, task *model.SystemTask, runnerID string) {
			ran <- stubSystemTaskRunResult{
				taskID: task.TaskID,
				err:    model.FinishSystemTask(task.TaskID, runnerID, model.SystemTaskStatusSucceeded, nil, ""),
			}
		},
	}
	withSystemTaskRegistry(t, handlerA, handlerB)

	firstA, err := model.CreateSystemTask(handlerA.taskType, nil, nil)
	require.NoError(t, err)
	secondTaskID, err := model.GenerateSystemTaskID()
	require.NoError(t, err)
	secondA := &model.SystemTask{
		TaskID: secondTaskID,
		Type:   handlerA.taskType,
		Status: model.SystemTaskStatusPending,
	}
	require.NoError(t, model.DB.Create(secondA).Error)
	firstB, err := model.CreateSystemTask(handlerB.taskType, nil, nil)
	require.NoError(t, err)

	runSystemTaskClaimPass("runner-dispatch")

	got := map[string]bool{}
	for range 2 {
		select {
		case result := <-ran:
			require.NoError(t, result.err)
			got[result.taskID] = true
		case <-time.After(2 * time.Second):
			t.Fatal("claimed tasks were not dispatched to their handlers")
		}
	}

	assert.True(t, got[firstA.TaskID])
	assert.True(t, got[firstB.TaskID])
	assert.False(t, got[secondA.TaskID])

	require.Eventually(t, func() bool {
		reloaded, err := model.GetSystemTaskByTaskID(secondA.TaskID)
		return err == nil && reloaded != nil && reloaded.Status == model.SystemTaskStatusPending
	}, 2*time.Second, 20*time.Millisecond)
}

func TestEnqueueSystemTaskReportsCreatedAndExistingActive(t *testing.T) {
	truncate(t)

	first, created, err := EnqueueSystemTask("test_enqueue", map[string]bool{"manual": true})
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, first)

	existing, created, err := EnqueueSystemTask("test_enqueue", nil)
	require.NoError(t, err)
	require.False(t, created)
	require.NotNil(t, existing)
	assert.Equal(t, first.TaskID, existing.TaskID)

	_, claimed, err := model.ClaimSystemTask(first.ID, first.Type, "runner-a", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, model.FinishSystemTask(first.TaskID, "runner-a", model.SystemTaskStatusSucceeded, nil, ""))

	second, created, err := EnqueueSystemTask("test_enqueue", nil)
	require.NoError(t, err)
	require.True(t, created)
	require.NotNil(t, second)
	assert.NotEqual(t, first.TaskID, second.TaskID)
}

// ---------------------------------------------------------------------------
// Contribution liveness probe
// ---------------------------------------------------------------------------

// seedProbeHostChannel inserts the multi-key host channel a contribution is
// pooled in, already carrying the contributed key at index 1. Polling mode makes
// key selection deterministic for the assertions below.
func seedProbeHostChannel(t *testing.T, contributedKey string) *model.Channel {
	t.Helper()
	channel := &model.Channel{
		Name:   t.Name(),
		Type:   1,
		Key:    "host-key-one\n" + contributedKey,
		Status: common.ChannelStatusEnabled,
		Models: "gpt-4o-mini",
		Group:  "default",
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       2,
			MultiKeyMode:       constant.MultiKeyModePolling,
			MultiKeyStatusList: map[int]int{},
		},
	}
	require.NoError(t, model.DB.Create(channel).Error)
	return channel
}

// seedProbeContribution records an active contribution whose plaintext key really
// sits in the host channel, so the probe can resolve it from the fingerprint.
func seedProbeContribution(t *testing.T, userId int, host *model.Channel, contributedKey string, subscriptionId int) *model.Contribution {
	t.Helper()
	contribution := &model.Contribution{
		UserId:         userId,
		ChannelType:    1,
		HostChannelId:  host.Id,
		KeyFingerprint: common.GetPointer(model.ContributionKeyFingerprint(host.GetBaseURL(), contributedKey)),
		KeyMask:        model.MaskContributionKey(contributedKey),
		SubscriptionId: subscriptionId,
		Status:         model.ContributionStatusActive,
		RewardGranted:  subscriptionId > 0,
	}
	require.NoError(t, contribution.Create())
	return contribution
}

// stubContributionProbe swaps the injected single-key probe for the duration of a
// test: the real one performs an outbound HTTP call to a third-party upstream.
func stubContributionProbe(t *testing.T, statusCode int) {
	t.Helper()
	previous := ContributionKeyProbeFunc
	ContributionKeyProbeFunc = func(string, *model.Channel) (int, error) { return statusCode, nil }
	t.Cleanup(func() { ContributionKeyProbeFunc = previous })
}

// 403, a timeout or transport failure, 5xx and a successful query that reports
// zero balance are all alive: only 401 proves the credential itself was rejected,
// and the balance is display-only.
func TestProbeContributionLivenessKeepsNonFatalOutcomesAlive(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
	}{
		{"forbidden", http.StatusForbidden},
		{"server error", http.StatusInternalServerError},
		{"timeout or transport failure", 0},
		{"successful query with zero balance", http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			truncate(t)
			const contributedKey = "sk-probe-alive"
			host := seedProbeHostChannel(t, contributedKey)
			record := seedProbeContribution(t, 501, host, contributedKey, 0)
			stubContributionProbe(t, test.statusCode)

			require.NoError(t, ProbeContributionLiveness(context.Background()))

			stored, err := model.GetContributionByFingerprint(*record.KeyFingerprint)
			require.NoError(t, err)
			assert.Equal(t, model.ContributionStatusActive, stored.Status)
			reloaded, err := model.GetChannelById(host.Id, true)
			require.NoError(t, err)
			assert.Equal(t, common.ChannelStatusEnabled, reloaded.GetMultiKeyStatus(1), "a non-fatal outcome must not disable the key")
		})
	}
}

// TestProbeContributionLivenessKillsUnauthorizedKey walks the whole death
// consequence: the record turns terminal, the key is auto-disabled in its host
// channel, the reward subscription is cancelled, the kill is audited and the
// contributor is notified through the existing per-user notification channel.
//
// The second scenario exhausts the notification limit gate, which must never undo
// a kill that already happened.
func TestProbeContributionLivenessKillsUnauthorizedKey(t *testing.T) {
	require.NoError(t, i18n.Init())

	for _, scenario := range []struct {
		name         string
		notifyLimit  int
		wantNotified bool
	}{
		{"the contributor is notified", 10, true},
		{"a refused notice never undoes the kill", 0, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			truncate(t)
			previousNotifyLimit := constant.NotifyLimitCount
			constant.NotifyLimitCount = scenario.notifyLimit
			t.Cleanup(func() { constant.NotifyLimitCount = previousNotifyLimit })

			// The webhook receiver below listens on a loopback port, which the SSRF
			// guard refuses by design; the guard is the transport's business, not the
			// notification's, so it is turned off for this fixture, exactly like
			// relay_error_test.go does.
			fetch := system_setting.GetFetchSetting()
			previousFetch := *fetch
			fetch.EnableSSRFProtection = false
			previousClient := httpClient
			t.Cleanup(func() {
				*fetch = previousFetch
				httpClient = previousClient
			})

			notices := make(chan WebhookPayload, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var payload WebhookPayload
				if err := common.Unmarshal(body, &payload); err == nil {
					notices <- payload
				}
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(upstream.Close)
			httpClient = upstream.Client()

			const contributedKey = "sk-probe-dead"
			host := seedProbeHostChannel(t, contributedKey)

			userSetting, err := common.Marshal(dto.UserSetting{
				NotifyType: dto.NotifyTypeWebhook,
				WebhookUrl: upstream.URL,
				Language:   "zh-CN",
			})
			require.NoError(t, err)
			contributor := &model.User{
				Id:          601,
				Username:    "probe-contributor",
				Status:      common.UserStatusEnabled,
				Group:       "default",
				AuthVersion: 1,
				Setting:     string(userSetting),
			}
			require.NoError(t, model.DB.Create(contributor).Error)

			now := common.GetTimestamp()
			subscription := &model.UserSubscription{
				Id:          9801,
				UserId:      contributor.Id,
				PlanId:      1,
				AmountTotal: 1000,
				StartTime:   now,
				EndTime:     now + 30*24*3600,
				Status:      "active",
				Source:      model.ContributionRewardSource,
			}
			require.NoError(t, model.DB.Create(subscription).Error)
			record := seedProbeContribution(t, contributor.Id, host, contributedKey, subscription.Id)

			stubContributionProbe(t, http.StatusUnauthorized)

			require.NoError(t, ProbeContributionLiveness(context.Background()))

			stored, err := model.GetContributionByFingerprint(*record.KeyFingerprint)
			require.NoError(t, err)
			assert.Equal(t, model.ContributionStatusDead, stored.Status)
			assert.Equal(t, model.ContributionReasonUpstreamUnauthorized, stored.Reason)

			reloaded, err := model.GetChannelById(host.Id, true)
			require.NoError(t, err)
			assert.Equal(t, "host-key-one\n"+contributedKey, reloaded.Key, "a dead key is disabled, never deleted")
			assert.Equal(t, common.ChannelStatusAutoDisabled, reloaded.GetMultiKeyStatus(1))
			assert.Equal(t, model.ContributionReasonUpstreamUnauthorized, reloaded.ChannelInfo.MultiKeyDisabledReason[1])

			cancelled, err := model.GetUserSubscriptionById(subscription.Id)
			require.NoError(t, err)
			assert.Equal(t, "cancelled", cancelled.Status)
			assert.LessOrEqual(t, cancelled.EndTime, common.GetTimestamp())

			// One audit row for the contributor, naming the contribution, its channel
			// type and its host channel - and never the key.
			var audits []model.AuditLog
			require.NoError(t, model.LOG_DB.Table("audit_logs").Where("user_id = ?", contributor.Id).Find(&audits).Error)
			require.Len(t, audits, 1)
			require.NotNil(t, audits[0].Other.Op)
			assert.Equal(t, "contribution.kill", audits[0].Other.Op.Action)
			auditJSON, err := common.Marshal(audits[0])
			require.NoError(t, err)
			assert.Contains(t, string(auditJSON), fmt.Sprintf(`"contribution_id":%d`, record.Id))
			assert.Contains(t, string(auditJSON), fmt.Sprintf(`"channel_type":%d`, 1))
			assert.Contains(t, string(auditJSON), fmt.Sprintf(`"host_channel_id":%d`, host.Id))
			assert.NotContains(t, string(auditJSON), contributedKey)

			if scenario.wantNotified {
				select {
				case notice := <-notices:
					assert.Equal(t, dto.NotifyTypeChannelUpdate, notice.Type)
					assert.Equal(t, i18n.Translate("zh-CN", i18n.MsgContributionKeyDeadTitle), notice.Title, "the notice is localized to the contributor's language")
					assert.NotEmpty(t, notice.Content)
					assert.NotEqual(t, i18n.MsgContributionKeyDeadContent, notice.Content, "the content must be the translated message, not the key")
					assert.NotContains(t, notice.Content, contributedKey)
				case <-time.After(2 * time.Second):
					t.Fatal("the contributor was not notified about the dead key")
				}
			} else {
				select {
				case <-notices:
					t.Fatal("a notification refused by the limit gate must not be delivered")
				case <-time.After(100 * time.Millisecond):
				}
			}

			// A second pass finds nothing: the record already left the active set, so
			// nothing is disabled twice and no second audit row is written.
			require.NoError(t, ProbeContributionLiveness(context.Background()))
			var auditCount int64
			require.NoError(t, model.LOG_DB.Table("audit_logs").Where("user_id = ?", contributor.Id).Count(&auditCount).Error)
			assert.EqualValues(t, 1, auditCount)
		})
	}
}

// The system task framework runs one task per type across master instances: the
// per-type database lease lets the first instance run and makes a second instance
// skip the already-claimed run.
func TestContributionProbeTaskLeaseSkipsASecondInstance(t *testing.T) {
	truncate(t)

	task, err := model.CreateSystemTask(model.SystemTaskTypeContributionProbe, nil, nil)
	require.NoError(t, err)

	claimedTask, claimed, err := model.ClaimSystemTask(task.ID, model.SystemTaskTypeContributionProbe, "runner-a", common.GetTimestamp()+60)
	require.NoError(t, err)
	require.True(t, claimed)
	require.NotNil(t, claimedTask)

	_, claimed, err = model.ClaimSystemTask(task.ID, model.SystemTaskTypeContributionProbe, "runner-b", common.GetTimestamp()+60)
	require.NoError(t, err)
	assert.False(t, claimed, "the per-type lease already belongs to another instance")

	// Finishing the run ends claimability too: the row is terminal, not pending.
	require.NoError(t, model.FinishSystemTask(task.TaskID, "runner-a", model.SystemTaskStatusSucceeded, nil, ""))
	_, claimed, err = model.ClaimSystemTask(task.ID, model.SystemTaskTypeContributionProbe, "runner-b", common.GetTimestamp()+60)
	require.NoError(t, err)
	assert.False(t, claimed)
}
