import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";

const hooks = fileURLToPath(new URL("../.githooks/", import.meta.url));
const shell = process.platform === "win32" ? "C:/Program Files/Git/bin/bash.exe" : "sh";
const zero = "0".repeat(40);

function repository(t) {
  const directory = mkdtempSync(path.join(tmpdir(), "catalogue-hooks-"));
  t.after(() => {
    assert.equal(path.dirname(directory), path.resolve(tmpdir()));
    assert.ok(path.basename(directory).startsWith("catalogue-hooks-"));
    rmSync(directory, { recursive: true, force: true });
  });
  const git = (...args) => spawnSync("git", args, { cwd: directory, encoding: "utf8" });
  for (const args of [
    ["init", "--quiet", "--initial-branch=feature"],
    ["config", "user.name", "Hook test"],
    ["config", "user.email", "hooks@example.invalid"],
    ["config", "core.hooksPath", hooks],
  ]) {
    const result = git(...args);
    assert.equal(result.status, 0, result.stderr);
  }
  writeFileSync(path.join(directory, "readme.txt"), "Synthetic hook fixture.\n");
  assert.equal(git("add", "readme.txt").status, 0);
  return { directory, git };
}

test("pre-commit allows clean changes and rejects environment files and tokens", t => {
  const { directory, git } = repository(t);
  const clean = git("commit", "--quiet", "-m", "chore: clean fixture");
  assert.equal(clean.status, 0, clean.stderr);
  writeFileSync(path.join(directory, ".env"), "DEMO=true\n");
  assert.equal(git("add", ".env").status, 0);
  const environment = git("commit", "--quiet", "-m", "chore: unsafe environment");
  assert.notEqual(environment.status, 0);
  assert.match(environment.stderr, /Refusing to commit environment file/);
  assert.equal(git("reset", "--quiet").status, 0);
  writeFileSync(path.join(directory, "token.txt"), `token="ghp_${randomBytes(18).toString("hex")}"\n`);
  assert.equal(git("add", "token.txt").status, 0);
  const secret = git("commit", "--quiet", "-m", "chore: synthetic token");
  assert.notEqual(secret.status, 0);
  assert.match(secret.stderr, /leaks found/);
});

test("pre-push blocks main and scans commits on feature branches", t => {
  const { directory, git } = repository(t);
  assert.equal(git("commit", "--quiet", "-m", "chore: fixture").status, 0);
  const sha = git("rev-parse", "HEAD").stdout.trim();
  const push = (remoteRef, localSHA = sha) => spawnSync(shell, [path.join(hooks, "pre-push")], {
    cwd: directory, encoding: "utf8", input: `refs/heads/feature ${localSHA} ${remoteRef} ${zero}\n`,
  });
  assert.equal(push("refs/heads/feature").status, 0);
  assert.notEqual(push("refs/heads/main").status, 0);
  assert.equal(git("branch", "--move", "main").status, 0);
  assert.notEqual(push("refs/heads/feature").status, 0);
  assert.equal(git("branch", "--move", "feature").status, 0);
  writeFileSync(path.join(directory, "token.txt"), `token="ghp_${randomBytes(18).toString("hex")}"\n`);
  assert.equal(git("add", "token.txt").status, 0);
  assert.equal(git("-c", "core.hooksPath=", "commit", "--quiet", "-m", "chore: synthetic unsafe history").status, 0);
  const unsafeSHA = git("rev-parse", "HEAD").stdout.trim();
  assert.notEqual(push("refs/heads/feature", unsafeSHA).status, 0);
});
