/*
 * Shared host plumbing for the QuickJS and MicroPython app runners.
 *
 * The host loads the public libcapsulad C ABI from CAPSULAD_LIBRARY (or the
 * platform SONAME), connects with the launcher-assigned run identity, opens
 * the AppBus and copies inbound events/calls into a bounded queue. Language
 * bindings own their interpreter; this file owns no script engine and no
 * framework protocol knowledge beyond the public ABI.
 */
#ifndef CAPSULAD_INTERPRETER_HOST_H
#define CAPSULAD_INTERPRETER_HOST_H

#include <pthread.h>
#include <stddef.h>
#include <stdint.h>

#include "capsulad/capsulad.h"

#ifndef CAPSULAD_DEFAULT_LIBRARY
#if defined(__APPLE__)
#define CAPSULAD_DEFAULT_LIBRARY "libcapsulad.1.dylib"
#else
#define CAPSULAD_DEFAULT_LIBRARY "libcapsulad.so.1"
#endif
#endif

/* Inbound work copied out of library callback threads. The language run loop
 * owns every job returned by capsulad_host_poll. */
typedef enum capsulad_host_job_kind {
  CAPSULAD_HOST_JOB_NONE = 0,
  CAPSULAD_HOST_JOB_EVENT = 1,
  CAPSULAD_HOST_JOB_CALL = 2,
  CAPSULAD_HOST_JOB_DISCONNECTED = 3
} capsulad_host_job_kind;

typedef struct capsulad_host_job {
  capsulad_host_job_kind kind;
  int32_t event_kind;
  uint8_t *json;
  size_t json_len;
  uint64_t call;
  uint8_t *info;
  size_t info_len;
  int32_t status;
  char *detail;
  struct capsulad_host_job *next;
} capsulad_host_job;

typedef struct capsulad_host {
  const capsulad_api *api;
  capsulad_handle client;
  capsulad_handle bus;

  pthread_mutex_t mu;
  pthread_cond_t cv;
  capsulad_host_job *head;
  capsulad_host_job *tail;
  size_t queued;

  int client_connected;
  int bus_ready;
  int disconnected;
  int stopped;
  int32_t disconnect_status;
  char disconnect_detail[512];

  char last_detail[512];
  int32_t last_status;
} capsulad_host;

/* capsulad_host_load resolves the public ABI. It never unloads the library:
 * ABI v1 owns a process-lifetime runtime, so the handle stays resident. */
int capsulad_host_load(capsulad_host *h, char *err, size_t errlen);

/* capsulad_host_connect establishes the framework client from CAPSULAD_SOCKET,
 * CAPSULAD_RUN_ID and CAPSULAD_APP_ID. */
int capsulad_host_connect(capsulad_host *h, uint32_t timeout_ms, char *err,
                         size_t errlen);

/* capsulad_host_start opens the AppBus and waits for READY. The library queues
 * AppHello/AppReady before the transport task, so READY is the daemon's
 * acknowledgement that this run's bus exists. */
int capsulad_host_start(capsulad_host *h, uint32_t timeout_ms, char *err,
                       size_t errlen);

/* capsulad_host_poll waits up to timeout_ms for the next queued job. A negative
 * timeout blocks. It returns 1 with a job, 0 when nothing is queued (timeout,
 * stop or disconnect). */
int capsulad_host_poll(capsulad_host *h, int timeout_ms, capsulad_host_job *out);

void capsulad_host_free_job(capsulad_host_job *job);

int capsulad_host_connected(capsulad_host *h);
int capsulad_host_disconnected(capsulad_host *h);
int capsulad_host_stopped(capsulad_host *h);
int capsulad_host_bus_ready(capsulad_host *h);

/* capsulad_host_disconnect_status copies the terminal status/detail observed by
 * the bus callback. It returns 1 once a disconnect was reported. */
int capsulad_host_disconnect_status(capsulad_host *h, int32_t *status,
                                   char *detail, size_t detail_len);

/* capsulad_host_close ends queued calls, closes the bus/session and drains the
 * queue. It is safe to call more than once. */
void capsulad_host_close(capsulad_host *h);

/* capsulad_host_text_len strips the optional trailing NUL the SDK appends to
 * JSON/UTF-8 buffers. It is safe for raw protobuf only when the caller knows
 * the payload is text; protobuf callers use the buffer length unchanged. */
size_t capsulad_host_text_len(const uint8_t *data, size_t len);

/* capsulad_host_error records status and formats the thread-local SDK detail. */
int capsulad_host_error(capsulad_host *h, int32_t status, char *err,
                       size_t errlen);

#endif /* CAPSULAD_INTERPRETER_HOST_H */
