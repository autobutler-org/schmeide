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
			h := Handler(Intake{Secret: []byte(testSecret), ReadyLabel: DefaultReadyLabel, Allow: allow, Publish: q.Publish}, slog.New(slog.DiscardHandler))
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
			h := Handler(Intake{Secret: []byte(testSecret), ReadyLabel: DefaultReadyLabel, Allow: allow, Publish: q.Publish}, slog.New(slog.DiscardHandler))
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
	h := Handler(Intake{Secret: []byte(testSecret), ReadyLabel: DefaultReadyLabel, Publish: q.Publish}, slog.New(slog.DiscardHandler))
	if got := deliver(t, h, delivery{body: ready, signature: sign(ready)}); got != http.StatusForbidden {
		t.Errorf("status = %d, want %d from an Intake with no allowlist", got, http.StatusForbidden)
	}
	if got := jobs(t, q); len(got) != 0 {
		t.Errorf("jobs = %+v, want none", got)
	}
}

func TestParseRunnerTokens(t *testing.T) {
	got, err := ParseRunnerTokens(" octocat:a:b , hubot:c,")
	if err != nil || len(got) != 2 || got["octocat"] != "a:b" || got["hubot"] != "c" {
		t.Errorf("ParseRunnerTokens = %v, %v, want octocat=a:b and hubot=c", got, err)
	}
	for _, bad := range []string{"", " , ", "octocat", "octocat:", ":t", "octo.cat:t", "octocat:a,octocat:b"} {
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
			RepositoryIDs []int64 `json:"repository_ids"`
		}
		reqBody, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(reqBody, &body); err != nil || len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 901234567 {
			t.Errorf("token request body = %s, want it limited to the job's repository", reqBody)
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
