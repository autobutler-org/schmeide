package control

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	// JobStream is the JetStream stream that holds jobs.
	JobStream = "SCHMEIDE_JOBS"
	// TokenSubjectPrefix plus a steward is the request subject a runner sends
	// a job's subject to in order to get that job's GitHub token. The reply is
	// a TokenReply. A steward can only publish on its own token subject. It
	// gets a token for its own jobs, and for a pool job when it registered
	// with the pool flag.
	TokenSubjectPrefix = "schmeide.token."

	jobSubjectPrefix = "schmeide.jobs."
	// PoolName is the subject token for a job whose steward has no registered
	// runner. The job stays on the job stream, at
	// schmeide.jobs.<PoolName>.<repo>.<issue>. PoolName is not a steward: that
	// name is rejected so no steward's job filter can match the pool subject.
	PoolName = "_pool"
	// controlUser is not a GitHub login, so no steward can share its name.
	controlUser  = "_schmeide_control"
	startTimeout = 10 * time.Second

	// issueBucket is the JetStream KV bucket that records, per issue, the
	// latest job and whether it has finished. Publish dedupes against it.
	issueBucket = "SCHMEIDE_ISSUES"
	// StageBucket is the JetStream KV bucket a runner records each job's
	// stage in, keyed by the job's subject without "schmeide.jobs.". A
	// restarted runner reads it to resume a job whose pull request is open.
	StageBucket = "SCHMEIDE_STAGES"
)

// jobStreamMaxMsgs caps the job stream once the issue bucket has been seeded
// from it, discarding the oldest jobs first. A runner holds a job for one
// session: hours at most, with a heartbeat every AckWait/3 (1m40s by default).
// 100,000 later jobs would have to be queued while one job is still held
// before that job is discarded. At a few KB per job (the issue title and
// body) that is a few hundred MB on disk at most. A var so tests can shrink it.
var jobStreamMaxMsgs int64 = 100_000

// ConsumerName is the durable JetStream consumer a steward's runners share.
func ConsumerName(steward string) string { return "runner-" + steward }

// JobFilter is the subject filter that selects a steward's jobs.
func JobFilter(steward string) string { return jobSubjectPrefix + steward + ".>" }

// InboxPrefix is the reply subject prefix a steward's runners must use. Each
// steward can only subscribe below its own prefix, so no steward can read the
// replies, and with them the tokens, meant for another.
func InboxPrefix(steward string) string { return "_INBOX_" + steward }

// TokenReply answers a token request. Error is set instead of Token
// when no token could be minted.
type TokenReply struct {
	Token

	Error string `json:"error,omitempty"`
}

// QueueOptions configures the embedded NATS server.
type QueueOptions struct {
	// Addr is the host:port runners connect to. Empty means no network
	// listener, which is only useful in tests.
	Addr string
	// RunnerTokens maps each steward to the token its runners present to NATS.
	// A runner connects as its steward and may only touch that steward's
	// jobs, statuses, and tokens.
	RunnerTokens map[string]string
	// DataDir is where JetStream keeps its files.
	DataDir string
	// TLSCertFile and TLSKeyFile are the PEM certificate and key the network
	// listener serves. Both set means runners must connect over TLS. Neither
	// means plain TCP. The in-process connection never uses TLS.
	TLSCertFile string
	TLSKeyFile  string
}

// Queue is the embedded NATS server with the JetStream job stream.
type Queue struct {
	srv     *server.Server
	nc      *nats.Conn
	stream  jetstream.Stream
	js      jetstream.JetStream
	issues  jetstream.KeyValue
	tracker *Tracker
}

// issueRecord is the issue bucket's value: the issue's latest job.
type issueRecord struct {
	// Subject is the job's subject (see Job.Subject).
	Subject string `json:"subject"`
	// State is the job's final state. Empty means the job has not finished.
	State State `json:"state,omitempty"`
	// After is the last status stream sequence when the job was published.
	// Only statuses stored after it can finish this job; earlier ones belong
	// to an earlier job for the same issue.
	After uint64 `json:"after,omitempty"`
}

func issueKey(job Job) string { return fmt.Sprintf("%d.%d", job.RepoID, job.Issue) }

