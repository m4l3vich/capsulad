/*
 * QuickJS app host.
 *
 * The embedded engine is real QuickJS (quickjs-ng); this file installs a thin
 * native bridge over the public libcapsulad C ABI and evaluates the SDK prelude
 * before the package script. Scripts never receive raw framework sockets,
 * child processes or a second protocol implementation.
 */
#if !defined(_XOPEN_SOURCE) && !defined(_GNU_SOURCE)
#define _XOPEN_SOURCE 700
#endif
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>

#include "capsulad_host.h"
#include "quickjs.h"

#define MAX_SCRIPT_BYTES (8u * 1024u * 1024u)
#define MAX_PACKAGE_FILE_BYTES (8u * 1024u * 1024u)
#define MAX_PACKAGE_PATH_BYTES 4096u
#define CAPSULAD_STR_INNER(x) #x
#define CAPSULAD_STR(x) CAPSULAD_STR_INNER(x)

static capsulad_host host;

static capsulad_host *ctx_host(JSContext *ctx) {
  return (capsulad_host *)JS_GetContextOpaque(ctx);
}

static JSValue throw_status(JSContext *ctx, capsulad_host *h, int32_t status) {
  char err[512];
  capsulad_host_error(h, status, err, sizeof(err));
  return JS_ThrowInternalError(ctx, "%s", err);
}

static const char *arg_string(JSContext *ctx, JSValueConst value, size_t *len) {
  size_t ignored = 0;
  const char *text = JS_ToCStringLen(ctx, len != NULL ? len : &ignored, value);
  return text;
}

static uint32_t arg_timeout(JSContext *ctx, JSValueConst value,
                            uint32_t fallback) {
  if (JS_IsUndefined(value) || JS_IsNull(value)) {
    return fallback;
  }
  int64_t timeout = 0;
  if (JS_ToInt64(ctx, &timeout, value) != 0) {
    return fallback;
  }
  if (timeout < 0) {
    return CAPSULAD_WAIT_FOREVER;
  }
  return (uint32_t)timeout;
}

static uint64_t arg_handle(JSContext *ctx, JSValueConst value) {
  int64_t handle = 0;
  if (JS_ToInt64(ctx, &handle, value) != 0 || handle <= 0) {
    return CAPSULAD_HANDLE_INVALID;
  }
  return (uint64_t)handle;
}

/* arg_bytes accepts only Uint8Array and reports an empty array as {NULL, 0}.
 * JS_GetUint8Array returns borrowed storage, not a copy. */
static const uint8_t *arg_bytes(JSContext *ctx, JSValueConst value,
                                size_t *length, int *ok) {
  *length = 0;
  *ok = 0;
  if (JS_GetTypedArrayType(value) != JS_TYPED_ARRAY_UINT8) {
    JS_ThrowTypeError(ctx, "expected a Uint8Array");
    return NULL;
  }
  size_t size = 0;
  const uint8_t *data = JS_GetUint8Array(ctx, &size, value);
  if (data == NULL && size > 0) {
    return NULL;
  }
  *length = size;
  *ok = 1;
  return data;
}

static JSValue js_start(JSContext *ctx, JSValueConst this_val, int argc,
                        JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  uint32_t timeout = arg_timeout(ctx, argc > 0 ? argv[0] : JS_UNDEFINED, 10000);
  char err[512];
  if (capsulad_host_start(h, timeout, err, sizeof(err)) != 0) {
    JS_ThrowInternalError(ctx, "%s", err);
    return JS_EXCEPTION;
  }
  return JS_UNDEFINED;
}

static JSValue js_poll(JSContext *ctx, JSValueConst this_val, int argc,
                       JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  int timeout = 0;
  if (argc > 0 && !JS_IsUndefined(argv[0])) {
    int32_t value = 0;
    if (JS_ToInt32(ctx, &value, argv[0]) != 0) {
      return JS_EXCEPTION;
    }
    timeout = value;
  }
  capsulad_host_job job;
  if (!capsulad_host_poll(h, timeout, &job)) {
    return JS_NULL;
  }
  JSValue out = JS_NewObject(ctx);
  switch (job.kind) {
    case CAPSULAD_HOST_JOB_EVENT: {
      JS_SetPropertyStr(ctx, out, "kind",
                        JS_NewString(ctx, "event"));
      JS_SetPropertyStr(ctx, out, "event", JS_NewInt32(ctx, job.event_kind));
      if (job.json != NULL && job.json_len > 0) {
        JS_SetPropertyStr(ctx, out, "data",
                          JS_NewStringLen(ctx, (const char *)job.json,
                                          job.json_len));
      } else {
        JS_SetPropertyStr(ctx, out, "data", JS_NULL);
      }
      break;
    }
    case CAPSULAD_HOST_JOB_CALL: {
      JS_SetPropertyStr(ctx, out, "kind", JS_NewString(ctx, "call"));
      JS_SetPropertyStr(ctx, out, "call", JS_NewInt64(ctx, (int64_t)job.call));
      if (job.info != NULL && job.info_len > 0) {
        JS_SetPropertyStr(ctx, out, "info",
                          JS_NewStringLen(ctx, (const char *)job.info,
                                          job.info_len));
      } else {
        JS_SetPropertyStr(ctx, out, "info", JS_NULL);
      }
      break;
    }
    case CAPSULAD_HOST_JOB_DISCONNECTED: {
      JS_SetPropertyStr(ctx, out, "kind", JS_NewString(ctx, "disconnect"));
      JS_SetPropertyStr(ctx, out, "status", JS_NewInt32(ctx, job.status));
      JS_SetPropertyStr(ctx, out, "detail",
                        JS_NewString(ctx, job.detail != NULL ? job.detail : ""));
      break;
    }
    default:
      break;
  }
  capsulad_host_free_job(&job);
  return out;
}

static JSValue js_stopped(JSContext *ctx, JSValueConst this_val, int argc,
                          JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  return JS_NewBool(ctx, capsulad_host_stopped(ctx_host(ctx)));
}

