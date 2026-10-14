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
	// a TokenReply. A steward can only publish on its own token subject, and
	// only gets tokens for its own jobs.
	TokenSubjectPrefix = "schmeide.token."

	jobSubjectPrefix = "schmeide.jobs."
	// controlUser is not a GitHub login, so no steward can share its name.
	controlUser  = "_schmeide_control"
	startTimeout = 10 * time.Second
)

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
}

// Queue is the embedded NATS server with the JetStream job stream.
type Queue struct {
	srv    *server.Server
	nc     *nats.Conn
	stream jetstream.Stream
	js     jetstream.JetStream
}

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
	// Jobs are kept (limits retention) so that the stream itself is the record
	// of which issues already have a job.
	q.stream, err = q.js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     JobStream,
		Subjects: []string{jobSubjectPrefix + ">"},
	})
	if err != nil {
		q.Close()
		return nil, fmt.Errorf("create job stream: %w", err)
	}
	return q, nil
}

// runnerPermissions lets a steward's runners pull and acknowledge that
// steward's jobs, report on them, and ask for their tokens, and nothing else.
func runnerPermissions(steward string) *server.Permissions {
	consumer := JobStream + "." + ConsumerName(steward)
	return &server.Permissions{
		Publish: &server.SubjectPermission{Allow: []string{
			"$JS.API.CONSUMER.CREATE." + consumer + "." + JobFilter(steward),
			"$JS.API.CONSUMER.INFO." + consumer,
			"$JS.API.CONSUMER.MSG.NEXT." + consumer,
			"$JS.ACK." + consumer + ".>",
			RegisterSubjectPrefix + steward,
			StatusSubjectPrefix + steward + ".>",
			TokenSubjectPrefix + steward,
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

// Publish stores the job unless its issue already has one, whoever the steward
// was at the time. It reports whether a job was created.
func (q *Queue) Publish(ctx context.Context, job Job) (bool, error) {
	data, err := json.Marshal(job)
	if err != nil {
		return false, err
	}
	anySteward := fmt.Sprintf("%s*.%d.%d", jobSubjectPrefix, job.RepoID, job.Issue)
	_, err = q.js.Publish(ctx, job.Subject(), data, jetstream.WithExpectLastSequenceForSubject(0, anySteward))
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence {
		return false, nil
	}
	return err == nil, err
}

// ServeTokens answers requests on a steward's token subject. The request body
// is the subject of one of that steward's jobs in the stream, so tokens are
// only minted for real jobs, and only for the steward who owns the job.
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
	if !strings.HasPrefix(subject, jobSubjectPrefix+steward+".") || strings.ContainsAny(subject, "*> ") {
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
	if job.Steward != steward {
		return Token{}, fmt.Errorf("job %q belongs to %q, not %q", subject, job.Steward, steward)
	}
	return mint(ctx, job)
}
