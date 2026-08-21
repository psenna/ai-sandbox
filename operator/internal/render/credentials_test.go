package render

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr/funcr"
)

// sentinelGitProxyToken/sentinelSnapshotSecretAccessKey/
// sentinelSnapshotAccessKeyID are deliberately low-entropy, obviously-fake
// values, modeled on internal/controller/secretleak_test.go's sentinelToken
// and internal/storage/credentials_test.go's sentinelSecret/
// sentinelAccessKeyID. If any of these ever shows up somewhere it shouldn't,
// that is unambiguously a leak, not a coincidence.
const sentinelGitProxyToken = "leak-canary-render-git-proxy-token-31"                //nolint:gosec // G101: deliberately fake, low-entropy sentinel value used to detect secret leaks, not a real credential
const sentinelSnapshotSecretAccessKey = "leak-canary-render-s3-secret-access-key-31" //nolint:gosec // G101: deliberately fake, low-entropy sentinel value used to detect secret leaks, not a real credential
const sentinelSnapshotAccessKeyID = "AKIALEAKCANARYRENDER00"                         //nolint:gosec // G101: deliberately fake, obviously-not-real sentinel access key ID used to detect secret leaks, not a real credential
const sentinelSnapshotSessionToken = "leak-canary-render-s3-session-token-31"        //nolint:gosec // G101: deliberately fake, low-entropy sentinel value used to detect secret leaks, not a real credential

// TestCredentialsRedaction runs a Credentials value containing every
// sentinel above through every stringification path this package exposes
// (bare Credentials and one embedded inside Inputs), plus a JSON round-trip
// and a logr/funcr round-trip, and asserts NO sentinel ever appears while
// "REDACTED" always does. Mirrors internal/storage/credentials_test.go's
// TestCredentialsNeverStringify.
func TestCredentialsRedaction(t *testing.T) {
	creds := Credentials{
		GitProxyToken:           sentinelGitProxyToken,
		SnapshotAccessKeyID:     sentinelSnapshotAccessKeyID,
		SnapshotSecretAccessKey: sentinelSnapshotSecretAccessKey,
		SnapshotSessionToken:    sentinelSnapshotSessionToken,
	}
	inputs := Inputs{Credentials: creds}

	outputs := map[string]string{
		"%v on Credentials":  fmt.Sprintf("%v", creds),
		"%s on Credentials":  fmt.Sprintf("%s", creds), //nolint:staticcheck // S1025: deliberately exercising the %s verb (fmt.Sprintf, not Credentials.String) to prove this specific stringification path is also redacted
		"%q on Credentials":  fmt.Sprintf("%q", creds),
		"%#v on Credentials": fmt.Sprintf("%#v", creds),
		"%+v on Credentials": fmt.Sprintf("%+v", creds),
		"%v on Inputs":       fmt.Sprintf("%v", inputs),
		"%#v on Inputs":      fmt.Sprintf("%#v", inputs),
		"%+v on Inputs":      fmt.Sprintf("%+v", inputs),
	}

	if b, err := json.Marshal(creds); err != nil {
		t.Errorf("json.Marshal(Credentials): %v", err)
	} else {
		outputs["json.Marshal bare Credentials"] = string(b)
	}
	if b, err := json.Marshal(inputs); err != nil {
		t.Errorf("json.Marshal(Inputs): %v", err)
	} else {
		outputs["json.Marshal Inputs"] = string(b)
	}
	if b, err := creds.MarshalText(); err != nil {
		t.Errorf("Credentials.MarshalText: %v", err)
	} else {
		outputs["MarshalText"] = string(b)
	}

	var sink strings.Builder
	logger := funcr.NewJSON(func(obj string) { sink.WriteString(obj) }, funcr.Options{})
	logger.Info("msg", "creds", creds, "inputs", inputs)
	outputs["logr funcr sink"] = sink.String()

	sentinels := []string{
		sentinelGitProxyToken, sentinelSnapshotAccessKeyID,
		sentinelSnapshotSecretAccessKey, sentinelSnapshotSessionToken,
	}
	for name, out := range outputs {
		for _, s := range sentinels {
			if strings.Contains(out, s) {
				t.Errorf("%s leaked a sentinel credential value: %q", name, out)
			}
		}
		if !strings.Contains(out, "REDACTED") {
			t.Errorf("%s did not contain REDACTED: %q", name, out)
		}
	}
}