static JSValue js_disconnected(JSContext *ctx, JSValueConst this_val, int argc,
                               JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  return JS_NewBool(ctx, capsulad_host_disconnected(ctx_host(ctx)));
}

static JSValue js_connected(JSContext *ctx, JSValueConst this_val, int argc,
                            JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  return JS_NewBool(ctx, capsulad_host_connected(ctx_host(ctx)));
}

static JSValue js_ready(JSContext *ctx, JSValueConst this_val, int argc,
                        JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  return JS_NewBool(ctx, capsulad_host_bus_ready(ctx_host(ctx)));
}

static JSValue js_disconnect_status(JSContext *ctx, JSValueConst this_val,
                                    int argc, JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  int32_t status = 0;
  char detail[512] = {0};
  capsulad_host_disconnect_status(ctx_host(ctx), &status, detail,
                                 sizeof(detail));
  return JS_NewInt32(ctx, status);
}

static JSValue js_disconnect_detail(JSContext *ctx, JSValueConst this_val,
                                    int argc, JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  int32_t status = 0;
  char detail[512] = {0};
  if (!capsulad_host_disconnect_status(ctx_host(ctx), &status, detail,
                                      sizeof(detail))) {
    return JS_NULL;
  }
  return JS_NewString(ctx, detail);
}

static JSValue js_last_status(JSContext *ctx, JSValueConst this_val, int argc,
                              JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  return JS_NewInt32(ctx, ctx_host(ctx)->last_status);
}

static JSValue js_last_error(JSContext *ctx, JSValueConst this_val, int argc,
                             JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  return JS_NewString(ctx, ctx_host(ctx)->last_detail);
}

static JSValue js_log(JSContext *ctx, JSValueConst this_val, int argc,
                      JSValueConst *argv) {
  (void)this_val;
  if (argc > 0) {
    size_t len = 0;
    const char *text = arg_string(ctx, argv[0], &len);
    if (text != NULL) {
      fprintf(stderr, "capsulad-script: %.*s\n", (int)len, text);
      fflush(stderr);
      JS_FreeCString(ctx, text);
    }
  }
  return JS_UNDEFINED;
}

static JSValue js_unary(JSContext *ctx, JSValueConst this_val, int argc,
                        JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 3) {
    return JS_ThrowTypeError(ctx, "unary(service, method, requestJSON, timeout?)");
  }
  size_t service_len = 0, method_len = 0, request_len = 0;
  const char *service = arg_string(ctx, argv[0], &service_len);
  const char *method = arg_string(ctx, argv[1], &method_len);
  const char *request = arg_string(ctx, argv[2], &request_len);
  if (service == NULL || method == NULL || request == NULL) {
    return JS_EXCEPTION;
  }
  capsulad_buf out = {0};
  int32_t status = h->api->client_unary(
      h->client, service, service_len, method, method_len,
      (const uint8_t *)request, request_len,
      arg_timeout(ctx, argc > 3 ? argv[3] : JS_UNDEFINED, 30000), &out);
  JS_FreeCString(ctx, service);
  JS_FreeCString(ctx, method);
  JS_FreeCString(ctx, request);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewStringLen(ctx, (const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}

static JSValue js_unary_proto(JSContext *ctx, JSValueConst this_val, int argc,
                              JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 3) {
    return JS_ThrowTypeError(ctx,
                             "unaryProto(service, method, bytes, timeout?)");
  }
  size_t service_len = 0, method_len = 0, request_len = 0;
  const char *service = arg_string(ctx, argv[0], &service_len);
  const char *method = arg_string(ctx, argv[1], &method_len);
  if (service == NULL || method == NULL) {
    return JS_EXCEPTION;
  }
  int bytes_ok = 0;
  const uint8_t *request = arg_bytes(ctx, argv[2], &request_len, &bytes_ok);
  if (!bytes_ok) {
    JS_FreeCString(ctx, service);
    JS_FreeCString(ctx, method);
    return JS_EXCEPTION;
  }
  capsulad_buf out = {0};
  int32_t status = h->api->client_unary_proto(
      h->client, service, service_len, method, method_len, request, request_len,
      arg_timeout(ctx, argc > 3 ? argv[3] : JS_UNDEFINED, 30000), &out);
  JS_FreeCString(ctx, service);
  JS_FreeCString(ctx, method);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewUint8ArrayCopy(ctx, out.data, out.len);
  h->api->buf_free(out);
  return result;
}

static JSValue js_call_start(JSContext *ctx, JSValueConst this_val, int argc,
                             JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 4) {
    return JS_ThrowTypeError(ctx,
                             "callStart(service, method, kind, timeout?)");
  }
  size_t service_len = 0, method_len = 0;
  const char *service = arg_string(ctx, argv[0], &service_len);
  const char *method = arg_string(ctx, argv[1], &method_len);
  int32_t kind = 0;
  if (service == NULL || method == NULL ||
      JS_ToInt32(ctx, &kind, argv[2]) != 0) {
    if (service != NULL) JS_FreeCString(ctx, service);
    if (method != NULL) JS_FreeCString(ctx, method);
    return JS_EXCEPTION;
  }
  capsulad_handle call = CAPSULAD_HANDLE_INVALID;
  int32_t status = h->api->call_start(
      h->client, service, service_len, method, method_len, kind,
      arg_timeout(ctx, argc > 3 ? argv[3] : JS_UNDEFINED, 30000), &call);
  JS_FreeCString(ctx, service);
  JS_FreeCString(ctx, method);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_NewInt64(ctx, (int64_t)call);
}

