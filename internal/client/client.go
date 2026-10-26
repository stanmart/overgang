// Package client is a minimal TRÅDFRI gateway client. It works against the real gateway
// and against ÖVERGÅNG, which makes it the tool for comparing the two.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	piondtls "github.com/pion/dtls/v3"
	"github.com/plgd-dev/go-coap/v3/dtls"
	"github.com/plgd-dev/go-coap/v3/message"
	"github.com/plgd-dev/go-coap/v3/message/codes"
	"github.com/plgd-dev/go-coap/v3/message/pool"
	"github.com/plgd-dev/go-coap/v3/options"
	udpClient "github.com/plgd-dev/go-coap/v3/udp/client"

	t "github.com/stanmart/overgang/internal/tradfri"
)

type Client struct {
	conn *udpClient.Conn
}

// Dial connects with an identity and its key.
func Dial(ctx context.Context, addr, identity, psk string) (*Client, error) {
	cfg := &piondtls.Config{
		PSK:                  func([]byte) ([]byte, error) { return []byte(psk), nil },
		PSKIdentityHint:      []byte(identity),
		CipherSuites:         []piondtls.CipherSuiteID{piondtls.TLS_PSK_WITH_AES_128_CCM_8},
		ExtendedMasterSecret: piondtls.RequestExtendedMasterSecret,
	}
	conn, err := dtls.Dial(addr, cfg, options.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("dial %s as %s: %w", addr, identity, err)
	}
	return &Client{conn: conn}, nil
}

// Pair connects with the security code and asks the gateway for a key for identity.
func Pair(ctx context.Context, addr, securityCode, identity string) (string, error) {
	return PairAs(ctx, addr, t.BootstrapIdentity, securityCode, identity)
}

// PairAs asks for a key for identity over a session authenticated as asIdentity with
// key. Only the bootstrap identity may pair; this exists to test that.
func PairAs(ctx context.Context, addr, asIdentity, key, identity string) (string, error) {
	c, err := Dial(ctx, addr, asIdentity, key)
	if err != nil {
		return "", err
	}
	defer c.Close()
	body, _ := json.Marshal(map[string]string{t.AttrIdentity: identity})
	resp, err := c.conn.Post(ctx, "/"+t.PathGateway+"/"+t.PathAuth, message.AppJSON, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	data, err := payload(resp)
	if err != nil {
		return "", err
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("pairing response %q: %w", data, err)
	}
	psk, _ := out[t.AttrPSK].(string)
	if psk == "" {
		return "", fmt.Errorf("pairing response has no key: %s", data)
	}
	return psk, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Get returns a resource's raw JSON.
func (c *Client) Get(ctx context.Context, path string) ([]byte, error) {
	resp, err := c.conn.Get(ctx, "/"+path)
	if err != nil {
		return nil, err
	}
	return payload(resp)
}

// Put writes JSON to a resource.
func (c *Client) Put(ctx context.Context, path string, body []byte) error {
	resp, err := c.conn.Put(ctx, "/"+path, message.AppJSON, bytes.NewReader(body))
	if err != nil {
		return err
	}
	_, err = payload(resp)
	return err
}

// Observe calls f with each new state of a resource until ctx is done.
func (c *Client) Observe(ctx context.Context, path string, f func([]byte)) error {
	obs, err := c.conn.Observe(ctx, "/"+path, func(m *pool.Message) {
		if b, err := m.ReadBody(); err == nil {
			f(b)
		}
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	return obs.Cancel(context.Background())
}

// Dump reads every resource the app would, keyed by path.
func (c *Client) Dump(ctx context.Context) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	get := func(path string) ([]byte, error) {
		b, err := c.Get(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out[path] = b
		return b, nil
	}
	list := func(path string) ([]int, error) {
		b, err := get(path)
		if err != nil {
			return nil, err
		}
		var ids []int
		return ids, json.Unmarshal(b, &ids)
	}

	if _, err := get(t.PathGateway + "/" + t.PathGatewayInfo); err != nil {
		return out, err
	}
	devices, err := list(t.PathDevices)
	if err != nil {
		return out, err
	}
	for _, id := range devices {
		if _, err := get(t.PathDevices + "/" + strconv.Itoa(id)); err != nil {
			return out, err
		}
	}
	groups, err := list(t.PathGroups)
	if err != nil {
		return out, err
	}
	for _, id := range groups {
		gp := strconv.Itoa(id)
		if _, err := get(t.PathGroups + "/" + gp); err != nil {
			return out, err
		}
		moods, err := list(t.PathMoods + "/" + gp)
		if err != nil {
			return out, err
		}
		for _, mid := range moods {
			if _, err := get(t.PathMoods + "/" + gp + "/" + strconv.Itoa(mid)); err != nil {
				return out, err
			}
		}
	}
	for _, path := range []string{t.PathSmartTasks, t.PathNotifications} {
		if _, err := get(path); err != nil {
			return out, err
		}
	}
	return out, nil
}

func payload(m *pool.Message) ([]byte, error) {
	b, _ := m.ReadBody()
	if m.Code() >= codes.BadRequest {
		return nil, fmt.Errorf("gateway answered %v %s", m.Code(), b)
	}
	return b, nil
}
