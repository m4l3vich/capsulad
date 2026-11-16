/*
 * MicroPython app host.
 *
 * A real MicroPython runtime is embedded through the upstream embed port. The
 * capsulad module is injected into the loaded-module table before the SDK
 * prelude runs, so package scripts import it like a normal built-in module.
 */
#include <errno.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "py/compile.h"
#include "py/lexer.h"
#include "py/mpconfig.h"
#include "py/nlr.h"
#include "py/parse.h"
#include "py/runtime.h"
#include "py/stackctrl.h"
#include "port/micropython_embed.h"

#include "capsulad_host.h"

#include "sdk_prelude.h"

#define MAX_SCRIPT_BYTES (8u * 1024u * 1024u)
#define HEAP_BYTES (4u * 1024u * 1024u)

static uint64_t gc_heap[HEAP_BYTES / sizeof(uint64_t)];
static capsulad_host host;

/* Declared by modcapsulad.c. */
void capsulad_module_set_host(capsulad_host *host);
void capsulad_module_init(void);

static int exec_source(const char *name, const char *source, size_t length) {
  nlr_buf_t nlr;
  if (nlr_push(&nlr) == 0) {
    qstr source_name = qstr_from_str(name);
    mp_lexer_t *lex =
        mp_lexer_new_from_str_len(source_name, source, length, 0);
    mp_parse_tree_t parse_tree = mp_parse(lex, MP_PARSE_FILE_INPUT);
    mp_obj_t module_fun = mp_compile(&parse_tree, source_name, true);
    mp_call_function_0(module_fun);
    nlr_pop();
    return 0;
  }
  mp_obj_print_exception(&mp_plat_print, (mp_obj_t)nlr.ret_val);
  return 1;
}

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

int main(int argc, char **argv) {
  if (argc == 2 && (strcmp(argv[1], "--version") == 0 ||
                    strcmp(argv[1], "-v") == 0)) {
    printf("micropython %s\n", MICROPY_VERSION_STRING);
    return 0;
  }
  if (argc < 2) {
    fprintf(stderr,
            "usage: capsulad-micropython SCRIPT.py\n"
            "environment: CAPSULAD_SOCKET, CAPSULAD_RUN_ID, CAPSULAD_APP_ID,\n"
            "             CAPSULAD_LIBRARY, CAPSULAD_APP_DIR\n");
    return 2;
  }

  char err[512];
  if (argc == 2 && strcmp(argv[1], "--check") == 0) {
    int stack_top = 0;
    mp_embed_init(&gc_heap[0], sizeof(gc_heap), &stack_top);
    capsulad_module_set_host(NULL);
    capsulad_module_init();
    int ok = exec_source("<capsulad-sdk>", sdk_prelude,
                         strlen(sdk_prelude)) == 0;
    mp_embed_deinit();
    if (ok) {
      printf("capsulad-micropython: SDK prelude compiled\n");
      return 0;
    }
    return 1;
  }
  if (capsulad_host_load(&host, err, sizeof(err)) != 0) {
    fprintf(stderr, "capsulad-micropython: %s\n", err);
    return 1;
  }
  if (capsulad_host_connect(&host, 10000, err, sizeof(err)) != 0) {
    fprintf(stderr, "capsulad-micropython: %s\n", err);
    return 1;
  }

  size_t script_len = 0;
  char *script = read_file(argv[1], &script_len);
  if (script == NULL) {
    fprintf(stderr, "capsulad-micropython: cannot read %s: %s\n", argv[1],
            strerror(errno));
    capsulad_host_close(&host);
    return 1;
  }

  int stack_top = 0;
  mp_embed_init(&gc_heap[0], sizeof(gc_heap), &stack_top);
  capsulad_module_set_host(&host);
  capsulad_module_init();

  int exit_code = 0;
  if (exec_source("<capsulad-sdk>", sdk_prelude, strlen(sdk_prelude)) != 0) {
    fprintf(stderr, "capsulad-micropython: SDK prelude failed\n");
    exit_code = 1;
  } else if (exec_source(argv[1], script, script_len) != 0) {
    fprintf(stderr, "capsulad-micropython: %s failed\n", argv[1]);
    exit_code = 1;
  } else if (!capsulad_host_bus_ready(&host)) {
    fprintf(stderr,
            "capsulad-micropython: script exited without a ready app bus; "
            "call capsulad.start() before returning\n");
    exit_code = 1;
  } else {
    nlr_buf_t nlr;
    if (nlr_push(&nlr) == 0) {
      mp_obj_t module =
          mp_import_name(qstr_from_str("capsulad"), mp_const_none,
                         MP_OBJ_NEW_SMALL_INT(0));
      mp_obj_t run = mp_load_attr(module, qstr_from_str("run"));
      mp_call_function_0(run);
      nlr_pop();
    } else {
      mp_obj_print_exception(&mp_plat_print, (mp_obj_t)nlr.ret_val);
      exit_code = 1;
    }
  }

  capsulad_host_close(&host);
  mp_embed_deinit();
  free(script);
  return exit_code;
}
