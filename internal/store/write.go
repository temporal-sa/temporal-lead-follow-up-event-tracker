package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/temporal-sa/temporal-lead-follow-up-event-tracker/internal/tracker"
)

// Writes come only from event workflows. Each is conditioned on the writing
// run's epoch, so a run that lost the event can't overwrite the run that took it
// over. Within an epoch, revisions only grow, and an Activity retry repeats the
// same revision: "revision <= :revision" accepts the retry and rejects older writes.

const fenced = "#epoch = :epoch AND #revision <= :revision"

func s(value string) types.AttributeValue { return &types.AttributeValueMemberS{Value: value} }
func n(value int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(value, 10)}
}

func fencedValues(state tracker.EventState) map[string]types.AttributeValue {
	return map[string]types.AttributeValue{":epoch": n(state.Epoch), ":revision": n(state.Event.Revision)}
}

// names aliases every attribute an expression uses, so none can collide with a
// DynamoDB reserved word.
func names(attributes ...string) map[string]string {
	aliases := make(map[string]string, len(attributes))
	for _, attribute := range attributes {
		aliases["#"+attribute] = attribute
	}
	return aliases
}

func (t *Table) Create(ctx context.Context, state tracker.EventState) error {
	event, err := marshal(newEventItem(state.Event, state.Sequence, state.Epoch, time.Now()))
	if err != nil {
		return err
	}
	link, err := marshal(linkItem{PK: linkPK(state.Event.ID), SK: linkSK, EventID: state.Event.ID})
	if err != nil {
		return err
	}
	// A retry finds its own event, recognized by its creation time. Any other
	// event or link with this ID means the ID is taken.
	_, err = t.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{
			TableName: &t.name, Item: event,
			ConditionExpression:       aws.String("attribute_not_exists(pk) OR (#createdAt = :created AND #epoch = :epoch)"),
			ExpressionAttributeNames:  names("createdAt", "epoch"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":created": event["createdAt"], ":epoch": n(state.Epoch)},
		}},
		{Put: &types.Put{
			TableName: &t.name, Item: link,
			ConditionExpression:       aws.String("attribute_not_exists(pk) OR #eventId = :event"),
			ExpressionAttributeNames:  names("eventId"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":event": s(state.Event.ID)},
		}},
	}})
	return transactionError(err, tracker.ErrTaken, tracker.ErrTaken)
}

func (t *Table) Claim(ctx context.Context, id string) (tracker.EventState, error) {
	output, err := t.client.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: &t.name, Key: key(eventPK(id), metaSK),
		UpdateExpression:          aws.String("SET #epoch = #epoch + :one"),
		ConditionExpression:       aws.String("attribute_exists(pk)"),
		ExpressionAttributeNames:  names("epoch"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":one": n(1)},
		ReturnValues:              types.ReturnValueAllNew,
	})
	var failed *types.ConditionalCheckFailedException
	if errors.As(err, &failed) {
		return tracker.EventState{}, tracker.ErrNotStored
	}
	if err != nil {
		return tracker.EventState{}, err
	}
	var item eventItem
	if err := unmarshal(output.Attributes, &item); err != nil {
		return tracker.EventState{}, err
	}
	return tracker.EventState{Event: item.Event, Sequence: item.Sequence, Epoch: item.Epoch}, nil
}

func (t *Table) SaveEvent(ctx context.Context, state tracker.EventState) error {
	item, err := marshal(newEventItem(state.Event, state.Sequence, state.Epoch, time.Now()))
	if err != nil {
		return err
	}
	_, err = t.client.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &t.name, Item: item,
		ConditionExpression: aws.String(fenced), ExpressionAttributeNames: names("epoch", "revision"), ExpressionAttributeValues: fencedValues(state),
	})
	var failed *types.ConditionalCheckFailedException
	if errors.As(err, &failed) {
		return tracker.ErrFenced
	}
	return err
}

