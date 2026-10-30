// Command plugin builds the CLIProxyAPI shared-library plugin for Ollama quota.
//
// Build with -buildmode=c-shared so the host can dlopen the artifact and call
// cliproxy_plugin_init. The process entrypoint (main) never runs: the library
// exports a function table and the host drives it over JSON RPC.
//
// All plugin behaviour lives in internal/quotaplugin so it can be tested with
// cgo disabled; this file is only the C ABI shim.
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

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"unsafe"

	"github.com/LambdaTheory/cpa-plugin-ollama-quota/internal/quotaplugin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

func main() {}

// runtime tracks every ABI generation the host has initialised and routes each
// call to the right one.
//
// Go package state survives dlclose, so a second load of the same file finds
// whatever the first load left behind. cliproxy_plugin_init therefore has to
// build a fresh instance rather than reuse the one the host already shut down —
// otherwise reinstalling after DELETE hands the host a stopped State and the
// plugin registers with no capabilities.
//
// The three entry points below are the same global symbols for every init, and
// the ABI gives call and shutdown no way to say which init they belong to (no
// host pointer, no epoch, and a C function pointer cannot close over per-init
// state). So generation identity comes from the host's call order instead: one
// init per loaded client, one shutdown per client, matched oldest first. See
// internal/quotaplugin/runtime.go for the ordering evidence and why retiring
// the oldest generation is what keeps a replaced client's late shutdown from
// stopping the one that replaced it.
var runtime = quotaplugin.NewRuntime()

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	// Every init is a new generation: unload then reinstall must work even for
	// the same file. The function table is filled with the same symbols every
	// time; which generation they act on is decided at call time by Runtime.
	runtime.Init()
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

// cliproxyPluginCall serves a call with the newest generation.
//
// The call carries no indication of which init's function table issued it, so a
// call made through a retired table is served by the newest generation instead
// of being refused. That is the best the ABI allows, and it is harmless: the
// host quiesces a client before replacing it and never calls it again after the
// replacement succeeds, so a call only reaches the newest generation while that
// generation is the live one.
//
//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, quotaplugin.ErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	handler := runtime.Current()
	if handler == nil {
		// Calls can only arrive after init populated the function table, but a
		// nil guard here beats a nil dereference inside the host process.
		writeResponse(response, quotaplugin.ErrorEnvelope("plugin_not_initialized", "cliproxy_plugin_init has not run"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handler.Handle(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, quotaplugin.ErrorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

// cliproxyPluginShutdown retires the oldest generation that is still live.
//
// The host calls shutdown once per init, and issues it for a replaced client
// only after the replacement is serving, so the generation this call belongs to
// is always the oldest one still waiting. Retiring that one — never the newest —
// is what keeps a replaced client's late shutdown from stopping the instance
// that replaced it.
//
//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	runtime.Shutdown()
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
