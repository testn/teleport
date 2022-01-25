/*
Copyright 2021 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"

	broadcast "github.com/dustin/go-broadcast"
	"github.com/google/uuid"
	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/kube/proxy/streamproto"
	tsession "github.com/gravitational/teleport/lib/session"
	"github.com/gravitational/teleport/lib/srv"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
	"github.com/julienschmidt/httprouter"
	log "github.com/sirupsen/logrus"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"
)

const PresenceVerifyInterval = time.Second * 15
const PresenceMaxDifference = time.Minute

// remoteClient is either a kubectl or websocket client.
type remoteClient interface {
	stdinStream() io.Reader
	stdoutStream() io.Writer
	stderrStream() io.Writer
	resizeQueue() <-chan *remotecommand.TerminalSize
	resize(size *remotecommand.TerminalSize) error
	forceTerminate() <-chan struct{}
	sendStatus(error) error
	io.Closer
}

type websocketClientStreams struct {
	stream *streamproto.SessionStream
}

func (p *websocketClientStreams) stdinStream() io.Reader {
	return p.stream
}

func (p *websocketClientStreams) stdoutStream() io.Writer {
	return p.stream
}

func (p *websocketClientStreams) stderrStream() io.Writer {
	return p.stream
}

func (p *websocketClientStreams) resizeQueue() <-chan *remotecommand.TerminalSize {
	return p.stream.ResizeQueue()
}

func (p *websocketClientStreams) resize(size *remotecommand.TerminalSize) error {
	return p.stream.Resize(size)
}

func (p *websocketClientStreams) forceTerminate() <-chan struct{} {
	return p.stream.ForceTerminateQueue()
}

func (p *websocketClientStreams) sendStatus(err error) error {
	return nil
}

func (p *websocketClientStreams) Close() error {
	return trace.Wrap(p.stream.Close())
}

type kubeProxyClientStreams struct {
	proxy     *remoteCommandProxy
	sizeQueue remotecommand.TerminalSizeQueue
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	close     chan struct{}
}

func newKubeProxyClientStreams(proxy *remoteCommandProxy) *kubeProxyClientStreams {
	options := proxy.options()

	return &kubeProxyClientStreams{
		proxy:     proxy,
		stdin:     options.Stdin,
		stdout:    options.Stdout,
		stderr:    options.Stderr,
		close:     make(chan struct{}),
		sizeQueue: proxy.resizeQueue,
	}
}

func (p *kubeProxyClientStreams) stdinStream() io.Reader {
	return p.stdin
}

func (p *kubeProxyClientStreams) stdoutStream() io.Writer {
	return p.stdout
}

func (p *kubeProxyClientStreams) stderrStream() io.Writer {
	return p.stderr
}

func (p *kubeProxyClientStreams) resizeQueue() <-chan *remotecommand.TerminalSize {
	ch := make(chan *remotecommand.TerminalSize)
	go func() {
		for {
			size := p.sizeQueue.Next()
			if size == nil {
				break
			}

			ch <- size
		}
	}()

	return ch
}

func (p *kubeProxyClientStreams) resize(size *remotecommand.TerminalSize) error {
	escape := fmt.Sprintf("\x1b[8;%d;%dt", size.Height, size.Width)
	_, err := p.stdout.Write([]byte(escape))
	return trace.Wrap(err)
}

func (p *kubeProxyClientStreams) forceTerminate() <-chan struct{} {
	return make(chan struct{})
}

func (p *kubeProxyClientStreams) sendStatus(err error) error {
	return trace.Wrap(p.proxy.sendStatus(err))
}

func (p *kubeProxyClientStreams) Close() error {
	close(p.close)
	return trace.Wrap(p.proxy.Close())
}

// multiResizeQueue is a merged queue of multiple terminal size queues.
type multiResizeQueue struct {
	queues   map[string]<-chan *remotecommand.TerminalSize
	cases    []reflect.SelectCase
	callback func(*remotecommand.TerminalSize)
}

func newMultiResizeQueue() *multiResizeQueue {
	return &multiResizeQueue{
		queues: make(map[string]<-chan *remotecommand.TerminalSize),
	}
}

func (r *multiResizeQueue) rebuild() {
	r.cases = nil
	for _, queue := range r.queues {
		r.cases = append(r.cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(queue),
		})
	}
}

func (r *multiResizeQueue) add(id string, queue <-chan *remotecommand.TerminalSize) {
	r.queues[id] = queue
	r.rebuild()
}

func (r *multiResizeQueue) remove(id string) {
	delete(r.queues, id)
	r.rebuild()
}

func (r *multiResizeQueue) Next() *remotecommand.TerminalSize {
	_, value, ok := reflect.Select(r.cases)
	if !ok {
		return nil
	}

	size := value.Interface().(*remotecommand.TerminalSize)
	r.callback(size)
	return size
}

// party represents one participant of the session and their associated state.
type party struct {
	Ctx       authContext
	ID        uuid.UUID
	Client    remoteClient
	Mode      types.SessionParticipantMode
	closeC    chan struct{}
	closeOnce sync.Once
}

// newParty creates a new party.
func newParty(ctx authContext, mode types.SessionParticipantMode, client remoteClient) *party {
	return &party{
		Ctx:    ctx,
		ID:     uuid.New(),
		Client: client,
		Mode:   mode,
		closeC: make(chan struct{}),
	}
}

// Close closes the party and disconnects the remote end.
func (p *party) Close() error {
	var err error

	p.closeOnce.Do(func() {
		close(p.closeC)
		err = p.Client.Close()
	})

	return trace.Wrap(err)
}

// session represents an ongoing k8s session.
type session struct {
	mu sync.RWMutex

	// ctx is the auth context of the session initiator
	ctx authContext

	forwarder *Forwarder

	req *http.Request

	params httprouter.Params

	id uuid.UUID

	// parties is a map of currently active parties.
	parties map[uuid.UUID]*party

	// partiesHistorical is a map of all current previous parties.
	// This is used for audit trails.
	partiesHistorical map[uuid.UUID]*party

	log *log.Entry

	io *srv.TermManager

	terminalSizeQueue *multiResizeQueue

	state types.SessionState

	// stateUpdate is used to broadcast state updates to listers.
	stateUpdate broadcast.Broadcaster

	accessEvaluator auth.SessionAccessEvaluator

	recorder events.StreamWriter

	emitter apievents.Emitter

	// tty is set if the session is using a TTY.
	tty bool

	podName string

	started bool

	initiator uuid.UUID

	expires time.Time

	// sess is the clusterSession used to establish this session.
	sess *clusterSession

	closeC chan struct{}

	closeOnce sync.Once

	// PresenceEnabled is set to true if MFA based presence is required.
	PresenceEnabled bool
}

// newSession creates a new session in pending mode.
func newSession(ctx authContext, forwarder *Forwarder, req *http.Request, params httprouter.Params, initiator *party, sess *clusterSession) (*session, error) {
	id := uuid.New()
	log := forwarder.log.WithField("session", id.String())
	log.Debug("Creating session")
	roles, err := getRolesByName(forwarder, ctx.Context.Identity.GetIdentity().Groups)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	accessEvaluator := auth.NewSessionAccessEvaluator(roles, types.KubernetesSessionKind)

	io := srv.NewTermManager()
	err = io.BroadcastMessage(fmt.Sprintf("Creating session with ID: %v...", id.String()))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	q := req.URL.Query()
	s := &session{
		ctx:               ctx,
		forwarder:         forwarder,
		req:               req,
		params:            params,
		id:                id,
		parties:           make(map[uuid.UUID]*party),
		partiesHistorical: make(map[uuid.UUID]*party),
		log:               log,
		io:                io,
		state:             types.SessionState_SessionStatePending,
		stateUpdate:       broadcast.NewBroadcaster(1),
		accessEvaluator:   accessEvaluator,
		emitter:           events.NewDiscardEmitter(),
		tty:               utils.AsBool(q.Get("tty")),
		terminalSizeQueue: newMultiResizeQueue(),
		started:           false,
		sess:              sess,
		closeC:            make(chan struct{}),
		initiator:         initiator.ID,
		expires:           time.Now().UTC().Add(time.Hour * 24),
		PresenceEnabled:   ctx.Identity.GetIdentity().MFAVerified != "",
	}

	err = s.trackerCreate(initiator, roles)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return s, nil
}

// waitOnAccess puts the session in pending mode and waits for the session
// to fulfill the access requirements again.
func (s *session) waitOnAccess() {
	s.io.Off()
	s.io.BroadcastMessage("Session paused, Waiting for required participants...")

	c := make(chan interface{})
	s.stateUpdate.Register(c)
	defer s.stateUpdate.Unregister(c)

outer:
	for {
		state := <-c
		actualState := state.(types.SessionState)
		switch actualState {
		case types.SessionState_SessionStatePending:
			continue
		case types.SessionState_SessionStateTerminated:
			return
		case types.SessionState_SessionStateRunning:
			break outer
		}
	}

	s.io.BroadcastMessage("Resuming session...")
	s.io.On()
}

// checkPresence checks the presence timestamp of involved moderators
// and kicks them if they are not active.
func (s *session) checkPresence() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, err := s.trackerGet()
	if err != nil {
		return trace.Wrap(err)
	}

	for _, participant := range sess.GetParticipants() {
		if participant.ID == s.initiator.String() {
			continue
		}

		if participant.Mode == string(types.SessionModeratorMode) && time.Now().UTC().After(participant.LastActive.Add(PresenceMaxDifference)) {
			s.log.Debugf("Participant %v is not active, kicking.", participant.ID)
			id, _ := uuid.Parse(participant.ID)
			err := s.leave(id)
			if err != nil {
				s.log.WithError(err).Warnf("Failed to kick participant %v for inactivity.", participant.ID)
			}
		}
	}

	return nil
}

// launch waits until the session meets access requirements and then transitions the session
// to a running state.
func (s *session) launch() error {
	defer func() {
		err := s.Close()
		if err != nil {
			s.log.WithError(err).Errorf("Failed to close session: %v", s.id)
		}
	}()

	go func() {
		select {
		case <-time.After(time.Until(s.expires)):
			s.mu.Lock()
			defer s.mu.Unlock()
			s.io.BroadcastMessage("Session expired, closing...")

			err := s.Close()
			if err != nil {
				s.log.WithError(err).Error("Failed to close session")
			}
		case <-s.closeC:
		}
	}()

	// If the identity is verified with an MFA device, we enabled MFA-based presence for the session.
	if s.PresenceEnabled {
		go func() {
			ticker := time.NewTicker(PresenceVerifyInterval)
		outer:
			for {
				select {
				case <-ticker.C:
					err := s.checkPresence()
					if err != nil {
						s.log.WithError(err).Error("Failed to check presence, closing session as a security measure")
						err := s.Close()
						if err != nil {
							s.log.WithError(err).Error("Failed to close session")
						}
					}
				case <-s.closeC:
					break outer
				}
			}
		}()
	}

	s.log.Debugf("Launching session: %v", s.id)
	s.mu.Lock()

	s.io.BroadcastMessage("Launching session...")
	q := s.req.URL.Query()
	request := remoteCommandRequest{
		podNamespace:       s.params.ByName("podNamespace"),
		podName:            s.params.ByName("podName"),
		containerName:      q.Get("container"),
		cmd:                q["command"],
		stdin:              utils.AsBool(q.Get("stdin")),
		stdout:             utils.AsBool(q.Get("stdout")),
		stderr:             utils.AsBool(q.Get("stderr")),
		tty:                utils.AsBool(q.Get("tty")),
		httpRequest:        s.req,
		httpResponseWriter: nil,
		context:            s.req.Context(),
		pingPeriod:         s.forwarder.cfg.ConnPingPeriod,
	}

	s.podName = request.podName
	err := s.trackerUpdateState(types.SessionState_SessionStateRunning)
	if err != nil {
		return trace.Wrap(err)
	}

	s.started = true
	sessionStart := s.forwarder.cfg.Clock.Now().UTC()
	if err != nil {
		s.log.Errorf("Failed to create cluster session: %v.", err)
		return trace.Wrap(err)
	}

	eventPodMeta := request.eventPodMeta(request.context, s.sess.creds)
	s.io.OnWriteError = func(idString string, err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.log.Errorf("Encountered error: %v with party %v. Disconnecting them from the session.", err, idString)
		id, _ := uuid.Parse(idString)
		if s.parties[id] != nil {
			err = s.leave(id)
			if err != nil {
				s.log.Errorf("Failed to disconnect party %v from the session: %v.", idString, err)
			}
		}
	}

	if !s.sess.noAuditEvents && s.tty {
		s.terminalSizeQueue.callback = func(resize *remotecommand.TerminalSize) {
			s.mu.Lock()
			defer s.mu.Unlock()

			for id, p := range s.parties {
				err := p.Client.resize(resize)
				if err != nil {
					s.log.WithError(err).Errorf("Failed to resize client: %v", id.String())
				}
			}

			params := tsession.TerminalParams{
				W: int(resize.Width),
				H: int(resize.Height),
			}

			resizeEvent := &apievents.Resize{
				Metadata: apievents.Metadata{
					Type:        events.ResizeEvent,
					Code:        events.TerminalResizeCode,
					ClusterName: s.forwarder.cfg.ClusterName,
				},
				ConnectionMetadata: apievents.ConnectionMetadata{
					RemoteAddr: s.req.RemoteAddr,
					Protocol:   events.EventProtocolKube,
				},
				ServerMetadata: apievents.ServerMetadata{
					ServerNamespace: s.forwarder.cfg.Namespace,
				},
				SessionMetadata: apievents.SessionMetadata{
					SessionID: s.id.String(),
					WithMFA:   s.ctx.Identity.GetIdentity().MFAVerified,
				},
				UserMetadata: apievents.UserMetadata{
					User:         s.ctx.User.GetName(),
					Login:        s.ctx.User.GetName(),
					Impersonator: s.ctx.Identity.GetIdentity().Impersonator,
				},
				TerminalSize:              params.Serialize(),
				KubernetesClusterMetadata: s.ctx.eventClusterMeta(),
				KubernetesPodMetadata:     eventPodMeta,
			}

			// Report the updated window size to the event log (this is so the sessions
			// can be replayed correctly).
			if err := s.recorder.EmitAuditEvent(s.forwarder.ctx, resizeEvent); err != nil {
				s.forwarder.log.WithError(err).Warn("Failed to emit terminal resize event.")
			}
		}
	} else {
		s.terminalSizeQueue.callback = func(resize *remotecommand.TerminalSize) {}
	}

	if !s.sess.noAuditEvents && request.tty {
		streamer, err := s.forwarder.newStreamer(&s.ctx)
		if err != nil {
			return trace.Wrap(err)
		}

		recorder, err := events.NewAuditWriter(events.AuditWriterConfig{
			// Audit stream is using server context, not session context,
			// to make sure that session is uploaded even after it is closed
			Context:      s.forwarder.ctx,
			Streamer:     streamer,
			Clock:        s.forwarder.cfg.Clock,
			SessionID:    tsession.ID(s.id.String()),
			ServerID:     s.forwarder.cfg.ServerID,
			Namespace:    s.forwarder.cfg.Namespace,
			RecordOutput: s.ctx.recordingConfig.GetMode() != types.RecordOff,
			Component:    teleport.Component(teleport.ComponentSession, teleport.ComponentProxyKube),
			ClusterName:  s.forwarder.cfg.ClusterName,
		})

		s.recorder = recorder
		s.emitter = recorder
		if err != nil {
			return trace.Wrap(err)
		}

		s.io.AddWriter("recorder", recorder)
	} else if !s.sess.noAuditEvents {
		s.emitter = s.forwarder.cfg.StreamEmitter
	}

	if request.tty {
		termParams := tsession.TerminalParams{
			W: 100,
			H: 100,
		}

		sessionStartEvent := &apievents.SessionStart{
			Metadata: apievents.Metadata{
				Type:        events.SessionStartEvent,
				Code:        events.SessionStartCode,
				ClusterName: s.forwarder.cfg.ClusterName,
			},
			ServerMetadata: apievents.ServerMetadata{
				ServerID:        s.forwarder.cfg.ServerID,
				ServerNamespace: s.forwarder.cfg.Namespace,
				ServerHostname:  s.sess.teleportCluster.name,
				ServerAddr:      s.sess.kubeAddress,
			},
			SessionMetadata: apievents.SessionMetadata{
				SessionID: s.id.String(),
				WithMFA:   s.ctx.Identity.GetIdentity().MFAVerified,
			},
			UserMetadata: apievents.UserMetadata{
				User:         s.ctx.User.GetName(),
				Login:        s.ctx.User.GetName(),
				Impersonator: s.ctx.Identity.GetIdentity().Impersonator,
			},
			ConnectionMetadata: apievents.ConnectionMetadata{
				RemoteAddr: s.req.RemoteAddr,
				LocalAddr:  s.sess.kubeAddress,
				Protocol:   events.EventProtocolKube,
			},
			TerminalSize:              termParams.Serialize(),
			KubernetesClusterMetadata: s.ctx.eventClusterMeta(),
			KubernetesPodMetadata:     eventPodMeta,
			InitialCommand:            q["command"],
			SessionRecording:          s.ctx.recordingConfig.GetMode(),
		}

		if err := s.emitter.EmitAuditEvent(s.forwarder.ctx, sessionStartEvent); err != nil {
			s.forwarder.log.WithError(err).Warn("Failed to emit event.")
		}
	}

	executor, err := s.forwarder.getExecutor(s.ctx, s.sess, s.req)
	if err != nil {
		s.log.WithError(err).Warning("Failed creating executor.")
		return trace.Wrap(err)
	}

	defer func() {
		for _, party := range s.parties {
			if err := party.Client.sendStatus(err); err != nil {
				s.forwarder.log.WithError(err).Warning("Failed to send status. Exec command was aborted by client.")
			}
		}

		if request.tty {
			sessionDataEvent := &apievents.SessionData{
				Metadata: apievents.Metadata{
					Type:        events.SessionDataEvent,
					Code:        events.SessionDataCode,
					ClusterName: s.forwarder.cfg.ClusterName,
				},
				ServerMetadata: apievents.ServerMetadata{
					ServerID:        s.forwarder.cfg.ServerID,
					ServerNamespace: s.forwarder.cfg.Namespace,
				},
				SessionMetadata: apievents.SessionMetadata{
					SessionID: s.id.String(),
					WithMFA:   s.ctx.Identity.GetIdentity().MFAVerified,
				},
				UserMetadata: apievents.UserMetadata{
					User:         s.ctx.User.GetName(),
					Login:        s.ctx.User.GetName(),
					Impersonator: s.ctx.Identity.GetIdentity().Impersonator,
				},
				ConnectionMetadata: apievents.ConnectionMetadata{
					RemoteAddr: s.req.RemoteAddr,
					LocalAddr:  s.sess.kubeAddress,
					Protocol:   events.EventProtocolKube,
				},
				// Bytes transmitted from user to pod.
				BytesTransmitted: s.io.CountRead(),
				// Bytes received from pod by user.
				BytesReceived: s.io.CountWritten(),
			}

			if err := s.emitter.EmitAuditEvent(s.forwarder.ctx, sessionDataEvent); err != nil {
				s.forwarder.log.WithError(err).Warn("Failed to emit session data event.")
			}

			sessionEndEvent := &apievents.SessionEnd{
				Metadata: apievents.Metadata{
					Type:        events.SessionEndEvent,
					Code:        events.SessionEndCode,
					ClusterName: s.forwarder.cfg.ClusterName,
				},
				ServerMetadata: apievents.ServerMetadata{
					ServerID:        s.forwarder.cfg.ServerID,
					ServerNamespace: s.forwarder.cfg.Namespace,
				},
				SessionMetadata: apievents.SessionMetadata{
					SessionID: s.id.String(),
					WithMFA:   s.ctx.Identity.GetIdentity().MFAVerified,
				},
				UserMetadata: apievents.UserMetadata{
					User:         s.ctx.User.GetName(),
					Login:        s.ctx.User.GetName(),
					Impersonator: s.ctx.Identity.GetIdentity().Impersonator,
				},
				ConnectionMetadata: apievents.ConnectionMetadata{
					RemoteAddr: s.req.RemoteAddr,
					LocalAddr:  s.sess.kubeAddress,
					Protocol:   events.EventProtocolKube,
				},
				Interactive:               true,
				Participants:              s.allParticipants(),
				StartTime:                 sessionStart,
				EndTime:                   s.forwarder.cfg.Clock.Now().UTC(),
				KubernetesClusterMetadata: s.ctx.eventClusterMeta(),
				KubernetesPodMetadata:     eventPodMeta,
				InitialCommand:            request.cmd,
				SessionRecording:          s.ctx.recordingConfig.GetMode(),
			}

			if err := s.emitter.EmitAuditEvent(s.forwarder.ctx, sessionEndEvent); err != nil {
				s.forwarder.log.WithError(err).Warn("Failed to emit session end event.")
			}
		} else {
			// send an exec event
			execEvent := &apievents.Exec{
				Metadata: apievents.Metadata{
					Type:        events.ExecEvent,
					ClusterName: s.forwarder.cfg.ClusterName,
				},
				ServerMetadata: apievents.ServerMetadata{
					ServerID:        s.forwarder.cfg.ServerID,
					ServerNamespace: s.forwarder.cfg.Namespace,
				},
				SessionMetadata: apievents.SessionMetadata{
					SessionID: s.id.String(),
					WithMFA:   s.ctx.Identity.GetIdentity().MFAVerified,
				},
				UserMetadata: apievents.UserMetadata{
					User:         s.ctx.User.GetName(),
					Login:        s.ctx.User.GetName(),
					Impersonator: s.ctx.Identity.GetIdentity().Impersonator,
				},
				ConnectionMetadata: apievents.ConnectionMetadata{
					RemoteAddr: s.req.RemoteAddr,
					LocalAddr:  s.sess.kubeAddress,
					Protocol:   events.EventProtocolKube,
				},
				CommandMetadata: apievents.CommandMetadata{
					Command: strings.Join(request.cmd, " "),
				},
				KubernetesClusterMetadata: s.ctx.eventClusterMeta(),
				KubernetesPodMetadata:     eventPodMeta,
			}

			if err != nil {
				execEvent.Code = events.ExecFailureCode
				execEvent.Error = err.Error()
				if exitErr, ok := err.(utilexec.ExitError); ok && exitErr.Exited() {
					execEvent.ExitCode = fmt.Sprintf("%d", exitErr.ExitStatus())
				}
			} else {
				execEvent.Code = events.ExecCode
			}

			if err := s.emitter.EmitAuditEvent(s.forwarder.ctx, execEvent); err != nil {
				s.forwarder.log.WithError(err).Warn("Failed to emit event.")
			}
		}
	}()

	options := remotecommand.StreamOptions{
		Stdin:             s.io,
		Stdout:            s.io,
		Stderr:            s.io,
		Tty:               request.tty,
		TerminalSizeQueue: s.terminalSizeQueue,
	}

	s.mu.Unlock()
	if err = executor.Stream(options); err != nil {
		s.log.WithError(err).Warning("Executor failed while streaming.")
		return trace.Wrap(err)
	}

	return nil
}

// join attempts to connect a party to the session.
func (s *session) join(p *party) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if p.Ctx.User.GetName() != s.ctx.User.GetName() {
		roleNames := p.Ctx.Identity.GetIdentity().Groups
		roles, err := getRolesByName(s.forwarder, roleNames)
		if err != nil {
			return trace.Wrap(err)
		}

		accessContext := auth.SessionAccessContext{
			Username: p.Ctx.User.GetName(),
			Roles:    roles,
		}

		modes, err := s.accessEvaluator.CanJoin(accessContext)
		if err != nil {
			return trace.Wrap(err)
		}

		if !auth.SliceContainsMode(modes, p.Mode) {
			return trace.AccessDenied("insufficient permissions to join session")
		}
	}

	stringID := p.ID.String()
	s.parties[p.ID] = p
	s.partiesHistorical[p.ID] = p

	err := s.trackerAddParticipant(p)
	if err != nil {
		return trace.Wrap(err)
	}

	sessionJoinEvent := &apievents.SessionJoin{
		Metadata: apievents.Metadata{
			Type:        events.SessionJoinEvent,
			Code:        events.SessionJoinCode,
			ClusterName: s.ctx.teleportCluster.name,
		},
		KubernetesClusterMetadata: apievents.KubernetesClusterMetadata{
			KubernetesCluster: s.ctx.kubeCluster,
			KubernetesUsers:   []string{},
			KubernetesGroups:  []string{},
		},
		SessionMetadata: apievents.SessionMetadata{
			SessionID: s.id.String(),
		},
		UserMetadata: apievents.UserMetadata{
			User:         p.Ctx.User.GetName(),
			Login:        "root",
			Impersonator: p.Ctx.Identity.GetIdentity().Impersonator,
		},
		ConnectionMetadata: apievents.ConnectionMetadata{
			RemoteAddr: s.params.ByName("podName"),
		},
	}

	if err := s.emitter.EmitAuditEvent(s.forwarder.ctx, sessionJoinEvent); err != nil {
		s.forwarder.log.WithError(err).Warn("Failed to emit event.")
	}

	s.io.BroadcastMessage(fmt.Sprintf("User %v joined the session.", p.Ctx.User.GetName()))

	if s.tty {
		s.terminalSizeQueue.add(stringID, p.Client.resizeQueue())
	}

	if s.tty && p.Ctx.User.GetName() == s.ctx.User.GetName() {
		s.io.AddReader(stringID, p.Client.stdinStream())
	}

	recentWrites := s.io.GetRecentHistory()
	_, err = p.Client.stdoutStream().Write(recentWrites)
	if err != nil {
		return trace.Wrap(err)
	}

	s.io.AddWriter(stringID, p.Client.stdoutStream())
	if p.Mode != types.SessionObserverMode {
		go func() {
			c := p.Client.forceTerminate()
			select {
			case <-c:
				go func() {
					s.log.Debugf("Received force termination request")
					err := s.Close()
					if err != nil {
						s.log.Errorf("Failed to close session: %v.", err)
					}
				}()
			case <-s.closeC:
				return
			}
		}()
	}

	if s.state != types.SessionState_SessionStatePending {
		return nil
	}

	canStart, _, err := s.canStart()
	if err != nil {
		return trace.Wrap(err)
	}

	if s.started && canStart {
		s.state = types.SessionState_SessionStateRunning
		s.stateUpdate.Submit(types.SessionState_SessionStateRunning)
		return nil
	}

	if canStart {
		go func() {
			if err := s.launch(); err != nil {
				s.log.WithError(err).Warning("Failed to launch Kubernetes session.")
			}
		}()
	} else if !s.tty {
		return trace.AccessDenied("insufficient permissions to launch non-interactive session")
	}

	if !canStart {
		s.io.BroadcastMessage("Waiting for required participants...")
	}

	return nil
}

// leave removes a party from the session.
func (s *session) leave(id uuid.UUID) error {
	if s.state == types.SessionState_SessionStateTerminated {
		return nil
	}

	stringID := id.String()
	party := s.parties[id]

	if party == nil {
		return nil
	}

	delete(s.parties, id)
	s.terminalSizeQueue.remove(stringID)
	s.io.DeleteReader(stringID)
	s.io.DeleteWriter(stringID)
	s.io.BroadcastMessage(fmt.Sprintf("User %v left the session.", party.Ctx.User.GetName()))

	sessionLeaveEvent := &apievents.SessionLeave{
		Metadata: apievents.Metadata{
			Type:        events.SessionJoinEvent,
			Code:        events.SessionJoinCode,
			ClusterName: s.ctx.teleportCluster.name,
		},
		SessionMetadata: apievents.SessionMetadata{
			SessionID: s.id.String(),
		},
		UserMetadata: apievents.UserMetadata{
			User:         party.Ctx.User.GetName(),
			Login:        "root",
			Impersonator: party.Ctx.Identity.GetIdentity().Impersonator,
		},
		ConnectionMetadata: apievents.ConnectionMetadata{
			RemoteAddr: s.params.ByName("podName"),
		},
	}

	if err := s.emitter.EmitAuditEvent(s.forwarder.ctx, sessionLeaveEvent); err != nil {
		s.forwarder.log.WithError(err).Warn("Failed to emit event.")
	}

	err := s.trackerRemoveParticipant(party.ID.String())
	if err != nil {
		return trace.Wrap(err)
	}

	err = party.Close()
	if err != nil {
		s.log.WithError(err).Error("Error closing party")
		return trace.Wrap(err)
	}

	if len(s.parties) == 0 || id == s.initiator {
		go func() {
			err := s.Close()
			if err != nil {
				s.log.WithError(err).Errorf("Failed to close session")
			}
		}()

		return nil
	}

	canStart, options, err := s.canStart()
	if err != nil {
		return trace.Wrap(err)
	}

	if !canStart {
		if options.TerminateOnLeave {
			go func() {
				err := s.Close()
				if err != nil {
					s.log.WithError(err).Errorf("Failed to close session")
				}
			}()
		} else {
			s.state = types.SessionState_SessionStatePending
			s.stateUpdate.Submit(types.SessionState_SessionStatePending)
			go s.waitOnAccess()
		}
	}

	return nil
}

// allParticipants returns a list of all historical participants of the session.
func (s *session) allParticipants() []string {
	var participants []string
	for _, p := range s.partiesHistorical {
		participants = append(participants, p.Ctx.User.GetName())
	}

	return participants
}

// canStart checks if a session can start with the current set of participants.
func (s *session) canStart() (bool, auth.PolicyOptions, error) {
	var participants []auth.SessionAccessContext
	for _, party := range s.parties {
		if party.Ctx.User.GetName() == s.ctx.User.GetName() {
			continue
		}

		roleNames := party.Ctx.Identity.GetIdentity().Groups
		roles, err := getRolesByName(s.forwarder, roleNames)
		if err != nil {
			return false, auth.PolicyOptions{}, trace.Wrap(err)
		}

		participants = append(participants, auth.SessionAccessContext{Roles: roles})
	}

	yes, options, err := s.accessEvaluator.FulfilledFor(participants)
	return yes, options, trace.Wrap(err)
}

// Close terminates a session and disconnects all participants.
func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closeOnce.Do(func() {
		s.io.BroadcastMessage("Closing session...")
		s.state = types.SessionState_SessionStateTerminated
		s.io.Close()
		s.stateUpdate.Submit(types.SessionState_SessionStateTerminated)
		s.stateUpdate.Close()
		err := s.trackerUpdateState(types.SessionState_SessionStateTerminated)
		if err != nil {
			s.log.WithError(err).Error("Failed to mark session tracker as terminated.")
		}

		s.log.Debugf("Closing session %v.", s.id.String())
		close(s.closeC)
		for id, party := range s.parties {
			err = party.Close()
			if err != nil {
				s.log.WithError(err).Errorf("Failed to disconnect party %v", id.String())
			}
		}

		if s.recorder != nil {
			s.recorder.Close(s.forwarder.ctx)
		}
	})

	return nil
}

func getRolesByName(forwarder *Forwarder, roleNames []string) ([]types.Role, error) {
	var roles []types.Role

	for _, roleName := range roleNames {
		role, err := forwarder.cfg.CachingAuthClient.GetRole(context.TODO(), roleName)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		roles = append(roles, role)
	}

	return roles, nil
}

func (s *session) trackerGet() (types.SessionTracker, error) {
	sess, err := s.forwarder.cfg.AuthClient.GetSessionTracker(s.forwarder.ctx, s.id.String())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return sess, nil
}

func (s *session) trackerCreate(p *party, hostRoles []types.Role) error {
	initator := &types.Participant{
		ID:         p.ID.String(),
		User:       p.Ctx.User.GetName(),
		LastActive: time.Now().UTC(),
	}

	hostRolesV5 := make([]*types.RoleV5, len(hostRoles))
	for _, role := range hostRoles {
		roleV5, ok := role.(*types.RoleV5)
		if !ok {
			return trace.BadParameter("Found unexpected role structure: %T", role)
		}

		hostRolesV5 = append(hostRolesV5, roleV5)
	}

	req := &proto.CreateSessionTrackerRequest{
		ID:                s.id.String(),
		Namespace:         defaults.Namespace,
		Type:              string(types.KubernetesSessionKind),
		Hostname:          s.podName,
		ClusterName:       s.ctx.teleportCluster.name,
		Login:             "root",
		Initiator:         initator,
		Expires:           s.expires,
		KubernetesCluster: s.ctx.kubeCluster,
		HostUser:          initator.User,
		HostRoles:         hostRolesV5,
	}

	_, err := s.forwarder.cfg.AuthClient.CreateSessionTracker(s.forwarder.ctx, req)
	return trace.Wrap(err)
}

func (s *session) trackerAddParticipant(participant *party) error {
	s.log.Debugf("Tracking participant: %v", participant.ID.String())
	req := &proto.UpdateSessionTrackerRequest{
		SessionID: s.id.String(),
		Update: &proto.UpdateSessionTrackerRequest_AddParticipant{
			AddParticipant: &proto.SessionTrackerAddParticipant{
				Participant: &types.Participant{
					ID:         participant.ID.String(),
					User:       participant.Ctx.User.GetName(),
					Mode:       string(participant.Mode),
					LastActive: time.Now().UTC(),
				},
			},
		},
	}

	err := s.forwarder.cfg.AuthClient.UpdateSessionTracker(s.forwarder.ctx, req)
	return trace.Wrap(err)
}

func (s *session) trackerRemoveParticipant(participantID string) error {
	s.log.Debugf("Not tracking participant: %v", participantID)
	req := &proto.UpdateSessionTrackerRequest{
		SessionID: s.id.String(),
		Update: &proto.UpdateSessionTrackerRequest_RemoveParticipant{
			RemoveParticipant: &proto.SessionTrackerRemoveParticipant{
				ParticipantID: participantID,
			},
		},
	}

	err := s.forwarder.cfg.AuthClient.UpdateSessionTracker(s.forwarder.ctx, req)
	return trace.Wrap(err)
}

func (s *session) trackerUpdateState(state types.SessionState) error {
	req := &proto.UpdateSessionTrackerRequest{
		SessionID: s.id.String(),
		Update: &proto.UpdateSessionTrackerRequest_UpdateState{
			UpdateState: &proto.SessionTrackerUpdateState{
				State: state,
			},
		},
	}

	err := s.forwarder.cfg.AuthClient.UpdateSessionTracker(s.forwarder.ctx, req)
	return trace.Wrap(err)
}
