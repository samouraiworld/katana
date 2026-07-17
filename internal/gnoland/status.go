package gnoland

import (
	"context"
	"fmt"
)

// tmStatusResponse models the subset of the /status RPC response katana needs.
type tmStatusResponse struct {
	Result struct {
		NodeInfo struct {
			Network string `json:"network"`
		} `json:"node_info"`
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
