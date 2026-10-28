package catalogue

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func position(n int64) string { return fmt.Sprintf("%020d", n) }

func (s *Store) rangeRows(ctx context.Context, pk string, after, through int64) ([]item, error) {
	rows := make([]item, 0)
	for after < through {
		out, err := s.DB.Query(ctx, &dynamodb.QueryInput{
			TableName: aws.String(s.Table), ConsistentRead: aws.Bool(true), Limit: aws.Int32(1000),
			KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :first AND :last"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: pk}, ":first": &types.AttributeValueMemberS{Value: position(after + 1)}, ":last": &types.AttributeValueMemberS{Value: position(through)},
			},
		})
		if err != nil {
			return nil, err
		}
		for _, attrs := range out.Items {
			var row item
			if err = attributevalue.UnmarshalMap(attrs, &row); err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
		if len(out.LastEvaluatedKey) == 0 {
			break
		}
		last, ok := out.LastEvaluatedKey["SK"].(*types.AttributeValueMemberS)
		if !ok {
			return nil, fmt.Errorf("missing read projection cursor")
		}
		after, err = strconv.ParseInt(last.Value, 10, 64)
		if err != nil {
			return nil, err
		}
	}
	return rows, nil
}

func (s *Store) allRows(ctx context.Context, pk string) ([]item, error) {
	rows := make([]item, 0)
	cursor := ""
	for {
		page, next, err := s.Rows(ctx, pk, cursor, 1000, false)
		if err != nil {
			return nil, err
		}
		rows = append(rows, page...)
		if next == "" {
			return rows, nil
		}
		cursor = next
	}
}

type catalogueSnapshot struct {
	Epoch      string
	Generation int64
	Revision   int64
	Products   []Product
}

// catalogueCache is a disposable read view. Epoch/revision checks make every
// response recoverable from DynamoDB; no correctness depends on process memory.
type catalogueCache struct {
	mu       sync.Mutex
	epoch    string
	revision int64
	products map[string]Product
}

func (c *catalogueCache) snapshot(ctx context.Context, store *Store) (catalogueSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		control, err := store.Control(ctx)
		if err != nil {
			return catalogueSnapshot{}, err
		}
		if c.products == nil || c.epoch != control.Epoch || c.revision > control.DataRevision {
			rows, e := store.allRows(ctx, control.Epoch+"|PRODUCTS")
			if e != nil {
				return catalogueSnapshot{}, e
			}
			products := make(map[string]Product, len(rows))
			for _, row := range rows {
				var p Product
				if e = json.Unmarshal([]byte(row.Data), &p); e != nil {
					return catalogueSnapshot{}, e
				}
				products[p.ID] = p
			}
			// Replaying deltas after the initial read closes concurrent additions,
			// withdrawals and corrections while that paginated read was in flight.
			c.products = products
			c.epoch = control.Epoch
			c.revision = control.DataRevision
			control, e = store.Control(ctx)
			if e != nil {
				return catalogueSnapshot{}, e
			}
			if c.epoch != control.Epoch {
				continue
			}
		}
		rows, err := store.rangeRows(ctx, c.epoch+"|FEED", c.revision, control.DataRevision)
		if err != nil {
			return catalogueSnapshot{}, err
		}
		if int64(len(rows)) != control.DataRevision-c.revision {
			c.products = nil
			continue
		}
		for index, row := range rows {
			if row.SK != position(c.revision+int64(index)+1) {
				return catalogueSnapshot{}, fmt.Errorf("catalogue delta sequence is incomplete")
			}
			var p Product
			if err = json.Unmarshal([]byte(row.Data), &p); err != nil {
				return catalogueSnapshot{}, err
			}
			c.products[p.ID] = p
		}
		c.revision = control.DataRevision
		products := make([]Product, 0, len(c.products))
		for _, p := range c.products {
			products = append(products, p)
		}
		return catalogueSnapshot{Epoch: c.epoch, Generation: control.Generation, Revision: c.revision, Products: products}, nil
	}
	return catalogueSnapshot{}, ErrConflict
}

type changeCache struct {
	mu    sync.Mutex
	jobID string
	last  int64
	rows  []Result
}

func (c *changeCache) snapshot(ctx context.Context, store *Store, job Job) ([]Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	identity := job.Epoch + "/" + job.ID + "/" + job.CreatedAt
	if c.jobID != identity {
		c.jobID = identity
		c.last = 0
		c.rows = nil
	}
	expected := job.Counts.Added + job.Counts.Changed + job.Counts.Withdrawn
	if int64(job.LastChange) <= c.last && expected <= len(c.rows) {
		return append([]Result{}, c.rows[:expected]...), nil
	}
	rows, err := store.rangeRows(ctx, job.Epoch+"|CHANGES|"+job.ID, c.last, int64(job.LastChange))
	if err != nil {
		return nil, err
	}
	additions := make([]Result, 0, len(rows))
	for _, row := range rows {
		var r Result
		if err = json.Unmarshal([]byte(row.Data), &r); err != nil {
			return nil, err
		}
		additions = append(additions, r)
	}
	if len(c.rows)+len(additions) != expected {
		return nil, fmt.Errorf("change projection is incomplete")
	}
	c.rows = append(c.rows, additions...)
	c.last = int64(job.LastChange)
	return append([]Result{}, c.rows...), nil
}
