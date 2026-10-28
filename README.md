# Supplier catalogue

A Go proof of concept for importing a supplier catalogue without losing saved
products when records repeat, arrive out of order, or processing stops.

Run the local stack with Docker Desktop's Linux engine:

```powershell
docker compose up --build -d
```

Open http://localhost:55801. Initialization loads 100 synthetic products into
persistent DynamoDB Local storage. Choose a scenario under **Demo controls**, then
click **Start update**. For an outage, wait for **Needs attention**, click
**Restore supplier**, then **Resume update**. Restoring availability does not
resume processing by itself. **Reset demo** builds a fresh baseline before
switching the visible catalogue and clearing previous demo records.

The floating **Demo controls** panel stays available while scrolling. Every table
has item numbers, exact page totals, search, status filters and sorting across all
matching records. The separate **Full 10,000-record import** remains available
for performance demonstrations. Existing local volumes keep their saved data;
use **Reset demo** to load the smaller baseline.

```powershell
go test ./...
go vet ./...
go test -race ./...
docker compose --profile tools run --build --rm test
docker compose down
```

The container test command includes the DynamoDB integration suite. Ordinary
`go test` skips those tests unless `INTEGRATION_DYNAMODB_ENDPOINT` is configured.
`docker compose down` preserves the database volume. Source requires Go 1.26;
Docker supplies it if the host has an older toolchain.

Configuration defaults are in `.env.example` and `docker-compose.yml`. Local
services use synthetic credentials. `DYNAMODB_ENDPOINT` selects DynamoDB Local;
AWS functions use execution roles and no custom endpoint. `TABLE_NAME` scopes all
storage; `SUPPLIER_URL` is a fixed trusted HTTP origin. `ALLOWED_ORIGIN` controls
browser CORS. `HTTP_ADDR` sets the local listener. `LEASE_SECONDS`,
`WORK_SLICE_SECONDS`, `RECORDS_PER_SLICE`, and `RECORD_INTERVAL_MS` bound worker
execution; `QUEUE_URL` is empty locally and is supplied by CDK in AWS. Local
Compose optionally reads AWS_REGION, TABLE_NAME, RECORDS_PER_SLICE and
RECORD_INTERVAL_MS from a `.env` file; never commit credentials.

The backend shares one importer between local processes and Go Lambda handlers.
AWS infrastructure uses Amplify Hosting, HTTP API Gateway, SQS, and one DynamoDB
Standard table with **20 provisioned RCUs and 20 WCUs**, without indexes or
autoscaling. Confirm the account's remaining allowance before deploying; other
AWS services have separate charges. This repository has not been deployed.

Build and validate deployment artifacts locally:

```powershell
powershell -ExecutionPolicy Bypass -File scripts/build-lambda.ps1
node scripts/package-web.mjs
Push-Location infra
npm.cmd ci
npm.cmd test
Pop-Location
```

Node.js 24 is recommended for infrastructure tooling. After a separately approved
CDK deployment, `scripts/publish-web.ps1` packages and publishes the static site
using `dist/outputs.json`.

Local documentation has three entry points: [codebase guide](docs/codebase.md),
[scenarios and triggers](docs/scenarios.md), and [run/deployment reference](docs/operations.md).
Those files are intentionally ignored and remain local. The staff interface has
no sign-in; publicly hosted demo actions can be triggered by visitors. Use
synthetic data only.
