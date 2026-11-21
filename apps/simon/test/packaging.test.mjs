// Packaging contract for the Simon Says package.
//
// The game's cues are files, so the staged package must contain every WAV
// byte-for-byte and no test tree. The test runs the real build script with
// PACKAGE=0; archive creation is the repository CLI's job and is checked by
// the build target separately.

import nodeTest from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { TONE_FILES } from "./wav.mjs";

const test = (name, fn) => nodeTest(name, { timeout: 30000 }, fn);

const here = dirname(fileURLToPath(import.meta.url));
const appDir = join(here, "..");
const root = join(appDir, "..", "..");

test("the build stages the manifest, script, guide and every packaged tone", () => {
  const out = mkdtempSync(join(tmpdir(), "simon-package-"));
  try {
    execFileSync("sh", [join(appDir, "build.sh")], {
      cwd: root,
      env: { ...process.env, OUT: out, PACKAGE: "0" },
      stdio: "pipe",
    });

    const stage = join(out, "org.capsulad.simon");
    assert.deepEqual(
      readdirSync(stage).sort(),
      ["README.md", "audio", "main.js", "manifest.toml"],
      "only documented package files may be staged",
    );
    assert.deepEqual(
      readdirSync(join(stage, "audio")).sort(),
      TONE_FILES.map((tone) => tone.name).sort(),
      "every source tone must be packaged",
    );

    for (const tone of TONE_FILES) {
      const source = readFileSync(join(appDir, "audio", tone.name));
      const staged = readFileSync(join(stage, "audio", tone.name));
      assert.ok(staged.equals(source), `${tone.name} must be staged byte-for-byte`);
    }
    assert.deepEqual(
      readFileSync(join(stage, "main.js")),
      readFileSync(join(appDir, "main.js")),
      "the packaged script must be the shipped main.js",
    );
    assert.ok(!readdirSync(stage).includes("test"), "the contract suite stays out of the package");
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
});
