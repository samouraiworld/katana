package proposal

import (
	"context"
	"errors"
	"strconv"
	"strings"
)

// TxResult is what katana reads back from a `gnokey maketx ... -broadcast`
// run (gno.land/pkg/keyscli PrintTxSuccess / PrintTxMetrics).
type TxResult struct {
	OK     bool   // "OK!" printed: DeliverTx succeeded
	Height int64  // 0 when nothing was committed (e.g. a failed simulation)
	TxHash string // base64; empty when gnokey printed none
	Events string // raw EVENTS JSON array
}

// ParseTxOutput extracts the transaction metrics from gnokey's combined output.
func ParseTxOutput(out string) TxResult {
	var r TxResult
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "OK!" {
			r.OK = true
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "HEIGHT":
			r.Height, _ = strconv.ParseInt(val, 10, 64)
		case "TX HASH":
			r.TxHash = val
		case "EVENTS":
			r.Events = val
		}
	}
	return r
}

// Outcome classifies a broadcast attempt for the lifecycle record.
type Outcome string

const (
	// Committed: gnokey reported a successful DeliverTx.
	Committed Outcome = "committed"
	// Failed: the tx definitively created nothing — it was never sent
	// (simulation, CheckTx or a local error) or it was included but its
	// DeliverTx failed, which reverts every state change.
	Failed Outcome = "failed"
	// Ambiguous: gnokey failed after handing the tx to the node (RPC timeout,
	// dropped connection, interrupted run). The tx may still be committed.
	Ambiguous Outcome = "ambiguous"
)

// broadcastTransportError is the wrap tm2's SignAndBroadcastHandler puts on a
// failed broadcast_tx_commit RPC call: the only gnokey error raised after the
// tx bytes were sent to the node.
const broadcastTransportError = "broadcasting bytes"

// Classify maps a Broadcast result to an Outcome. It fails closed: an error it
// cannot prove happened before the tx reached the node is Ambiguous.
func Classify(out string, err error) Outcome {
	if err == nil {
		return Committed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(out, broadcastTransportError) {
		return Ambiguous
	}
	return Failed
}
