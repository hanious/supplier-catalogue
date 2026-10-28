import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";

const origin = process.argv[2] || "";
if (origin && !/^https:\/\/[a-z0-9.-]+(?::\d+)?$/i.test(origin))
  throw new Error("Use an HTTPS API origin without a path.");
await mkdir("dist/web", { recursive: true });
let html = await readFile("web/index.html", "utf8");
for (const name of ["app.js", "style.css"]) {
  const contents = await readFile(`web/${name}`);
  const hash = createHash("sha256").update(contents).digest("hex").slice(0, 12);
  const [base, extension] = name.split(".");
  const fingerprinted = `${base}.${hash}.${extension}`;
  html = html.replace(name, fingerprinted);
  await writeFile(`dist/web/${fingerprinted}`, contents);
}
await writeFile("dist/web/index.html", html);
await writeFile("dist/web/favicon.svg", await readFile("web/favicon.svg"));
await writeFile(
  "dist/web/config.js",
  `window.CATALOGUE_API = ${JSON.stringify(origin)};\n`,
);
console.log("Packaged static assets in dist/web");