static JSValue js_call_start_routed(JSContext *ctx, JSValueConst this_val,
                                    int argc, JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 4) {
    return JS_ThrowTypeError(
        ctx, "callStartRouted(appId, service, method, timeout?)");
  }
  size_t app_len = 0, service_len = 0, method_len = 0;
  const char *app = arg_string(ctx, argv[0], &app_len);
  const char *service = arg_string(ctx, argv[1], &service_len);
  const char *method = arg_string(ctx, argv[2], &method_len);
  if (app == NULL || service == NULL || method == NULL) {
    if (app != NULL) JS_FreeCString(ctx, app);
    if (service != NULL) JS_FreeCString(ctx, service);
    if (method != NULL) JS_FreeCString(ctx, method);
    return JS_EXCEPTION;
  }
  capsulad_handle call = CAPSULAD_HANDLE_INVALID;
  int32_t status = h->api->call_start_routed(
      h->client, app, app_len, service, service_len, method, method_len,
      arg_timeout(ctx, argc > 3 ? argv[3] : JS_UNDEFINED, 30000), &call);
  JS_FreeCString(ctx, app);
  JS_FreeCString(ctx, service);
  JS_FreeCString(ctx, method);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_NewInt64(ctx, (int64_t)call);
}

static JSValue js_send_json(JSContext *ctx, JSValueConst this_val, int argc,
                            JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 2) {
    return JS_ThrowTypeError(ctx, "sendJson(call, json)");
  }
  uint64_t call = arg_handle(ctx, argv[0]);
  size_t len = 0;
  const char *json = arg_string(ctx, argv[1], &len);
  if (json == NULL) {
    return JS_EXCEPTION;
  }
  int32_t status = h->api->call_send_json(call, (const uint8_t *)json, len);
  JS_FreeCString(ctx, json);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_UNDEFINED;
}

static JSValue js_send_proto(JSContext *ctx, JSValueConst this_val, int argc,
                             JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 2) {
    return JS_ThrowTypeError(ctx, "sendProto(call, bytes)");
  }
  uint64_t call = arg_handle(ctx, argv[0]);
  size_t len = 0;
  int bytes_ok = 0;
  const uint8_t *data = arg_bytes(ctx, argv[1], &len, &bytes_ok);
  if (!bytes_ok) {
    return JS_EXCEPTION;
  }
  int32_t status = h->api->call_send_proto(call, data, len);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_UNDEFINED;
}

static JSValue js_half_close(JSContext *ctx, JSValueConst this_val, int argc,
                             JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "halfClose(call)");
  }
  int32_t status = h->api->call_half_close(arg_handle(ctx, argv[0]));
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_UNDEFINED;
}

static JSValue js_recv_json(JSContext *ctx, JSValueConst this_val, int argc,
                            JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "recvJson(call, timeout?)");
  }
  capsulad_buf out = {0};
  int32_t status = h->api->call_recv_json(
      arg_handle(ctx, argv[0]),
      arg_timeout(ctx, argc > 1 ? argv[1] : JS_UNDEFINED, 30000), &out);
  if (status == CAPSULAD_STATUS_END_OF_STREAM) {
    return JS_NULL;
  }
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewStringLen(ctx, (const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}

static JSValue js_recv_proto(JSContext *ctx, JSValueConst this_val, int argc,
                             JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "recvProto(call, timeout?)");
  }
  capsulad_buf out = {0};
  int32_t status = h->api->call_recv_proto(
      arg_handle(ctx, argv[0]),
      arg_timeout(ctx, argc > 1 ? argv[1] : JS_UNDEFINED, 30000), &out);
  if (status == CAPSULAD_STATUS_END_OF_STREAM) {
    return JS_NULL;
  }
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewUint8ArrayCopy(ctx, out.data, out.len);
  h->api->buf_free(out);
  return result;
}

/* try_recv_json/try_recv_proto are the bounded, non-blocking reads. The SDK
 * returns CAPSULAD_STATUS_TIMEOUT with the call still open when no message
 * arrived; that is a normal poll result, not a failure, so it maps to
 * JS_UNDEFINED ("nothing yet") while END_OF_STREAM stays JS_NULL and every
 * real transport/status error still throws. The ABI is unchanged: the host
 * passes the caller's timeout through call_recv_json/call_recv_proto, and 0
 * polls once. */
static JSValue js_try_recv_json(JSContext *ctx, JSValueConst this_val,
                                int argc, JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "tryRecvJson(call, timeout?)");
  }
  capsulad_buf out = {0};
  int32_t status = h->api->call_recv_json(
      arg_handle(ctx, argv[0]),
      arg_timeout(ctx, argc > 1 ? argv[1] : JS_UNDEFINED, 0), &out);
  if (status == CAPSULAD_STATUS_END_OF_STREAM) {
    return JS_NULL;
  }
  if (status == CAPSULAD_STATUS_TIMEOUT) {
    return JS_UNDEFINED;
  }
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewStringLen(ctx, (const char *)out.data,
                                   capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}

static JSValue js_try_recv_proto(JSContext *ctx, JSValueConst this_val,
                                 int argc, JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "tryRecvProto(call, timeout?)");
  }
  capsulad_buf out = {0};
  int32_t status = h->api->call_recv_proto(
      arg_handle(ctx, argv[0]),
      arg_timeout(ctx, argc > 1 ? argv[1] : JS_UNDEFINED, 0), &out);
  if (status == CAPSULAD_STATUS_END_OF_STREAM) {
    return JS_NULL;
  }
  if (status == CAPSULAD_STATUS_TIMEOUT) {
    return JS_UNDEFINED;
  }
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewUint8ArrayCopy(ctx, out.data, out.len);
  h->api->buf_free(out);
  return result;
}

static JSValue js_call_end(JSContext *ctx, JSValueConst this_val, int argc,
                           JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "endCall(call, code, detail)");
  }
  int32_t code = 0;
  if (argc > 1 && JS_ToInt32(ctx, &code, argv[1]) != 0) {
    return JS_EXCEPTION;
  }
  size_t len = 0;
  const char *detail = "";
  int detail_allocated = 0;
  if (argc > 2 && !JS_IsUndefined(argv[2]) && !JS_IsNull(argv[2])) {
    detail = arg_string(ctx, argv[2], &len);
    if (detail == NULL) {
      return JS_EXCEPTION;
    }
    detail_allocated = 1;
  }
  int32_t status = h->api->call_end(arg_handle(ctx, argv[0]), code, detail, len);
  if (detail_allocated) {
    JS_FreeCString(ctx, detail);
  }
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_UNDEFINED;
}

