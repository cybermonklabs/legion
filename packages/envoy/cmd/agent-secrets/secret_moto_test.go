//go:build moto

// packages/envoy/cmd/agent-secrets/secret_moto_test.go
//
// The built agent-secrets binary against moto's AWS (Secrets Manager, STS and IAM), with no AWS
// account: the AWS SDK's own profile and credential-chain resolution, which the in-process tests
// replace (useAWS), decides the sign-in, and moto's IAM checks decide what it may do. moto ignores
// a policy's conditions, so no IAM policy is proved here (the decision
// "AWS is proved without an AWS account"). packages/envoy/scripts/aws-moto-check.sh starts moto
// and runs this test:
//
//	packages/envoy/scripts/aws-moto-check.sh [BINARY]
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	"github.com/sjawhar/envoy/internal/broker/policy/policytest"
)

// motoKeys is an access key pair moto issued.
type motoKeys struct{ id, secret string }

// motoIAM makes one IAM Query API call against moto at endpoint, signed with keys, and answers
// its body. A call moto refuses fails t.
func motoIAM(t *testing.T, endpoint string, keys motoKeys, params url.Values) []byte {
	t.Helper()
	params.Set("Version", "2010-05-08")
	body := params.Encode()
	req, err := http.NewRequest(http.MethodPost, endpoint+"/", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	sum := sha256.Sum256([]byte(body))
	creds := aws.Credentials{AccessKeyID: keys.id, SecretAccessKey: keys.secret}
	if err := v4.NewSigner().SignHTTP(context.Background(), creds, req, hex.EncodeToString(sum[:]), "iam", "us-east-1", time.Now()); err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("IAM %s: %v", params.Get("Action"), err)
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("IAM %s: HTTP %d: %s", params.Get("Action"), resp.StatusCode, answer)
	}
	return answer
}

// motoUser creates the IAM user name in moto, with policy as its one inline policy when it is not
// empty, and an access key, which it answers.
func motoUser(t *testing.T, endpoint string, root motoKeys, name, policy string) motoKeys {
	t.Helper()
	motoIAM(t, endpoint, root, url.Values{"Action": {"CreateUser"}, "UserName": {name}})
	if policy != "" {
		motoIAM(t, endpoint, root, url.Values{"Action": {"PutUserPolicy"}, "UserName": {name}, "PolicyName": {name}, "PolicyDocument": {policy}})
	}
	var created struct {
		ID     string `xml:"CreateAccessKeyResult>AccessKey>AccessKeyId"`
		Secret string `xml:"CreateAccessKeyResult>AccessKey>SecretAccessKey"`
	}
	if err := xml.Unmarshal(motoIAM(t, endpoint, root, url.Values{"Action": {"CreateAccessKey"}, "UserName": {name}}), &created); err != nil || created.ID == "" || created.Secret == "" {
		t.Fatalf("CreateAccessKey for %s answered no key: %v", name, err)
	}
	return motoKeys{created.ID, created.Secret}
}