// StartQueue starts the embedded NATS server and makes sure the job stream
// exists.
func StartQueue(ctx context.Context, o QueueOptions) (*Queue, error) {
	controlPassword := rand.Text()
	users := []*server.User{{Username: controlUser, Password: controlPassword}}
	for steward, token := range o.RunnerTokens {
		users = append(users, &server.User{Username: steward, Password: token, Permissions: runnerPermissions(steward)})
	}
	opts := &server.Options{
		ServerName: "schmeide-control",
		DontListen: o.Addr == "",
		JetStream:  true,
		StoreDir:   o.DataDir,
		Users:      users,
		NoSigs:     true,
		NoLog:      true,
	}
	if o.Addr != "" {
		host, portText, err := net.SplitHostPort(o.Addr)
		if err != nil {
			return nil, fmt.Errorf("nats address %q is not host:port: %w", o.Addr, err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			return nil, fmt.Errorf("nats address %q has a non-numeric port: %w", o.Addr, err)
		}
		opts.Host, opts.Port = host, port
	}
	if o.TLSCertFile != "" || o.TLSKeyFile != "" {
		tlsConfig, err := natsTLSConfig(o.TLSCertFile, o.TLSKeyFile)
		if err != nil {
			return nil, err
		}
		opts.TLSConfig = tlsConfig
	}
	srv, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("configure embedded nats: %w", err)
	}
	srv.Start()
	if !srv.ReadyForConnections(startTimeout) {
		srv.Shutdown()
		return nil, fmt.Errorf("embedded nats did not start on %q within %s; is the address already in use?", o.Addr, startTimeout)
	}

	q := &Queue{srv: srv}
	if q.nc, err = nats.Connect("", nats.InProcessServer(srv), nats.UserInfo(controlUser, controlPassword)); err != nil {
		q.Close()
		return nil, fmt.Errorf("connect to embedded nats: %w", err)
	}
	if q.js, err = jetstream.New(q.nc); err != nil {
		q.Close()
		return nil, fmt.Errorf("open jetstream: %w", err)
	}
	if err := q.openJobs(ctx); err != nil {
		q.Close()
		return nil, err
	}
	return q, nil
}

// openJobs opens the issue bucket and the job stream. The job stream used to
// be the only record of which issues have a job, so every job it holds is
// copied into the issue bucket before the stream is allowed to discard any.
// A restart repeats this safely: an issue that already has a record keeps it.
func (q *Queue) openJobs(ctx context.Context) error {
	var err error
	if q.issues, err = q.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: issueBucket}); err != nil {
		return fmt.Errorf("create issue bucket: %w", err)
	}
	if _, err = q.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: StageBucket}); err != nil {
		return fmt.Errorf("create stage bucket: %w", err)
	}
	// Create the stream without a limit if it is new; leave an existing one
	// as it is until the seed is done.
	config := jetstream.StreamConfig{Name: JobStream, Subjects: []string{jobSubjectPrefix + ">"}}
	if q.stream, err = q.js.Stream(ctx, JobStream); errors.Is(err, jetstream.ErrStreamNotFound) {
		q.stream, err = q.js.CreateStream(ctx, config)
	}
	if err != nil {
		return fmt.Errorf("open job stream: %w", err)
	}
	if err := q.seedIssues(ctx); err != nil {
		return err
	}
	config.MaxMsgs = jobStreamMaxMsgs
	if q.stream, err = q.js.UpdateStream(ctx, config); err != nil {
		return fmt.Errorf("limit job stream: %w", err)
	}
	return nil
}

// seedIssues records every job in the stream whose issue has no record yet.
// The record says the job has not finished; Track then replays the status
// history and finishes it.
func (q *Queue) seedIssues(ctx context.Context) error {
	info, err := q.stream.Info(ctx)
	if err != nil {
		return fmt.Errorf("read job stream: %w", err)
	}
	for seq := info.State.FirstSeq; seq <= info.State.LastSeq && info.State.Msgs > 0; seq++ {
		msg, err := q.stream.GetMsg(ctx, seq)
		if errors.Is(err, jetstream.ErrMsgNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("seed issue bucket: %w", err)
		}
		var job Job
		if err := json.Unmarshal(msg.Data, &job); err != nil {
			continue
		}
		data, _ := json.Marshal(issueRecord{Subject: msg.Subject})
		if _, err := q.issues.Create(ctx, issueKey(job), data); err != nil && !errors.Is(err, jetstream.ErrKeyExists) {
			return fmt.Errorf("seed issue bucket: %w", err)
		}
	}
	return nil
}