static JSValue js_cancel(JSContext *ctx, JSValueConst this_val, int argc,
                         JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "cancel(call)");
  }
  int32_t status = h->api->call_cancel(arg_handle(ctx, argv[0]));
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_UNDEFINED;
}

static JSValue js_release(JSContext *ctx, JSValueConst this_val, int argc,
                          JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "release(call)");
  }
  int32_t status = h->api->call_release(arg_handle(ctx, argv[0]));
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_UNDEFINED;
}

static JSValue js_call_info(JSContext *ctx, JSValueConst this_val, int argc,
                            JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "callInfo(call)");
  }
  capsulad_buf out = {0};
  int32_t status = h->api->call_info(arg_handle(ctx, argv[0]), &out);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewStringLen(ctx, (const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}

static JSValue js_call_status(JSContext *ctx, JSValueConst this_val, int argc,
                              JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "callStatus(call)");
  }
  int32_t code = 0;
  capsulad_buf detail = {0};
  int32_t status =
      h->api->call_status(arg_handle(ctx, argv[0]), &code, &detail);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewArray(ctx);
  JS_SetPropertyUint32(ctx, result, 0, JS_NewInt32(ctx, code));
  JS_SetPropertyUint32(
      ctx, result, 1,
      JS_NewStringLen(ctx, (const char *)detail.data,
                      capsulad_host_text_len(detail.data, detail.len)));
  h->api->buf_free(detail);
  return result;
}

static JSValue js_notify_invocation(JSContext *ctx, JSValueConst this_val,
                                    int argc, JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 2) {
    return JS_ThrowTypeError(ctx,
                             "notifyInvocation(id, accepted, detail?)");
  }
  size_t id_len = 0, detail_len = 0;
  const char *id = arg_string(ctx, argv[0], &id_len);
  int32_t accepted = 0;
  if (id == NULL || JS_ToInt32(ctx, &accepted, argv[1]) != 0) {
    if (id != NULL) JS_FreeCString(ctx, id);
    return JS_EXCEPTION;
  }
  const char *detail = "";
  int detail_allocated = 0;
  if (argc > 2 && !JS_IsUndefined(argv[2]) && !JS_IsNull(argv[2])) {
    detail = arg_string(ctx, argv[2], &detail_len);
    if (detail == NULL) {
      JS_FreeCString(ctx, id);
      return JS_EXCEPTION;
    }
    detail_allocated = 1;
  }
  int32_t status = h->api->bus_notify_invocation(h->bus, id, id_len, accepted,
                                                 detail, detail_len);
  JS_FreeCString(ctx, id);
  if (detail_allocated) {
    JS_FreeCString(ctx, detail);
  }
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_UNDEFINED;
}

static JSValue js_notify_action(JSContext *ctx, JSValueConst this_val,
                                int argc, JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 2) {
    return JS_ThrowTypeError(ctx, "notifyAction(id, succeeded, detail?)");
  }
  size_t id_len = 0, detail_len = 0;
  const char *id = arg_string(ctx, argv[0], &id_len);
  int32_t succeeded = 0;
  if (id == NULL || JS_ToInt32(ctx, &succeeded, argv[1]) != 0) {
    if (id != NULL) JS_FreeCString(ctx, id);
    return JS_EXCEPTION;
  }
  const char *detail = "";
  int detail_allocated = 0;
  if (argc > 2 && !JS_IsUndefined(argv[2]) && !JS_IsNull(argv[2])) {
    detail = arg_string(ctx, argv[2], &detail_len);
    if (detail == NULL) {
      JS_FreeCString(ctx, id);
      return JS_EXCEPTION;
    }
    detail_allocated = 1;
  }
  int32_t status = h->api->bus_notify_action(h->bus, id, id_len, succeeded,
                                             detail, detail_len);
  JS_FreeCString(ctx, id);
  if (detail_allocated) {
    JS_FreeCString(ctx, detail);
  }
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_UNDEFINED;
}

static JSValue js_client_info(JSContext *ctx, JSValueConst this_val, int argc,
                              JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  capsulad_host *h = ctx_host(ctx);
  capsulad_buf out = {0};
  int32_t status = h->api->client_info(h->client, &out);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewStringLen(ctx, (const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}

static JSValue js_handle_info(JSContext *ctx, JSValueConst this_val, int argc,
                              JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "handleInfo(handle)");
  }
  capsulad_buf out = {0};
  int32_t status = h->api->handle_info(arg_handle(ctx, argv[0]), &out);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  JSValue result = JS_NewStringLen(ctx, (const char *)out.data, capsulad_host_text_len(out.data, out.len));
  h->api->buf_free(out);
  return result;
}

static JSValue js_register_descriptors(JSContext *ctx, JSValueConst this_val,
                                       int argc, JSValueConst *argv) {
  (void)this_val;
  capsulad_host *h = ctx_host(ctx);
  if (argc < 1) {
    return JS_ThrowTypeError(ctx, "registerDescriptors(bytes)");
  }
  size_t len = 0;
  int bytes_ok = 0;
  const uint8_t *data = arg_bytes(ctx, argv[0], &len, &bytes_ok);
  if (!bytes_ok) {
    return JS_EXCEPTION;
  }
  int32_t status = h->api->client_register_descriptors(h->client, data, len);
  if (status != CAPSULAD_STATUS_OK) {
    return throw_status(ctx, h, status);
  }
  return JS_UNDEFINED;
}

static JSValue js_env(JSContext *ctx, JSValueConst this_val, int argc,
                      JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  const char *keys[] = {"CAPSULAD_SOCKET", "CAPSULAD_RUN_ID", "CAPSULAD_APP_ID",
                        "CAPSULAD_SDK", "CAPSULAD_LIBRARY", "CAPSULAD_APP_DIR"};
  const char *names[] = {"socket", "runId", "appId", "sdk", "library",
                         "appDir"};
  JSValue env = JS_NewObject(ctx);
  for (size_t i = 0; i < sizeof(keys) / sizeof(keys[0]); i++) {
    const char *value = getenv(keys[i]);
    JS_SetPropertyStr(ctx, env, names[i],
                      value != NULL ? JS_NewString(ctx, value) : JS_NULL);
  }
  return env;
}

