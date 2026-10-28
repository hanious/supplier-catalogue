package catalogue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Control serializes supplier starts, reset publication, and cache generations.
type Control struct {
	Epoch        string   `json:"epoch"`
	Active       string   `json:"active"`
	Current      string   `json:"current"`
	Latest       string   `json:"latest"`
	Sequence     int64    `json:"sequence"`
	Generation   int64    `json:"generation"`
	DataRevision int64    `json:"data_revision"`
	Revision     int64    `json:"-"`
	ResetFrom    string   `json:"reset_from,omitempty"`
	Retired      []string `json:"retired,omitempty"`
}

// Store uses DynamoDB transactions as the persistence boundary, locally and in AWS.
type Store struct {
	DB    *dynamodb.Client
	Table string
}

type item struct {
	PK    string
	SK    string
	Data  string
	Rev   int64
	Lease int64
	Token string
}

func key(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{"PK": &types.AttributeValueMemberS{Value: pk}, "SK": &types.AttributeValueMemberS{Value: sk}}
}

func encode(pk, sk string, value any, rev int64) (map[string]types.AttributeValue, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	row := item{PK: pk, SK: sk, Data: string(data), Rev: rev}
	if job, ok := value.(Job); ok {
		row.Lease = job.LeaseUntil
		row.Token = job.Token
	}
	return attributevalue.MarshalMap(row)
}

func (s *Store) read(ctx context.Context, pk, sk string, value any) (int64, error) {
	out, err := s.DB.GetItem(ctx, &dynamodb.GetItemInput{TableName: aws.String(s.Table), Key: key(pk, sk), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", pk, err)
	}
	if len(out.Item) == 0 {
		return 0, ErrNotFound
	}
	var row item
	if err := attributevalue.UnmarshalMap(out.Item, &row); err != nil {
		return 0, err
	}
	if err := json.Unmarshal([]byte(row.Data), value); err != nil {
		return 0, err
	}
	if job, ok := value.(*Job); ok {
		job.Token = row.Token
		job.LeaseUntil = row.Lease
	}
	return row.Rev, nil
}

func (s *Store) put(pk, sk string, value any, rev int64) (types.TransactWriteItem, error) {
	attrs, err := encode(pk, sk, value, rev+1)
	if err != nil {
		return types.TransactWriteItem{}, err
	}
	p := &types.Put{TableName: aws.String(s.Table), Item: attrs, ConditionExpression: aws.String("attribute_not_exists(PK)")}
	if rev > 0 {
		p.ConditionExpression = aws.String("Rev = :rev")
		p.ExpressionAttributeValues = map[string]types.AttributeValue{":rev": &types.AttributeValueMemberN{Value: strconv.FormatInt(rev, 10)}}
	}
	return types.TransactWriteItem{Put: p}, nil
}

func (s *Store) transaction(ctx context.Context, writes ...types.TransactWriteItem) error {
	_, err := s.DB.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: writes})
	var cancelled *types.TransactionCanceledException
	if errors.As(err, &cancelled) {
		for _, reason := range cancelled.CancellationReasons {
			if aws.ToString(reason.Code) == "ConditionalCheckFailed" {
				return ErrConflict
			}
		}
	}
	return err
}

// EnsureTable creates only the explicitly configured local table.
func (s *Store) EnsureTable(ctx context.Context) error {
	_, err := s.DB.CreateTable(ctx, &dynamodb.CreateTableInput{TableName: aws.String(s.Table), AttributeDefinitions: []types.AttributeDefinition{{AttributeName: aws.String("PK"), AttributeType: types.ScalarAttributeTypeS}, {AttributeName: aws.String("SK"), AttributeType: types.ScalarAttributeTypeS}}, KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("PK"), KeyType: types.KeyTypeHash}, {AttributeName: aws.String("SK"), KeyType: types.KeyTypeRange}}, BillingMode: types.BillingModeProvisioned, ProvisionedThroughput: &types.ProvisionedThroughput{ReadCapacityUnits: aws.Int64(20), WriteCapacityUnits: aws.Int64(20)}})
	var exists *types.ResourceInUseException
	if err != nil && !errors.As(err, &exists) {
		return err
	}
	return dynamodb.NewTableExistsWaiter(s.DB).Wait(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(s.Table)}, time.Minute)
}

