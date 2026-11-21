package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// HostCaller performs one host callback over the C ABI. cmd/opencodepool
// provides the CGO implementation; tests provide fakes.
type HostCaller interface {
	CallHost(method string, payload []byte) ([]byte, error)
}

// hostHTTPReq is the wire form for host.http.do / host.http.do_stream.
// HostCallbackID scopes the upstream operation to the client request context
// so client cancellation propagates (host falls back to Background when
// empty — http_operation_bridge.go:369).
type hostHTTPReq struct {
	HostCallbackID string      `json:"host_callback_id,omitempty"`
	Method         string      `json:"method"`
	URL            string      `json:"url"`
	Headers        http.Header `json:"headers"`
	Body           []byte      `json:"body,omitempty"`
}

type hostStreamStartResp struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

type hostStreamReadResp struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type hostStreamIDReq struct {
	StreamID string `json:"stream_id"`
}

// hostPluginStreamEmitReq is the wire form for host.stream.emit / close
// (internal/pluginhost: stream_id + payload + error + structured error_detail).
type hostPluginStreamEmitReq struct {
	StreamID    string           `json:"stream_id"`
	Payload     []byte           `json:"payload,omitempty"`
	Error       string           `json:"error,omitempty"`
	ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
}

type hostAuthListResp struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

type hostAuthSaveReq struct {
	Auth pluginapi.AuthData `json:"auth"`
}

// Bridge wraps host callbacks with envelope handling and shutdown draining.
type Bridge struct {
	caller   HostCaller
	inFlight sync.WaitGroup
}

// NewBridge builds a Bridge over a HostCaller.
func NewBridge(c HostCaller) *Bridge { return &Bridge{caller: c} }

func decodeEnvelope(raw []byte) (pluginabi.Envelope, error) {
	var env pluginabi.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, fmt.Errorf("undecodable host response")
	}
	return env, nil
}

func (b *Bridge) invoke(method string, payload any, what string) (pluginabi.Envelope, error) {
	raw, _ := json.Marshal(payload)
	b.inFlight.Add(1)
	defer b.inFlight.Done()
	resp, err := b.caller.CallHost(method, raw)
	if err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %w", what, err)
	}
	env, err := decodeEnvelope(resp)
	if err != nil {
		return pluginabi.Envelope{}, fmt.Errorf("%s failed: %w", what, err)
	}
	if !env.OK {
		msg := ""
		if env.Error != nil {
			msg = env.Error.Message
		}
		return env, fmt.Errorf("%s failed: %s", what, msg)
	}
	return env, nil
}

// WaitForInFlight drains callbacks for shutdown.
func (b *Bridge) WaitForInFlight(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() { b.inFlight.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Do issues one upstream HTTP request through host.http.do.
func (b *Bridge) Do(ctx context.Context, req pluginapi.HTTPRequest, callbackID string) (pluginapi.HTTPResponse, error) {
	_ = ctx // host call has no per-call deadline channel in this ABI shape
	env, err := b.invoke(pluginabi.MethodHostHTTPDo, hostHTTPReq{
		HostCallbackID: callbackID, Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body,
	}, "host http do")
	if err != nil {
		return pluginapi.HTTPResponse{}, err
	}
	var out pluginapi.HTTPResponse
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &out); err != nil {
			return pluginapi.HTTPResponse{}, fmt.Errorf("host http do failed: undecodable response")
		}
	}
	return out, nil
}

// DoStream opens an upstream stream and returns (status, headers, streamID).
func (b *Bridge) DoStream(ctx context.Context, req pluginapi.HTTPRequest, callbackID string) (int, http.Header, string, error) {
	_ = ctx
	env, err := b.invoke(pluginabi.MethodHostHTTPDoStream, hostHTTPReq{
		HostCallbackID: callbackID, Method: req.Method, URL: req.URL, Headers: req.Headers, Body: req.Body,
	}, "host http do_stream")
	if err != nil {
		return 0, nil, "", err
	}
	var out hostStreamStartResp
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &out); err != nil {
			return 0, nil, "", fmt.Errorf("host http do_stream failed: undecodable response")
		}
	}
	return out.StatusCode, out.Headers, out.StreamID, nil
}

// StreamRead reads one chunk from an upstream stream.
func (b *Bridge) StreamRead(streamID string) ([]byte, string, bool, error) {
	env, err := b.invoke(pluginabi.MethodHostHTTPStreamRead, hostStreamIDReq{StreamID: streamID}, "host stream read")
	if err != nil {
		return nil, "", false, err
	}
	var out hostStreamReadResp
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &out); err != nil {
			return nil, "", false, fmt.Errorf("host stream read failed: undecodable response")
		}
	}
	return out.Payload, out.Error, out.Done, nil
}

// StreamClose releases an upstream stream.
func (b *Bridge) StreamClose(streamID string) error {
	_, err := b.invoke(pluginabi.MethodHostHTTPStreamClose, hostStreamIDReq{StreamID: streamID}, "host stream close")
	return err
}

// EmitStream pushes one chunk to the host-side client stream via
// host.stream.emit (async streaming mode).
func (b *Bridge) EmitStream(streamID string, payload []byte) error {
	_, err := b.invoke(pluginabi.MethodHostStreamEmit, hostPluginStreamEmitReq{StreamID: streamID, Payload: payload}, "host stream emit")
	return err
}

// FinishStream closes the host-side client stream via host.stream.close;
// a non-empty error/detail reports a mid-stream failure to the client.
func (b *Bridge) FinishStream(streamID string, errMsg string, detail *pluginabi.Error) {
	_, _ = b.invoke(pluginabi.MethodHostStreamClose, hostPluginStreamEmitReq{
		StreamID: streamID, Error: errMsg, ErrorDetail: detail,
	}, "host stream finish")
}

// AuthList returns the host auth file entries.
func (b *Bridge) AuthList() ([]pluginapi.HostAuthFileEntry, error) {
	env, err := b.invoke(pluginabi.MethodHostAuthList, map[string]any{}, "host auth list")
	if err != nil {
		return nil, err
	}
	var out hostAuthListResp
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &out); err != nil {
			return nil, fmt.Errorf("host auth list failed: undecodable response")
		}
	}
	return out.Files, nil
}

// AuthSave materializes an auth record on the host.
func (b *Bridge) AuthSave(auth pluginapi.AuthData) error {
	_, err := b.invoke(pluginabi.MethodHostAuthSave, hostAuthSaveReq{Auth: auth}, "host auth save")
	return err
}

// ResetCooldown clears host-side cooldown state for an auth.
func (b *Bridge) ResetCooldown(authID string) error {
	_, err := b.invoke(pluginabi.MethodHostRoutingResetCooldown, map[string]string{"auth_id": authID}, "host routing reset cooldown")
	return err
}

// Log writes a plugin log line through the host logger.
func (b *Bridge) Log(level, message string) {
	payload := map[string]any{"level": level, "message": message, "fields": map[string]any{"plugin_id": PluginID}}
	_, _ = b.invoke(pluginabi.MethodHostLog, payload, "host log")
}

// AuthGet returns one auth document's raw storage JSON.
func (b *Bridge) AuthGet(authID string) ([]byte, error) {
	env, err := b.invoke(pluginabi.MethodHostAuthGet, map[string]string{"auth_id": authID}, "host auth get")
	if err != nil {
		return nil, err
	}
	var out struct {
		Auth pluginapi.AuthData `json:"auth"`
	}
	if len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, &out); err != nil {
			return nil, fmt.Errorf("host auth get failed: undecodable response")
		}
	}
	return out.Auth.StorageJSON, nil
}
