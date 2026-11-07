package dispatch

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

// targetJSON is the on-disk form of a Target: lowerCamelCase fields, with
// batchWindow as a Go duration string.
type targetJSON struct {
	Name        *string           `json:"name"`
	Tables      *string           `json:"tables"`
	URL         *string           `json:"url"`
	BatchSize   *int              `json:"batchSize"`
	BatchWindow *string           `json:"batchWindow"`
	Headers     map[string]string `json:"headers"`
	Timeout     *string           `json:"timeout"`
}

// maxTimeout is a Lambda function's longest run.
const maxTimeout = 15 * time.Minute

// ParseTargets parses the DYNAMOAT_STREAM_TARGETS value: a JSON array of
// targets, or "@<path>" to read that array from a file. An empty value means
// no targets.
func ParseTargets(spec string) ([]Target, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	data := []byte(spec)
	if strings.HasPrefix(spec, "@") {
		b, err := os.ReadFile(spec[1:])
		if err != nil {
			return nil, fmt.Errorf("dynamoat: reading stream targets file %q: %w", spec[1:], err)
		}
		data = b
	}
	var raw []targetJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("dynamoat: DYNAMOAT_STREAM_TARGETS must be a JSON array of targets: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	out := make([]Target, 0, len(raw))
	for i, r := range raw {
		t, err := parseTarget(r)
		if err != nil {
			return nil, fmt.Errorf("dynamoat: stream target %d: %w", i, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func parseTarget(r targetJSON) (Target, error) {
	var t Target
	if r.Name == nil || *r.Name == "" {
		return t, fmt.Errorf("missing \"name\"")
	}
	t.Name = *r.Name
	if r.Tables == nil || *r.Tables == "" {
		return t, fmt.Errorf("missing \"tables\"")
	}
	t.Tables = *r.Tables
	if _, err := path.Match(t.Tables, ""); err != nil {
		return t, fmt.Errorf("invalid tables glob %q: %v", t.Tables, err)
	}
	if r.URL == nil || *r.URL == "" {
		return t, fmt.Errorf("missing \"url\"")
	}
	u, err := url.Parse(*r.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return t, fmt.Errorf("invalid url %q", *r.URL)
	}
	t.URL = *r.URL
	t.BatchSize = defaultBatch
	if r.BatchSize != nil {
		if *r.BatchSize < 1 || *r.BatchSize > maxBatch {
			return t, fmt.Errorf("batchSize %d is outside 1..%d", *r.BatchSize, maxBatch)
		}
		t.BatchSize = *r.BatchSize
	}
	if r.BatchWindow != nil {
		d, err := time.ParseDuration(*r.BatchWindow)
		if err != nil {
			return t, fmt.Errorf("invalid batchWindow %q: %v", *r.BatchWindow, err)
		}
		if d < 0 {
			return t, fmt.Errorf("batchWindow %q is negative", *r.BatchWindow)
		}
		t.BatchWindow = d
	}
	if r.Timeout != nil {
		d, err := time.ParseDuration(*r.Timeout)
		if err != nil {
			return t, fmt.Errorf("invalid timeout %q: %v", *r.Timeout, err)
		}
		if d <= 0 || d > maxTimeout {
			return t, fmt.Errorf("timeout %q is outside (0, %s]", *r.Timeout, maxTimeout)
		}
		t.Timeout = d
	}
	t.Headers = r.Headers
	return t, nil
}
