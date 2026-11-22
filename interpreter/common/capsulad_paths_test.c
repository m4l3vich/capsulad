/*
 * Pure path-rule test for the script hosts. It needs no libcapsulad, no
 * interpreter and no filesystem: the same header the QuickJS host compiles.
 */
#include <assert.h>
#include <string.h>

#include "capsulad_paths.h"

int main(void) {
  struct {
    const char *path;
    int ok;
  } relative[] = {
      {"audio/beep.wav", 1},
      {"a", 1},
      {"a/b/c", 1},
      {"", 0},
      {"/absolute", 0},
      {"../secret", 0},
      {"a/../b", 0},
      {".", 0},
      {"a/./b", 0},
      {"a//b", 0},
      {"a/", 0},
      {"back\\slash", 0},
  };
  for (size_t i = 0; i < sizeof(relative) / sizeof(relative[0]); i++) {
    assert(capsulad_relative_path_ok(relative[i].path,
                                     strlen(relative[i].path)) == relative[i].ok);
  }

  struct {
    const char *path;
    int ok;
    const char *relative;
  } assets[] = {
      {"/assets/audio/beep.wav", 1, "audio/beep.wav"},
      {"/assets/beep.wav", 1, "beep.wav"},
      {"/assets/", 0, NULL},
      {"/assets", 0, NULL},
      {"assets/beep.wav", 0, NULL},
      {"/assets/../secret.wav", 0, NULL},
      {"/assets/a//b.wav", 0, NULL},
      {"/assets/a\\b.wav", 0, NULL},
      {"/assets/.hidden.wav", 1, ".hidden.wav"},
  };
  for (size_t i = 0; i < sizeof(assets) / sizeof(assets[0]); i++) {
    const char *relative_path = NULL;
    size_t relative_len = 0;
    int ok = capsulad_asset_relative_path(assets[i].path, strlen(assets[i].path),
                                          &relative_path, &relative_len);
    assert(ok == assets[i].ok);
    if (ok) {
      assert(relative_path != NULL);
      assert(relative_len == strlen(assets[i].relative));
      assert(memcmp(relative_path, assets[i].relative, relative_len) == 0);
    }
  }
  return 0;
}