/* A package path is slash-relative and names only regular components. The
 * package format already forbids symlinks, but uid/none runs use a host path;
 * rejecting dot components and verifying the resolved target below keeps the
 * read inside CAPSULAD_APP_DIR there too. */
static int package_path_ok(const char *path, size_t len) {
  if (len == 0 || len > MAX_PACKAGE_PATH_BYTES || path[0] == '/') {
    return 0;
  }
  size_t start = 0;
  for (size_t i = 0; i <= len; i++) {
    if (i == len || path[i] == '/') {
      size_t component = i - start;
      if (component == 0) {
        return 0;
      }
      if (component == 1 && path[start] == '.') {
        return 0;
      }
      if (component == 2 && path[start] == '.' && path[start + 1] == '.') {
        return 0;
      }
      start = i + 1;
    } else if (path[i] == '\0' || path[i] == '\\') {
      return 0;
    }
  }
  return 1;
}

static JSValue js_read_package_file(JSContext *ctx, JSValueConst this_val,
                                    int argc, JSValueConst *argv) {
  (void)this_val;
  JSValue result = JS_EXCEPTION;
  const char *path = NULL;
  char *root = NULL;
  char *full = NULL;
  char *resolved = NULL;
  uint8_t *data = NULL;
  FILE *f = NULL;

  if (argc < 1 || !JS_IsString(argv[0])) {
    return JS_ThrowTypeError(ctx, "readPackageFile(path)");
  }
  size_t path_len = 0;
  path = arg_string(ctx, argv[0], &path_len);
  if (path == NULL) {
    return JS_EXCEPTION;
  }
  if (!package_path_ok(path, path_len)) {
    JS_ThrowTypeError(
        ctx, "readPackageFile: path must be relative and stay inside the package");
    goto done;
  }
  const char *app_dir = getenv("CAPSULAD_APP_DIR");
  if (app_dir == NULL || app_dir[0] == '\0') {
    JS_ThrowPlainError(ctx, "readPackageFile: CAPSULAD_APP_DIR is not set");
    goto done;
  }
  root = realpath(app_dir, NULL);
  if (root == NULL) {
    JS_ThrowPlainError(ctx, "readPackageFile: package root: %s",
                       strerror(errno));
    goto done;
  }
  size_t full_len = strlen(root) + 1 + path_len + 1;
  full = malloc(full_len);
  if (full == NULL) {
    JS_ThrowPlainError(ctx, "readPackageFile: out of memory");
    goto done;
  }
  snprintf(full, full_len, "%s/%s", root, path);
  resolved = realpath(full, NULL);
  if (resolved == NULL) {
    JS_ThrowPlainError(ctx, "readPackageFile %s: %s", path, strerror(errno));
    goto done;
  }
  size_t root_len = strlen(root);
  if (strncmp(resolved, root, root_len) != 0 ||
      (resolved[root_len] != '\0' && resolved[root_len] != '/')) {
    JS_ThrowPlainError(ctx, "readPackageFile %s: path is outside the package",
                       path);
    goto done;
  }
  struct stat st;
  if (stat(resolved, &st) != 0 || !S_ISREG(st.st_mode)) {
    JS_ThrowPlainError(ctx, "readPackageFile %s: not a regular file", path);
    goto done;
  }
  if (st.st_size < 0 ||
      (uint64_t)st.st_size > (uint64_t)MAX_PACKAGE_FILE_BYTES) {
    JS_ThrowRangeError(ctx,
                       "readPackageFile %s: file is larger than %u bytes", path,
                       (unsigned)MAX_PACKAGE_FILE_BYTES);
    goto done;
  }
  size_t size = (size_t)st.st_size;
  data = malloc(size == 0 ? 1 : size);
  if (data == NULL) {
    JS_ThrowPlainError(ctx, "readPackageFile %s: out of memory", path);
    goto done;
  }
  if (size > 0) {
    f = fopen(resolved, "rb");
    if (f == NULL) {
      JS_ThrowPlainError(ctx, "readPackageFile %s: %s", path, strerror(errno));
      goto done;
    }
    if (fread(data, 1, size, f) != size || ferror(f)) {
      JS_ThrowPlainError(ctx, "readPackageFile %s: read failed", path);
      goto done;
    }
  }
  result = JS_NewUint8ArrayCopy(ctx, data, size);

done:
  if (f != NULL) {
    fclose(f);
  }
  free(data);
  free(full);
  free(resolved);
  free(root);
  if (path != NULL) {
    JS_FreeCString(ctx, path);
  }
  return result;
}

static JSValue js_stop(JSContext *ctx, JSValueConst this_val, int argc,
                       JSValueConst *argv) {
  (void)this_val;
  (void)argc;
  (void)argv;
  capsulad_host *h = ctx_host(ctx);
  pthread_mutex_lock(&h->mu);
  h->stopped = 1;
  pthread_cond_broadcast(&h->cv);
  pthread_mutex_unlock(&h->mu);
  return JS_UNDEFINED;
}

static void set_fn(JSContext *ctx, JSValueConst obj, const char *name,
                   JSCFunction *fn, int length) {
  JS_SetPropertyStr(ctx, obj, name, JS_NewCFunction(ctx, fn, name, length));
}

