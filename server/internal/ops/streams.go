package ops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/api"
	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
	"github.com/toclocoinc/dynamoat/server/internal/streams"
)

func init() {
	register(api.StreamsOp+"ListStreams", (*Ops).listStreams)
	register(api.StreamsOp+"DescribeStream", (*Ops).describeStream)
	register(api.StreamsOp+"GetShardIterator", (*Ops).getShardIterator)
	register(api.StreamsOp+"GetRecords", (*Ops).getRecords)
}

// streams returns the StreamStore capability, or the ValidationException every
// streams operation reports when the store has no streams.
func (o *Ops) streams() (storage.StreamStore, error) {
	ss, ok := o.Store.(storage.StreamStore)
	if !ok {
		return nil, ddberr.Validation("dynamoat: this store does not support streams")
	}
	return ss, nil
}

// streamTable loads the table a stream ARN names and checks the stream is still
// enabled with the same label, so a disabled or re-enabled stream is not found.
func (o *Ops) streamTable(ctx context.Context, arn string) (*storage.Table, string, error) {
	name, label, ok := parseStreamARN(arn)
	if !ok {
		return nil, "", ddberr.ResourceNotFound("Requested resource not found")
	}
	t, err := o.Store.GetTable(ctx, name)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, "", ddberr.ResourceNotFound("Requested resource not found")
	}
	if err != nil {
		return nil, "", err
	}
	if t.StreamView == "" || t.StreamLabel != label {
		return nil, "", ddberr.ResourceNotFound("Requested resource not found")
	}
	return t, label, nil
}

// parseStreamARN splits arn:aws:dynamodb:<region>:<account>:table/<name>/stream/<label>.
func parseStreamARN(arn string) (name, label string, ok bool) {
	rest, found := strings.CutPrefix(arn, "arn:aws:dynamodb:")
	if !found {
		return "", "", false
	}
	_, after, found := strings.Cut(rest, ":table/")
	if !found {
		return "", "", false
	}
	name, label, found = strings.Cut(after, "/stream/")
	if !found || name == "" || label == "" {
		return "", "", false
	}
	return name, label, true
}

// --- ListStreams ---

type listStreamsInput struct {
	TableName               string `json:"TableName"`
	Limit                   *int   `json:"Limit"`
	ExclusiveStartStreamArn string `json:"ExclusiveStartStreamArn"`
}

type streamRef struct {
	arn   string
	name  string
	label string
}

func (o *Ops) listStreams(ctx context.Context, body []byte) (any, error) {
	if _, err := o.streams(); err != nil {
		return nil, err
	}
	var req listStreamsInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	limit := 100
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 {
			return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1", limit)
		}
		if limit > 100 {
			return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value less than or equal to 100", limit)
		}
	}

	names, err := o.Store.ListTables(ctx, "", 0)
	if err != nil {
		return nil, err
	}
	refs := make([]streamRef, 0, len(names))
	for _, n := range names {
		if req.TableName != "" && n != req.TableName {
			continue
		}
		t, err := o.Store.GetTable(ctx, n)
		if err != nil {
			return nil, err
		}
		if t.StreamView == "" {
			continue
		}
		refs = append(refs, streamRef{
			arn:   streams.StreamARN(o.region(), t.Name, t.StreamLabel),
			name:  t.Name,
			label: t.StreamLabel,
		})
	}

	start := 0
	if req.ExclusiveStartStreamArn != "" {
		start = len(refs)
		for i, r := range refs {
			if r.arn == req.ExclusiveStartStreamArn {
				start = i + 1
				break
			}
		}
	}

	end := start + limit
	if end > len(refs) {
		end = len(refs)
	}
	out := make([]map[string]any, 0, end-start)
	for _, r := range refs[start:end] {
		out = append(out, map[string]any{
			"StreamArn":   r.arn,
			"TableName":   r.name,
			"StreamLabel": r.label,
		})
	}
	res := map[string]any{"Streams": out}
	if end < len(refs) {
		res["LastEvaluatedStreamArn"] = refs[end-1].arn
	}
	return res, nil
}

