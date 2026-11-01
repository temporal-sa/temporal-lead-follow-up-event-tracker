// Package store keeps events and participants in DynamoDB, outside Temporal, so
// they survive a namespace reset. Workflows write through Activities; the web
// server and Nexus handlers only read.
//
// One table holds every item:
//
//	EVENT#{id}   META      the event (GSI1: directory; GSI2: open events)
//	EVENT#{id}   P#{email} a participant (LSI1: first-submitted order)
//	EVENT#{id}   R#{req}   a submission receipt, for idempotent retries
//	LINK#{slug}  LINK      which event a slug routes to
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
)

// Key attribute and index names. deploy/create-store.sh creates the same schema
// in AWS; EnsureTable creates it for local development.
const (
	attrPK     = "pk"
	attrSK     = "sk"
	attrGSI1PK = "gsi1pk"
	attrGSI1SK = "gsi1sk"
	attrGSI2PK = "gsi2pk"
	attrGSI2SK = "gsi2sk"
	attrLSI1SK = "lsi1sk"

	IndexDirectory    = "gsi1"
	IndexOpenEvents   = "gsi2"
	IndexParticipants = "lsi1"

	metaSK         = "META"
	linkSK         = "LINK"
	directoryPK    = "EVENTS"
	openPK         = "OPEN"
	participantSKP = "P#"
	receiptSKP     = "R#"
)

// ErrArchiveNewer means the table already holds a later version of an event than
// the one being imported, so the import would lose data.
var ErrArchiveNewer = errors.New("the archive holds a newer version of this event; the backfill must not run after the cut-over")

type Table struct {
	client *dynamodb.Client
	name   string
}

type Config struct {
	Table string
	// Endpoint points at DynamoDB Local. Empty uses AWS with the default credential chain.
	Endpoint string
}

func Open(ctx context.Context, cfg Config) (*Table, error) {
	if cfg.Table == "" {
		return nil, errors.New("EVENT_LEADS_TABLE is required")
	}
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	client := dynamodb.NewFromConfig(awsConfig, func(o *dynamodb.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})
	return &Table{client: client, name: cfg.Table}, nil
}

func (t *Table) Health(ctx context.Context) error {
	_, err := t.client.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &t.name})
	return err
}

type eventItem struct {
	PK     string `json:"pk"`
	SK     string `json:"sk"`
	GSI1PK string `json:"gsi1pk"`
	GSI1SK string `json:"gsi1sk"`
	// GSI2 lists only events that still need a running workflow.
	GSI2PK   string `json:"gsi2pk,omitempty"`
	GSI2SK   string `json:"gsi2sk,omitempty"`
	Sequence int64  `json:"sequence"`
	// Epoch fences writers: each workflow run that takes over an event increments it.
	Epoch int64 `json:"epoch"`
	tracker.Event
}

type participantItem struct {
	PK     string `json:"pk"`
	SK     string `json:"sk"`
	LSI1SK string `json:"lsi1sk"`
	tracker.Lead
}

type receiptItem struct {
	PK string `json:"pk"`
	SK string `json:"sk"`
	tracker.Receipt
}

type linkItem struct {
	PK      string `json:"pk"`
	SK      string `json:"sk"`
	EventID string `json:"eventId"`
}

func eventPK(id string) string  { return "EVENT#" + id }
func linkPK(slug string) string { return "LINK#" + slug }

// sortTime is fixed-width so sort keys order the same as the times they hold.
func sortTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func newEventItem(event tracker.Event, sequence, epoch int64, now time.Time) eventItem {
	event.Status, event.Busy, event.Shards = "", false, nil
	item := eventItem{
		PK: eventPK(event.ID), SK: metaSK,
		GSI1PK: directoryPK, GSI1SK: sortTime(event.CreatedAt) + "#" + event.ID,
		Sequence: sequence, Epoch: epoch, Event: event,
	}
	if event.DeletedAt == nil && event.CompletesAt.After(now) {
		item.GSI2PK, item.GSI2SK = openPK, sortTime(event.CompletesAt)+"#"+event.ID
	}
	return item
}

func newParticipantItem(eventID string, lead tracker.Lead) participantItem {
	return participantItem{
		PK: eventPK(eventID), SK: participantSKP + lead.Email,
		LSI1SK: sortTime(lead.FirstSubmittedAt) + "#" + lead.Email, Lead: lead,
	}
}