// Control returns or initializes the small durable coordinator.
func (s *Store) Control(ctx context.Context) (Control, error) {
	var c Control
	rev, err := s.read(ctx, "META", "CONTROL", &c)
	if errors.Is(err, ErrNotFound) {
		c = Control{Epoch: "E" + digest(time.Now().UTC().String())}
		write, e := s.put("META", "CONTROL", c, 0)
		if e != nil {
			return c, e
		}
		if e = s.transaction(ctx, write); e != nil && !errors.Is(e, ErrConflict) {
			return c, e
		}
		return s.Control(ctx)
	}
	c.Revision = rev
	return c, err
}

// Job loads durable status and its fencing revision.
func (s *Store) Job(ctx context.Context, id string) (Job, error) {
	var j Job
	rev, err := s.read(ctx, "JOBS", id, &j)
	j.Revision = rev
	return j, err
}

// Manifest retrieves the original retained stream, never a moving latest feed.
func (s *Store) Manifest(ctx context.Context, id string) (Manifest, error) {
	var m Manifest
	_, err := s.read(ctx, "UPDATES", id, &m)
	return m, err
}

// Product reads one product using strong consistency.
func (s *Store) Product(ctx context.Context, epoch, id string) (*Product, error) {
	var p Product
	_, err := s.read(ctx, epoch+"|PRODUCTS", id, &p)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return &p, err
}

// Start atomically publishes an immutable fixture, takes the supplier lock, and saves a job.
// Reusing a request key or an update ID returns the original job.
func (s *Store) Start(ctx context.Context, scenario, requestID, updateID string) (Job, error) {
	if !ValidID(requestID) {
		return Job{}, invalid("a bounded idempotency key is required")
	}
	for attempt := 0; attempt < 8; attempt++ {
		var prior struct {
			JobID string
			Epoch string
		}
		if _, err := s.read(ctx, "REQUESTS", requestID, &prior); err == nil {
			return s.Job(ctx, prior.JobID)
		} else if !errors.Is(err, ErrNotFound) {
			return Job{}, err
		}
		if updateID != "" {
			if existing, err := s.Job(ctx, updateID); err == nil {
				return existing, nil
			} else if !errors.Is(err, ErrNotFound) {
				return Job{}, err
			}
		}
		c, err := s.Control(ctx)
		if err != nil {
			return Job{}, err
		}
		if c.Current == "" && scenario != "baseline" {
			return Job{}, invalid("load the demo baseline with Reset demo first")
		}
		if scenario == "baseline" && len(c.Retired) > 0 {
			return Job{}, fmt.Errorf("%w: previous demo cleanup is still running", ErrConflict)
		}
		var abandoned *Job
		if c.Active != "" && scenario == "baseline" {
			old, e := s.Job(ctx, c.Active)
			if e != nil {
				return Job{}, e
			}
			if old.Status == "interrupted" {
				old.Status = "abandoned"
				old.FinishedAt = timestamp()
				abandoned = &old
			} else {
				return Job{}, fmt.Errorf("%w: wait for active processing before resetting", ErrConflict)
			}
		}
		if c.Active != "" && abandoned == nil {
			return Job{}, fmt.Errorf("%w: an unfinished update already exists", ErrConflict)
		}
		// Bound retained jobs and their record ledgers. Reset starts a fresh demo.
		if c.Sequence >= 50 && scenario != "baseline" {
			return Job{}, invalid("demo history limit reached; reset the demo")
		}
		var m Manifest
		var manifestWrite *types.TransactWriteItem
		if scenario != "baseline" && (updateID != "" || c.Latest != "") {
			id := updateID
			if id == "" {
				id = c.Latest
			}
			m, err = s.Manifest(ctx, id)
			if err != nil {
				return Job{}, err
			}
			if _, e := s.Job(ctx, id); e == nil {
				m = Manifest{}
			} else if !errors.Is(e, ErrNotFound) {
				return Job{}, e
			}
		}
		if m.ID == "" {
			c.Sequence++
			m, err = NewManifest("U"+digest(requestID), scenario, c.Sequence)
			if err != nil {
				return Job{}, err
			}
			m.Epoch = c.Epoch
			if scenario == "baseline" {
				m.Epoch = "E" + digest(requestID)
			}
			write, e := s.put("UPDATES", m.ID, m, 0)
			if e != nil {
				return Job{}, e
			}
			manifestWrite = &write
		}
		j := Job{ID: m.ID, Epoch: c.Epoch, Scenario: m.Scenario, Status: "running", Total: m.Total, CreatedAt: timestamp(), UpdatedAt: timestamp()}
		if m.Scenario == "baseline" {
			j.Epoch = "E" + digest(requestID)
			c.ResetFrom = c.Epoch
		}
		c.Active = j.ID
		c.Current = j.ID
		c.Latest = m.ID
		cw, e := s.put("META", "CONTROL", c, c.Revision)
		if e != nil {
			return j, e
		}
		jw, e := s.put("JOBS", j.ID, j, 0)
		if e != nil {
			return j, e
		}
		rw, e := s.put("REQUESTS", requestID, struct {
			JobID string
			Epoch string
		}{JobID: j.ID, Epoch: j.Epoch}, 0)
		if e != nil {
			return j, e
		}
		writes := []types.TransactWriteItem{cw, jw, rw}
		if abandoned != nil {
			aw, e := s.put("JOBS", abandoned.ID, *abandoned, abandoned.Revision)
			if e != nil {
				return j, e
			}
			writes = append(writes, aw)
		}
		if manifestWrite != nil {
			writes = append(writes, *manifestWrite)
		}
		if err = s.transaction(ctx, writes...); errors.Is(err, ErrConflict) {
			continue
		} else if err != nil {
			return j, err
		}
		return s.Job(ctx, j.ID)
	}
	return Job{}, ErrConflict
}