// --- DescribeStream ---

type describeStreamInput struct {
	StreamArn string `json:"StreamArn"`
}

func (o *Ops) describeStream(ctx context.Context, body []byte) (any, error) {
	ss, err := o.streams()
	if err != nil {
		return nil, err
	}
	var req describeStreamInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if req.StreamArn == "" {
		return nil, ddberr.Validation("1 validation error detected: Value null at 'streamArn' failed to satisfy constraint: Member must not be null")
	}
	t, label, err := o.streamTable(ctx, req.StreamArn)
	if err != nil {
		return nil, err
	}

	// The shard's starting sequence number is the stream's oldest kept record;
	// a freshly enabled stream has not written one yet, so it starts at 1.
	starting := int64(1)
	if recs, err := ss.ReadStream(ctx, t, 0, 1); err == nil && len(recs) > 0 {
		starting = recs[0].Seq
	}

	return map[string]any{
		"StreamDescription": map[string]any{
			"StreamArn":               req.StreamArn,
			"StreamLabel":             label,
			"StreamStatus":            "ENABLED",
			"StreamViewType":          t.StreamView,
			"CreationRequestDateTime": epochSeconds(streamCreatedAt(t)),
			"TableName":               t.Name,
			"KeySchema":               keySchemaJSON(t.Key),
			"Shards": []map[string]any{
				{
					"ShardId": streams.ShardID(t.ID),
					"SequenceNumberRange": map[string]any{
						"StartingSequenceNumber": fmt.Sprintf("%021d", starting),
					},
				},
			},
		},
	}, nil
}

// streamCreatedAt is when the stream was enabled: the time its label carries,
// falling back to the table's creation time if the label cannot be parsed.
func streamCreatedAt(t *storage.Table) time.Time {
	if at, err := time.Parse("2006-01-02T15:04:05.000", t.StreamLabel); err == nil {
		return at
	}
	return t.CreatedAt
}

// --- GetShardIterator ---

type getShardIteratorInput struct {
	StreamArn         string `json:"StreamArn"`
	ShardId           string `json:"ShardId"`
	ShardIteratorType string `json:"ShardIteratorType"`
	SequenceNumber    string `json:"SequenceNumber"`
}

func (o *Ops) getShardIterator(ctx context.Context, body []byte) (any, error) {
	ss, err := o.streams()
	if err != nil {
		return nil, err
	}
	var req getShardIteratorInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if req.StreamArn == "" {
		return nil, ddberr.Validation("1 validation error detected: Value null at 'streamArn' failed to satisfy constraint: Member must not be null")
	}
	t, label, err := o.streamTable(ctx, req.StreamArn)
	if err != nil {
		return nil, err
	}
	if req.ShardId != streams.ShardID(t.ID) {
		return nil, ddberr.ResourceNotFound("Requested resource not found")
	}

	var after int64
	switch req.ShardIteratorType {
	case "TRIM_HORIZON":
		if req.SequenceNumber != "" {
			return nil, ddberr.Validation("SequenceNumber should not be provided when ShardIteratorType is TRIM_HORIZON or LATEST")
		}
		after = 0
	case "LATEST":
		if req.SequenceNumber != "" {
			return nil, ddberr.Validation("SequenceNumber should not be provided when ShardIteratorType is TRIM_HORIZON or LATEST")
		}
		end, err := ss.StreamEnd(ctx, t)
		if err != nil {
			return nil, err
		}
		after = end
	case "AT_SEQUENCE_NUMBER", "AFTER_SEQUENCE_NUMBER":
		if req.SequenceNumber == "" {
			return nil, ddberr.Validation("1 validation error detected: Value null at 'sequenceNumber' failed to satisfy constraint: Member must not be null")
		}
		seq, err := strconv.ParseInt(req.SequenceNumber, 10, 64)
		if err != nil || seq <= 0 {
			return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'sequenceNumber' failed to satisfy constraint: Member must satisfy regular expression pattern: [0-9]{21}", req.SequenceNumber)
		}
		if req.ShardIteratorType == "AT_SEQUENCE_NUMBER" {
			after = seq - 1
		} else {
			after = seq
		}
	default:
		return nil, ddberr.Validation("1 validation error detected: Value '%s' at 'shardIteratorType' failed to satisfy constraint: Member must satisfy enum value set: [TRIM_HORIZON, LATEST, AT_SEQUENCE_NUMBER, AFTER_SEQUENCE_NUMBER]", req.ShardIteratorType)
	}

	it := streamIterator{
		TableID:   t.ID,
		TableName: t.Name,
		Label:     label,
		After:     after,
		IssuedAt:  o.now().Unix(),
	}
	return map[string]any{"ShardIterator": encodeIterator(it)}, nil
}