func marshal(item any) (map[string]types.AttributeValue, error) {
	return attributevalue.MarshalMapWithOptions(item, func(o *attributevalue.EncoderOptions) { o.TagKey = "json" })
}

func unmarshal(values map[string]types.AttributeValue, item any) error {
	return attributevalue.UnmarshalMapWithOptions(values, item, func(o *attributevalue.DecoderOptions) { o.TagKey = "json" })
}

func key(pk, sk string) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		attrPK: &types.AttributeValueMemberS{Value: pk},
		attrSK: &types.AttributeValueMemberS{Value: sk},
	}
}

func (t *Table) Import(ctx context.Context, snapshot tracker.Snapshot) (tracker.ImportResult, error) {
	var result tracker.ImportResult
	id := snapshot.Event.ID
	// Links are claimed on every pass, so a conflict is reported each time it exists.
	for _, slug := range snapshot.Links {
		owned, err := t.claimLink(ctx, slug, id)
		if err != nil {
			return result, err
		}
		if !owned {
			result.LinkConflicts = append(result.LinkConflicts, slug)
		}
	}
	current, err := t.getEvent(ctx, id)
	if err != nil && !errors.Is(err, tracker.ErrNotArchived) {
		return result, err
	}
	if err == nil {
		// A claimed event is owned by a workflow that writes here directly; the
		// histories the backfill reads are older than what the table holds.
		if current.Epoch > 0 || current.Revision > snapshot.Event.Revision {
			return result, ErrArchiveNewer
		}
		if current.Revision == snapshot.Event.Revision {
			result.Unchanged = true
			return result, nil
		}
	}
	writes := make([]any, 0, len(snapshot.Participants)+len(snapshot.Receipts))
	for _, lead := range snapshot.Participants {
		writes = append(writes, newParticipantItem(id, lead))
	}
	for _, receipt := range snapshot.Receipts {
		writes = append(writes, receiptItem{PK: eventPK(id), SK: receiptSKP + receipt.ID, Receipt: receipt})
	}
	if err := t.putAll(ctx, writes); err != nil {
		return result, err
	}
	// The event goes last: an import interrupted before it is redone in full next time.
	item, err := marshal(newEventItem(snapshot.Event, snapshot.Sequence, 0, time.Now()))
	if err != nil {
		return result, err
	}
	_, err = t.client.PutItem(ctx, &dynamodb.PutItemInput{TableName: &t.name, Item: item})
	return result, err
}

// claimLink records that slug routes to eventID unless another event owns it.
func (t *Table) claimLink(ctx context.Context, slug, eventID string) (bool, error) {
	item, err := marshal(linkItem{PK: linkPK(slug), SK: linkSK, EventID: eventID})
	if err != nil {
		return false, err
	}
	_, err = t.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &t.name, Item: item,
		ConditionExpression:       aws.String("attribute_not_exists(pk) OR eventId = :event"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":event": &types.AttributeValueMemberS{Value: eventID}},
	})
	var failed *types.ConditionalCheckFailedException
	if errors.As(err, &failed) {
		return false, nil
	}
	return err == nil, err
}

// putAll writes items in batches, retrying what DynamoDB leaves unprocessed.
func (t *Table) putAll(ctx context.Context, items []any) error {
	for start := 0; start < len(items); start += 25 {
		requests := make([]types.WriteRequest, 0, 25)
		for _, item := range items[start:min(start+25, len(items))] {
			values, err := marshal(item)
			if err != nil {
				return err
			}
			requests = append(requests, types.WriteRequest{PutRequest: &types.PutRequest{Item: values}})
		}
		delay := 50 * time.Millisecond
		for attempt := 0; len(requests) > 0; attempt++ {
			if attempt == 8 {
				return errors.New("DynamoDB kept throttling a batch write")
			}
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(delay):
				}
				delay *= 2
			}
			output, err := t.client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: map[string][]types.WriteRequest{t.name: requests}})
			if err != nil {
				return err
			}
			requests = output.UnprocessedItems[t.name]
		}
	}
	return nil
}