// Publish queues a new source manifest without replacing the active update.
func (s *Store) Publish(ctx context.Context, scenario, requestID string) (Manifest, error) {
	if !ValidID(requestID) || scenario == "baseline" {
		return Manifest{}, invalid("invalid publication")
	}
	id := "U" + digest(requestID)
	if m, err := s.Manifest(ctx, id); err == nil {
		return m, nil
	}
	c, err := s.Control(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if c.Sequence >= 50 {
		return Manifest{}, invalid("demo history limit reached; reset the demo")
	}
	c.Sequence++
	m, err := NewManifest(id, scenario, c.Sequence)
	if err != nil {
		return m, err
	}
	m.Epoch = c.Epoch
	c.Latest = id
	cw, err := s.put("META", "CONTROL", c, c.Revision)
	if err != nil {
		return m, err
	}
	mw, err := s.put("UPDATES", id, m, 0)
	if err != nil {
		return m, err
	}
	return m, s.transaction(ctx, cw, mw)
}

// Restore changes availability only; it neither resumes nor imports records.
func (s *Store) Restore(ctx context.Context, id string) error {
	var m Manifest
	rev, err := s.read(ctx, "UPDATES", id, &m)
	if err != nil {
		return err
	}
	m.Restored = true
	w, err := s.put("UPDATES", id, m, rev)
	if err != nil {
		return err
	}
	return s.transaction(ctx, w)
}

// Acquire takes a lease with a fresh fencing token after any prior lease expires.
func (s *Store) Acquire(ctx context.Context, id, token string, lease time.Duration) (Job, error) {
	j, err := s.Job(ctx, id)
	if err != nil {
		return j, err
	}
	now := time.Now().UnixMilli()
	if j.Status != "running" || j.LeaseUntil > now || j.NextAttempt > now {
		return j, ErrConflict
	}
	j.Token = token
	j.LeaseUntil = time.Now().Add(lease).UnixMilli()
	j.UpdatedAt = timestamp()
	w, err := s.put("JOBS", id, j, j.Revision)
	if err != nil {
		return j, err
	}
	check := types.TransactWriteItem{ConditionCheck: &types.ConditionCheck{TableName: aws.String(s.Table), Key: key("META", "CONTROL"), ConditionExpression: aws.String("contains(#data, :id)"), ExpressionAttributeNames: map[string]string{"#data": "Data"}, ExpressionAttributeValues: map[string]types.AttributeValue{":id": &types.AttributeValueMemberS{Value: `"active":"` + id + `"`}}}}
	if err = s.transaction(ctx, w, check); err != nil {
		return j, err
	}
	j.Revision++
	return j, nil
}

func (s *Store) fencedJob(j Job) (types.TransactWriteItem, error) {
	w, err := s.put("JOBS", j.ID, j, j.Revision)
	if err != nil {
		return w, err
	}
	w.Put.ConditionExpression = aws.String("Rev = :rev AND Lease >= :now AND #token = :token")
	w.Put.ExpressionAttributeNames = map[string]string{"#token": "Token"}
	w.Put.ExpressionAttributeValues[":now"] = &types.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().UnixMilli(), 10)}
	w.Put.ExpressionAttributeValues[":token"] = &types.AttributeValueMemberS{Value: j.Token}
	return w, nil
}

