package sigv4

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/toclocoinc/dynamoat/server/internal/ddberr"
)

const (
	testKeyID  = "AKIDDYNAMOATTEST"
	testSecret = "dynamoat/test/secret/key+0123456789abcd"
)

var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

type fixture struct {
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("testdata/sdk-getitem.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// build rebuilds the SDK-signed request, letting mod change one thing.
func build(t *testing.T, mod func(f *fixture)) (*http.Request, []byte) {
	t.Helper()
	f := loadFixture(t)
	if mod != nil {
		mod(&f)
	}
	r := httptest.NewRequest("POST", "/", strings.NewReader(f.Body))
	r.Host = f.Headers["host"]
	for k, v := range f.Headers {
		if k != "host" {
			r.Header.Set(k, v)
		}
	}
	return r, []byte(f.Body)
}

func verifier(region string) *Verifier {
	return &Verifier{
		Keys:   StaticKeys{testKeyID: testSecret},
		Region: region,
		Now:    func() time.Time { return testNow },
	}
}

func TestAuthenticateSDKRequest(t *testing.T) {
	for _, tc := range []struct {
		region string
		ok     bool
	}{{"eu-west-2", true}, {"", true}, {"us-east-1", false}} {
		r, body := build(t, nil)
		err := verifier(tc.region).Authenticate(r, body)
		if tc.ok && err != nil {
			t.Errorf("region %q: %v", tc.region, err)
		}
		if !tc.ok && !isType(err, ddberr.InvalidSignature().Type) {
			t.Errorf("region %q: got %v, want InvalidSignature", tc.region, err)
		}
	}
}

func isType(err error, typ string) bool {
	e, ok := err.(*ddberr.Error)
	return ok && e.Type == typ
}

func TestAuthenticateRejections(t *testing.T) {
	invalid := ddberr.InvalidSignature().Type
	cases := []struct {
		name string
		mod  func(f *fixture)
		v    *Verifier
		typ  string
	}{
		{"changed body byte", func(f *fixture) { f.Body = strings.Replace(f.Body, "c1", "c2", 1) }, nil, invalid},
		{"changed signed header value", func(f *fixture) { f.Headers["x-amz-target"] = "DynamoDB_20120810.PutItem" }, nil, invalid},
		{"changed host", func(f *fixture) { f.Headers["host"] = "other:8000" }, nil, invalid},
		{"wrong secret", nil, &Verifier{Keys: StaticKeys{testKeyID: "nope"}, Now: func() time.Time { return testNow }}, invalid},
		{"unknown key id", nil, &Verifier{Keys: StaticKeys{"other": testSecret}, Now: func() time.Time { return testNow }}, ddberr.UnrecognizedClient().Type},
		{"no authorization", func(f *fixture) { delete(f.Headers, "authorization") }, nil, ddberr.MissingAuthenticationToken().Type},
		{"other algorithm", func(f *fixture) { f.Headers["authorization"] = "Basic abc" }, nil, ddberr.MissingAuthenticationToken().Type},
		{"scope for s3", func(f *fixture) {
			f.Headers["authorization"] = strings.Replace(f.Headers["authorization"], "/dynamodb/", "/s3/", 1)
		}, nil, invalid},
		{"scope date differs", func(f *fixture) {
			f.Headers["authorization"] = strings.Replace(f.Headers["authorization"], "/20261105/", "/20261104/", 1)
		}, nil, invalid},
		{"wrong content sha", func(f *fixture) { f.Headers["x-amz-content-sha256"] = strings.Repeat("0", 64) }, nil, invalid},
		{"missing signed header", func(f *fixture) { delete(f.Headers, "amz-sdk-invocation-id") }, nil, invalid},
		{"malformed authorization", func(f *fixture) { f.Headers["authorization"] = "AWS4-HMAC-SHA256 garbage" }, nil, invalid},
		{"missing date", func(f *fixture) { delete(f.Headers, "x-amz-date") }, nil, invalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, body := build(t, tc.mod)
			v := tc.v
			if v == nil {
				v = verifier("")
			}
			err := v.Authenticate(r, body)
			if !isType(err, tc.typ) {
				t.Fatalf("got %v, want type %s", err, tc.typ)
			}
		})
	}
}

func TestAuthenticateSkew(t *testing.T) {
	r, body := build(t, nil)
	v := verifier("")
	v.Now = func() time.Time { return testNow.Add(6 * time.Minute) }
	err := v.Authenticate(r, body)
	want := "Signature expired: 20261105T120000Z is now earlier than 20261105T120100Z (20261105T120600Z - 5 min.)"
	if e, ok := err.(*ddberr.Error); !ok || e.Message != want || e.Type != ddberr.InvalidSignature().Type || e.Status != 400 {
		t.Errorf("old request: got %v, want %q", err, want)
	}

	v.Now = func() time.Time { return testNow.Add(-6 * time.Minute) }
	err = v.Authenticate(r, body)
	want = "Signature not yet current: 20261105T120000Z is still later than 20261105T115900Z (20261105T115400Z + 5 min.)"
	if e, ok := err.(*ddberr.Error); !ok || e.Message != want || e.Type != ddberr.InvalidSignature().Type || e.Status != 400 {
		t.Errorf("future request: got %v, want %q", err, want)
	}

	v.Now = func() time.Time { return testNow.Add(4 * time.Minute) }
	if err := v.Authenticate(r, body); err != nil {
		t.Errorf("within skew: %v", err)
	}
}

func TestUnsignedHeaderMayChange(t *testing.T) {
	r, body := build(t, func(f *fixture) { f.Headers["user-agent"] = "something-else" })
	r.Header.Set("Accept-Encoding", "gzip")
	if err := verifier("").Authenticate(r, body); err != nil {
		t.Fatal(err)
	}
}

func TestLoadKeysFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "keys")
	content := "# comment\n\nAKID1 secret1\n  \nAKID2\tsecret/2+x\n#AKID3 no\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := LoadKeys(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 2 || k["AKID1"] != "secret1" || k["AKID2"] != "secret/2+x" {
		t.Errorf("got %v", k)
	}
}

func TestLoadKeysDirectory(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "AKID1"), []byte("secret1\n"), 0o600)
	os.WriteFile(filepath.Join(d, "AKID2"), []byte("  secret2  \n"), 0o600)
	os.WriteFile(filepath.Join(d, ".hidden"), []byte("x"), 0o600)
	os.Mkdir(filepath.Join(d, "..data"), 0o700)
	os.Mkdir(filepath.Join(d, "subdir"), 0o700)
	k, err := LoadKeys(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(k) != 2 || k["AKID1"] != "secret1" || k["AKID2"] != "secret2" {
		t.Errorf("got %v", k)
	}
}

func TestLoadKeysErrors(t *testing.T) {
	d := t.TempDir()
	empty := filepath.Join(d, "empty")
	os.WriteFile(empty, []byte("# only a comment\n\n"), 0o600)
	bad := filepath.Join(d, "bad")
	os.WriteFile(bad, []byte("justoneword\n"), 0o600)
	for name, p := range map[string]string{"empty file": empty, "bad line": bad, "empty dir": t.TempDir(), "missing": filepath.Join(d, "nope")} {
		if _, err := LoadKeys(p); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