static int install_native(JSContext *ctx) {
  JSValue native = JS_NewObject(ctx);
  set_fn(ctx, native, "start", js_start, 1);
  set_fn(ctx, native, "poll", js_poll, 1);
  set_fn(ctx, native, "stopped", js_stopped, 0);
  set_fn(ctx, native, "disconnected", js_disconnected, 0);
  set_fn(ctx, native, "connected", js_connected, 0);
  set_fn(ctx, native, "ready", js_ready, 0);
  set_fn(ctx, native, "disconnect_status", js_disconnect_status, 0);
  set_fn(ctx, native, "disconnect_detail", js_disconnect_detail, 0);
  set_fn(ctx, native, "last_status", js_last_status, 0);
  set_fn(ctx, native, "last_error", js_last_error, 0);
  set_fn(ctx, native, "log", js_log, 1);
  set_fn(ctx, native, "unary", js_unary, 4);
  set_fn(ctx, native, "unary_proto", js_unary_proto, 4);
  set_fn(ctx, native, "call_start", js_call_start, 4);
  set_fn(ctx, native, "call_start_routed", js_call_start_routed, 4);
  set_fn(ctx, native, "send_json", js_send_json, 2);
  set_fn(ctx, native, "send_proto", js_send_proto, 2);
  set_fn(ctx, native, "half_close", js_half_close, 1);
  set_fn(ctx, native, "recv_json", js_recv_json, 2);
  set_fn(ctx, native, "recv_proto", js_recv_proto, 2);
  set_fn(ctx, native, "try_recv_json", js_try_recv_json, 2);
  set_fn(ctx, native, "try_recv_proto", js_try_recv_proto, 2);
  set_fn(ctx, native, "end_call", js_call_end, 3);
  set_fn(ctx, native, "cancel", js_cancel, 1);
  set_fn(ctx, native, "release", js_release, 1);
  set_fn(ctx, native, "call_info", js_call_info, 1);
  set_fn(ctx, native, "call_status", js_call_status, 1);
  set_fn(ctx, native, "notify_invocation", js_notify_invocation, 3);
  set_fn(ctx, native, "notify_action", js_notify_action, 3);
  set_fn(ctx, native, "client_info", js_client_info, 0);
  set_fn(ctx, native, "handle_info", js_handle_info, 1);
  set_fn(ctx, native, "register_descriptors", js_register_descriptors, 1);
  set_fn(ctx, native, "env", js_env, 0);
  set_fn(ctx, native, "read_package_file", js_read_package_file, 1);
  set_fn(ctx, native, "stop", js_stop, 0);

  JSValue global = JS_GetGlobalObject(ctx);
  JS_SetPropertyStr(ctx, global, "__capsulad_native", native);
  JS_FreeValue(ctx, global);
  return 0;
}