// Commit saves a product, its outcome, counters, and generation in one transaction.
func (s *Store) Commit(ctx context.Context, j *Job, record Record) error {
	if !ValidID(record.SourceID) {
		return fmt.Errorf("supplier record has no stable source identity")
	}
	var previous Result
	if _, err := s.read(ctx, j.Epoch+"|RESULTS|"+j.ID, record.SourceID, &previous); err == nil {
		j.Offset++
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	var old *Product
	var err error
	if ValidID(record.Product.ID) {
		old, err = s.Product(ctx, j.Epoch, record.Product.ID)
		if err != nil {
			return err
		}
	}
	r := Decide(old, record)
	r.UpdateID = j.ID
	c, err := s.Control(ctx)
	if err != nil {
		return err
	}
	if c.Active != j.ID {
		return ErrConflict
	}
	next := *j
	next.Processed++
	next.Offset++
	next.Counts.Add(r.Kind)
	next.UpdatedAt = timestamp()
	jw, err := s.fencedJob(next)
	if err != nil {
		return err
	}
	rw, err := s.put(j.Epoch+"|RESULTS|"+j.ID, record.SourceID, r, 0)
	if err != nil {
		return err
	}
	writes := []types.TransactWriteItem{jw, rw}
	if r.Kind != "issue" && (old == nil || r.After.Version > old.Version) {
		// All product writes are fenced by the job CAS and the single supplier lock.
		attrs, e := encode(j.Epoch+"|PRODUCTS", r.After.ID, r.After, 1)
		if e != nil {
			return e
		}
		writes = append(writes, types.TransactWriteItem{Put: &types.Put{TableName: aws.String(s.Table), Item: attrs}})
		if j.Epoch == c.Epoch {
			c.DataRevision++
			feed, e := s.put(j.Epoch+"|FEED", position(c.DataRevision), r.After, 0)
			if e != nil {
				return e
			}
			writes = append(writes, feed)
		}
		if r.Kind != "ignored" && j.Epoch == c.Epoch {
			c.Generation++
		}
	}
	if r.Kind != "issue" && r.Kind != "ignored" {
		next.LastChange = next.Processed
		// An ordered read projection keeps sparse change pages and totals exact.
		change, e := s.put(j.Epoch+"|CHANGES|"+j.ID, position(int64(next.Processed)), r, 0)
		if e != nil {
			return e
		}
		writes = append(writes, change)
		jw, e = s.fencedJob(next)
		if e != nil {
			return e
		}
		writes[0] = jw
	}
	if r.Kind == "issue" {
		iw, e := s.put(j.Epoch+"|ISSUES", j.ID+"|"+record.SourceID, r, 0)
		if e != nil {
			return e
		}
		writes = append(writes, iw)
	}
	cw, err := s.put("META", "CONTROL", c, c.Revision)
	if err != nil {
		return err
	}
	writes = append(writes, cw)
	if err = s.transaction(ctx, writes...); err != nil {
		return err
	}
	next.Revision++
	*j = next
	return nil
}

// Checkpoint advances only after every page record has a durable outcome.
func (s *Store) Checkpoint(ctx context.Context, j *Job, page Page) error {
	for _, record := range page.Records {
		var result Result
		if _, err := s.read(ctx, j.Epoch+"|RESULTS|"+j.ID, record.SourceID, &result); err != nil {
			return fmt.Errorf("checkpoint missing outcome: %w", err)
		}
	}
	next := *j
	next.Cursor = page.Next
	next.Offset = 0
	next.Attempts = 0
	next.NextAttempt = 0
	next.Message = ""
	next.UpdatedAt = timestamp()
	c, err := s.Control(ctx)
	if err != nil {
		return err
	}
	if c.Active != j.ID {
		return ErrConflict
	}
	if page.End {
		status, e := next.Completion(true)
		if e != nil {
			return e
		}
		next.Status = status
		next.FinishedAt = timestamp()
		c.Active = ""
		if j.Scenario == "baseline" {
			c.Retired = append(c.Retired, c.Epoch)
			c.Epoch = j.Epoch
			c.DataRevision = 0
			c.Sequence = 0
			c.Generation++
			c.ResetFrom = ""
		}
	}
	jw, err := s.fencedJob(next)
	if err != nil {
		return err
	}
	cw, err := s.put("META", "CONTROL", c, c.Revision)
	if err != nil {
		return err
	}
	if err = s.transaction(ctx, jw, cw); err != nil {
		return err
	}
	next.Revision++
	*j = next
	return nil
}

// Release persists a bounded invocation's state and returns its lease.
func (s *Store) Release(ctx context.Context, j *Job) error {
	next := *j
	next.LeaseUntil = 0
	next.UpdatedAt = timestamp()
	w, err := s.fencedJob(next)
	if err != nil {
		return err
	}
	writes := []types.TransactWriteItem{w}
	if next.Status == "unavailable" {
		c, e := s.Control(ctx)
		if e != nil {
			return e
		}
		if c.Active != j.ID {
			return ErrConflict
		}
		c.Active = ""
		if c.Latest == j.ID {
			c.Latest = ""
		}
		cw, e := s.put("META", "CONTROL", c, c.Revision)
		if e != nil {
			return e
		}
		writes = append(writes, cw)
	}
	if err = s.transaction(ctx, writes...); err != nil {
		return err
	}
	next.Revision++
	*j = next
	return nil
}

// Resume restarts only interrupted work, preserving its exact update and cursor.
func (s *Store) Resume(ctx context.Context, id string) (Job, error) {
	j, err := s.Job(ctx, id)
	if err != nil {
		return j, err
	}
	if j.Status == "running" || !j.Unfinished() {
		return j, nil
	}
	c, err := s.Control(ctx)
	if err != nil {
		return j, err
	}
	if c.Active != id {
		return j, ErrConflict
	}
	j.Status = "running"
	j.Message = ""
	j.Attempts = 0
	j.NextAttempt = 0
	j.Token = ""
	j.LeaseUntil = 0
	j.UpdatedAt = timestamp()
	w, err := s.put("JOBS", id, j, j.Revision)
	if err != nil {
		return j, err
	}
	if err = s.transaction(ctx, w); err != nil {
		return j, err
	}
	return s.Job(ctx, id)
}

// Rows performs bounded, strongly consistent key pagination with no table scans.
func (s *Store) Rows(ctx context.Context, pk, cursor string, limit int, descending bool) ([]item, string, error) {
	input := &dynamodb.QueryInput{TableName: aws.String(s.Table), KeyConditionExpression: aws.String("PK = :pk"), ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: pk}}, Limit: aws.Int32(int32(limit)), ConsistentRead: aws.Bool(true), ScanIndexForward: aws.Bool(!descending)}
	if cursor != "" {
		input.ExclusiveStartKey = key(pk, cursor)
	}
	out, err := s.DB.Query(ctx, input)
	if err != nil {
		return nil, "", err
	}
	rows := make([]item, 0, len(out.Items))
	for _, attrs := range out.Items {
		var row item
		if err := attributevalue.UnmarshalMap(attrs, &row); err != nil {
			return nil, "", err
		}
		rows = append(rows, row)
	}
	next := ""
	if sk, ok := out.LastEvaluatedKey["SK"].(*types.AttributeValueMemberS); ok {
		next = sk.Value
	}
	return rows, next, nil
}

