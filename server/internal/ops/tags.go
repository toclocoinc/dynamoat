package ops

import (
	"context"
	"encoding/base64"
	"errors"
	"sort"
	"strings"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func init() {
	register("ListTagsOfResource", (*Ops).listTagsOfResource)
	register("TagResource", (*Ops).tagResource)
	register("UntagResource", (*Ops).untagResource)
}

// Tag limits, as DynamoDB enforces them.
const (
	maxTagsPerResource = 50
	maxTagKeyLen       = 128
	maxTagValueLen     = 256
)

// tagPageSize is the number of tags ListTagsOfResource returns per page.
// Tests lower it (via Ops.TagPageSize) to exercise NextToken paging without
// hitting the 50-tag per-resource limit.
func (o *Ops) tagPageSize() int {
	if o.TagPageSize <= 0 {
		return 50
	}
	return o.TagPageSize
}

type listTagsInput struct {
	ResourceArn string `json:"ResourceArn"`
	NextToken   string `json:"NextToken"`
}

type tagResourceInput struct {
	ResourceArn string `json:"ResourceArn"`
	Tags        []tag  `json:"Tags"`
}

type untagResourceInput struct {
	ResourceArn string   `json:"ResourceArn"`
	TagKeys     []string `json:"TagKeys"`
}

func (o *Ops) listTagsOfResource(ctx context.Context, body []byte) (any, error) {
	var req listTagsInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkResourceArn(req.ResourceArn); err != nil {
		return nil, err
	}
	name, err := tableNameFromArn(req.ResourceArn)
	if err != nil {
		return nil, err
	}

	t, err := o.Store.GetTable(ctx, name)
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, arnNotFound(req.ResourceArn)
	}
	if err != nil {
		return nil, err
	}

	keys := sortedTagKeys(t.Tags)
	start := 0
	if req.NextToken != "" {
		after, err := decodeTagToken(req.NextToken)
		if err != nil {
			return nil, ddberr.Validation("Invalid NextToken")
		}
		for start < len(keys) && keys[start] <= after {
			start++
		}
	}

	end := start + o.tagPageSize()
	if end > len(keys) {
		end = len(keys)
	}
	tags := make([]map[string]string, 0, end-start)
	for _, k := range keys[start:end] {
		tags = append(tags, map[string]string{"Key": k, "Value": t.Tags[k]})
	}
	resp := map[string]any{"Tags": tags}
	if end < len(keys) {
		resp["NextToken"] = encodeTagToken(keys[end-1])
	}
	return resp, nil
}

func (o *Ops) tagResource(ctx context.Context, body []byte) (any, error) {
	var req tagResourceInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkResourceArn(req.ResourceArn); err != nil {
		return nil, err
	}
	name, err := tableNameFromArn(req.ResourceArn)
	if err != nil {
		return nil, err
	}
	if err := validateTags(req.Tags); err != nil {
		return nil, err
	}

	_, err = o.Store.UpdateTable(ctx, name, func(t *storage.Table) error {
		if t.Tags == nil {
			t.Tags = make(map[string]string)
		}
		for _, tg := range req.Tags {
			if _, exists := t.Tags[tg.Key]; !exists && len(t.Tags) >= maxTagsPerResource {
				return ddberr.Validation("One or more parameter values were invalid: Number of Tags exceeds the limit of 50")
			}
			t.Tags[tg.Key] = tg.Value
		}
		return nil
	})
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, arnNotFound(req.ResourceArn)
	}
	if err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

func (o *Ops) untagResource(ctx context.Context, body []byte) (any, error) {
	var req untagResourceInput
	if err := decode(body, &req); err != nil {
		return nil, err
	}
	if err := checkResourceArn(req.ResourceArn); err != nil {
		return nil, err
	}
	name, err := tableNameFromArn(req.ResourceArn)
	if err != nil {
		return nil, err
	}

	_, err = o.Store.UpdateTable(ctx, name, func(t *storage.Table) error {
		if t.Tags == nil {
			return nil
		}
		for _, k := range req.TagKeys {
			delete(t.Tags, k)
		}
		if len(t.Tags) == 0 {
			t.Tags = nil
		}
		return nil
	})
	if errors.Is(err, storage.ErrTableNotFound) {
		return nil, arnNotFound(req.ResourceArn)
	}
	if err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

// checkResourceArn requires the ResourceArn member, as the AWS SDKs validate it.
func checkResourceArn(arn string) error {
	if arn == "" {
		return ddberr.Validation("1 validation error detected: Value null at 'resourceArn' failed to satisfy constraint: Member must not be null")
	}
	return nil
}

// tableNameFromArn returns the table name a table ARN names. A stream ARN, an
// index ARN or any other resource is not a table ARN and is a ValidationException.
func tableNameFromArn(arn string) (string, error) {
	parts := strings.Split(arn, ":")
	if len(parts) != 6 ||
		parts[0] != "arn" || parts[1] != "aws" || parts[2] != "dynamodb" ||
		parts[3] == "" || parts[4] == "" {
		return "", ddberr.Validation("Invalid TableArn: Invalid ResourceArn provided as input %s", arn)
	}
	name, ok := strings.CutPrefix(parts[5], "table/")
	if !ok || name == "" || strings.Contains(name, "/") {
		return "", ddberr.Validation("Invalid TableArn: Invalid ResourceArn provided as input %s", arn)
	}
	return name, nil
}

// arnNotFound is the ResourceNotFoundException for an ARN whose table is gone.
// DynamoDB's text, misspelling included, as best known; unconfirmed until the
// conformance suite records it.
func arnNotFound(arn string) *ddberr.Error {
	return ddberr.ResourceNotFound("Requested resource not found: ResourcArn: " + arn + " not found")
}

// validateTags applies DynamoDB's tag constraints to a request's tag list, for
// both CreateTable and TagResource: at most 50 tags, keys 1-128 chars, values
// 0-256 chars, and no key starting with the reserved "aws:" prefix.
func validateTags(tags []tag) error {
	if len(tags) > maxTagsPerResource {
		return ddberr.Validation("One or more parameter values were invalid: Number of Tags exceeds the limit of 50")
	}
	for _, t := range tags {
		if err := validateTag(t); err != nil {
			return err
		}
	}
	return nil
}

func validateTag(t tag) error {
	switch {
	case len(t.Key) < 1 || len(t.Key) > maxTagKeyLen:
		return ddberr.Validation("One or more parameter values were invalid: Tag key length must be between 1 and 128 characters")
	case len(t.Value) > maxTagValueLen:
		return ddberr.Validation("One or more parameter values were invalid: Tag value length must be between 0 and 256 characters")
	case strings.HasPrefix(t.Key, "aws:"):
		return ddberr.Validation("One or more parameter values were invalid: Tag keys starting with 'aws:' are reserved for use by AWS services")
	}
	return nil
}

func sortedTagKeys(tags map[string]string) []string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The NextToken is opaque to clients; it carries the last tag key returned so a
// later request resumes after it, without exposing the table's key space.
func encodeTagToken(lastKey string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(lastKey))
}

func decodeTagToken(tok string) (string, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