static const char *prelude =
    "globalThis.capsulad = (() => {\n"
    "  \"use strict\";\n"
    "  const n = globalThis.__capsulad_native;\n"
    "  delete globalThis.__capsulad_native;\n"
    "  const EVENT_NAMES = {1: \"invocation\", 2: \"settings_changed\",\n"
    "                       3: \"action\", 4: \"activation\",\n"
    "                       5: \"touch_released\"};\n"
    "  const handlers = Object.create(null);\n"
    "  let callHandler = null, disconnectHandler = null;\n"
    "  let started = false, finished = false;\n"
    "  const live = new Set();\n"
    "  function statusError(message) {\n"
    "    const err = new Error(message + \" (status \" + n.last_status() + \")\");\n"
    "    err.status = n.last_status();\n"
    "    return err;\n"
    "  }\n"
    "  class Call {\n"
    "    constructor(handle, info) {\n"
    "      this._handle = handle;\n"
    "      this._closed = false;\n"
    "      this.info = info || null;\n"
    "      live.add(this);\n"
    "    }\n"
    "    send(value) { this._check(); n.send_json(this._handle, JSON.stringify(value)); return this; }\n"
    "    sendBytes(bytes) { this._check(); n.send_proto(this._handle, bytes); return this; }\n"
    "    halfClose() { this._check(); n.half_close(this._handle); return this; }\n"
    "    recv(timeout) {\n"
    "      this._check();\n"
    "      const text = n.recv_json(this._handle, timeout === undefined ? 30000 : timeout);\n"
    "      return text === null ? null : JSON.parse(text);\n"
    "    }\n"
    "    recvBytes(timeout) {\n"
    "      this._check();\n"
    "      return n.recv_proto(this._handle, timeout === undefined ? 30000 : timeout);\n"
    "    }\n"
    "    tryRecv(timeout) {\n"
    "      this._check();\n"
    "      const wait = typeof timeout === \"number\" && timeout > 0 ? timeout : 0;\n"
    "      const text = n.try_recv_json(this._handle, wait);\n"
    "      if (text === undefined) return undefined;\n"
    "      return text === null ? null : JSON.parse(text);\n"
    "    }\n"
    "    tryRecvBytes(timeout) {\n"
    "      this._check();\n"
    "      const wait = typeof timeout === \"number\" && timeout > 0 ? timeout : 0;\n"
    "      return n.try_recv_proto(this._handle, wait);\n"
    "    }\n"
    "    end(code, detail) { this._finish(n.end_call, [this._handle, code === undefined ? 0 : code, detail === undefined ? \"\" : String(detail)]); return this; }\n"
    "    cancel() { this._finish(n.cancel, [this._handle]); return this; }\n"
    "    status() { const s = n.call_status(this._handle); return {code: s[0], detail: s[1]}; }\n"
    "    callInfo() { return JSON.parse(n.call_info(this._handle)); }\n"
    "    _check() { if (this._closed) throw new Error(\"call is closed\"); }\n"
    "    _finish(fn, args) {\n"
    "      if (this._closed) return;\n"
    "      this._closed = true;\n"
    "      try { fn.apply(n, args); } finally { this._releaseQuiet(); }\n"
    "    }\n"
    "    _releaseQuiet() {\n"
    "      if (!live.has(this)) return;\n"
    "      live.delete(this);\n"
    "      try { n.release(this._handle); } catch (_) {}\n"
    "    }\n"
    "  }\n"
    "  function track(call) { return call; }\n"
    "  function releaseAll() {\n"
    "    for (const call of Array.from(live)) {\n"
    "      try { call.cancel(); } catch (_) { call._releaseQuiet(); }\n"
    "    }\n"
    "  }\n"
    "  function start(timeout) {\n"
    "    if (started) return;\n"
    "    n.start(timeout === undefined ? 10000 : timeout);\n"
    "    started = true;\n"
    "  }\n"
    "  function on(kind, fn) {\n"
    "    if (typeof fn !== \"function\") throw new TypeError(\"handler must be a function\");\n"
    "    if (kind === \"call\") { callHandler = fn; return; }\n"
    "    if (kind === \"disconnect\") { disconnectHandler = fn; return; }\n"
    "    if (Object.values(EVENT_NAMES).indexOf(kind) < 0) {\n"
    "      throw new Error(\"unknown event \" + kind);\n"
    "    }\n"
    "    handlers[kind] = fn;\n"
    "  }\n"
    "  function run() {\n"
    "    if (!started) throw new Error(\"capsulad.start() must be called first\");\n"
    "    if (finished) return {status: n.disconnect_status(), detail: n.disconnect_detail()};\n"
    "    try {\n"
    "      for (;;) {\n"
    "        const job = n.poll(1000);\n"
    "        if (job === null) {\n"
    "          if (n.stopped() || n.disconnected()) break;\n"
    "          continue;\n"
    "        }\n"
    "        if (job.kind === \"event\") {\n"
    "          const name = EVENT_NAMES[job.event] || (\"unknown_\" + job.event);\n"
    "          const handler = handlers[name];\n"
    "          if (handler) handler(job.data === null ? null : JSON.parse(job.data));\n"
    "        } else if (job.kind === \"call\") {\n"
    "          const call = track(new Call(job.call, job.info === null ? null : JSON.parse(job.info)));\n"
    "          if (!callHandler) { call.end(12, \"no call handler registered\"); continue; }\n"
    "          try { callHandler(call); }\n"
    "          catch (err) {\n"
    "            if (!call._closed) call.end(13, String(err && err.message || err));\n"
    "            n.log(\"call handler failed: \" + String(err && err.stack || err));\n"
    "          }\n"
    "        } else if (job.kind === \"disconnect\") {\n"
    "          if (disconnectHandler) disconnectHandler({status: job.status, detail: job.detail});\n"
    "          break;\n"
    "        }\n"
    "      }\n"
    "    } finally { finished = true; releaseAll(); }\n"
    "    return {status: n.disconnect_status(), detail: n.disconnect_detail()};\n"
    "  }\n"
    "  function call(service, method, request, timeout) {\n"
    "    const text = n.unary(service, method, JSON.stringify(request === undefined ? {} : request), timeout);\n"
    "    return JSON.parse(text);\n"
    "  }\n"
    "  function callBytes(service, method, request, timeout) {\n"
    "    return n.unary_proto(service, method, request, timeout);\n"
    "  }\n"
    "  function open(service, method, kind, timeout) {\n"
    "    return track(new Call(n.call_start(service, method, kind === undefined ? 1 : kind, timeout), null));\n"
    "  }\n"
    "  function openRouted(appId, service, method, timeout) {\n"
    "    return track(new Call(n.call_start_routed(appId, service, method, timeout), null));\n"
    "  }\n"
    "  function stream(service, method, request, timeout) {\n"
    "    const c = open(service, method, 1, timeout);\n"
    "    if (request !== undefined && request !== null) c.send(request);\n"
    "    c.halfClose();\n"
    "    return c;\n"
    "  }\n"
    "  function routed(appId, service, method, request, timeout) {\n"
    "    const c = openRouted(appId, service, method, timeout);\n"
    "    try {\n"
    "      c.send(request === undefined ? {} : request);\n"
    "      c.halfClose();\n"
    "      const reply = c.recv(timeout === undefined ? 30000 : timeout);\n"
    "      c.end(0, \"\");\n"
    "      return reply;\n"
    "    } catch (err) { c.cancel(); throw err; }\n"
    "  }\n"
    "  function routedStream(appId, service, method, request, timeout) {\n"
    "    const c = openRouted(appId, service, method, timeout);\n"
    "    if (request !== undefined && request !== null) c.send(request);\n"
    "    c.halfClose();\n"
    "    return c;\n"
    "  }\n"
    "  return {\n"
    "    start, run, on, stop: () => { n.stop(); },\n"
    "    call, callBytes, open, openRouted, stream, routed, routedStream,\n"
    "    notifyInvocation: (id, accepted, detail) => n.notify_invocation(id, accepted, detail),\n"
    "    notifyAction: (id, succeeded, detail) => n.notify_action(id, succeeded, detail),\n"
    "    info: () => JSON.parse(n.client_info()),\n"
    "    handleInfo: (h) => JSON.parse(n.handle_info(h)),\n"
    "    registerDescriptors: (bytes) => n.register_descriptors(bytes),\n"
    "    readPackageFile: (path) => n.read_package_file(path),\n"
    "    ready: () => n.ready(),\n"
    "    connected: () => n.connected(),\n"
    "    disconnected: () => n.disconnected(),\n"
    "    state: () => ({ready: n.ready(), connected: n.connected(),\n"
    "                   disconnected: n.disconnected(),\n"
    "                   status: n.disconnect_status(), detail: n.disconnect_detail()}),\n"
    "    log: (message) => n.log(message),\n"
    "    env: n.env(),\n"
    "    WAIT_FOREVER: 0xFFFFFFFF\n"
    "  };\n"
    "})();\n";

static char *read_file(const char *path, size_t *length) {
  FILE *f = fopen(path, "rb");
  if (f == NULL) {
    return NULL;
  }
  if (fseek(f, 0, SEEK_END) != 0) {
    fclose(f);
    return NULL;
  }
  long size = ftell(f);
  if (size < 0 || (size_t)size > MAX_SCRIPT_BYTES) {
    fclose(f);
    return NULL;
  }
  rewind(f);
  char *data = (char *)malloc((size_t)size + 1);
  if (data == NULL) {
    fclose(f);
    return NULL;
  }
  size_t got = fread(data, 1, (size_t)size, f);
  fclose(f);
  if (got != (size_t)size) {
    free(data);
    return NULL;
  }
  data[got] = '\0';
  *length = got;
  return data;
}

static void dump_exception(JSContext *ctx, const char *where) {
  JSValue exception = JS_GetException(ctx);
  const char *text = JS_ToCString(ctx, exception);
  fprintf(stderr, "capsulad-quickjs: %s: %s\n", where,
          text != NULL ? text : "JavaScript exception");
  fflush(stderr);
  if (text != NULL) {
    JS_FreeCString(ctx, text);
  }
  JS_FreeValue(ctx, exception);
}

