package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
)

// sentinelSecret is a deliberately low-entropy, obviously-fake secret
// value, modeled on internal/controller/secretleak_test.go's sentinelToken.
// If this string ever shows up anywhere it shouldn't, that is unambiguously
// a leak, not a coincidence.
const sentinelSecret = "leak-canary-s3-secret-access-key-27" //nolint:gosec // G101: deliberately fake, low-entropy sentinel value used to detect secret leaks, not a real credential

const sentinelAccessKeyID = "AKIALEAKCANARY0000000" //nolint:gosec // G101: deliberately fake, obviously-not-real sentinel access key ID used to detect secret leaks, not a real credential

// TestCredentialsNeverStringify runs a Credentials value containing
// sentinelSecret through every stringification path this package exposes
// and asserts the sentinel never appears, while [REDACTED] always does.
func TestCredentialsNeverStringify(t *testing.T) {
	creds := Credentials{AccessKeyID: sentinelAccessKeyID, SecretAccessKey: Secret(sentinelSecret), SessionToken: Secret("session-" + sentinelSecret)}

	// Positive control: Reveal must still return the real value, otherwise
	// the rest of this test is vacuous.
	if creds.SecretAccessKey.Reveal() != sentinelSecret {
		t.Fatalf("positive control failed: Reveal() = %q, want %q", creds.SecretAccessKey.Reveal(), sentinelSecret)
	}

	outputs := map[string]string{
		"%v on Secret":       fmt.Sprintf("%v", creds.SecretAccessKey),
		"%s on Secret":       fmt.Sprintf("%s", creds.SecretAccessKey), //nolint:staticcheck // S1025: deliberately exercising the %s verb (fmt.Sprintf, not Secret.String) to prove this specific stringification path is also redacted
		"%q on Secret":       fmt.Sprintf("%q", creds.SecretAccessKey),
		"%#v on Secret":      fmt.Sprintf("%#v", creds.SecretAccessKey),
		"%+v on Secret":      fmt.Sprintf("%+v", creds.SecretAccessKey),
		"%v on Credentials":  fmt.Sprintf("%v", creds),
		"%s on Credentials":  fmt.Sprintf("%s", creds), //nolint:staticcheck // S1025: deliberately exercising the %s verb (fmt.Sprintf, not Secret.String) to prove this specific stringification path is also redacted
		"%#v on Credentials": fmt.Sprintf("%#v", creds),
		"%+v on Credentials": fmt.Sprintf("%+v", creds),
	}

	if b, err := json.Marshal(creds.SecretAccessKey); err != nil {
		t.Errorf("json.Marshal(Secret): %v", err)
	} else {
		outputs["json.Marshal bare Secret"] = string(b)
	}
	if b, err := json.Marshal(creds); err != nil {
		t.Errorf("json.Marshal(Credentials): %v", err)
	} else {
		outputs["json.Marshal bare Credentials"] = string(b)
	}

	type wrapper struct {
		Creds Credentials `json:"creds"`
	}
	if b, err := json.Marshal(wrapper{Creds: creds}); err != nil {
		t.Errorf("json.Marshal(wrapper{Credentials}): %v", err)
	} else {
		outputs["json.Marshal nested Credentials"] = string(b)
	}

	var sink strings.Builder
	logger := funcr.NewJSON(func(obj string) { sink.WriteString(obj) }, funcr.Options{})
	logger.Info("msg", "creds", creds, "secret", creds.SecretAccessKey)
	outputs["logr funcr sink"] = sink.String()

	for name, out := range outputs {
		if strings.Contains(out, sentinelSecret) {
			t.Errorf("%s leaked the sentinel secret: %q", name, out)
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("%s did not contain [REDACTED]: %q", name, out)
		}
	}
}

// TestCredentialsValidate checks the required-field validation.
func TestCredentialsValidate(t *testing.T) {
	cases := []struct {
		name    string
		creds   Credentials
		wantErr bool
	}{
		{"valid", Credentials{AccessKeyID: "id", SecretAccessKey: Secret("secret")}, false},
		{"missing access key id", Credentials{SecretAccessKey: Secret("secret")}, true},
		{"missing secret", Credentials{AccessKeyID: "id"}, true},
		{"both missing", Credentials{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.creds.Validate()
			if tc.wantErr && !IsInvalid(err) {
				t.Errorf("Validate() = %v, want ErrInvalid-kinded", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

// TestSecret_IsZero checks the empty-value helper.
func TestSecret_IsZero(t *testing.T) {
	if !Secret("").IsZero() {
		t.Error("empty Secret: IsZero() = false, want true")
	}
	if Secret("x").IsZero() {
		t.Error("non-empty Secret: IsZero() = true, want false")
	}
}

// TestErrorsNeverCarryCredentials constructs an S3 backend with sentinel
// creds against a dead endpoint and against a real HTTP server that always
// answers 403 with the sentinel access key rejected, and asserts every
// Backend method's returned error string contains neither the sentinel
// secret nor the sentinel access key ID.
func TestErrorsNeverCarryCredentials(t *testing.T) {
	assertClean := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			return
		}
		msg := err.Error()
		if strings.Contains(msg, sentinelSecret) {
			t.Errorf("error leaked the sentinel secret: %q", msg)
		}
		if strings.Contains(msg, sentinelAccessKeyID) {
			t.Errorf("error leaked the sentinel access key ID: %q", msg)
		}
	}

	creds := Credentials{AccessKeyID: sentinelAccessKeyID, SecretAccessKey: Secret(sentinelSecret)}

	t.Run("dead endpoint", func(t *testing.T) {
		be, err := NewS3(S3Config{Endpoint: "http://127.0.0.1:1", Bucket: "bucket-a", Retry: RetryPolicy{MaxAttempts: 1}}, creds)
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, statErr := be.Stat(ctx, "some/key")
		assertClean(t, statErr)
		_, _, getErr := be.Get(ctx, "some/key")
		assertClean(t, getErr)
		_, putErr := be.Put(ctx, "some/key", strings.NewReader("data"), PutOptions{})
		assertClean(t, putErr)
		delErr := be.Delete(ctx, "some/key")
		assertClean(t, delErr)
		_, listErr := be.List(ctx, "some/")
		assertClean(t, listErr)
	})

	t.Run("server rejects credentials", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>AccessDenied</Code><Message>Access Denied for ` + sentinelAccessKeyID + `</Message></Error>`))
		}))
		defer srv.Close()

		be, err := NewS3(S3Config{Endpoint: srv.URL, Bucket: "bucket-a", ForcePathStyle: true, Retry: RetryPolicy{MaxAttempts: 1}}, creds)
		if err != nil {
			t.Fatalf("NewS3: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_, statErr := be.Stat(ctx, "some/key")
		assertClean(t, statErr)
		if statErr == nil {
			t.Fatal("expected an error from a server that always answers 403")
		}
	})
}

// TestRevealCallSitesAreMinimal parses this package's own non-test .go
// files with go/ast and asserts ".Reveal()" appears in exactly the
// sanctioned file (s3.go) -- a new call site anywhere else must fail this
// test, since Reveal is the only way to recover a Secret's real value.
func TestRevealCallSitesAreMinimal(t *testing.T) {
	const allowed = "s3.go"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		count := 0
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "Reveal" {
				count++
			}
			return true
		})

		if count > 0 && name != allowed {
			t.Errorf("%s calls .Reveal() %d time(s); only %s is a sanctioned call site", name, count, allowed)
		}
	}
}
