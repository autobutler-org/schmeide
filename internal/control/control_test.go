package control

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const testSecret = "it's a secret to everybody"

func sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func payload(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

var testRunnerTokens = map[string]string{"octocat": "octocat-token", "hubot": "hubot-token"}

func startQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := StartQueue(t.Context(), QueueOptions{DataDir: t.TempDir(), RunnerTokens: testRunnerTokens})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	return q
}

// jobs returns every job in the stream.
func jobs(t *testing.T, q *Queue) []Job {
	t.Helper()
	info, err := q.stream.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []Job
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && info.State.Msgs > 0; seq++ {
		msg, err := q.stream.GetMsg(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		var job Job
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			t.Fatal(err)
		}
		if msg.Subject != job.Subject() {
			t.Errorf("job stored on %q, want %q", msg.Subject, job.Subject())
		}
		got = append(got, job)
	}
	return got
}

type delivery struct {
	event     string // defaults to "issues"
	body      []byte
	signature string
	id        string // X-Github-Delivery; empty means the header is omitted
}

// deliver posts one webhook to the handler and returns the status code.
func deliver(t *testing.T, h http.Handler, d delivery) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(d.body))
	if d.event == "" {
		d.event = "issues"
	}
	req.Header.Set("X-Github-Event", d.event)
	if d.signature != "" {
		req.Header.Set("X-Hub-Signature-256", d.signature)
	}
	if d.id != "" {
		req.Header.Set("X-Github-Delivery", d.id)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestWebhookIntake(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json")
	want := Job{
		Repo: "autobutler-org/quark", RepoID: 901234567, Issue: 42, Steward: "octocat", InstallationID: 61234567,
		Title: "add a retry to the uploader", Body: "The uploader gives up on the first 503.",
	}
	// The same issue after it was reassigned: still the same job.
	reassigned := bytes.ReplaceAll(ready, []byte(`"octocat"`), []byte(`"hubot"`))
	// A ready issue its author edited: the job keeps the text it was labeled with.
	edited := bytes.Replace(bytes.Replace(ready, []byte(`"action": "labeled"`), []byte(`"action": "edited"`), 1),
		[]byte("The uploader gives up on the first 503."), []byte("Something else entirely."), 1)
	reopened := bytes.Replace(ready, []byte(`"action": "labeled"`), []byte(`"action": "reopened"`), 1)
	unassigned := payload(t, "issues_labeled_unassigned.json")
	unlabeled := payload(t, "issues_assigned_unlabeled.json")
	closed := bytes.Replace(ready, []byte(`"state": "open"`), []byte(`"state": "closed"`), 1)

	tests := []struct {
		name       string
		deliveries []delivery
		wantCodes  []int
		wantJobs   []Job
	}{
		{
			name:       "assigned and labeled creates one job",
			deliveries: []delivery{{body: ready, signature: sign(ready)}},
			wantCodes:  []int{http.StatusAccepted},
			wantJobs:   []Job{want},
		},
		{
			name:       "duplicate delivery creates one job",
			deliveries: []delivery{{body: ready, signature: sign(ready)}, {body: ready, signature: sign(ready)}},
			wantCodes:  []int{http.StatusAccepted, http.StatusOK},
			wantJobs:   []Job{want},
		},
		{
			name:       "reassigned issue keeps its one job",
			deliveries: []delivery{{body: ready, signature: sign(ready)}, {body: reassigned, signature: sign(reassigned)}},
			wantCodes:  []int{http.StatusAccepted, http.StatusOK},
			wantJobs:   []Job{want},
		},
		{
			name:       "edit after the job exists does not change it",
			deliveries: []delivery{{body: ready, signature: sign(ready)}, {body: edited, signature: sign(edited)}},
			wantCodes:  []int{http.StatusAccepted, http.StatusOK},
			wantJobs:   []Job{want},
		},
		{
			name:       "edit of a ready issue creates no job",
			deliveries: []delivery{{body: edited, signature: sign(edited)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "reopening a ready issue creates no job",
			deliveries: []delivery{{body: reopened, signature: sign(reopened)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "labeled but unassigned creates no job",
			deliveries: []delivery{{body: unassigned, signature: sign(unassigned)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "assigned but unlabeled creates no job",
			deliveries: []delivery{{body: unlabeled, signature: sign(unlabeled)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "closed issue creates no job",
			deliveries: []delivery{{body: closed, signature: sign(closed)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "other events are ignored",
			deliveries: []delivery{{event: "issue_comment", body: ready, signature: sign(ready)}},
			wantCodes:  []int{http.StatusOK},
		},
		{
			name:       "missing signature is rejected",
			deliveries: []delivery{{body: ready}},
			wantCodes:  []int{http.StatusUnauthorized},
		},
		{
			name: "bad signature is rejected",
			deliveries: []delivery{
				{body: ready, signature: sign(unlabeled)},
				{body: ready, signature: "sha256=nothex"},
				{body: ready, signature: strings.TrimPrefix(sign(ready), "sha256=")},
			},
			wantCodes: []int{http.StatusUnauthorized, http.StatusUnauthorized, http.StatusUnauthorized},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := startQueue(t)
			allow := Allowlist{Orgs: []string{"autobutler-org"}}
			h := Handler(Intake{Secret: []byte(testSecret), ReadyLabel: DefaultReadyLabel, Allow: allow, Publish: q.Publish}, slog.New(slog.DiscardHandler), WebhookPolicy{})
			for i, d := range tt.deliveries {
				if got := deliver(t, h, d); got != tt.wantCodes[i] {
					t.Errorf("delivery %d: status = %d, want %d", i, got, tt.wantCodes[i])
				}
			}
			got := jobs(t, q)
			if len(got) != len(tt.wantJobs) {
				t.Fatalf("jobs = %+v, want %+v", got, tt.wantJobs)
			}
			for i := range got {
				if got[i] != tt.wantJobs[i] {
					t.Errorf("job %d = %+v, want %+v", i, got[i], tt.wantJobs[i])
				}
			}
		})
	}
}

func TestWebhookAllowlist(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json") // autobutler-org/quark, installation 61234567
	unlabeled := payload(t, "issues_assigned_unlabeled.json")
	for name, tt := range map[string]struct {
		orgs, installations string
		body                []byte
		wantCode, wantJobs  int
	}{
		"allowed org":                        {orgs: "exokomodo, Autobutler-Org", body: ready, wantCode: http.StatusAccepted, wantJobs: 1},
		"allowed installation":               {installations: "1, 61234567", body: ready, wantCode: http.StatusAccepted, wantJobs: 1},
		"either list is enough":              {orgs: "exokomodo", installations: "61234567", body: ready, wantCode: http.StatusAccepted, wantJobs: 1},
		"other org is rejected":              {orgs: "exokomodo", body: ready, wantCode: http.StatusForbidden},
		"other installation is rejected":     {installations: "1", body: ready, wantCode: http.StatusForbidden},
		"org prefix is not a match":          {orgs: "autobutler", body: ready, wantCode: http.StatusForbidden},
		"no job from another org is ignored": {orgs: "exokomodo", body: unlabeled, wantCode: http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			allow, err := ParseAllowlist(tt.orgs, tt.installations)
			if err != nil {
				t.Fatal(err)
			}
			q := startQueue(t)
			h := Handler(Intake{Secret: []byte(testSecret), ReadyLabel: DefaultReadyLabel, Allow: allow, Publish: q.Publish}, slog.New(slog.DiscardHandler), WebhookPolicy{})
			if got := deliver(t, h, delivery{body: tt.body, signature: sign(tt.body)}); got != tt.wantCode {
				t.Errorf("status = %d, want %d", got, tt.wantCode)
			}
			if got := jobs(t, q); len(got) != tt.wantJobs {
				t.Errorf("jobs = %+v, want %d", got, tt.wantJobs)
			}
		})
	}
}

func TestEmptyAllowlistAllowsNoOne(t *testing.T) {
	if _, err := ParseAllowlist(" ", ""); err == nil || !strings.Contains(err.Error(), "--allowed-orgs") {
		t.Errorf("err = %v, want an empty allowlist refused with a pointer to the flags", err)
	}
	ready := payload(t, "issues_assigned_labeled.json")
	q := startQueue(t)
	h := Handler(Intake{Secret: []byte(testSecret), ReadyLabel: DefaultReadyLabel, Publish: q.Publish}, slog.New(slog.DiscardHandler), WebhookPolicy{})
	if got := deliver(t, h, delivery{body: ready, signature: sign(ready)}); got != http.StatusForbidden {
		t.Errorf("status = %d, want %d from an Intake with no allowlist", got, http.StatusForbidden)
	}
	if got := jobs(t, q); len(got) != 0 {
		t.Errorf("jobs = %+v, want none", got)
	}
}

// webhookHandler builds the intake handler. published counts Publish calls
// when it is non-nil.
func webhookHandler(q *Queue, policy WebhookPolicy, published *int) http.Handler {
	return Handler(Intake{
		Secret:     []byte(testSecret),
		ReadyLabel: DefaultReadyLabel,
		Allow:      Allowlist{Orgs: []string{"autobutler-org"}},
		Publish: func(ctx context.Context, job Job) (bool, error) {
			if published != nil {
				*published++
			}
			return q.Publish(ctx, job)
		},
	}, slog.New(slog.DiscardHandler), policy)
}

func TestOversizedWebhookPublishesNothing(t *testing.T) {
	body := make([]byte, maxWebhookBytes+1)
	q := startQueue(t)
	var published int
	h := webhookHandler(q, WebhookPolicy{}, &published)
	if got := deliver(t, h, delivery{body: body, signature: sign(body)}); got != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", got, http.StatusBadRequest)
	}
	if published != 0 {
		t.Errorf("Publish calls = %d, want 0", published)
	}
	if got := jobs(t, q); len(got) != 0 {
		t.Errorf("jobs = %+v, want none", got)
	}
}

func TestWebhookRateLimitPublishesNoJob(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json")
	again := bytes.Replace(ready, []byte(`"number": 42`), []byte(`"number": 43`), 1)
	now := time.Now()
	q := startQueue(t)
	var published int
	h := webhookHandler(q, WebhookPolicy{
		RequestsPerMinute: 1,
		DeliveryTTL:       time.Minute,
		now:               func() time.Time { return now },
	}, &published)

	first := delivery{body: ready, signature: sign(ready), id: "rate-1"}
	if got := deliver(t, h, first); got != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d", got, http.StatusAccepted)
	}
	if published != 1 {
		t.Fatalf("Publish calls = %d, want 1", published)
	}
	second := delivery{body: again, signature: sign(again), id: "rate-2"}
	if got := deliver(t, h, second); got != http.StatusTooManyRequests {
		t.Fatalf("limited status = %d, want %d", got, http.StatusTooManyRequests)
	}
	if published != 1 {
		t.Errorf("Publish calls = %d, want 1; the rejected request must publish no job", published)
	}
	if got := jobs(t, q); len(got) != 1 || got[0].Issue != 42 {
		t.Errorf("jobs = %+v, want only issue 42", got)
	}

	now = now.Add(time.Minute)
	if got := deliver(t, h, second); got != http.StatusAccepted {
		t.Fatalf("status after the window = %d, want %d", got, http.StatusAccepted)
	}
	if published != 2 {
		t.Errorf("Publish calls = %d, want 2 after the window resets", published)
	}
	if got := jobs(t, q); len(got) != 2 {
		t.Errorf("jobs = %+v, want both issues once the window resets", got)
	}
}

func TestRepeatedDeliveryIsAcknowledgedWithoutPublishing(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json")
	now := time.Now()
	q := startQueue(t)
	var published int
	h := webhookHandler(q, WebhookPolicy{
		RequestsPerMinute: 100,
		DeliveryTTL:       10 * time.Minute,
		now:               func() time.Time { return now },
	}, &published)

	d := delivery{body: ready, signature: sign(ready), id: "deliv-1"}
	if got := deliver(t, h, d); got != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d", got, http.StatusAccepted)
	}
	if published != 1 {
		t.Fatalf("Publish calls = %d, want 1", published)
	}
	if got := deliver(t, h, d); got/100 != 2 {
		t.Fatalf("replay status = %d, want a 2xx so GitHub does not retry", got)
	}
	if published != 1 {
		t.Errorf("Publish calls = %d, want 1; a repeated delivery id must not publish again", published)
	}
	if got := jobs(t, q); len(got) != 1 {
		t.Errorf("jobs = %+v, want one", got)
	}

	now = now.Add(10*time.Minute + time.Second)
	if got := deliver(t, h, d); got != http.StatusOK {
		t.Fatalf("status after the ttl = %d, want %d from the idempotent job, not a replay", got, http.StatusOK)
	}
	if published != 2 {
		t.Errorf("Publish calls = %d, want 2 once the delivery id expires", published)
	}
	if got := jobs(t, q); len(got) != 1 {
		t.Errorf("jobs = %+v, want the one idempotent job", got)
	}
}

func TestDifferentDeliveryIDKeepsIdempotentJob(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json")
	q := startQueue(t)
	var published int
	h := webhookHandler(q, WebhookPolicy{RequestsPerMinute: 100, DeliveryTTL: 10 * time.Minute}, &published)
	first := delivery{body: ready, signature: sign(ready), id: "deliv-a"}
	second := delivery{body: ready, signature: sign(ready), id: "deliv-b"}
	if got := deliver(t, h, first); got != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d", got, http.StatusAccepted)
	}
	if got := deliver(t, h, second); got != http.StatusOK {
		t.Fatalf("second status = %d, want %d", got, http.StatusOK)
	}
	if published != 2 {
		t.Errorf("Publish calls = %d, want 2; a new delivery id still reaches Publish", published)
	}
	if got := jobs(t, q); len(got) != 1 {
		t.Errorf("jobs = %+v, want one job", got)
	}
}

func TestWebhookReadTimeoutCutsOffASlowClient(t *testing.T) {
	var published int
	h := Handler(Intake{
		Secret:     []byte(testSecret),
		ReadyLabel: DefaultReadyLabel,
		Allow:      Allowlist{Orgs: []string{"autobutler-org"}},
		Publish: func(context.Context, Job) (bool, error) {
			published++
			return true, nil
		},
	}, slog.New(slog.DiscardHandler), WebhookPolicy{RequestsPerMinute: 10, DeliveryTTL: time.Minute})
	srv := newWebhookServer(h)
	if srv.ReadTimeout <= 0 || srv.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadTimeout is unset; a slow client can hold the connection open")
	}
	srv.ReadHeaderTimeout = time.Second
	srv.ReadTimeout = 200 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// Headers and one body byte, then stall. The rest never arrives.
	stall := "POST /webhook HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 64\r\nX-Github-Event: issues\r\n\r\nx"
	if _, err := conn.Write([]byte(stall)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if !strings.Contains(string(buf[:n]), "400") {
		t.Errorf("response = %q, want 400 once ReadTimeout cuts off the body", buf[:n])
	}
	if published != 0 {
		t.Errorf("Publish calls = %d, want 0", published)
	}
}

func TestRejectedDeliveryIDIsNotRemembered(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json")
	q := startQueue(t)
	var published int
	h := webhookHandler(q, WebhookPolicy{RequestsPerMinute: 100, DeliveryTTL: 10 * time.Minute}, &published)
	bad := delivery{body: ready, signature: "sha256=abcd", id: "deliv-bad"}
	if got := deliver(t, h, bad); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", got, http.StatusUnauthorized)
	}
	good := delivery{body: ready, signature: sign(ready), id: "deliv-bad"}
	if got := deliver(t, h, good); got != http.StatusAccepted {
		t.Fatalf("status = %d, want %d; a rejected id must not block the real delivery", got, http.StatusAccepted)
	}
	if published != 1 {
		t.Errorf("Publish calls = %d, want 1", published)
	}
}

func TestRunLogsPlainHTTPAndShutsDown(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "app.pem")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	natsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	natsAddr := natsLn.Addr().String()
	if err := natsLn.Close(); err != nil {
		t.Fatal(err)
	}

	var logs lockedBuffer
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			HTTPAddr:                "127.0.0.1:0",
			NATSAddr:                natsAddr,
			RunnerTokens:            "octocat:t",
			DataDir:                 filepath.Join(dir, "data"),
			WebhookSecret:           testSecret,
			ReadyLabel:              DefaultReadyLabel,
			AllowedOrgs:             "autobutler-org",
			GitHubAppID:             "12345",
			GitHubAppPrivateKeyFile: keyPath,
			GitHubAPIURL:            DefaultGitHubAPIURL,
			WebhookRate:             "1000",
			DeliveryTTL:             "10m",
		}, slog.New(slog.NewTextHandler(&logs, nil)))
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		text := logs.String()
		if strings.Contains(text, "plain HTTP") && strings.Contains(text, "TLS-terminating proxy") && strings.Contains(text, "http://") {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Run returned before the startup log: %v; log=%s", err, logs.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup log = %q, want plain HTTP behind a TLS-terminating proxy", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want a clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// lockedBuffer is a bytes.Buffer safe for one writer and one reader.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func TestParseRunnerTokens(t *testing.T) {
	got, err := ParseRunnerTokens(" octocat:a:b , hubot:c,")
	if err != nil || len(got) != 2 || got["octocat"] != "a:b" || got["hubot"] != "c" {
		t.Errorf("ParseRunnerTokens = %v, %v, want octocat=a:b and hubot=c", got, err)
	}
	for _, bad := range []string{"", " , ", "octocat", "octocat:", ":t", "octo.cat:t", "octocat:a,octocat:b", PoolName + ":t"} {
		if _, err := ParseRunnerTokens(bad); err == nil {
			t.Errorf("ParseRunnerTokens(%q) succeeded, want an error", bad)
		} else if strings.Contains(err.Error(), "a,") || strings.Contains(err.Error(), ":b") {
			t.Errorf("ParseRunnerTokens(%q) error %q shows a token", bad, err)
		}
	}
}

func TestReadyLabelIsConfigurable(t *testing.T) {
	body := payload(t, "issues_assigned_labeled.json")
	job, err := JobFromIssuesEvent(body, "enhancement")
	if err != nil || job == nil {
		t.Fatalf("job = %v, err = %v, want a job for the configured label", job, err)
	}
	if job, err = JobFromIssuesEvent(body, "other:ready"); err != nil || job != nil {
		t.Fatalf("job = %v, err = %v, want no job without the configured label", job, err)
	}
}

// fakeGitHub is a GitHub API that issues one installation token after checking
// the App JWT against the App's public key.
func fakeGitHub(t *testing.T, key *rsa.PublicKey, expires time.Time) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/61234567/access_tokens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
		if len(parts) != 3 {
			t.Fatalf("Authorization = %q, want a bearer JWT", r.Header.Get("Authorization"))
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
			t.Errorf("app JWT signature: %v", err)
		}
		var claims struct {
			Iss      string `json:"iss"`
			Iat, Exp int64
		}
		claimBytes, _ := base64.RawURLEncoding.DecodeString(parts[1])
		if err := json.Unmarshal(claimBytes, &claims); err != nil {
			t.Errorf("app JWT claims: %v", err)
		}
		if now := time.Now().Unix(); claims.Iss != "12345" || claims.Iat > now || claims.Exp <= now || claims.Exp > now+600 {
			t.Errorf("app JWT claims = %+v, want issuer 12345 and a lifetime within GitHub's 10 minute cap", claims)
		}
		var body struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		reqBody, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(reqBody, &body); err != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 901234567 {
			t.Errorf("token request body = %s, want it limited to the job's repository", reqBody)
		}
		wantPerms := map[string]string{
			"contents":      "write",
			"pull_requests": "write",
			"issues":        "write",
			"checks":        "read",
		}
		if len(body.Permissions) != len(wantPerms) {
			t.Errorf("permissions = %v, want %v", body.Permissions, wantPerms)
		} else {
			for name, level := range wantPerms {
				if body.Permissions[name] != level {
					t.Errorf("permissions[%s] = %q, want %q", name, body.Permissions[name], level)
				}
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Token{Token: "ghs_fake", ExpiresAt: expires})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTokenPerJob(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	gh := fakeGitHub(t, &key.PublicKey, expires)
	minter := TokenMinter{AppID: "12345", Key: parsed, APIURL: gh.URL}

	q := startQueue(t)
	job := Job{Repo: "autobutler-org/quark", RepoID: 901234567, Issue: 42, Steward: "octocat", InstallationID: 61234567}
	hubots := Job{Repo: "autobutler-org/quark", RepoID: 901234567, Issue: 43, Steward: "hubot", InstallationID: 61234567}
	if created, err := q.Publish(t.Context(), hubots); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	if created, err := q.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	if err := q.ServeTokens(minter.Mint, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	request := func(steward, subject string) TokenReply {
		t.Helper()
		msg, err := q.nc.Request(TokenSubjectPrefix+steward, []byte(subject), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var reply TokenReply
		if err := json.Unmarshal(msg.Data, &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if got := request("octocat", job.Subject()); got.Error != "" || got.Token.Token != "ghs_fake" || !got.ExpiresAt.Equal(expires) {
		t.Errorf("token reply = %+v, want ghs_fake expiring at %s", got, expires)
	}
	for _, tt := range []struct{ steward, subject string }{
		{"octocat", "schmeide.jobs.octocat.901234567.43"},
		{"octocat", "schmeide.jobs.>"},
		{"octocat", "elsewhere"},
		{"octocat", hubots.Subject()},
		{"hubot", job.Subject()},
	} {
		if got := request(tt.steward, tt.subject); got.Error == "" || got.Token.Token != "" {
			t.Errorf("token reply to %s for %q = %+v, want an error and no token", tt.steward, tt.subject, got)
		}
	}
}

func TestMintReportsGitHubRefusal(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Integration not found"}`, http.StatusNotFound)
	}))
	defer gh.Close()
	_, err = TokenMinter{AppID: "1", Key: key, APIURL: gh.URL}.Mint(context.Background(), Job{InstallationID: 7})
	if err == nil || !strings.Contains(err.Error(), "Integration not found") {
		t.Errorf("err = %v, want GitHub's message", err)
	}
}

// connectAs connects to the queue's NATS as a steward, the way a runner does,
// and returns the connection with the permission errors NATS reports on it.
func connectAs(t *testing.T, q *Queue, steward, token string) (*nats.Conn, <-chan error) {
	t.Helper()
	errs := make(chan error, 16)
	nc, err := nats.Connect("", nats.InProcessServer(q.srv), nats.UserInfo(steward, token),
		nats.CustomInboxPrefix(InboxPrefix(steward)),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) { errs <- err }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	return nc, errs
}

func TestRunnerCredentialsAreScopedToTheirSteward(t *testing.T) {
	// Over TCP, so a refused login fails at once instead of waiting out the
	// in-process connection.
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	q, err := StartQueue(t.Context(), QueueOptions{Addr: addr, DataDir: t.TempDir(), RunnerTokens: testRunnerTokens})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	if err := q.ServeTokens(func(context.Context, Job) (Token, error) { return Token{Token: "ghs_fake"}, nil }, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	octocats := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", InstallationID: 1, Title: "t"}
	if _, err := q.Publish(t.Context(), octocats); err != nil {
		t.Fatal(err)
	}

	if _, err := nats.Connect("nats://" + addr); err == nil {
		t.Error("connected without credentials")
	}
	if _, err := nats.Connect("nats://"+addr, nats.UserInfo("octocat", "hubot-token")); err == nil {
		t.Error("connected as octocat with hubot's token")
	}

	octocat, _ := connectAs(t, q, "octocat", "octocat-token")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	js, err := jetstream.New(octocat)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := js.CreateOrUpdateConsumer(ctx, JobStream, jetstream.ConsumerConfig{
		Durable: ConsumerName("octocat"), FilterSubject: JobFilter("octocat"), AckPolicy: jetstream.AckExplicitPolicy,
	}); err != nil {
		t.Fatalf("octocat cannot pull its own jobs: %v", err)
	}
	msg, err := octocat.RequestWithContext(ctx, TokenSubjectPrefix+"octocat", []byte(octocats.Subject()))
	if err != nil || !strings.Contains(string(msg.Data), "ghs_fake") {
		t.Fatalf("octocat's token request = %v, %v; want its job's token", msg, err)
	}

	hubot, errs := connectAs(t, q, "hubot", "hubot-token")
	short := func() context.Context {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		t.Cleanup(cancel)
		return ctx
	}
	if msg, err := hubot.RequestWithContext(short(), TokenSubjectPrefix+"octocat", []byte(octocats.Subject())); err == nil {
		t.Errorf("hubot got a reply on octocat's token subject: %s", msg.Data)
	}
	if msg, err := hubot.RequestWithContext(short(), TokenSubjectPrefix+"hubot", []byte(octocats.Subject())); err != nil || strings.Contains(string(msg.Data), "ghs_fake") {
		t.Errorf("hubot's request for octocat's job = %v, %v; want a refusal", msg, err)
	}
	hubotJS, err := jetstream.New(hubot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hubotJS.CreateOrUpdateConsumer(short(), JobStream, jetstream.ConsumerConfig{
		Durable: ConsumerName("hubot"), FilterSubject: JobFilter("octocat"), AckPolicy: jetstream.AckExplicitPolicy,
	}); err == nil {
		t.Error("hubot created a consumer over octocat's jobs")
	}
	if _, err := hubotJS.Stream(short(), JobStream); err == nil {
		t.Error("hubot read the job stream's info")
	}
	for len(errs) > 0 {
		<-errs // the refusals of the publishes above
	}
	for _, subject := range []string{"_INBOX.>", InboxPrefix("octocat") + ".>", jobSubjectPrefix + ">"} {
		if _, err := hubot.SubscribeSync(subject); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-errs:
			if !errors.Is(err, nats.ErrPermissionViolation) || !strings.Contains(err.Error(), `"`+subject+`"`) {
				t.Errorf("subscribe to %s: %v, want a permission violation for it", subject, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("hubot subscribed to %s", subject)
		}
	}
}

func TestPublishWithoutTrackingKeepsTheStewardSubject(t *testing.T) {
	q := startQueue(t)
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", Title: "t"}
	if created, err := q.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	got := jobs(t, q)
	if len(got) != 1 || got[0].Pool || got[0].Subject() != "schmeide.jobs.octocat.7.1" {
		t.Fatalf("jobs = %+v, want the steward subject", got)
	}
}

func TestPublishRoutesByRegistration(t *testing.T) {
	q := startQueue(t)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)
	runner.publish(RegisterSubjectPrefix+"octocat", Registration{
		Steward: "octocat", Harnesses: []string{"fake"}, Host: "laptop", Pool: true,
	})
	eventually(t, func() bool { return tracker.hasRunner("octocat") })

	kept := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", InstallationID: 1, Title: "t"}
	if created, err := q.Publish(t.Context(), kept); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	pooled := Job{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "hubot", InstallationID: 1, Title: "t"}
	if created, err := q.Publish(t.Context(), pooled); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	// A later registration does not move a job that is already on the pool,
	// and does not create a second one.
	runner.publish(RegisterSubjectPrefix+"hubot", Registration{Steward: "hubot", Harnesses: []string{"fake"}, Host: "desk"})
	eventually(t, func() bool { return tracker.hasRunner("hubot") })
	if created, err := q.Publish(t.Context(), pooled); err != nil || created {
		t.Fatalf("second Publish = %v, %v, want a duplicate", created, err)
	}

	got := jobs(t, q)
	if len(got) != 2 {
		t.Fatalf("jobs = %+v, want two", got)
	}
	if got[0].Pool || got[0].Subject() != kept.Subject() {
		t.Errorf("registered steward's job = %+v on %s, want the steward subject", got[0], got[0].Subject())
	}
	if !got[1].Pool || got[1].Steward != "hubot" || got[1].Subject() != "schmeide.jobs."+PoolName+".7.2" {
		t.Errorf("unregistered steward's job = %+v on %s, want the pool subject", got[1], got[1].Subject())
	}
	if strings.HasPrefix(got[1].Subject(), strings.TrimSuffix(JobFilter("octocat"), ">")) ||
		strings.HasPrefix(got[1].Subject(), strings.TrimSuffix(JobFilter("hubot"), ">")) {
		t.Errorf("pool subject %s matches a steward filter", got[1].Subject())
	}
	if !strings.HasPrefix(got[1].Subject(), strings.TrimSuffix(JobFilter(PoolName), ">")) {
		t.Errorf("pool subject %s is outside %s", got[1].Subject(), JobFilter(PoolName))
	}
}

func TestPoolNameIsRejectedAsASteward(t *testing.T) {
	q := startQueue(t)
	if _, err := q.Publish(t.Context(), Job{Steward: PoolName, RepoID: 1, Issue: 1}); err == nil {
		t.Fatal("Publish accepted the pool name as a steward")
	}
	if got := jobs(t, q); len(got) != 0 {
		t.Fatalf("jobs = %+v, want none", got)
	}
	body := bytes.ReplaceAll(payload(t, "issues_assigned_labeled.json"), []byte("octocat"), []byte(PoolName))
	if job, err := JobFromIssuesEvent(body, DefaultReadyLabel); err == nil {
		t.Fatalf("JobFromIssuesEvent = %+v, %v; want the pool name rejected", job, err)
	}
}

func TestPoolJobTokenRequiresThePoolFlag(t *testing.T) {
	q := startQueue(t)
	tracker := startTracker(t, q)
	if err := q.ServeTokens(func(context.Context, Job) (Token, error) {
		return Token{Token: "ghs_pool"}, nil
	}, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", InstallationID: 1, Title: "t"}
	if created, err := q.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	stored := jobs(t, q)
	if len(stored) != 1 || !stored[0].Pool {
		t.Fatalf("jobs = %+v, want one pool job", stored)
	}
	request := func(steward, subject string) TokenReply {
		t.Helper()
		msg, err := q.nc.Request(TokenSubjectPrefix+steward, []byte(subject), 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		var reply TokenReply
		if err := json.Unmarshal(msg.Data, &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if got := request("hubot", stored[0].Subject()); got.Error == "" || got.Token.Token != "" {
		t.Fatalf("token before opt-in = %+v, want a refusal", got)
	}
	runner := startFakeRunner(t, q)
	runner.publish(RegisterSubjectPrefix+"octocat", Registration{Steward: "octocat", Harnesses: []string{"fake"}, Host: "h"})
	eventually(t, func() bool { return tracker.hasRunner("octocat") && !tracker.poolRunner("octocat") })
	if got := request("octocat", stored[0].Subject()); got.Error == "" || got.Token.Token != "" {
		t.Fatalf("token without --pool = %+v, want a refusal", got)
	}
	runner.publish(RegisterSubjectPrefix+"hubot", Registration{
		Steward: "hubot", Harnesses: []string{"fake"}, Host: "h", Pool: true,
	})
	eventually(t, func() bool { return tracker.poolRunner("hubot") })
	if got := request("hubot", stored[0].Subject()); got.Error != "" || got.Token.Token != "ghs_pool" {
		t.Fatalf("pool token = %+v, want ghs_pool", got)
	}
}

func issueRecordOf(t *testing.T, q *Queue, job Job) (issueRecord, uint64) {
	t.Helper()
	entry, err := q.issues.Get(t.Context(), issueKey(job))
	if err != nil {
		t.Fatal(err)
	}
	var record issueRecord
	if err := json.Unmarshal(entry.Value(), &record); err != nil {
		t.Fatal(err)
	}
	return record, entry.Revision()
}

func TestPublishRefusesAnUnfinishedOrSucceededIssue(t *testing.T) {
	q := startQueue(t)
	startTracker(t, q)
	runner := startFakeRunner(t, q)
	job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", Title: "t"}
	if created, err := q.Publish(t.Context(), job); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	job.Pool = true // Track is running and octocat has no runner.
	for _, state := range []State{"", StateClaimed, StateRetrying, StatePROpened} {
		if state != "" {
			runner.report(job, state, "")
		}
		eventually(t, func() bool { r, _ := issueRecordOf(t, q, job); return r.State == state || !state.final() })
		if created, err := q.Publish(t.Context(), job); err != nil || created {
			t.Fatalf("Publish after %q = %v, %v, want a duplicate", state, created, err)
		}
	}
	if record, _ := issueRecordOf(t, q, job); record.State != StatePROpened {
		t.Errorf("record = %+v, want %s", record, StatePROpened)
	}
	if got := jobs(t, q); len(got) != 1 {
		t.Errorf("jobs = %+v, want one", got)
	}
}

func TestPublishReplacesAFailedJob(t *testing.T) {
	for _, final := range []State{StateFailed, StateGateFailed} {
		t.Run(string(final), func(t *testing.T) {
			dir := t.TempDir()
			q, err := StartQueue(t.Context(), QueueOptions{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			startTracker(t, q)
			runner := startFakeRunner(t, q)
			job := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", Title: "t", Pool: true}
			if created, err := q.Publish(t.Context(), job); err != nil || !created {
				t.Fatalf("Publish = %v, %v", created, err)
			}
			runner.report(job, final, "boom")
			eventually(t, func() bool { r, _ := issueRecordOf(t, q, job); return r.State == final })
			if created, err := q.Publish(t.Context(), job); err != nil || !created {
				t.Fatalf("Publish after %s = %v, %v, want a new job", final, created, err)
			}
			if created, err := q.Publish(t.Context(), job); err != nil || created {
				t.Fatalf("third Publish = %v, %v, want a duplicate", created, err)
			}
			if got := jobs(t, q); len(got) != 2 {
				t.Errorf("jobs = %+v, want the failed one and one new one", got)
			}
			q.Close()

			// A restart replays the first job's final status. It must not
			// finish the second job, which is on the same subject.
			q, err = StartQueue(t.Context(), QueueOptions{DataDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			startTracker(t, q)
			if record, _ := issueRecordOf(t, q, job); record.State != "" {
				t.Errorf("record after restart = %+v, want the new job unfinished", record)
			}
			if created, err := q.Publish(t.Context(), job); err != nil || created {
				t.Fatalf("Publish after restart = %v, %v, want a duplicate", created, err)
			}
		})
	}
}

func TestIssueBucketIsSeededBeforeTheStreamIsLimited(t *testing.T) {
	dir := t.TempDir()
	q, err := StartQueue(t.Context(), QueueOptions{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	recorded := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", Title: "t"}
	if created, err := q.Publish(t.Context(), recorded); err != nil || !created {
		t.Fatalf("Publish = %v, %v", created, err)
	}
	// Jobs stored by a control node from before the issue bucket.
	legacy := []Job{
		{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "octocat", Title: "t"},
		{Repo: "o/r", RepoID: 7, Issue: 3, Steward: "hubot", Title: "t"},
	}
	for _, job := range legacy {
		data, _ := json.Marshal(job)
		if _, err := q.js.Publish(t.Context(), job.Subject(), data); err != nil {
			t.Fatal(err)
		}
	}
	_, revision := issueRecordOf(t, q, recorded)
	q.Close()

	// A limit applied before the seed would discard the two oldest jobs
	// before they were recorded.
	defer func(n int64) { jobStreamMaxMsgs = n }(jobStreamMaxMsgs)
	jobStreamMaxMsgs = 1
	for range 2 { // A second restart seeds again and changes nothing.
		q, err = StartQueue(t.Context(), QueueOptions{DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if _, rev := issueRecordOf(t, q, recorded); rev != revision {
			t.Errorf("recorded issue revision = %d, want %d", rev, revision)
		}
		for _, job := range legacy {
			if record, _ := issueRecordOf(t, q, job); record.Subject != job.Subject() || record.State != "" {
				t.Errorf("record = %+v, want %s unfinished", record, job.Subject())
			}
		}
		keys, err := q.issues.Keys(t.Context())
		if err != nil || len(keys) != 3 {
			t.Errorf("keys = %v, %v, want three", keys, err)
		}
		if got := jobs(t, q); len(got) != 1 {
			t.Errorf("jobs = %+v, want the stream limited to one", got)
		}
		for _, job := range append(legacy, recorded) {
			if created, err := q.Publish(t.Context(), job); err != nil || created {
				t.Errorf("Publish %s = %v, %v, want a duplicate", job.Subject(), created, err)
			}
		}
		q.Close()
	}
}

// TestStewardCannotMintAnotherStewardsToken locks the refusals in tokenFor:
// each names the subject and the steward, and none reaches the minter.
func TestStewardCannotMintAnotherStewardsToken(t *testing.T) {
	q := startQueue(t)
	tracker := startTracker(t, q)
	runner := startFakeRunner(t, q)
	runner.publish(RegisterSubjectPrefix+"octocat", Registration{Steward: "octocat", Harnesses: []string{"fake"}, Host: "h"})
	eventually(t, func() bool { return tracker.hasRunner("octocat") })

	octocats := Job{Repo: "o/r", RepoID: 7, Issue: 1, Steward: "octocat", InstallationID: 1, Title: "t"}
	pooled := Job{Repo: "o/r", RepoID: 7, Issue: 2, Steward: "monalisa", InstallationID: 1, Title: "t"}
	for _, job := range []Job{octocats, pooled} {
		if created, err := q.Publish(t.Context(), job); err != nil || !created {
			t.Fatalf("Publish = %v, %v", created, err)
		}
	}
	stored := jobs(t, q)
	if len(stored) != 2 || stored[0].Pool || !stored[1].Pool {
		t.Fatalf("jobs = %+v, want octocat's own job then a pool job", stored)
	}
	pooled = stored[1]
	// Jobs the control node never publishes, stored by hand to reach the
	// checks behind the subject prefix: hubot's job on octocat's subject, and
	// a job on the pool subject that is not marked as a pool job.
	forged, err := json.Marshal(Job{Repo: "o/r", RepoID: 7, Issue: 3, Steward: "hubot", InstallationID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.js.Publish(t.Context(), "schmeide.jobs.octocat.7.3", forged); err != nil {
		t.Fatal(err)
	}
	unpooled, err := json.Marshal(Job{Repo: "o/r", RepoID: 7, Issue: 4, Steward: "octocat", InstallationID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.js.Publish(t.Context(), jobSubjectPrefix+PoolName+".7.4", unpooled); err != nil {
		t.Fatal(err)
	}

	var minted []Job
	mint := func(_ context.Context, job Job) (Token, error) {
		minted = append(minted, job)
		return Token{Token: "ghs_" + job.Steward}, nil
	}
	poolSubject := pooled.Subject()
	for _, tt := range []struct{ name, steward, subject, want string }{
		{
			"another steward's job", "hubot", octocats.Subject(),
			`"schmeide.jobs.octocat.7.1" is not a job of steward "hubot"`,
		},
		{
			"a wildcard over another steward", "hubot", "schmeide.jobs.octocat.>",
			`"schmeide.jobs.octocat.>" is not a job of steward "hubot"`,
		},
		{
			"a wildcard over every job", "hubot", "schmeide.jobs.*.7.1",
			`"schmeide.jobs.*.7.1" is not a job of steward "hubot"`,
		},
		{
			"a job stored on the asker's subject for another steward", "octocat", "schmeide.jobs.octocat.7.3",
			`job "schmeide.jobs.octocat.7.3" belongs to "hubot", not "octocat"`,
		},
		{
			"a pool job without a registration", "hubot", poolSubject,
			`"` + poolSubject + `" is not a pool job "hubot" can take`,
		},
		{
			"a pool job without the pool flag", "octocat", poolSubject,
			`"` + poolSubject + `" is not a pool job "octocat" can take`,
		},
		{
			"a job on the pool subject that is not a pool job", "octocat", jobSubjectPrefix + PoolName + ".7.4",
			`"schmeide.jobs._pool.7.4" is not a pool job "octocat" can take`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			token, err := q.tokenFor(t.Context(), tt.steward, tt.subject, mint)
			if err == nil || err.Error() != tt.want || token.Token != "" {
				t.Errorf("tokenFor(%s, %s) = %+v, %v; want %s", tt.steward, tt.subject, token, err, tt.want)
			}
		})
	}
	if len(minted) != 0 {
		t.Fatalf("minted %+v for refused requests", minted)
	}

	// A pool steward gets the pool job only once it registers for the pool.
	runner.publish(RegisterSubjectPrefix+"hubot", Registration{Steward: "hubot", Harnesses: []string{"fake"}, Host: "h", Pool: true})
	eventually(t, func() bool { return tracker.poolRunner("hubot") })
	if token, err := q.tokenFor(t.Context(), "hubot", poolSubject, mint); err != nil || token.Token != "ghs_monalisa" {
		t.Fatalf("pool token = %+v, %v; want ghs_monalisa", token, err)
	}
	if token, err := q.tokenFor(t.Context(), "octocat", octocats.Subject(), mint); err != nil || token.Token != "ghs_octocat" {
		t.Fatalf("own token = %+v, %v; want ghs_octocat", token, err)
	}
	if token, err := q.tokenFor(t.Context(), "hubot", octocats.Subject(), mint); err == nil {
		t.Fatalf("pool steward got another steward's own job: %+v", token)
	}
}

// TestRunnerCannotPublishOutsideItsAllowList connects as hubot and publishes
// on subjects runnerPermissions does not grant it. Each must be refused.
func TestRunnerCannotPublishOutsideItsAllowList(t *testing.T) {
	q := startQueue(t)
	hubot, errs := connectAs(t, q, "hubot", "hubot-token")
	octocatConsumer := JobStream + "." + ConsumerName("octocat")
	for _, subject := range []string{
		TokenSubjectPrefix + "octocat",
		TokenSubjectPrefix + PoolName,
		RegisterSubjectPrefix + "octocat",
		RegisterSubject("octocat", "desk"),
		StatusSubjectPrefix + "octocat.7.1",
		jobSubjectPrefix + "hubot.7.1",
		jobSubjectPrefix + PoolName + ".7.1",
		"$JS.API.CONSUMER.CREATE." + octocatConsumer + "." + JobFilter("octocat"),
		"$JS.API.CONSUMER.CREATE." + JobStream + "." + ConsumerName("hubot") + "." + JobFilter("octocat"),
		"$JS.API.CONSUMER.MSG.NEXT." + octocatConsumer,
		"$JS.ACK." + octocatConsumer + ".1.1.1.1.1.0.0",
		"$JS.API.STREAM.PURGE." + JobStream,
		"$KV." + issueBucket + ".7.1",
		InboxPrefix("octocat") + ".reply",
	} {
		if err := hubot.Publish(subject, []byte("{}")); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-errs:
			if !errors.Is(err, nats.ErrPermissionViolation) || !strings.Contains(err.Error(), `"`+subject+`"`) {
				t.Errorf("publish to %s: %v, want a permission violation for it", subject, err)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("hubot published to %s", subject)
		}
	}
}

// TestUnsignedRequestNeverReachesTheReplayCache pins the intake order: the
// signature is checked before the delivery id is looked up, so a request
// without the secret learns nothing from the cache and gets no 2xx.
func TestUnsignedRequestNeverReachesTheReplayCache(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json")
	q := startQueue(t)
	var published int
	h := webhookHandler(q, WebhookPolicy{RequestsPerMinute: 100, DeliveryTTL: 10 * time.Minute}, &published)
	if got := deliver(t, h, delivery{body: ready, signature: sign(ready), id: "deliv-1"}); got != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d", got, http.StatusAccepted)
	}
	for _, signature := range []string{"", "sha256=abcd"} {
		if got := deliver(t, h, delivery{body: ready, signature: signature, id: "deliv-1"}); got != http.StatusUnauthorized {
			t.Errorf("replayed id with signature %q: status = %d, want %d", signature, got, http.StatusUnauthorized)
		}
	}
	if published != 1 {
		t.Errorf("Publish calls = %d, want 1", published)
	}
}

// TestReplayCountsAgainstTheRateLimit pins that the rate limit runs before
// the replay check, so repeating a delivery id is not a way around it.
func TestReplayCountsAgainstTheRateLimit(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json")
	now := time.Now()
	q := startQueue(t)
	h := webhookHandler(q, WebhookPolicy{
		RequestsPerMinute: 1,
		DeliveryTTL:       10 * time.Minute,
		now:               func() time.Time { return now },
	}, nil)
	d := delivery{body: ready, signature: sign(ready), id: "deliv-1"}
	if got := deliver(t, h, d); got != http.StatusAccepted {
		t.Fatalf("first status = %d, want %d", got, http.StatusAccepted)
	}
	if got := deliver(t, h, d); got != http.StatusTooManyRequests {
		t.Errorf("replay over the limit: status = %d, want %d", got, http.StatusTooManyRequests)
	}
}

// TestUnsignedRequestsDoNotUseTheRateBudget pins that the signature is checked
// before the rate limit, so a flood without the secret cannot crowd out a
// genuine delivery.
func TestUnsignedRequestsDoNotUseTheRateBudget(t *testing.T) {
	ready := payload(t, "issues_assigned_labeled.json")
	now := time.Now()
	q := startQueue(t)
	h := webhookHandler(q, WebhookPolicy{
		RequestsPerMinute: 1,
		DeliveryTTL:       10 * time.Minute,
		now:               func() time.Time { return now },
	}, nil)
	for range 3 {
		if got := deliver(t, h, delivery{body: ready, id: "flood"}); got != http.StatusUnauthorized {
			t.Fatalf("unsigned status = %d, want %d", got, http.StatusUnauthorized)
		}
	}
	if got := deliver(t, h, delivery{body: ready, signature: sign(ready), id: "deliv-1"}); got != http.StatusAccepted {
		t.Errorf("signed status after an unsigned flood = %d, want %d", got, http.StatusAccepted)
	}
}
