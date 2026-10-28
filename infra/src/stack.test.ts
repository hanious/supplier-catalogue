import { test } from "node:test";
import assert from "node:assert/strict";
import { Annotations, Match, Template } from "aws-cdk-lib/assertions";
import {
  App,
  Stack,
  aws_sqs as sqs,
  aws_amplify as amplify,
} from "aws-cdk-lib";
import { AwsSolutionsChecks } from "cdk-nag";
import { createApp } from "./stack";

test("deployment bounds capacity, keeps state, and wires durable recovery", () => {
  const app = createApp();
  const data = app.node.findChild("CatalogueData") as Stack;
  const services = app.node.findChild("CatalogueServices") as Stack;
  const state = Template.fromStack(data);
  state.resourceCountIs("AWS::DynamoDB::Table", 1);
  state.hasResourceProperties("AWS::DynamoDB::Table", {
    ProvisionedThroughput: { ReadCapacityUnits: 20, WriteCapacityUnits: 20 },
    DeletionProtectionEnabled: true,
    GlobalSecondaryIndexes: Match.absent(),
    TimeToLiveSpecification: Match.absent(),
  });
  state.hasResource("AWS::DynamoDB::Table", {
    DeletionPolicy: "Retain",
    UpdateReplacePolicy: "Retain",
  });
  state.resourceCountIs("AWS::ApplicationAutoScaling::ScalableTarget", 0);
  const template = Template.fromStack(services);
  template.resourceCountIs("AWS::Lambda::Function", 4);
  template.allResourcesProperties("AWS::Lambda::Function", {
    Runtime: "provided.al2023",
    Architectures: ["arm64"],
  });
  template.hasResourceProperties("AWS::Lambda::Function", {Environment: {Variables: Match.objectLike({MODE:"worker",RECORDS_PER_SLICE:"2"})}});
  template.hasResourceProperties("AWS::Lambda::EventSourceMapping", {
    BatchSize: 1,
    FunctionResponseTypes: ["ReportBatchItemFailures"],
    ScalingConfig: { MaximumConcurrency: 2 },
  });
  template.hasResourceProperties("AWS::SQS::Queue", {
    VisibilityTimeout: 180,
    RedrivePolicy: Match.objectLike({ maxReceiveCount: 5 }),
  });
  template.hasResourceProperties("AWS::Events::Rule", {
    ScheduleExpression: "rate(1 minute)",
  });
  template.hasResourceProperties("AWS::ApiGatewayV2::Stage", {
    DefaultRouteSettings: { ThrottlingBurstLimit: 10, ThrottlingRateLimit: 5 },
  });
  template.hasResourceProperties("AWS::Amplify::Branch", {
    BranchName: "demo",
    EnableAutoBuild: false,
  });
  const headers = JSON.parse(
    (services.node.findChild("StaffSite") as amplify.CfnApp).customHeaders!,
  );
  assert.ok(
    Array.isArray(headers.customHeaders),
    "Amplify header configuration must use the documented root key",
  );
  const serialized = JSON.stringify(template.toJSON());
  assert.ok(
    !serialized.includes('"Action":"*"'),
    "wildcard IAM actions must not be introduced",
  );
  assert.ok(
    !serialized.includes("AWS_ACCESS_KEY_ID"),
    "deployed functions must use roles",
  );
  for (const stack of [data, services])
    Annotations.fromStack(stack).hasNoError("*", Match.anyValue());
  const report = new AwsSolutionsChecks().validateScope(app);
  assert.equal(
    report.success,
    true,
    JSON.stringify(report.violations, null, 2),
  );
});

test("security checks reject an insecure queue", () => {
  const app = new App();
  const stack = new Stack(app, "UnsafeFixture");
  new sqs.Queue(stack, "UnsafeQueue", {
    encryption: sqs.QueueEncryption.UNENCRYPTED,
  });
  Template.fromStack(stack);
  const report = new AwsSolutionsChecks().validateScope(app);
  assert.equal(
    report.success,
    false,
    "security checker must actually examine the construct tree",
  );
  assert.ok(report.violations.length > 0);
});
