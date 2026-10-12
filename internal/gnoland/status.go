package gnoland

import (
	"context"
	"fmt"
	"strconv"
)

// tmStatusResponse models the subset of the /status RPC response katana needs.
type tmStatusResponse struct {
	Result struct {
		NodeInfo struct {
			Network string `json:"network"`
		} `json:"node_info"`
		SyncInfo struct {
			LatestBlockHeight string `json:"latest_block_height"`
		} `json:"sync_info"`
	} `json:"result"`
	Error *struct {
		Message string `json:"message"`
		Data    string `json:"data"`
	} `json:"error"`
}

// Network returns the chain id the node reports (/status node_info.network).
// katana compares it against the configured chain_id before broadcasting, so a
// genesis reset (same endpoint, new network id) is detected instead of
// producing signature failures — or worse, transactions on the wrong chain.
func (c *Client) Network(ctx context.Context) (string, error) {
	var body tmStatusResponse
	if err := c.getJSON(ctx, "/status", nil, &body); err != nil {
		return "", err
	}
	if body.Error != nil {
		return "", fmt.Errorf("RPC error: %s (%s)", body.Error.Message, body.Error.Data)
	}
	if body.Result.NodeInfo.Network == "" {
		return "", fmt.Errorf("GET %s/status: node reported no node_info.network", c.rpcURL)
	}
	return body.Result.NodeInfo.Network, nil
}

// LatestHeight returns the node's latest block height
// (/status sync_info.latest_block_height). katana records it just before a
// broadcast so a tx whose hash was lost can later be searched for from there.
func (c *Client) LatestHeight(ctx context.Context) (int64, error) {
	var body tmStatusResponse
	if err := c.getJSON(ctx, "/status", nil, &body); err != nil {
		return 0, err
	}
	if body.Error != nil {
		return 0, fmt.Errorf("RPC error: %s (%s)", body.Error.Message, body.Error.Data)
	}
	h, err := strconv.ParseInt(body.Result.SyncInfo.LatestBlockHeight, 10, 64)
	if err != nil || h <= 0 {
		return 0, fmt.Errorf("GET %s/status: invalid sync_info.latest_block_height %q", c.rpcURL, body.Result.SyncInfo.LatestBlockHeight)
	}
	return h, nil
}