// runnerPermissions lets a steward's runners pull and acknowledge that
// steward's jobs, report on them, and ask for their tokens. Every steward
// may also use the one shared pool consumer and report a pool job's status.
// A runner only pulls from that consumer when it was started with --pool.
// Any steward may read the status stream, which is what `schmeide status`
// shows: every runner's registration and every job's latest state. A steward
// reads and writes the stage records of its own jobs and of pool jobs.
func runnerPermissions(steward string) *server.Permissions {
	consumer := JobStream + "." + ConsumerName(steward)
	pool := JobStream + "." + ConsumerName(PoolName)
	stages := "$KV." + StageBucket + "."
	stageGet := "$JS.API.DIRECT.GET.KV_" + StageBucket + "." + stages
	return &server.Permissions{
		Publish: &server.SubjectPermission{Allow: []string{
			"$JS.API.CONSUMER.CREATE." + consumer + "." + JobFilter(steward),
			"$JS.API.CONSUMER.INFO." + consumer,
			"$JS.API.CONSUMER.MSG.NEXT." + consumer,
			"$JS.ACK." + consumer + ".>",
			"$JS.API.CONSUMER.CREATE." + pool + "." + JobFilter(PoolName),
			"$JS.API.CONSUMER.INFO." + pool,
			"$JS.API.CONSUMER.MSG.NEXT." + pool,
			"$JS.ACK." + pool + ".>",
			RegisterSubjectPrefix + steward,
			StatusSubjectPrefix + steward + ".>",
			StatusSubjectPrefix + PoolName + ".>",
			TokenSubjectPrefix + steward,
			"$JS.API.STREAM.INFO." + StatusStream,
			"$JS.API.STREAM.MSG.GET." + StatusStream,
			"$JS.API.STREAM.INFO.KV_" + StageBucket,
			stages + steward + ".>",
			stages + PoolName + ".>",
			stageGet + steward + ".>",
			stageGet + PoolName + ".>",
		}},
		Subscribe: &server.SubjectPermission{Allow: []string{InboxPrefix(steward) + ".>"}},
	}
}

// Conn is the control node's own connection to the embedded NATS server,
// which may publish and subscribe anywhere.
func (q *Queue) Conn() *nats.Conn { return q.nc }

// Close stops the embedded NATS server.
func (q *Queue) Close() {
	if q.nc != nil {
		q.nc.Close()
	}
	q.srv.Shutdown()
	q.srv.WaitForShutdown()
}

