/*
 * MicroPython configuration for the Capsula app host.
 *
 * The embed port provides the compiler, GC and minimal runtime. The capsula
 * module is injected into the loaded-module table by the host; JSON payloads
 * are encoded by the SDK prelude so no filesystem/IO module is needed.
 */
#include <port/mpconfigport_common.h>

#define MICROPY_CONFIG_ROM_LEVEL (MICROPY_CONFIG_ROM_LEVEL_MINIMUM)

#define MICROPY_ENABLE_COMPILER (1)
#define MICROPY_ENABLE_GC (1)
#define MICROPY_ENABLE_SOURCE_LINE (1)
/* The generic register-capture helper uses uninitialised fixed-register
 * variables, which current clang rejects on aarch64. setjmp is portable and
 * correct; it is only a GC root accuracy tradeoff. */
#define MICROPY_GCREGS_SETJMP (1)
#define MICROPY_PY_GC (1)
#define MICROPY_PY_SYS (1)
#define MICROPY_PY_BUILTINS_STR_OP_MODULO (1)
#define MICROPY_PY_BUILTINS_STR_UNICODE (1)
#define MICROPY_PY_BUILTINS_BYTEARRAY (1)
#define MICROPY_CPYTHON_COMPAT (1)
#define MICROPY_FLOAT_IMPL (MICROPY_FLOAT_IMPL_DOUBLE)
#define MICROPY_PY_SYS_PLATFORM "capsula"
#define MICROPY_PY_BUILTINS_SLICE (1)
#define MICROPY_PY_BUILTINS_SLICE_ATTRS (1)
#define MICROPY_PY_BUILTINS_SLICE_INDICES (1)

/* Scripts never load .mpy files or import from the filesystem; only built-in
 * modules (including the injected capsula module) exist. */
#define MICROPY_ENABLE_EXTERNAL_IMPORT (0)
#define MICROPY_MODULE_FROZEN_MPY (0)
#define MICROPY_MODULE_FROZEN_STR (0)
