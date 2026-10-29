# Supplier catalogue

A Go proof of concept for importing a supplier catalogue without losing saved
products when records repeat, arrive out of order, or processing stops.

## Run locally

Requires Docker Desktop with the Linux engine running. From the repository root:

```powershell
docker compose up --build -d
```

1. Open [http://localhost:55801](http://localhost:55801).
2. Choose a scenario under **Demo controls**, click **Start update**.

- For an outage, wait for **Needs attention**, click **Restore supplier**, then **Resume update**.
- **Reset demo** clears previous demo records.
- Use **Reset demo** between independent scenarios.
- Choose **Full 10,000-record import** for a larger demonstration. Try closing the browser.

Stop the stack with:

```powershell
docker compose down
```

Saved data survives `docker compose down` and subsequent starts. Add `-v` only
to delete the local database volume.

## Run tests

The GitHub **Integration** workflow checks secrets, Go formatting/vet, Docker
integration tests with the race detector, and live HTTP scenarios. It requires
no AWS account and does not deploy anything.

For repository contributions, install Gitleaks 8.30.1 on your PATH and run
`git config core.hooksPath .githooks`
to enable hooks that block secret/environment-file commits and direct main pushes.

From the repository root, run the Go tests with the race detector and DynamoDB
integration suite in Docker. This command starts DynamoDB Local if needed:

```powershell
docker compose --profile tools run --build --rm test
```

For faster checks on the host, install Go 1.26 and run:

```powershell
go test ./...
go vet ./...
```

Host tests skip the DynamoDB integration suite unless
`INTEGRATION_DYNAMODB_ENDPOINT` points to a running instance. Docker supplies Go
and the integration endpoint automatically.

## Configuration (for future AWS deployment)

Configuration defaults are in `.env.example` and `docker-compose.yml`.
`DYNAMODB_ENDPOINT` is required and selects DynamoDB Local. `AWS_REGION` is used
for SDK request signing. `TABLE_NAME` scopes all storage; `SUPPLIER_URL` is a fixed
trusted HTTP origin. `ALLOWED_ORIGIN` controls browser CORS. `HTTP_ADDR` sets the
local listener. `LEASE_SECONDS`,
`WORK_SLICE_SECONDS`, `RECORDS_PER_SLICE`, and `RECORD_INTERVAL_MS` bound worker
execution. Compose optionally reads
`AWS_REGION`, `TABLE_NAME`, `RECORDS_PER_SLICE` and
`RECORD_INTERVAL_MS` from a `.env` file; never commit credentials.

## Assumptions

- One supplier provides immutable updates with stable event IDs and pagination,
  plus increasing product versions. Versions determine order, not arrival time.
- Prices are integer cents. Withdrawals are explicit inactive records; missing
  products are not treated as deleted.
- One update runs at a time. Resuming requires the supplier to retain the original
  update; otherwise saved products remain and a new update must be started.
- This is a synthetic-data demo. There is no sign-in, so anyone with access can
  trigger demo actions.

## Unfinished work

- AWS deployment is unfinished, including infrastructure, cloud runtime handlers,
  packaging, hosting and cloud end-to-end tests.
- A real supplier connection and staff authentication are outside this proof of
  concept's current scope.

## AI usage

As Golang is a language foreign to me by the time of this assessment, AI usage was required. 

1. First, for every project, I like to set up my codebase with relevant skills. For this project, the ones I used are most notably Golang coding support skill and AWS deployment.
2. I then updated project context in AGENTS.md, the goal I would like to achieve, and my personal coding conventions.
3. For every commit, I have the AI run through an evaluation set to ensure the codes meet coding standards.
4. When the project is done scaffolding, I have the AI teach me back the codes, trace the data flow, and help me understand everything bits by bits.

That is my workflow mostly simplified, I would be more than delighted to be discussing more on this!
For any further information, please contact me at tvbngocc@gmail.com.
