package controller

import (
	"errors"
	"net/http"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
)

// init hands the single-key probe to the service-layer liveness orchestrator. The
// dependency points this way because the reverse one is an import cycle:
// controller already imports service, so the scheduler's per-key walk lives in
// service and reaches the channel-type balance dispatch through this function.
func init() {
	service.ContributionKeyProbeFunc = func(key string, hostChannel *model.Channel) (int, error) {
		result := probeContributionKey(key, hostChannel)
		return result.StatusCode, result.Err
	}
}

// contributionProbeResult is what a single-key liveness probe observed.
type contributionProbeResult struct {
	// StatusCode is the HTTP status the upstream balance endpoint returned.
	// 0 means no HTTP response was observed at all: a transport failure or
	// timeout, or a channel type that has no balance query implemented.
	StatusCode int
	// Balance is display-only and is never an input to
	// model.IsContributionKeyDead.
	Balance    float64
	HasBalance bool
	Err        error
}

// singleKeyProbeChannel returns a throwaway copy of the host channel that
// queries exactly one key, so the channel-type balance dispatch can be reused
// per key.
//
// The copy is what makes the probe safe and per-key:
//   - Id is zeroed. The balance paths call channel.UpdateBalance, and
//     Channel.GetSetting/GetOtherSettings call channel.Save when a persisted
//     JSON blob is malformed; both write through to the row named by the
//     primary key. With no id there is no production row to overwrite, and GORM
//     refuses the write outright ("WHERE conditions required").
//   - ChannelInfo is reset to a single-key shape: IsMultiKey false, multi-key
//     size and polling cursor zero, and the per-index status/reason/time maps
//     dropped. Balance queries read channel.Key directly, but a copy that still
//     looked like a multi-key channel would let GetNextEnabledKey pick a
//     different key from the host channel's list.
//   - Keys is cleared. It is a cache-only field, so a channel taken from the
//     cache can carry the host channel's entire key list.
//
// The caller keeps ownership of hostChannel: this never writes through that
// pointer, which is essential because callers hand in pointers that come from
// the shared channel cache (model.CacheGetChannel).
func singleKeyProbeChannel(hostChannel *model.Channel, key string) *model.Channel {
	probeChannel := *hostChannel
	probeChannel.Id = 0
	probeChannel.Key = key
	probeChannel.ChannelInfo = model.ChannelInfo{}
	probeChannel.Keys = nil
	return &probeChannel
}

// probeContributionKey probes one contributed upstream key by running the
// host channel's channel-type balance query against a single-key copy of it.
//
// It deliberately does not go through UpdateChannelBalance (the admin
// balance handler): that entry point refuses multi-key channels, and the host
// channel of a contribution always is one. It also performs no inference
// request of any kind - only the GET-based balance endpoints that
// updateChannelBalance already dispatches to.
//
// The result carries the upstream status plus the balance for display; death is
// decided exclusively by model.IsContributionKeyDead on StatusCode.
func probeContributionKey(key string, hostChannel *model.Channel) contributionProbeResult {
	probeChannel := singleKeyProbeChannel(hostChannel, key)
	result, err := updateChannelBalance(probeChannel)
	if err != nil {
		statusCode := 0
		var statusErr *UpstreamStatusError
		if errors.As(err, &statusErr) {
			statusCode = statusErr.StatusCode
		}
		return contributionProbeResult{StatusCode: statusCode, Err: err}
	}
	// An empty RawResponse means the dispatch reduced the answer to a number.
	// A non-empty one means the upstream answered 200 with a payload it could
	// not turn into a balance (advanced custom channels): still alive, but
	// nothing to display.
	return contributionProbeResult{
		StatusCode: http.StatusOK,
		Balance:    result.Balance,
		HasBalance: result.RawResponse == "",
	}
}