// SeedLocal creates the initial baseline only before any app services start.
func (s *Store) SeedLocal(ctx context.Context) error {
	c, err := s.Control(ctx)
	if err != nil {
		return err
	}
	if c.Current != "" {
		return nil
	}
	for start := 1; start <= DemoProductCount; start += 25 {
		requests := make([]types.WriteRequest, 0, 25)
		for i := start; i < min(start+25, DemoProductCount+1); i++ {
			p := BaselineProduct(i)
			attrs, e := encode(c.Epoch+"|PRODUCTS", p.ID, p, 1)
			if e != nil {
				return e
			}
			requests = append(requests, types.WriteRequest{PutRequest: &types.PutRequest{Item: attrs}})
		}
		for len(requests) > 0 {
			out, e := s.DB.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{s.Table: requests}})
			if e != nil {
				return e
			}
			requests = out.UnprocessedItems[s.Table]
			if len(requests) > 0 {
				if e = wait(ctx, 100*time.Millisecond); e != nil {
					return e
				}
			}
		}
	}
	c.Current = "baseline"
	c.Generation++
	w, err := s.put("META", "CONTROL", c, c.Revision)
	if err != nil {
		return err
	}
	return s.transaction(ctx, w)
}

// Cleanup retires only old demo epochs after a reset publishes its new baseline.
// Each call deletes at most 25 items, keeping worker invocations bounded.
func (s *Store) Cleanup(ctx context.Context) (bool, error) {
	c, err := s.Control(ctx)
	if err != nil {
		return false, err
	}
	if len(c.Retired) == 0 {
		return false, nil
	}
	epoch := c.Retired[0]
	for _, pk := range []string{epoch + "|PRODUCTS", epoch + "|ISSUES", epoch + "|FEED"} {
		rows, _, e := s.Rows(ctx, pk, "", 25, false)
		if e != nil {
			return true, e
		}
		if len(rows) > 0 {
			return true, s.deleteRows(ctx, rows)
		}
	}
	jobs, _, err := s.Rows(ctx, "JOBS", "", 200, false)
	if err != nil {
		return true, err
	}
	for _, row := range jobs {
		var j Job
		if err = json.Unmarshal([]byte(row.Data), &j); err != nil {
			return true, err
		}
		if j.Epoch != epoch {
			continue
		}
		for _, pk := range []string{epoch + "|RESULTS|" + j.ID, epoch + "|CHANGES|" + j.ID} {
			results, _, e := s.Rows(ctx, pk, "", 25, false)
			if e != nil {
				return true, e
			}
			if len(results) > 0 {
				return true, s.deleteRows(ctx, results)
			}
		}
		return true, s.deleteRows(ctx, []item{row, {PK: "UPDATES", SK: j.ID}})
	}
	for _, pk := range []string{"UPDATES", "REQUESTS"} {
		rows, _, e := s.Rows(ctx, pk, "", 200, false)
		if e != nil {
			return true, e
		}
		for _, row := range rows {
			var owned struct{ Epoch string }
			if e = json.Unmarshal([]byte(row.Data), &owned); e != nil {
				return true, e
			}
			if owned.Epoch == epoch {
				return true, s.deleteRows(ctx, []item{row})
			}
		}
	}
	c.Retired = c.Retired[1:]
	w, err := s.put("META", "CONTROL", c, c.Revision)
	if err != nil {
		return true, err
	}
	return len(c.Retired) > 0, s.transaction(ctx, w)
}

func (s *Store) deleteRows(ctx context.Context, rows []item) error {
	requests := make([]types.WriteRequest, 0, len(rows))
	for _, row := range rows {
		requests = append(requests, types.WriteRequest{DeleteRequest: &types.DeleteRequest{Key: key(row.PK, row.SK)}})
	}
	out, err := s.DB.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{s.Table: requests}})
	if err != nil {
		return err
	}
	if len(out.UnprocessedItems[s.Table]) > 0 {
		return fmt.Errorf("cleanup throttled; durable retirement will retry")
	}
	return nil
}

func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
