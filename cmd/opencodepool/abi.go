package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int OpenCodePoolPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void OpenCodePoolPluginFree(void*, size_t);
extern void OpenCodePoolPluginShutdown(void);

static int ocp_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}

// ocp_free_host releases a buffer the HOST allocated (LocalAlloc on Windows);
// freeing it with C.free would cross allocators (deep-check P0).
static void ocp_free_host(cliproxy_host_api* api, void* ptr, size_t len) {
	if (ptr != NULL && api->free_buffer != NULL) {
		api->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"cpa-plugin-opencode-pool/internal/plugin"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

var abiState = struct {
	sync.Mutex
	host         *C.cliproxy_host_api
	plugin       *plugin.Plugin
	shuttingDown bool
	inFlight     sync.WaitGroup
}{}

const stateDirRelative = "plugins/data/opencode-pool"

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, pluginAPI *C.cliproxy_plugin_api) C.int {
	if host == nil || pluginAPI == nil {
		return 1
	}
	abiState.Lock()
	abiState.host = host
	abiState.shuttingDown = false
	abiState.Unlock()

	pluginAPI.abi_version = C.uint32_t(pluginabi.ABIVersion)
	pluginAPI.call = C.cliproxy_plugin_call_fn(C.OpenCodePoolPluginCall)
	pluginAPI.free_buffer = C.cliproxy_plugin_free_fn(C.OpenCodePoolPluginFree)
	pluginAPI.shutdown = C.cliproxy_plugin_shutdown_fn(C.OpenCodePoolPluginShutdown)
	return 0
}

//export OpenCodePoolPluginCall
func OpenCodePoolPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelopeBytes("invalid_method", "method is required"))
		return 0
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(context.Background(), C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelopeBytes("plugin_error", errHandle.Error()))
		return 0
	}
	writeResponse(response, raw)
	return 0
}

//export OpenCodePoolPluginFree
func OpenCodePoolPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export OpenCodePoolPluginShutdown
func OpenCodePoolPluginShutdown() {
	abiState.Lock()
	abiState.shuttingDown = true
	p := abiState.plugin
	abiState.plugin = nil
	abiState.Unlock()
	if p != nil {
		p.Shutdown() // final state Flush
		if p.Bridge() != nil {
			p.Bridge().WaitForInFlight(5 * time.Second)
		}
	}
}

func handleMethod(_ context.Context, method string, request []byte) ([]byte, error) {
	if method == pluginabi.MethodPluginRegister || method == pluginabi.MethodPluginReconfigure {
		return handleLifecycle(method, request)
	}
	abiState.Lock()
	if abiState.shuttingDown || abiState.plugin == nil {
		abiState.Unlock()
		return nil, fmt.Errorf("opencode-pool plugin is not registered")
	}
	p := abiState.plugin
	abiState.inFlight.Add(1)
	abiState.Unlock()
	defer abiState.inFlight.Done()
	return p.HandleMethod(method, request)
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

func handleLifecycle(method string, request []byte) ([]byte, error) {
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err != nil {
			return nil, err
		}
	}
	abiState.Lock()
	host := abiState.host
	prev := abiState.plugin
	abiState.Unlock()

	caller := cgoCaller{host: host}
	dataDir := resolveStateDir()
	p, err := plugin.New(caller, nil, dataDir)
	if err != nil {
		return nil, err
	}
	_ = prev // state restore + auto-flush live in plugin.restoreState (register)
	abiState.Lock()
	abiState.plugin = p
	abiState.Unlock()
	return p.HandleMethod(method, request)
}

// resolveStateDir uses the host process working directory (plugins dir
// convention) and falls back beside the shared library binary when that
// location is unwritable.
func resolveStateDir() string {
	if wd, err := os.Getwd(); err == nil {
		dir := filepath.Join(wd, filepath.FromSlash(stateDirRelative))
		if err := os.MkdirAll(dir, 0o700); err == nil {
			return dir
		}
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Join(filepath.Dir(exe), "data")
		_ = os.MkdirAll(dir, 0o700)
		return dir
	}
	return ""
}

// cgoCaller adapts the stored host API pointers to plugin.HostCaller.
type cgoCaller struct{ host *C.cliproxy_host_api }

func (c cgoCaller) CallHost(method string, payload []byte) ([]byte, error) {
	abiState.Lock()
	host := c.host
	abiState.Unlock()
	if host == nil {
		return nil, fmt.Errorf("host callback unavailable")
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cPayload unsafe.Pointer
	if len(payload) > 0 {
		cPayload = C.CBytes(payload)
		if cPayload == nil {
			return nil, fmt.Errorf("allocate host callback payload")
		}
		defer C.free(cPayload)
	}
	var response C.cliproxy_buffer
	rc := C.ocp_call_host(host, cMethod, (*C.uint8_t)(cPayload), C.size_t(len(payload)), &response)
	if rc != 0 {
		return nil, fmt.Errorf("host call %s failed: rc=%d", method, int(rc))
	}
	var out []byte
	if response.ptr != nil && response.len > 0 {
		out = C.GoBytes(response.ptr, C.int(response.len))
		C.ocp_free_host(host, unsafe.Pointer(response.ptr), C.size_t(response.len))
	}
	return out, nil
}

func errorEnvelopeBytes(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: pluginabi.NewError(code, message)})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
