import * as path from "node:path";
import { Construct } from "constructs";
import {
  App,
  CfnOutput,
  Duration,
  RemovalPolicy,
  Stack,
  StackProps,
  Validations,
  aws_amplify as amplify,
  aws_apigatewayv2 as apigw,
  aws_apigatewayv2_integrations as integrations,
  aws_cloudwatch as cloudwatch,
  aws_dynamodb as dynamodb,
  aws_events as events,
  aws_events_targets as targets,
  aws_iam as iam,
  aws_lambda as lambda,
  aws_lambda_event_sources as sources,
  aws_logs as logs,
  aws_sqs as sqs,
} from "aws-cdk-lib";

function acknowledge(
  construct: Construct,
  rules: { id: string; reason: string }[],
): void {
  for (const rule of rules) Validations.of(construct).acknowledge(rule);
}

export class DataStack extends Stack {
  readonly table: dynamodb.Table;
  constructor(scope: Construct, id: string, props?: StackProps) {
    super(scope, id, { ...props, terminationProtection: true });
    this.table = new dynamodb.Table(this, "Catalogue", {
      partitionKey: { name: "PK", type: dynamodb.AttributeType.STRING },
      sortKey: { name: "SK", type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PROVISIONED,
      readCapacity: 20,
      writeCapacity: 20,
      tableClass: dynamodb.TableClass.STANDARD,
      encryption: dynamodb.TableEncryption.DEFAULT,
      removalPolicy: RemovalPolicy.RETAIN,
      deletionProtection: true,
    });
    acknowledge(this.table, [
      {
        id: "AwsSolutions-DDB3",
        reason:
          "Synthetic demo only; recovery is replay of retained supplier fixtures. PITR is an optional separately priced service and is not needed for this POC.",
      },
    ]);
    new CfnOutput(this, "TableName", { value: this.table.tableName });
  }
}

interface ServiceProps extends StackProps {
  table: dynamodb.ITable;
}

export class ServiceStack extends Stack {
  constructor(scope: Construct, id: string, props: ServiceProps) {
    super(scope, id, props);
    const headers = JSON.stringify({
      customHeaders: [
        {
          pattern: "**",
          headers: [
            { key: "X-Content-Type-Options", value: "nosniff" },
            { key: "Referrer-Policy", value: "no-referrer" },
            {
              key: "Content-Security-Policy",
              value: `default-src 'self'; connect-src 'self' https://*.execute-api.${this.region}.${this.urlSuffix}; object-src 'none'; frame-ancestors 'none'; base-uri 'none'`,
            },
          ],
        },
        {
          pattern: "/app.*.js",
          headers: [
            {
              key: "Cache-Control",
              value: "public, max-age=31536000, immutable",
            },
          ],
        },
        {
          pattern: "/style.*.css",
          headers: [
            {
              key: "Cache-Control",
              value: "public, max-age=31536000, immutable",
            },
          ],
        },
        {
          pattern: "/config.js",
          headers: [{ key: "Cache-Control", value: "no-cache" }],
        },
        {
          pattern: "/index.html",
          headers: [{ key: "Cache-Control", value: "no-cache" }],
        },
      ],
    });
    const site = new amplify.CfnApp(this, "StaffSite", {
      name: `${this.stackName}-staff`,
      platform: "WEB",
      customHeaders: headers,
      enableBranchAutoDeletion: false,
    });
    new amplify.CfnBranch(this, "DemoBranch", {
      appId: site.attrAppId,
      branchName: "demo",
      enableAutoBuild: false,
      stage: "DEVELOPMENT",
    });
    const frontendOrigin = `https://demo.${site.attrDefaultDomain}`;
    const deadLetters = new sqs.Queue(this, "DeadLetters", {
      encryption: sqs.QueueEncryption.SQS_MANAGED,
      enforceSSL: true,
      retentionPeriod: Duration.days(4),
    });
    const queue = new sqs.Queue(this, "WorkQueue", {
      encryption: sqs.QueueEncryption.SQS_MANAGED,
      enforceSSL: true,
      visibilityTimeout: Duration.seconds(180),
      retentionPeriod: Duration.days(1),
      deadLetterQueue: { queue: deadLetters, maxReceiveCount: 5 },
    });
    const environment = {
      TABLE_NAME: props.table.tableName,
      SUPPLIER_URL: "http://unused.invalid",
      ALLOWED_ORIGIN: frontendOrigin,
      QUEUE_URL: queue.queueUrl,
      RECORD_INTERVAL_MS: "0",
      RECORDS_PER_SLICE: "4",
      LEASE_SECONDS: "45",
      WORK_SLICE_SECONDS: "15",
    };
    const code = lambda.Code.fromAsset(
      path.resolve(__dirname, "../../dist/lambda.zip"),
    );
    const makeFunction = (
      id: string,
      mode: string,
      timeout: number,
      concurrency: number,
    ) => {
      const logGroup = new logs.LogGroup(this, `${id}Logs`, {
        retention: logs.RetentionDays.ONE_WEEK,
        removalPolicy: RemovalPolicy.DESTROY,
      });
      const role = new iam.Role(this, `${id}Role`, {
        assumedBy: new iam.ServicePrincipal("lambda.amazonaws.com"),
      });
      logGroup.grantWrite(role);
      const fn = new lambda.Function(this, id, {
        code,
        handler: "bootstrap",
        runtime: lambda.Runtime.PROVIDED_AL2023,
        architecture: lambda.Architecture.ARM_64,
        memorySize: 256,
        timeout: Duration.seconds(timeout),
        reservedConcurrentExecutions: concurrency,
        role,
        logGroup,
        environment: { ...environment, MODE: mode },
      });
      acknowledge(fn, [
        {
          id: "AwsSolutions-L1",
          reason:
            "Go uses the supported provided.al2023 custom runtime; the deployment package contains a Go 1.26 ARM64 executable.",
        },
      ]);
      return fn;
    };
    const reads = ["dynamodb:GetItem", "dynamodb:Query"];
    const writes = [
      "dynamodb:PutItem",
      "dynamodb:ConditionCheckItem",
      "dynamodb:BatchWriteItem",
    ];
    const grant = (fn: lambda.Function, actions: string[]) =>
      fn.addToRolePolicy(
        new iam.PolicyStatement({ actions, resources: [props.table.tableArn] }),
      );
    const supplier = makeFunction("Supplier", "supplier", 10, 2);
    grant(supplier, ["dynamodb:GetItem"]);
    const supplierApi = new apigw.HttpApi(this, "SupplierAPI");
    supplierApi.addRoutes({
      path: "/updates/{id}",
      methods: [apigw.HttpMethod.GET],
      integration: new integrations.HttpLambdaIntegration(
        "SupplierIntegration",
        supplier,
      ),
    });
    const worker = makeFunction("Worker", "worker", 30, 2);
    worker.addEnvironment("SUPPLIER_URL", supplierApi.apiEndpoint);
    grant(worker, [...reads, ...writes]);
    queue.grantSendMessages(worker);
    worker.addEventSource(
      new sources.SqsEventSource(queue, {
        batchSize: 1,
        maxConcurrency: 2,
        reportBatchItemFailures: true,
      }),
    );
    const api = makeFunction("API", "api", 20, 2);
    grant(api, [...reads, "dynamodb:PutItem"]);
    queue.grantSendMessages(api);
    const httpApi = new apigw.HttpApi(this, "StaffAPI", {
      corsPreflight: {
        allowOrigins: [frontendOrigin],
        allowMethods: [
          apigw.CorsHttpMethod.GET,
          apigw.CorsHttpMethod.POST,
          apigw.CorsHttpMethod.OPTIONS,
        ],
        allowHeaders: ["Content-Type", "Idempotency-Key", "If-None-Match"],
        exposeHeaders: ["ETag"],
        maxAge: Duration.minutes(10),
      },
    });
    httpApi.addRoutes({
      path: "/api/{proxy+}",
      methods: [apigw.HttpMethod.GET, apigw.HttpMethod.POST],
      integration: new integrations.HttpLambdaIntegration(
        "StaffIntegration",
        api,
      ),
    });
    for (const http of [httpApi, supplierApi]) {
      const stage = http.defaultStage?.node.defaultChild as apigw.CfnStage;
      const access = new logs.LogGroup(this, `${http.node.id}AccessLogs`, {
        retention: logs.RetentionDays.ONE_WEEK,
        removalPolicy: RemovalPolicy.DESTROY,
      });
      stage.defaultRouteSettings = {
        throttlingBurstLimit: 10,
        throttlingRateLimit: 5,
      };
      stage.accessLogSettings = {
        destinationArn: access.logGroupArn,
        format: JSON.stringify({
          requestId: "$context.requestId",
          status: "$context.status",
          latency: "$context.responseLatency",
          route: "$context.routeKey",
        }),
      };
    }
    const dispatcher = makeFunction("Dispatcher", "dispatcher", 10, 1);
    grant(dispatcher, reads);
    queue.grantSendMessages(dispatcher);
    // First-use initialization may conditionally write META/CONTROL.
    grant(dispatcher, ["dynamodb:PutItem"]);
    const dispatch = new events.Rule(this, "RecoverDispatch", {
      schedule: events.Schedule.rate(Duration.minutes(1)),
    });
    dispatch.addTarget(
      new targets.LambdaFunction(dispatcher, {
        retryAttempts: 2,
        maxEventAge: Duration.minutes(5),
      }),
    );
    new cloudwatch.Alarm(this, "DeadLetterAlarm", {
      metric: deadLetters.metricApproximateNumberOfMessagesVisible(),
      threshold: 1,
      evaluationPeriods: 1,
      alarmDescription:
        "Inspect failed demo wakeups; durable update progress remains in DynamoDB.",
    });
    acknowledge(deadLetters, [
      {
        id: "AwsSolutions-SQS3",
        reason:
          "This queue is the terminal dead-letter queue. A second DLQ would only move the same failure again.",
      },
    ]);
    acknowledge(this, [
      {
        id: "AwsSolutions-APIG4",
        reason:
          "The agreed POC deliberately has no authentication and synthetic data only; gateway throttles and reserved concurrency bound public demo actions.",
      },
      {
        id: "AwsSolutions-COG4",
        reason:
          "No authentication is in the agreed scope; no Cognito user pool is required.",
      },
      {
        id: "AwsSolutions-IAM5",
        reason:
          "Only CDK-generated log-stream suffixes and SQS resource-policy principals use wildcards; write actions are scoped to the exact log group, queue, or table ARN. SSL deny policies must cover all principals.",
      },
    ]);
    new CfnOutput(this, "APIURL", { value: httpApi.apiEndpoint });
    new CfnOutput(this, "FrontendURL", { value: frontendOrigin });
    new CfnOutput(this, "AmplifyAppID", { value: site.attrAppId });
    new CfnOutput(this, "QueueURL", { value: queue.queueUrl });
  }
}

export function createApp(): App {
  const app = new App();
  const data = new DataStack(app, "CatalogueData");
  new ServiceStack(app, "CatalogueServices", { table: data.table });
  return app;
}