// Publish stores the job unless its issue already has a job, whoever the
// steward was at the time, that is unfinished or opened a pull request. A job
// that ended gate_failed or failed is replaced by this one. It reports whether
// a job was created. Once Track is running, a steward with no registration
// gets the job on the pool subject. Until then there is no registration
// record, and the job stays on the steward's subject. Once Track is running,
// a new job's first status, StatePublished, is stored before the job.
func (q *Queue) Publish(ctx context.Context, job Job) (bool, error) {
	if job.Steward == PoolName {
		return false, fmt.Errorf("steward %q is reserved and is not a GitHub login", job.Steward)
	}
	job.Pool = q.tracker != nil && !q.tracker.hasRunner(job.Steward)
	data, err := json.Marshal(job)
	if err != nil {
		return false, err
	}
	key := issueKey(job)
	var rev uint64
	entry, err := q.issues.Get(ctx, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
	case err != nil:
		return false, fmt.Errorf("look up issue %s: %w", key, err)
	default:
		var prev issueRecord
		if err := json.Unmarshal(entry.Value(), &prev); err != nil {
			return false, fmt.Errorf("decode issue %s: %w", key, err)
		}
		if !prev.State.failure() {
			return false, nil
		}
		rev = entry.Revision()
	}
	record := issueRecord{Subject: job.Subject()}
	if q.tracker != nil {
		if record.After, err = q.tracker.lastSeq(ctx); err != nil {
			return false, err
		}
	}
	recordData, _ := json.Marshal(record)
	// Create and Update only succeed against the revision read above, so of
	// two concurrent publishes for one issue, one creates the job.
	if rev == 0 {
		_, err = q.issues.Create(ctx, key, recordData)
	} else {
		_, err = q.issues.Update(ctx, key, recordData, rev)
	}
	var apiErr *jetstream.APIError
	if errors.Is(err, jetstream.ErrKeyExists) || errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("record issue %s: %w", key, err)
	}
	// Put the previous record back, so the issue is not stuck with a job
	// that was never stored.
	undo := func() {
		if rev == 0 {
			_ = q.issues.Delete(ctx, key)
		} else {
			_, _ = q.issues.Put(ctx, key, entry.Value())
		}
	}
	// The published status is stored before the job, so a runner's claim,
	// which needs the job, always lands after it and replaces it.
	statusSubject := StatusSubjectPrefix + strings.TrimPrefix(job.Subject(), jobSubjectPrefix)
	if q.tracker != nil {
		status, _ := json.Marshal(Status{State: StatePublished, Detail: "waiting for a runner; steward " + job.Steward})
		if _, err := q.js.Publish(ctx, statusSubject, status); err != nil {
			undo()
			return false, fmt.Errorf("record published status: %w", err)
		}
	}
	if _, err := q.js.Publish(ctx, job.Subject(), data); err != nil {
		undo()
		if q.tracker != nil {
			// A job that was never stored must not stay listed as published.
			status, _ := json.Marshal(Status{State: StateFailed, Detail: "the control node could not store the job"})
			_, _ = q.js.Publish(ctx, statusSubject, status)
		}
		return false, err
	}
	return true, nil
}

// ServeTokens answers requests on a steward's token subject. The request body
// is the subject of a job in the stream, so tokens are only minted for real
// jobs. A steward gets a token for its own jobs, and for a pool job when it
// registered with the pool flag.
func (q *Queue) ServeTokens(mint func(context.Context, Job) (Token, error), log *slog.Logger) error {
	_, err := q.nc.Subscribe(TokenSubjectPrefix+"*", func(m *nats.Msg) {
		ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
		defer cancel()
		reply := TokenReply{}
		steward := strings.TrimPrefix(m.Subject, TokenSubjectPrefix)
		token, err := q.tokenFor(ctx, steward, string(m.Data), mint)
		if err != nil {
			log.Error("token request failed", "steward", steward, "job", string(m.Data), "err", err)
			reply.Error = err.Error()
		}
		reply.Token = token
		data, _ := json.Marshal(reply)
		if err := m.Respond(data); err != nil {
			log.Error("token reply failed", "err", err)
		}
	})
	return err
}

func (q *Queue) tokenFor(ctx context.Context, steward, subject string, mint func(context.Context, Job) (Token, error)) (Token, error) {
	if strings.ContainsAny(subject, "*> ") {
		return Token{}, fmt.Errorf("%q is not a job of steward %q", subject, steward)
	}
	own := strings.HasPrefix(subject, jobSubjectPrefix+steward+".")
	pooled := strings.HasPrefix(subject, jobSubjectPrefix+PoolName+".")
	if !own && !pooled {
		return Token{}, fmt.Errorf("%q is not a job of steward %q", subject, steward)
	}
	msg, err := q.stream.GetLastMsgForSubject(ctx, subject)
	if err != nil {
		return Token{}, fmt.Errorf("look up job %q: %w", subject, err)
	}
	var job Job
	if err := json.Unmarshal(msg.Data, &job); err != nil {
		return Token{}, fmt.Errorf("decode job %q: %w", subject, err)
	}
	switch {
	case pooled:
		if !job.Pool || job.Subject() != subject || q.tracker == nil || !q.tracker.poolRunner(steward) {
			return Token{}, fmt.Errorf("%q is not a pool job %q can take", subject, steward)
		}
	case job.Steward != steward:
		return Token{}, fmt.Errorf("job %q belongs to %q, not %q", subject, job.Steward, steward)
	}
	return mint(ctx, job)
}
