/*
 * Minimal C consumer: connect, call DeviceService.GetInfo with JSON, print it,
 * and free every library-owned buffer.
 *
 * Build (host):
 *   cc -I sdk/native/include examples/c/device_info.c \
 *      -L build/lib -lcapsula -o build/bin/device_info
 * Run with CAPSULA_RUN_ID and optionally CAPSULA_SOCKET set.
 */
#include <capsula/capsula.h>

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static void fail(const capsula_api *api, const char *what, int32_t status) {
    fprintf(stderr, "%s failed: %d (%s)\n", what, status, api->last_error());
}

int main(void) {
    const capsula_api *api = capsula_api_v1();
    if (api == NULL || api->abi_major != CAPSULA_ABI_MAJOR) {
        fprintf(stderr, "unsupported native SDK ABI\n");
        return 1;
    }

    const char *run_id = getenv("CAPSULA_RUN_ID");
    if (run_id == NULL || run_id[0] == '\0') {
        fprintf(stderr, "CAPSULA_RUN_ID is required\n");
        return 1;
    }
    const char *socket = getenv("CAPSULA_SOCKET");

    capsula_handle client = CAPSULA_HANDLE_INVALID;
    int32_t status = api->client_connect(
        socket, socket ? strlen(socket) : 0,
        run_id, strlen(run_id),
        NULL, 0, 5000, &client);
    if (status != CAPSULA_STATUS_OK) {
        fail(api, "client_connect", status);
        return 1;
    }

    static const char service[] = "capsula.framework.v1.DeviceService";
    static const char method[] = "GetInfo";
    capsula_buf info = {0};
    status = api->client_unary(
        client, service, sizeof service - 1, method, sizeof method - 1,
        (const uint8_t *)"{}", 2, 5000, &info);
    if (status != CAPSULA_STATUS_OK) {
        fail(api, "DeviceService.GetInfo", status);
        api->client_close(client);
        return 1;
    }

    printf("%.*s\n", (int)info.len, (const char *)info.data);
    api->buf_free(info);
    api->client_close(client);
    return 0;
}
