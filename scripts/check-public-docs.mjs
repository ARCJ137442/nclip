// Check tracked publication inputs without printing matching private values.
import { execFileSync } from "node:child_process";
import { readFileSync, existsSync } from "node:fs";
import { dirname, resolve, relative } from "node:path";

const root = execFileSync("git", ["rev-parse", "--show-toplevel"], {
  encoding: "utf8",
}).trim();
const files = execFileSync("git", ["ls-files", "-z"], { cwd: root })
  .toString()
  .split("\0")
  .filter(Boolean);
// Review additions: arbitrary deployment hosts do not belong here.
const referenceHosts = new Set([
  "github.com",
  "api.github.com",
  "raw.githubusercontent.com",
  "docs.github.com",
  "ghcr.io",
  "go.dev",
  "golang.org",
  "google.github.io",
  "docs.aws.amazon.com",
  "kubernetes.io",
  "kubectl.docs.kubernetes.io",
  "www.w3.org",
]);
const failures = [];
const documents = new Map();
function fail(file, rule) {
  failures.push(`${file}: ${rule}`);
}
function exampleHost(host) {
  return (
    referenceHosts.has(host) ||
    host === "localhost" ||
    /^127(?:\.\d{1,3}){3}$/.test(host) ||
    /^(?:192\.0\.2|198\.51\.100|203\.0\.113)\.\d{1,3}$/.test(host) ||
    /(^|\.)example\.(com|org|net)$/.test(host) ||
    /\.(example|test|invalid)$/.test(host)
  );
}
for (const file of files) {
  const full = resolve(root, file);
  if (!existsSync(full)) continue;
  if (/(^|\/)(?:\.private|private|[^/]+-private|data|pastes|\.accounts)(\/|$)/.test(file))
    fail(file, "private data directory is tracked");
  if (/(^|\/)\.env(?:$|\.)/.test(file) && !file.endsWith(".env.example"))
    fail(file, "environment file is tracked");
  if (/\.(pem|key|bundle|tar|tar\.gz)$/i.test(file))
    fail(file, "private or archived deployment material is tracked");
  const data = readFileSync(full);
  if (data.includes(0)) continue;
  const text = data.toString("utf8");
  if (/-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----/.test(text))
    fail(file, "private key content");
  if (
    /(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{30,}|AKIA[A-Z0-9]{16})/.test(
      text,
    )
  )
    fail(file, "credential-like token");
  if (
    /(?:[A-Za-z]:[\\/]Users[\\/][^<$\s]+|\/home\/[^<$\s/]+|\/root\/)/.test(text)
  )
    fail(file, "personal filesystem path");
  for (const match of text.matchAll(
    /https?:\/\/([A-Za-z0-9_.-]+)(?=[:/\s"'`<>)]|$)/g,
  )) {
    if (!exampleHost(match[1].toLowerCase()))
      fail(file, "unreviewed external or deployment host");
  }
  if (file.endsWith(".md")) documents.set(file, text);
}
for (const [file, text] of documents) {
  for (const match of text.matchAll(/\]\(([^)]+)\)/g)) {
    let target = match[1].replace(/^<|>$/g, "");
    if (/^[a-z][a-z0-9+.-]*:/i.test(target) || target.startsWith("#")) continue;
    target = target.split("#")[0];
    if (!target) continue;
    const path = resolve(root, dirname(file), decodeURIComponent(target));
    if (relative(root, path).startsWith("..") || !existsSync(path))
      fail(file, "missing or checkout-external relative link");
  }
  if (
    /\b(?:today|yesterday|recently|last week)\b|今天|昨天|刚刚|最近|上周/i.test(
      text,
    )
  )
    fail(file, "relative-time wording in maintained documentation");
}
const readme = documents.get("README.md") || "";
const config = readFileSync(resolve(root, "config/config.go"), "utf8");
for (const [, name] of config.matchAll(
  /(?:set\w+Env\(|os\.Getenv\()"(NCLIP_[A-Z_]+)"/g,
)) {
  if (!readme.includes(`\`${name}\``))
    fail("README.md", "configured environment variable is undocumented");
}
for (const doc of documents.keys()) {
  if (doc.startsWith("Documents/") && doc !== "Documents/README.md") {
    const index = documents.get("Documents/README.md") || "";
    if (!index.includes(doc.slice("Documents/".length)))
      fail(doc, "missing from documentation index");
  }
}
if (failures.length) {
  console.error([...new Set(failures)].join("\n"));
  process.exitCode = 1;
} else {
  console.log(
    `PASS ${documents.size} Markdown files: links, inventory, configuration coverage and publication checks (${files.length} tracked files scanned)`,
  );
}
