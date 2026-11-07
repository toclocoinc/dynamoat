package ops

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/storage"
)

func tagARN(table string) string {
	return "arn:aws:dynamodb:us-east-1:000000000000:table/" + table
}

func TestTagLifecycle(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})
	arn := tagARN("tags")

	rec := post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"env","Value":"prod"},{"Key":"owner","Value":"platform"}]}`)
	wantStatus(t, rec, 200)

	rec = post(t, h, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m["Tags"], []any{
		map[string]any{"Key": "env", "Value": "prod"},
		map[string]any{"Key": "owner", "Value": "platform"},
	}, "ListTagsOfResource tags")
	if _, ok := m["NextToken"]; ok {
		t.Errorf("ListTagsOfResource has NextToken when all tags fit: %v", m)
	}

	rec = post(t, h, "UntagResource", `{"ResourceArn":"`+arn+`","TagKeys":["env"]}`)
	wantStatus(t, rec, 200)

	rec = post(t, h, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	m = wantStatus(t, rec, 200)
	equalJSON(t, m["Tags"], []any{map[string]any{"Key": "owner", "Value": "platform"}}, "after untag")

	// Untagging a missing key is fine.
	rec = post(t, h, "UntagResource", `{"ResourceArn":"`+arn+`","TagKeys":["owner","nope"]}`)
	wantStatus(t, rec, 200)
	rec = post(t, h, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	m = wantStatus(t, rec, 200)
	equalJSON(t, m["Tags"], []any{}, "after untag all")
}

func TestTagResourceReplace(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})
	arn := tagARN("tags")

	if rec := post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"k","Value":"one"}]}`); rec.Code != 200 {
		t.Fatalf("first tag: %d %s", rec.Code, rec.Body.String())
	}
	rec := post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"k","Value":"two"}]}`)
	wantStatus(t, rec, 200)
	rec = post(t, h, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m["Tags"], []any{map[string]any{"Key": "k", "Value": "two"}}, "replaced value")
}

func TestTagInvalidArn(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})

	cases := []struct {
		op   string
		body string
		arn  string
	}{
		{"ListTagsOfResource", `{"ResourceArn":"%s"}`, "arn:aws:dynamodb:us-east-1:000000000000:table/tags/stream/2026-11-06T00:00:00.000"},
		{"TagResource", `{"ResourceArn":"%s","Tags":[{"Key":"k","Value":"v"}]}`, "arn:aws:dynamodb:us-east-1:000000000000:table/tags/index/gsi"},
		{"UntagResource", `{"ResourceArn":"%s","TagKeys":["k"]}`, "arn:aws:dynamodb:us-east-1:000000000000:table/tags/index/gsi"},
	}
	for _, c := range cases {
		arn := c.arn
		rec := post(t, h, c.op, fmt.Sprintf(c.body, arn))
		wantErr(t, rec, typValidation, "Invalid TableArn: Invalid ResourceArn provided as input "+arn)
	}
}

func TestTagArnAnyRegionAccount(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})
	// Any region and account in the ARN resolve the table.
	arn := "arn:aws:dynamodb:eu-west-1:123456789012:table/tags"
	rec := post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"k","Value":"v"}]}`)
	wantStatus(t, rec, 200)
	rec = post(t, h, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	m := wantStatus(t, rec, 200)
	equalJSON(t, m["Tags"], []any{map[string]any{"Key": "k", "Value": "v"}}, "tags via foreign ARN")
}

