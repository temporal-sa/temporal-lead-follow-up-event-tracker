package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
)

const (
	directoryPageSize = 25
	openPageSize      = 100
)

func (t *Table) Event(ctx context.Context, id string) (tracker.Event, error) {
	item, err := t.getEvent(ctx, id)
	return item.Event, err
}

func (t *Table) Resolve(ctx context.Context, slug string) (tracker.Event, error) {
	output, err := t.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: &t.name, Key: key(linkPK(slug), linkSK), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return tracker.Event{}, err
	}
	if output.Item == nil {
		return tracker.Event{}, tracker.ErrNotStored
	}
	var link linkItem
	if err := unmarshal(output.Item, &link); err != nil {
		return tracker.Event{}, err
	}
	return t.Event(ctx, link.EventID)
}

func (t *Table) Events(ctx context.Context, cursor string) ([]tracker.Event, string, error) {
	return t.queryEvents(ctx, cursor, &dynamodb.QueryInput{
		TableName: &t.name, IndexName: aws.String(IndexDirectory), Limit: aws.Int32(directoryPageSize),
		ScanIndexForward:          aws.Bool(false),
		KeyConditionExpression:    aws.String("gsi1pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(directoryPK)},
	}, directoryPK)
}

func (t *Table) OpenEvents(ctx context.Context, cursor string) ([]tracker.Event, string, error) {
	// An event leaves the index when its workflow completes; this also skips any
	// whose completion time passed before that save.
	return t.queryEvents(ctx, cursor, &dynamodb.QueryInput{
		TableName: &t.name, IndexName: aws.String(IndexOpenEvents), Limit: aws.Int32(openPageSize),
		KeyConditionExpression:    aws.String("gsi2pk = :pk AND gsi2sk > :now"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(openPK), ":now": s(sortTime(time.Now()))},
	}, openPK)
}

func (t *Table) queryEvents(ctx context.Context, cursor string, input *dynamodb.QueryInput, partition string) ([]tracker.Event, string, error) {
	start, err := decodeCursor(cursor, partition)
	if err != nil {
		return nil, "", err
	}
	input.ExclusiveStartKey = start
	output, err := t.client.Query(ctx, input)
	if err != nil {
		return nil, "", err
	}
	events := make([]tracker.Event, 0, len(output.Items))
	for _, values := range output.Items {
		var item eventItem
		if err := unmarshal(values, &item); err != nil {
			return nil, "", err
		}
		events = append(events, item.Event)
	}
	next, err := encodeCursor(output.LastEvaluatedKey)
	return events, next, err
}

func (t *Table) Participants(ctx context.Context, eventID, cursor string, limit int) ([]tracker.Lead, string, error) {
	start, err := decodeCursor(cursor, eventPK(eventID))
	if err != nil {
		return nil, "", err
	}
	if limit < 1 || limit > tracker.PageSize {
		limit = tracker.PageSize
	}
	output, err := t.client.Query(ctx, &dynamodb.QueryInput{
		TableName: &t.name, IndexName: aws.String(IndexParticipants), ConsistentRead: aws.Bool(true),
		Limit: aws.Int32(int32(limit)), ExclusiveStartKey: start,
		KeyConditionExpression:    aws.String("pk = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":pk": s(eventPK(eventID))},
	})
	if err != nil {
		return nil, "", err
	}
	leads := make([]tracker.Lead, 0, len(output.Items))
	for _, values := range output.Items {
		var item participantItem
		if err := unmarshal(values, &item); err != nil {
			return nil, "", err
		}
		leads = append(leads, item.Lead)
	}
	next, err := encodeCursor(output.LastEvaluatedKey)
	return leads, next, err
}

func (t *Table) Participant(ctx context.Context, eventID, email string) (*tracker.Lead, error) {
	output, err := t.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: &t.name, Key: key(eventPK(eventID), participantSKP+email), ConsistentRead: aws.Bool(true)})
	if err != nil || output.Item == nil {
		return nil, err
	}
	var item participantItem
	if err := unmarshal(output.Item, &item); err != nil {
		return nil, err
	}
	return &item.Lead, nil
}

// A cursor is the query's last evaluated key. Every key attribute is a string,
// and the cursor must belong to the partition being read.
func encodeCursor(last map[string]types.AttributeValue) (string, error) {
	if len(last) == 0 {
		return "", nil
	}
	values := make(map[string]string, len(last))
	for name, value := range last {
		text, ok := value.(*types.AttributeValueMemberS)
		if !ok {
			return "", tracker.ErrInvalidCursor
		}
		values[name] = text.Value
	}
	data, err := json.Marshal(values)
	return base64.RawURLEncoding.EncodeToString(data), err
}

func decodeCursor(cursor, partition string) (map[string]types.AttributeValue, error) {
	if cursor == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(data) > 4096 {
		return nil, tracker.ErrInvalidCursor
	}
	var values map[string]string
	if json.Unmarshal(data, &values) != nil {
		return nil, tracker.ErrInvalidCursor
	}
	allowed := map[string]bool{attrPK: true, attrSK: true, attrGSI1PK: true, attrGSI1SK: true, attrGSI2PK: true, attrGSI2SK: true, attrLSI1SK: true}
	matches := false
	start := make(map[string]types.AttributeValue, len(values))
	for name, value := range values {
		if !allowed[name] {
			return nil, tracker.ErrInvalidCursor
		}
		matches = matches || ((name == attrPK || name == attrGSI1PK || name == attrGSI2PK) && value == partition)
		start[name] = s(value)
	}
	if !matches {
		return nil, tracker.ErrInvalidCursor
	}
	return start, nil
}
