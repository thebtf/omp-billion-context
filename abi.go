package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_buffer_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_buffer_free_fn free_buffer;
} cliproxy_host_api;

typedef struct {
	uint32_t abi_version;
	int (*call)(const char*, const uint8_t*, size_t, cliproxy_buffer*);
	cliproxy_buffer_free_fn free_buffer;
	void (*shutdown)(void);
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static int plugin_call(const char* method, const uint8_t* req, size_t len, cliproxy_buffer* out) {
	return cliproxyPluginCall((char*)method, (uint8_t*)req, len, out);
}

static void set_plugin_api(cliproxy_plugin_api* api, uint32_t version) {
	api->abi_version = version;
	api->call = plugin_call;
	api->free_buffer = cliproxyPluginFree;
	api->shutdown = cliproxyPluginShutdown;
}

static int call_host_api(const cliproxy_host_api* host, const char* method, const uint8_t* req, size_t len, cliproxy_buffer* out) {
	return host->call(host->host_ctx, method, req, len, out);
}

static void free_host_buffer(const cliproxy_host_api* host, cliproxy_buffer* out) {
	host->free_buffer(out->ptr, out->len);
}
*/
import "C"

import (
	"encoding/json"
	"sync/atomic"
	"unsafe"
)

const abiVersion = 1

type nativeInstance struct {
	plugin      *plugin
	configError error
}

var native atomic.Pointer[nativeInstance]

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || host.abi_version != abiVersion || host.call == nil || host.free_buffer == nil {
		return 1
	}
	cfg, err := loadConfig()
	instance := &nativeInstance{configError: err}
	if err == nil {
		instance.plugin = newPlugin(cfg, func(method string, payload any) bool {
			return nativeHostCall(host, method, payload)
		})
	}
	if !native.CompareAndSwap(nil, instance) {
		return 1
	}
	C.set_plugin_api(api, abiVersion)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response == nil {
		return 1
	}
	response.ptr, response.len = nil, 0
	instance := native.Load()
	var result any
	var failure *rpcError
	switch {
	case instance == nil:
		failure = &rpcError{Code: "plugin_unavailable", Message: "plugin is not initialized"}
	case instance.configError != nil:
		failure = &rpcError{Code: "invalid_config", Message: instance.configError.Error()}
	case method == nil || (request == nil && requestLen != 0) || requestLen > 1<<31-1:
		failure = &rpcError{Code: "invalid_request", Message: "invalid native RPC request"}
	default:
		result, failure = instance.plugin.handle(C.GoString(method), unsafe.Slice((*byte)(unsafe.Pointer(request)), int(requestLen)))
	}
	encoded, err := json.Marshal(rpcEnvelope{OK: failure == nil, Result: result, Error: failure})
	if err != nil {
		return 1
	}
	response.ptr = C.CBytes(encoded)
	response.len = C.size_t(len(encoded))
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	C.free(ptr)
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	if instance := native.Swap(nil); instance != nil && instance.plugin != nil {
		instance.plugin.shutdown()
	}
}

func nativeHostCall(host *C.cliproxy_host_api, method string, payload any) bool {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	name := C.CString(method)
	defer C.free(unsafe.Pointer(name))
	var out C.cliproxy_buffer
	rc := C.call_host_api(host, name, (*C.uint8_t)(unsafe.Pointer(unsafe.SliceData(encoded))), C.size_t(len(encoded)), &out)
	if out.ptr != nil {
		defer C.free_host_buffer(host, &out)
	}
	if rc != 0 || out.ptr == nil || out.len > 1<<31-1 {
		return false
	}
	var status struct{ OK bool `json:"ok"` }
	return json.Unmarshal(unsafe.Slice((*byte)(out.ptr), int(out.len)), &status) == nil && status.OK
}
