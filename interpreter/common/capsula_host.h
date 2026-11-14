/*
 * Shared host plumbing for the QuickJS and MicroPython app runners.
 *
 * The host loads the public libcapsula C ABI from CAPSULA_LIBRARY (or the
 * platform SONAME), connects with the launcher-assigned run identity, opens
 * the AppBus and copies inbound events/calls into a bounded queue. Language
 * bindings own their interpreter; this file owns no script engine and no
 * framework protocol knowledge beyond the public ABI.
 */
#ifndef CAPSULA_INTERPRETER_HOST_H
#define CAPSULA_INTERPRETER_HOST_H

#include <pthread.h>
#include <stddef.h>
#include <stdint.h>

#include "capsula/capsula.h"

#ifndef CAPSULA_DEFAULT_LIBRARY
#if defined(__APPLE__)
#define CAPSULA_DEFAULT_LIBRARY "libcapsula.1.dylib"
#else
#define CAPSULA_DEFAULT_LIBRARY "libcapsula.so.1"
#endif
#endif

/* Inbound work copied out of library callback threads. The language run loop
 * owns every job returned by capsula_host_poll. */
typedef enum capsula_host_job_kind {
  CAPSULA_HOST_JOB_NONE = 0,
  CAPSULA_HOST_JOB_EVENT = 1,
  CAPSULA_HOST_JOB_CALL = 2,
  CAPSULA_HOST_JOB_DISCONNECTED = 3
} capsula_host_job_kind;

typedef struct capsula_host_job {
  capsula_host_job_kind kind;
  int32_t event_kind;
  uint8_t *json;
  size_t json_len;
  uint64_t call;
  uint8_t *info;
  size_t info_len;
  int32_t status;
  char *detail;
  struct capsula_host_job *next;
} capsula_host_job;

typedef struct capsula_host {
  const capsula_api *api;
  capsula_handle client;
  capsula_handle bus;

  pthread_mutex_t mu;
  pthread_cond_t cv;
  capsula_host_job *head;
  capsula_host_job *tail;
  size_t queued;

  int client_connected;
  int bus_ready;
  int disconnected;
  int stopped;
  int32_t disconnect_status;
  char disconnect_detail[512];

  char last_detail[512];
  int32_t last_status;
} capsula_host;

/* capsula_host_load resolves the public ABI. It never unloads the library:
 * ABI v1 owns a process-lifetime runtime, so the handle stays resident. */
int capsula_host_load(capsula_host *h, char *err, size_t errlen);

/* capsula_host_connect establishes the framework client from CAPSULA_SOCKET,
 * CAPSULA_RUN_ID and CAPSULA_APP_ID. */
int capsula_host_connect(capsula_host *h, uint32_t timeout_ms, char *err,
                         size_t errlen);

/* capsula_host_start opens the AppBus and waits for READY. The library queues
 * AppHello/AppReady before the transport task, so READY is the daemon's
 * acknowledgement that this run's bus exists. */
int capsula_host_start(capsula_host *h, uint32_t timeout_ms, char *err,
                       size_t errlen);

/* capsula_host_poll waits up to timeout_ms for the next queued job. A negative
 * timeout blocks. It returns 1 with a job, 0 when nothing is queued (timeout,
 * stop or disconnect). */
int capsula_host_poll(capsula_host *h, int timeout_ms, capsula_host_job *out);

void capsula_host_free_job(capsula_host_job *job);

int capsula_host_connected(capsula_host *h);
int capsula_host_disconnected(capsula_host *h);
int capsula_host_stopped(capsula_host *h);
int capsula_host_bus_ready(capsula_host *h);

/* capsula_host_disconnect_status copies the terminal status/detail observed by
 * the bus callback. It returns 1 once a disconnect was reported. */
int capsula_host_disconnect_status(capsula_host *h, int32_t *status,
                                   char *detail, size_t detail_len);

/* capsula_host_close ends queued calls, closes the bus/session and drains the
 * queue. It is safe to call more than once. */
void capsula_host_close(capsula_host *h);

/* capsula_host_text_len strips the optional trailing NUL the SDK appends to
 * JSON/UTF-8 buffers. It is safe for raw protobuf only when the caller knows
 * the payload is text; protobuf callers use the buffer length unchanged. */
size_t capsula_host_text_len(const uint8_t *data, size_t len);

/* capsula_host_error records status and formats the thread-local SDK detail. */
int capsula_host_error(capsula_host *h, int32_t status, char *err,
                       size_t errlen);

#endif /* CAPSULA_INTERPRETER_HOST_H */