int main(int argc, char **argv) {
  if (argc == 2 && (strcmp(argv[1], "--version") == 0 ||
                    strcmp(argv[1], "-v") == 0)) {
    printf("quickjs-ng %s.%s.%s%s\n", CAPSULAD_STR(QJS_VERSION_MAJOR),
           CAPSULAD_STR(QJS_VERSION_MINOR), CAPSULAD_STR(QJS_VERSION_PATCH),
           QJS_VERSION_SUFFIX);
    return 0;
  }
  if (argc == 2 && strcmp(argv[1], "--check") == 0) {
    JSRuntime *check_rt = JS_NewRuntime();
    JSContext *check_ctx = check_rt != NULL ? JS_NewContext(check_rt) : NULL;
    if (check_ctx == NULL) {
      fprintf(stderr, "capsulad-quickjs: cannot create check context\n");
      return 1;
    }
    JS_SetContextOpaque(check_ctx, &host);
    install_native(check_ctx);
    JSValue check_result =
        JS_Eval(check_ctx, prelude, strlen(prelude), "<capsulad-sdk>",
                JS_EVAL_TYPE_GLOBAL);
    int check_ok = !JS_IsException(check_result);
    if (!check_ok) {
      dump_exception(check_ctx, "SDK prelude");
    } else {
      JS_FreeValue(check_ctx, check_result);
      printf("capsulad-quickjs: SDK prelude compiled\n");
    }
    JS_FreeContext(check_ctx);
    JS_FreeRuntime(check_rt);
    return check_ok ? 0 : 1;
  }
  if (argc < 2) {
    fprintf(stderr,
            "usage: capsulad-quickjs SCRIPT.js\n"
            "environment: CAPSULAD_SOCKET, CAPSULAD_RUN_ID, CAPSULAD_APP_ID,\n"
            "             CAPSULAD_LIBRARY, CAPSULAD_APP_DIR\n");
    return 2;
  }

  char err[512];
  if (capsulad_host_load(&host, err, sizeof(err)) != 0) {
    fprintf(stderr, "capsulad-quickjs: %s\n", err);
    return 1;
  }
  if (capsulad_host_connect(&host, 10000, err, sizeof(err)) != 0) {
    fprintf(stderr, "capsulad-quickjs: %s\n", err);
    return 1;
  }

  size_t script_len = 0;
  char *script = read_file(argv[1], &script_len);
  if (script == NULL) {
    fprintf(stderr, "capsulad-quickjs: cannot read %s: %s\n", argv[1],
            strerror(errno));
    return 1;
  }

  JSRuntime *rt = JS_NewRuntime();
  if (rt == NULL) {
    fprintf(stderr, "capsulad-quickjs: cannot create JavaScript runtime\n");
    free(script);
    return 1;
  }
  JS_SetMemoryLimit(rt, 64u * 1024u * 1024u);
  JS_SetMaxStackSize(rt, 1024u * 1024u);
  JS_SetCanBlock(rt, true);
  JSContext *ctx = JS_NewContext(rt);
  if (ctx == NULL) {
    fprintf(stderr, "capsulad-quickjs: cannot create JavaScript context\n");
    JS_FreeRuntime(rt);
    free(script);
    return 1;
  }
  JS_SetContextOpaque(ctx, &host);
  install_native(ctx);

  int exit_code = 0;
  JSValue prelude_result =
      JS_Eval(ctx, prelude, strlen(prelude), "<capsulad-sdk>",
              JS_EVAL_TYPE_GLOBAL);
  if (JS_IsException(prelude_result)) {
    dump_exception(ctx, "SDK prelude");
    exit_code = 1;
  } else {
    JS_FreeValue(ctx, prelude_result);
    JSValue result = JS_Eval(ctx, script, script_len, argv[1],
                             JS_EVAL_TYPE_GLOBAL);
    if (JS_IsException(result)) {
      dump_exception(ctx, argv[1]);
      exit_code = 1;
    } else {
      JS_FreeValue(ctx, result);
      JSValue global = JS_GetGlobalObject(ctx);
      JSValue capsulad = JS_GetPropertyStr(ctx, global, "capsulad");
      JSValue state = JS_UNDEFINED;
      if (!JS_IsUndefined(capsulad)) {
        JSValue state_fn = JS_GetPropertyStr(ctx, capsulad, "state");
        if (JS_IsFunction(ctx, state_fn)) {
          state = JS_Call(ctx, state_fn, capsulad, 0, NULL);
        }
        JS_FreeValue(ctx, state_fn);
      }
      int was_started = 0;
      if (!JS_IsUndefined(state) && !JS_IsException(state)) {
        JSValue started_value = JS_GetPropertyStr(ctx, state, "ready");
        was_started = JS_ToBool(ctx, started_value) > 0;
        JS_FreeValue(ctx, started_value);
        JS_FreeValue(ctx, state);
      }
      if (!was_started) {
        fprintf(stderr,
                "capsulad-quickjs: script exited without a ready app bus; "
                "call capsulad.start() before returning\n");
        exit_code = 1;
      } else {
        JSValue run = JS_GetPropertyStr(ctx, capsulad, "run");
        if (!JS_IsFunction(ctx, run)) {
          fprintf(stderr, "capsulad-quickjs: SDK run() is missing\n");
          exit_code = 1;
        } else {
          JSValue run_result = JS_Call(ctx, run, capsulad, 0, NULL);
          if (JS_IsException(run_result)) {
            dump_exception(ctx, "capsulad.run");
            exit_code = 1;
          } else {
            JS_FreeValue(ctx, run_result);
          }
        }
        JS_FreeValue(ctx, run);
      }
      JS_FreeValue(ctx, capsulad);
      JS_FreeValue(ctx, global);
    }
  }

  capsulad_host_close(&host);
  JS_FreeContext(ctx);
  JS_FreeRuntime(rt);
  free(script);
  return exit_code;
}