func (t *Table) SaveSlug(ctx context.Context, state tracker.EventState, slug string) error {
	link, err := marshal(linkItem{PK: linkPK(slug), SK: linkSK, EventID: state.Event.ID})
	if err != nil {
		return err
	}
	event, err := marshal(newEventItem(state.Event, state.Sequence, state.Epoch, time.Now()))
	if err != nil {
		return err
	}
	_, err = t.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{
			TableName: &t.name, Item: link,
			ConditionExpression:       aws.String("attribute_not_exists(pk) OR #eventId = :event"),
			ExpressionAttributeNames:  names("eventId"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":event": s(state.Event.ID)},
		}},
		{Put: &types.Put{TableName: &t.name, Item: event, ConditionExpression: aws.String(fenced), ExpressionAttributeNames: names("epoch", "revision"), ExpressionAttributeValues: fencedValues(state)}},
	}})
	return transactionError(err, tracker.ErrTaken, tracker.ErrFenced)
}

func (t *Table) Lookup(ctx context.Context, eventID, email, requestID string) (tracker.Existing, error) {
	var existing tracker.Existing
	participant, err := t.Participant(ctx, eventID, email)
	if err != nil {
		return existing, err
	}
	if participant != nil {
		existing.Found, existing.RequestID, existing.FirstSubmittedAt, existing.Digest = true, participant.RequestID, participant.FirstSubmittedAt, participant.Digest()
	}
	output, err := t.client.GetItem(ctx, &dynamodb.GetItemInput{TableName: &t.name, Key: key(eventPK(eventID), receiptSKP+requestID), ConsistentRead: aws.Bool(true)})
	if err != nil {
		return existing, err
	}
	if output.Item != nil {
		var receipt receiptItem
		if err := unmarshal(output.Item, &receipt); err != nil {
			return existing, err
		}
		existing.Receipt = &receipt.Receipt
	}
	return existing, nil
}

func (t *Table) SaveSubmission(ctx context.Context, state tracker.EventState, lead tracker.Lead, receipt tracker.Receipt) error {
	participant, err := marshal(newParticipantItem(state.Event.ID, lead))
	if err != nil {
		return err
	}
	stored, err := marshal(receiptItem{PK: eventPK(state.Event.ID), SK: receiptSKP + receipt.ID, Receipt: receipt})
	if err != nil {
		return err
	}
	values := fencedValues(state)
	values[":count"], values[":sequence"] = n(int64(state.Event.Count)), n(state.Sequence)
	// The event's count and sequence change in place, so a submission doesn't rewrite the form.
	_, err = t.client.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{Update: &types.Update{
			TableName: &t.name, Key: key(eventPK(state.Event.ID), metaSK),
			UpdateExpression:          aws.String("SET #count = :count, #revision = :revision, #sequence = :sequence"),
			ConditionExpression:       aws.String(fenced),
			ExpressionAttributeNames:  names("count", "revision", "sequence", "epoch"),
			ExpressionAttributeValues: values,
		}},
		{Put: &types.Put{
			TableName: &t.name, Item: participant,
			ConditionExpression:       aws.String("attribute_not_exists(pk) OR #sequence <= :sequence"),
			ExpressionAttributeNames:  names("sequence"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":sequence": n(lead.Sequence)},
		}},
		{Put: &types.Put{
			TableName: &t.name, Item: stored,
			ConditionExpression:       aws.String("attribute_not_exists(pk) OR #digest = :digest"),
			ExpressionAttributeNames:  names("digest"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":digest": s(receipt.Digest)},
		}},
	}})
	return transactionError(err, tracker.ErrFenced, tracker.ErrFenced)
}

// transactionError names why DynamoDB canceled a transaction whose first item
// is the one that decides ownership. A failed condition on the first item means
// first; on any later item, rest. A conflict with another transaction is
// returned as is, so the Activity retries it.
func transactionError(err, first, rest error) error {
	var canceled *types.TransactionCanceledException
	if !errors.As(err, &canceled) {
		return err
	}
	for i, reason := range canceled.CancellationReasons {
		if aws.ToString(reason.Code) != "ConditionalCheckFailed" {
			continue
		}
		if i == 0 {
			return first
		}
		return rest
	}
	return err
}
