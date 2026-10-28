import { createApp } from "./stack";
import { AwsSolutionsChecks } from "cdk-nag";
import { writeFileSync } from "node:fs";
import { join } from "node:path";

const app = createApp();
const assembly = app.synth();
// Run against the construct tree explicitly: this works even when the installed
// CDK version does not pass appConstruct to validation plugins.
const report = new AwsSolutionsChecks().validateScope(app);
writeFileSync(
  join(assembly.directory, "nag-report.json"),
  JSON.stringify(report, null, 2),
);
if (!report.success)
  throw new Error(JSON.stringify(report.violations, null, 2));