func (t *Table) getEvent(ctx context.Context, id string) (eventItem, error) {
	output, err := t.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: &t.name, Key: key(eventPK(id), metaSK), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return eventItem{}, err
	}
	if output.Item == nil {
		return eventItem{}, tracker.ErrNotArchived
	}
	var item eventItem
	err = unmarshal(output.Item, &item)
	return item, err
}

func (t *Table) Load(ctx context.Context, id string) (tracker.Snapshot, error) {
	item, err := t.getEvent(ctx, id)
	if err != nil {
		return tracker.Snapshot{}, err
	}
	snapshot := tracker.Snapshot{Event: item.Event, Sequence: item.Sequence}
	paginator := dynamodb.NewQueryPaginator(t.client, &dynamodb.QueryInput{
		TableName: &t.name, ConsistentRead: aws.Bool(true),
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": &types.AttributeValueMemberS{Value: eventPK(id)}},
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return tracker.Snapshot{}, err
		}
		for _, values := range page.Items {
			sk, _ := values[attrSK].(*types.AttributeValueMemberS)
			switch {
			case sk == nil:
				continue
			case strings.HasPrefix(sk.Value, participantSKP):
				var participant participantItem
				if err := unmarshal(values, &participant); err != nil {
					return tracker.Snapshot{}, err
				}
				snapshot.Participants = append(snapshot.Participants, participant.Lead)
			case strings.HasPrefix(sk.Value, receiptSKP):
				var receipt receiptItem
				if err := unmarshal(values, &receipt); err != nil {
					return tracker.Snapshot{}, err
				}
				snapshot.Receipts = append(snapshot.Receipts, receipt.Receipt)
			}
		}
	}
	for _, slug := range append([]string{id}, item.Aliases...) {
		output, err := t.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: &t.name, Key: key(linkPK(slug), linkSK), ConsistentRead: aws.Bool(true)})
		if err != nil {
			return tracker.Snapshot{}, err
		}
		var link linkItem
		if output.Item != nil {
			if err := unmarshal(output.Item, &link); err != nil {
				return tracker.Snapshot{}, err
			}
		}
		if link.EventID == id {
			snapshot.Links = append(snapshot.Links, slug)
		}
	}
	return snapshot, nil
}

// EnsureTable creates the table with its indexes if it doesn't exist. It is for
// DynamoDB Local; in AWS the platform's infrastructure owns the table.
func (t *Table) EnsureTable(ctx context.Context) error {
	attribute := func(name string) types.AttributeDefinition {
		return types.AttributeDefinition{AttributeName: aws.String(name), AttributeType: types.ScalarAttributeTypeS}
	}
	schema := func(hash, sort string) []types.KeySchemaElement {
		return []types.KeySchemaElement{
			{AttributeName: aws.String(hash), KeyType: types.KeyTypeHash},
			{AttributeName: aws.String(sort), KeyType: types.KeyTypeRange},
		}
	}
	all := &types.Projection{ProjectionType: types.ProjectionTypeAll}
	_, err := t.client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   &t.name,
		BillingMode: types.BillingModePayPerRequest,
		AttributeDefinitions: []types.AttributeDefinition{
			attribute(attrPK), attribute(attrSK), attribute(attrGSI1PK), attribute(attrGSI1SK),
			attribute(attrGSI2PK), attribute(attrGSI2SK), attribute(attrLSI1SK),
		},
		KeySchema: schema(attrPK, attrSK),
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{IndexName: aws.String(IndexDirectory), KeySchema: schema(attrGSI1PK, attrGSI1SK), Projection: all},
			{IndexName: aws.String(IndexOpenEvents), KeySchema: schema(attrGSI2PK, attrGSI2SK), Projection: all},
		},
		LocalSecondaryIndexes: []types.LocalSecondaryIndex{
			{IndexName: aws.String(IndexParticipants), KeySchema: schema(attrPK, attrLSI1SK), Projection: all},
		},
	})
	var exists *types.ResourceInUseException
	if errors.As(err, &exists) {
		return nil
	}
	if err != nil {
		return err
	}
	return dynamodb.NewTableExistsWaiter(t.client).Wait(ctx, &dynamodb.DescribeTableInput{TableName: &t.name}, time.Minute)
}

var _ tracker.Archive = (*Table)(nil)