// TestTheBuiltCLIAgainstMoto: the binary takes its AWS sign-in from AWS_PROFILE, then
// AWS_DEFAULT_PROFILE, then the SDK's default chain, says which it used on every form, and runs
// profile's IAM user; a user no policy allows is named and refused.
func TestTheBuiltCLIAgainstMoto(t *testing.T) {
	endpoint := os.Getenv("AGENT_SECRETS_MOTO_URL")
	if endpoint == "" {
		t.Fatal("AGENT_SECRETS_MOTO_URL is unset: run packages/envoy/scripts/aws-moto-check.sh, which starts moto and names it")
	}
	binary := os.Getenv("AGENT_SECRETS_BINARY")
	if binary == "" {
		binary = buildAgentSecrets(t)
	}

	root := motoKeys{"AKIAROOTEXAMPLE", "root"}
	writer := motoUser(t, endpoint, root, "writer", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["secretsmanager:*","sts:GetCallerIdentity"],"Resource":"*"}]}`)
	nobody := motoUser(t, endpoint, root, "nobody", "")
	dir := t.TempDir()
	sharedCredentials, sharedConfig := filepath.Join(dir, "credentials"), filepath.Join(dir, "config")
	keys := strings.Join([]string{
		"[writer]",
		"aws_access_key_id = " + writer.id,
		"aws_secret_access_key = " + writer.secret,
		"",
		"[nobody]",
		"aws_access_key_id = " + nobody.id,
		"aws_secret_access_key = " + nobody.secret,
		"",
	}, "\n")
	if err := os.WriteFile(sharedCredentials, []byte(keys), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sharedConfig, []byte("[profile writer]\nregion = us-east-1\n\n[profile nobody]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// From here on moto checks every call against IAM. A form content type answers 500.
	reset, err := http.Post(endpoint+"/moto-api/reset-auth", "text/plain", strings.NewReader("0"))
	if err != nil {
		t.Fatal(err)
	}
	reset.Body.Close()
	if reset.StatusCode != http.StatusOK {
		t.Fatalf("moto's reset-auth answered HTTP %d", reset.StatusCode)
	}

	asWriter := secretsmanager.New(secretsmanager.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(endpoint),
		Credentials: credentials.NewStaticCredentialsProvider(writer.id, writer.secret, ""),
	})
	startSecretBroker(t, servedBy(asWriter))
	home := t.TempDir()
	run := func(t *testing.T, extra []string, stdin string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"secret"}, args...)...)
		cmd.Env = append([]string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + home,
			"AGENT_SECRETS_URL=" + os.Getenv("AGENT_SECRETS_URL"), "AWS_ENDPOINT_URL=" + endpoint,
			"AWS_SHARED_CREDENTIALS_FILE=" + sharedCredentials, "AWS_CONFIG_FILE=" + sharedConfig,
		}, extra...)
		cmd.Stdin = strings.NewReader(stdin)
		var stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = io.Discard, &stderr
		err := cmd.Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("run %v: %v", args, err)
		}
		return stderr.String(), code
	}
	const writerARN = "arn:aws:iam::111122223333:user/writer"

	t.Run("1 AWS_PROFILE=writer on every form", func(t *testing.T) {
		using := "using the AWS profile writer (AWS_PROFILE): " + writerARN
		for _, form := range []struct {
			stdin string
			args  []string
		}{
			{"first-value", []string{"create", "MOTO_TEST", "--owner", "shared", "--tier", "agent"}},
			{"", []string{"show", "MOTO_TEST"}},
			{"", []string{"list"}},
			{"second-value", []string{"set", "MOTO_TEST"}},
			{"", []string{"retag", "MOTO_TEST", "--owner", "ada@example.com"}},
			{"", []string{"delete", "MOTO_TEST"}},
			{"", []string{"restore", "MOTO_TEST"}},
		} {
			stderr, code := run(t, []string{"AWS_PROFILE=writer"}, form.stdin, form.args...)
			if code != 0 || !strings.Contains(stderr, "agent-secrets secret "+form.args[0]+": "+using+"\n") {
				t.Fatalf("%v: exit %d, stderr %q; want 0 and %q", form.args, code, stderr, using)
			}
		}
		value, err := asWriter.GetSecretValue(context.Background(), &secretsmanager.GetSecretValueInput{SecretId: aws.String(policytest.ID("MOTO_TEST"))})
		if err != nil {
			t.Fatalf("read moto's %s: %v", policytest.ID("MOTO_TEST"), err)
		}
		if aws.ToString(value.SecretString) != "second-value" {
			t.Fatalf("moto holds a value of %d bytes for %s, want set's %d", len(aws.ToString(value.SecretString)), policytest.ID("MOTO_TEST"), len("second-value"))
		}
	})
	t.Run("2 AWS_PROFILE=writer", func(t *testing.T) {
		stderr, code := run(t, []string{"AWS_PROFILE=writer"}, "", "show", "MOTO_TEST")
		if want := "using the AWS profile writer (AWS_PROFILE): " + writerARN; code != 0 || !strings.Contains(stderr, want) {
			t.Fatalf("exit %d, stderr %q; want 0 and %q", code, stderr, want)
		}
	})
	t.Run("3 the default chain", func(t *testing.T) {
		stderr, code := run(t, []string{"AWS_ACCESS_KEY_ID=" + writer.id, "AWS_SECRET_ACCESS_KEY=" + writer.secret, "AWS_REGION=us-east-1"}, "", "show", "MOTO_TEST")
		if want := "using the AWS SDK's default credential chain: " + writerARN; code != 0 || !strings.Contains(stderr, want) {
			t.Fatalf("exit %d, stderr %q; want 0 and %q", code, stderr, want)
		}
	})
	t.Run("4 AWS_PROFILE=nobody is refused", func(t *testing.T) {
		stderr, code := run(t, []string{"AWS_PROFILE=nobody"}, "", "list")
		if want := "using the AWS profile nobody (AWS_PROFILE): arn:aws:iam::111122223333:user/nobody"; code != 1 || !strings.Contains(stderr, want) {
			t.Fatalf("exit %d, stderr %q; want 1 and %q", code, stderr, want)
		}
	})
}
