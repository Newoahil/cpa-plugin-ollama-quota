/*
 * ABI smoke test for the copilot-quota shared library.
 *
 * This is not a CPA integration test: it only proves the library loads with
 * the same function-table contract CPA uses, initialises, registers as a quota
 * provider, and answers the identifier/describe calls without a host. The host
 * callback is stubbed; it answers host.auth.list with an empty list and every
 * other method with an empty success envelope.
 *
 * Build (inside a Linux container, same as the library):
 *   gcc -O1 -o abi-smoke abi-smoke.c -ldl
 *   ./abi-smoke ./copilot-quota-v0.1.0.so
 */
#include <dlfcn.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

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

typedef int (*init_fn)(cliproxy_host_api*, cliproxy_plugin_api*);

static cliproxy_buffer static_response;

static int host_call(void* ctx, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	(void)ctx;
	(void)request;
	(void)request_len;
	const char* result = "{}";
	if (strcmp(method, "host.auth.list") == 0) {
		result = "{\"files\":[]}";
	}
	size_t len = strlen(result);
	response->ptr = malloc(len);
	memcpy(response->ptr, result, len);
	response->len = len;
	return 0;
}

static void host_free(void* ptr, size_t len) {
	(void)len;
	free(ptr);
}

static void print_buffer(const char* label, cliproxy_buffer buffer) {
	printf("%s: ", label);
	fwrite(buffer.ptr, 1, buffer.len, stdout);
	printf("\n");
}

static int check(const char* label, const char* needle, cliproxy_buffer buffer) {
	if (buffer.ptr == NULL || buffer.len == 0) {
		printf("FAIL %s: empty response\n", label);
		return 1;
	}
	if (strstr((const char*)buffer.ptr, needle) == NULL) {
		printf("FAIL %s: response does not contain %s\n", label, needle);
		print_buffer(label, buffer);
		return 1;
	}
	printf("ok   %s\n", label);
	return 0;
}

static cliproxy_buffer call(cliproxy_plugin_api* api, const char* method, const char* request) {
	cliproxy_buffer response = {NULL, 0};
	api->call((char*)method, (uint8_t*)request, request ? strlen(request) : 0, &response);
	return response;
}

int main(int argc, char** argv) {
	if (argc < 2) {
		fprintf(stderr, "usage: %s <shared-library>\n", argv[0]);
		return 2;
	}
	void* handle = dlopen(argv[1], RTLD_NOW | RTLD_LOCAL);
	if (handle == NULL) {
		fprintf(stderr, "FAIL dlopen: %s\n", dlerror());
		return 1;
	}
	init_fn init = (init_fn)dlsym(handle, "cliproxy_plugin_init");
	if (init == NULL) {
		fprintf(stderr, "FAIL dlsym cliproxy_plugin_init: %s\n", dlerror());
		return 1;
	}

	cliproxy_host_api host = {1, NULL, host_call, host_free};
	cliproxy_plugin_api api = {0, NULL, NULL, NULL};
	int rc = init(&host, &api);
	if (rc != 0) {
		printf("FAIL init returned %d\n", rc);
		return 1;
	}
	printf("ok   init (abi_version=%u)\n", api.abi_version);
	if (api.call == NULL || api.free_buffer == NULL || api.shutdown == NULL) {
		printf("FAIL function table incomplete\n");
		return 1;
	}

	int failed = 0;
	cliproxy_buffer response;

	response = call(&api, "plugin.register", "{\"config_yaml\":null,\"schema_version\":1}");
	failed |= check("plugin.register", "\"quota_provider\":true", response);
	api.free_buffer(response.ptr, response.len);

	response = call(&api, "quota.identifier", "");
	failed |= check("quota.identifier", "\"identifier\":\"copilot-quota\"", response);
	api.free_buffer(response.ptr, response.len);

	response = call(&api, "quota.describe", "{}");
	failed |= check("quota.describe", "\"supported_providers\":[\"copilot\"]", response);
	api.free_buffer(response.ptr, response.len);

	response = call(&api, "quota.reset", "{}");
	failed |= check("quota.reset", "\"success\":false", response);
	api.free_buffer(response.ptr, response.len);

	response = call(&api, "plugin.quiesce", "{}");
	failed |= check("plugin.quiesce", "\"ok\":true", response);
	api.free_buffer(response.ptr, response.len);

	api.shutdown();
	printf("ok   shutdown\n");
	dlclose(handle);
	return failed ? 1 : 0;
}
