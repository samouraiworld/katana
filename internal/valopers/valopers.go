// Package valopers resolves a validator's operator address from its consensus
// signing address by reading the gno.land valopers realm.
//
// gnomonitoring keys metrics by the signing/consensus address, but a valset
// ejection proposal (r/sys/validators/v3) is keyed by the operator address.
// This resolver bridges the two, mirroring gnomonitoring's valoper.go: the
// valopers list render exposes operator addresses; each valoper profile render
// exposes its "Signing Address".
package valopers

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	// listRe matches `[Moniker](/r/gnops/valopers:operatorAddr)` in the list render.
	listRe = regexp.MustCompile(`\[\s*([^\]]+?)\s*]\(/r/gnops/valopers:([a-z0-9]+)\)`)
	// signingRe matches `Signing Address: g1...` in a profile render.
	signingRe = regexp.MustCompile(`Signing Address:\s*(g1[a-z0-9]+)`)
)

// Resolver caches a signing→operator map, refreshed on a TTL.
type Resolver struct {
	rpcURL string
	realm  string
	ttl    time.Duration
	http   *http.Client

	mu        sync.Mutex
	bySigning map[string]string // signing address → operator address
	fetchedAt time.Time
}

// New builds a Resolver for the given RPC endpoint and valopers realm path.
func New(rpcURL, realm string, ttl time.Duration) *Resolver {
	return &Resolver{
		rpcURL:    strings.TrimRight(rpcURL, "/"),
		realm:     realm,
		ttl:       ttl,
		http:      &http.Client{Timeout: 15 * time.Second},
		bySigning: map[string]string{},
	}
}

// OperatorFor returns the operator address for a signing address, refreshing the
// cache when stale. The bool is false when no mapping is known.
func (r *Resolver) OperatorFor(ctx context.Context, signingAddr string) (string, bool) {
	r.mu.Lock()
	stale := time.Since(r.fetchedAt) > r.ttl
	r.mu.Unlock()

	if stale {
		if err := r.refresh(ctx); err != nil {
			// Serve the stale cache on refresh failure rather than nothing.
			r.mu.Lock()
			op, ok := r.bySigning[signingAddr]
			r.mu.Unlock()
			return op, ok
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	op, ok := r.bySigning[signingAddr]
	return op, ok
}

// refresh rebuilds the signing→operator map from the realm renders.
func (r *Resolver) refresh(ctx context.Context) error {
	operators, err := r.listOperators(ctx)
	if err != nil {
		return err
	}
	next := make(map[string]string, len(operators))
	for _, op := range operators {
		signing, err := r.signingOf(ctx, op)
		if err != nil || signing == "" {
			continue // best-effort per valoper
		}
		next[signing] = op
	}
	r.mu.Lock()
	r.bySigning = next
	r.fetchedAt = time.Now()
	r.mu.Unlock()
	return nil
}

// listOperators pages through the valopers list and returns operator addresses.
func (r *Resolver) listOperators(ctx context.Context) ([]string, error) {
	var ops []string
	for page := 1; page <= 50; page++ {
		md, err := r.render(ctx, fmt.Sprintf("?page=%d", page))
		if err != nil {
			return nil, err
		}
		matches := listRe.FindAllStringSubmatch(md, -1)
		if len(matches) == 0 {
			break
		}
		for _, m := range matches {
			ops = append(ops, m[2])
		}
	}
	return ops, nil
}

// signingOf returns the Signing Address declared by an operator's profile.
func (r *Resolver) signingOf(ctx context.Context, operatorAddr string) (string, error) {
	md, err := r.render(ctx, operatorAddr)
	if err != nil {
		return "", err
	}
	if m := signingRe.FindStringSubmatch(md); len(m) == 2 {
		return m[1], nil
	}
	return "", nil
}

// render evaluates `<realm>.Render(arg)` over RPC vm/qeval and returns the output.
func (r *Resolver) render(ctx context.Context, arg string) (string, error) {
	cmd := fmt.Sprintf("%s.Render(%q)", r.realm, arg)

	q := url.Values{}
	q.Set("path", `"vm/qeval"`)
	q.Set("data", "0x"+hex.EncodeToString([]byte(cmd)))

	var body struct {
		Result struct {
			Response struct {
				// gno's RPC abci_query nests the result under ResponseBase.Data
				// (base64), not a flat "value" field.
				ResponseBase struct {
					Data  string `json:"Data"`
					Error *struct {
						Message string `json:"message"`
					} `json:"Error"`
				} `json:"ResponseBase"`
			} `json:"response"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	endpoint := r.rpcURL + "/abci_query?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.Error != nil {
		return "", fmt.Errorf("RPC error: %s", body.Error.Message)
	}
	if e := body.Result.Response.ResponseBase.Error; e != nil {
		return "", fmt.Errorf("abci error: %s", e.Message)
	}
	raw, err := base64.StdEncoding.DecodeString(body.Result.Response.ResponseBase.Data)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
