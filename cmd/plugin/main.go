// Command plugin builds the CLIProxyAPI shared-library plugin for Ollama quota.
//
// Build with -buildmode=c-shared so the host can dlopen the artifact and call
// cliproxy_plugin_init. The process entrypoint (main) never runs: the library
// exports a function table and the host drives it over JSON RPC.
//
// All plugin behaviour lives in internal/quotaplugin so it can be tested with
// cgo disabled; this file is only the C ABI shim and the host-callback bridge.
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

// cliproxy_clone_host_api copies the function table the host handed to one
// cliproxy_plugin_init. Each init must get its own copy: a hot replacement can
// load byte-identical artifacts through the same shadow path, so the host calls
// init again on the library instance that is already serving. A single global
// table would then route the replaced generation's still-running executor pump
// through the new client's host_ctx, and the host rejects those callbacks as
// belonging to another plugin instance.
//
// The copy is deliberately never freed. The host leaks the original context
// and table for the same reason (its loader cannot know when the resident
// plugin runtime stops using them), and a retired generation's pump may keep
// calling this table until its last event. One small block per init is the
// price of keeping that path safe.
static cliproxy_host_api* cliproxy_clone_host_api(const cliproxy_host_api* host) {
	if (host == NULL) {
		return NULL;
	}
	cliproxy_host_api* copy = (cliproxy_host_api*)malloc(sizeof(cliproxy_host_api));
	if (copy == NULL) {
		return NULL;
	}
	*copy = *host;
	return copy;
}

static int call_host_api(const cliproxy_host_api* host, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(const cliproxy_host_api* host, void* ptr, size_t len) {
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"context"
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

// cliproxy_plugin_init starts a new generation bound to the host table passed
// to this init.
//
// The table is the plugin's only route to the network, the credential store and
// the log — QuotaFetchRequest.HTTPClient cannot cross the C ABI — and it
// belongs to one loaded host client. Binding it per generation, rather than
// keeping the latest table in a package-level variable, is what keeps a
// replaced generation's asynchronous executor pump alive: a hot replacement
// with byte-identical bytes calls init again on this same library instance
// while the old pump is still emitting, and those callbacks must keep their
// original host_ctx.
//
//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	// Every init is a new generation: unload then reinstall must work even for
	// the same file. The function table is filled with the same symbols every
	// time; which generation they act on is decided at call time by Runtime.
	runtime.Init(hostCallFor(host))
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

// hostCallFor returns the HostCall of one generation, bound to that
// generation's copy of the host table.
//
// It is the production implementation of quotaplugin.HostCall. The request and
// response cross as JSON, which is why every host API this plugin uses is
// expressed as a JSON envelope — including the upstream HTTP request, whose
// headers carry the credential and therefore must not be rendered into any
// error string on the way back.
//
// The returned closure must own the table it calls. Reading a package-level
// "latest host" here is the F1 failure from the PR #21 review: after a hot
// replacement of an identical artifact, the previous generation's pump would
// call through the new client's host_ctx and the host would reject the stream
// mid-body ("host callback ID does not belong to the calling plugin instance").
func hostCallFor(host *C.cliproxy_host_api) quotaplugin.HostCall {
	table := C.cliproxy_clone_host_api(host)
	if table == nil {
		return func(context.Context, string, []byte) ([]byte, error) {
			return nil, quotaplugin.ErrHostUnavailable
		}
	}
	return func(_ context.Context, method string, request []byte) ([]byte, error) {
		cMethod := C.CString(method)
		defer C.free(unsafe.Pointer(cMethod))

		var response C.cliproxy_buffer
		var requestPtr *C.uint8_t
		if len(request) > 0 {
			requestPtr = (*C.uint8_t)(C.CBytes(request))
			defer C.free(unsafe.Pointer(requestPtr))
		}
		if C.call_host_api(table, cMethod, requestPtr, C.size_t(len(request)), &response) != 0 || response.ptr == nil {
			return nil, quotaplugin.ErrHostUnavailable
		}
		// Copy before freeing: the buffer belongs to the host.
		raw := C.GoBytes(response.ptr, C.int(response.len))
		C.free_host_buffer(table, response.ptr, response.len)
		return raw, nil
	}
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