// --- GetRecords ---

type getRecordsInput struct {
	ShardIterator string `json:"ShardIterator"`
	Limit         *int   `json:"Limit"`
}

func (o *Ops) getRecords(ctx context.Context, body []byte) (any, error) {
	ss, err := o.streams()
	if err != nil {
		return nil, err
	}
	var req getRecordsInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	limit := 1000
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 {
			return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value greater than or equal to 1", limit)
		}
		if limit > 1000 {
			return nil, ddberr.Validation("1 validation error detected: Value '%d' at 'limit' failed to satisfy constraint: Member must have value less than or equal to 1000", limit)
		}
	}
	if req.ShardIterator == "" {
		return nil, ddberr.Validation("1 validation error detected: Value null at 'shardIterator' failed to satisfy constraint: Member must not be null")
	}
	it, err := decodeIterator(req.ShardIterator)
	if err != nil {
		return nil, err
	}
	if o.now().Unix()-it.IssuedAt > 15*60 {
		return nil, ddberr.ExpiredIterator("The shard iterator has expired and can no longer be used to read the stream.")
	}

	t, err := o.Store.GetTable(ctx, it.TableName)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, ddberr.ResourceNotFound("Requested resource not found")
	}
	if err != nil {
		return nil, err
	}
	if t.StreamView == "" || t.StreamLabel != it.Label {
		return nil, ddberr.ResourceNotFound("Requested resource not found")
	}

	// A position after a real sequence number that has since been trimmed is
	// unreachable; a TRIM_HORIZON position (after 0) always reads from the
	// oldest kept record.
	if it.After > 0 {
		first, err := ss.ReadStream(ctx, t, 0, 1)
		if err != nil {
			return nil, err
		}
		if len(first) == 0 || it.After < first[0].Seq {
			return nil, ddberr.TrimmedDataAccess("The data you are trying to access has been trimmed from the stream.")
		}
	}

	recs, err := ss.ReadStream(ctx, t, it.After, limit)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(recs))
	after := it.After
	for _, r := range recs {
		out = append(out, streams.Record(t, o.region(), r))
		after = r.Seq
	}
	next := streamIterator{
		TableID:   t.ID,
		TableName: t.Name,
		Label:     t.StreamLabel,
		After:     after,
		IssuedAt:  o.now().Unix(),
	}
	return map[string]any{
		"Records":           out,
		"NextShardIterator": encodeIterator(next),
	}, nil
}

// streamIterator is the opaque shard iterator: base64url JSON. It carries the
// table so GetRecords can load the definition and check the stream label still
// matches.
type streamIterator struct {
	TableID   int32  `json:"tableId"`
	TableName string `json:"tableName"`
	Label     string `json:"streamLabel"`
	After     int64  `json:"after"`
	IssuedAt  int64  `json:"issuedAt"`
}

func encodeIterator(it streamIterator) string {
	b, _ := json.Marshal(it)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeIterator(s string) (streamIterator, error) {
	var it streamIterator
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return it, ddberr.Validation("The shard iterator is invalid")
	}
	if err := json.Unmarshal(b, &it); err != nil {
		return it, ddberr.Validation("The shard iterator is invalid")
	}
	return it, nil
}