func TestTagResourceNotFound(t *testing.T) {
	_, h, _ := newOps(t)
	arn := tagARN("nope")
	msg := "Requested resource not found: ResourcArn: " + arn + " not found"

	rec := post(t, h, "ListTagsOfResource", `{"ResourceArn":"`+arn+`"}`)
	wantErr(t, rec, typResourceMissing, msg)
	rec = post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"k","Value":"v"}]}`)
	wantErr(t, rec, typResourceMissing, msg)
	rec = post(t, h, "UntagResource", `{"ResourceArn":"`+arn+`","TagKeys":["k"]}`)
	wantErr(t, rec, typResourceMissing, msg)
}

func TestTagLimits(t *testing.T) {
	_, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})
	arn := tagARN("tags")

	rec := post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"`+strings.Repeat("k", 129)+`","Value":"v"}]}`)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Tag key length must be between 1 and 128 characters")

	rec = post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"","Value":"v"}]}`)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Tag key length must be between 1 and 128 characters")

	rec = post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"k","Value":"`+strings.Repeat("v", 257)+`"}]}`)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Tag value length must be between 0 and 256 characters")

	rec = post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"aws:thing","Value":"v"}]}`)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Tag keys starting with 'aws:' are reserved for use by AWS services")

	// 50 tags is fine, the 51st (a new key) is rejected.
	tags := make([]string, 50)
	for i := range tags {
		tags[i] = `{"Key":"k` + pad2(i) + `","Value":"v"}`
	}
	rec = post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[`+strings.Join(tags, ",")+`]}`)
	wantStatus(t, rec, 200)
	rec = post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[{"Key":"extra","Value":"v"}]}`)
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Number of Tags exceeds the limit of 50")
}

func pad2(i int) string {
	if i < 10 {
		return "0" + string(rune('0'+i))
	}
	return string(rune('0'+i/10)) + string(rune('0'+i%10))
}

func TestCreateTableTagLimits(t *testing.T) {
	h := newTestHandler(t, time.Now())

	base := `{"TableName":"%s","KeySchema":[{"AttributeName":"id","KeyType":"HASH"}],"AttributeDefinitions":[{"AttributeName":"id","AttributeType":"S"}],"BillingMode":"PAY_PER_REQUEST","Tags":%s}`

	var tags []string
	for i := 0; i < 51; i++ {
		tags = append(tags, `{"Key":"k`+pad2(i)+`","Value":"v"}`)
	}
	rec := post(t, h, "CreateTable", fmt.Sprintf(base, "over", "["+strings.Join(tags, ",")+"]"))
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Number of Tags exceeds the limit of 50")

	tags = tags[:50]
	rec = post(t, h, "CreateTable", fmt.Sprintf(base, "fifty", "["+strings.Join(tags, ",")+"]"))
	wantStatus(t, rec, 200)

	rec = post(t, h, "CreateTable", fmt.Sprintf(base, "reserved", `[{"Key":"aws:x","Value":"v"}]`))
	wantErr(t, rec, typValidation, "One or more parameter values were invalid: Tag keys starting with 'aws:' are reserved for use by AWS services")
}

func TestListTagsPagination(t *testing.T) {
	o, h, store := newOps(t)
	makeTable(t, store, "tags", storage.KeySchema{Hash: "id"})
	arn := tagARN("tags")

	var tags []string
	for i := 0; i < 7; i++ {
		tags = append(tags, `{"Key":"tag-`+pad2(i)+`","Value":"v"}`)
	}
	rec := post(t, h, "TagResource", `{"ResourceArn":"`+arn+`","Tags":[`+strings.Join(tags, ",")+`]}`)
	wantStatus(t, rec, 200)

	o.TagPageSize = 2
	var got []string
	token := ""
	for i := 0; i < 10; i++ {
		body := `{"ResourceArn":"` + arn + `"`
		if token != "" {
			body += `,"NextToken":"` + token + `"`
		}
		body += `}`
		rec = post(t, h, "ListTagsOfResource", body)
		m := wantStatus(t, rec, 200)
		for _, tg := range m["Tags"].([]any) {
			got = append(got, tg.(map[string]any)["Key"].(string))
		}
		if nt, ok := m["NextToken"].(string); ok && nt != "" {
			token = nt
		} else {
			break
		}
	}

	want := []string{"tag-00", "tag-01", "tag-02", "tag-03", "tag-04", "tag-05", "tag-06"}
	equalJSON(t, got, want, "paged tag keys in key order")
}
