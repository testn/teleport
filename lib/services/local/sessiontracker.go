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

package local

import (
	"context"
	"time"

	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/backend"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
	log "github.com/sirupsen/logrus"
)

const (
	sessionPrefix               = "session_tracker"
	sessionList                 = "list"
	gcDelay       time.Duration = time.Minute * 5
	retryDelay    time.Duration = time.Second
)

type sessionTracker struct {
	bk backend.Backend
}

func NewSessionTrackerService(bk backend.Backend) (services.SessionTrackerService, error) {
	_, err := bk.Get(context.TODO(), backend.Key(sessionPrefix, sessionList))
	if trace.IsNotFound(err) {
		err := createList(bk)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	} else if err != nil {
		return nil, trace.Wrap(err)
	}

	return &sessionTracker{bk}, nil
}

func createList(bk backend.Backend) error {
	data := []byte("[]")
	_, err := bk.Create(context.TODO(), backend.Item{Key: backend.Key(sessionPrefix, sessionList), Value: data})
	if err != nil {
		return err
	}

	return nil
}

func (s *sessionTracker) loadSession(ctx context.Context, sessionID string) (types.SessionTracker, error) {
	sessionJSON, err := s.bk.Get(ctx, backend.Key(sessionPrefix, sessionID))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	session, err := unmarshalSession(sessionJSON.Value)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return session, nil
}

// UpdatePresence updates the presence status of a user in a session.
func (s *sessionTracker) UpdatePresence(ctx context.Context, sessionID, user string) error {
	sessionItem, err := s.bk.Get(ctx, backend.Key(sessionPrefix, sessionID))
	if err != nil {
		return trace.Wrap(err)
	}

	session, err := unmarshalSession(sessionItem.Value)
	if err != nil {
		return trace.Wrap(err)
	}

	session.UpdatePresence(user)

	sessionJSON, err := marshalSession(session)
	if err != nil {
		return trace.Wrap(err)
	}

	item := backend.Item{Key: backend.Key(sessionPrefix, sessionID), Value: sessionJSON}
	_, err = s.bk.CompareAndSwap(ctx, *sessionItem, item)
	if trace.IsCompareFailed(err) {
		log.Infof("Session resource %v presence update failed, retrying: %v", sessionID, err)
		time.Sleep(retryDelay)
		return s.UpdatePresence(ctx, sessionID, user)
	}

	return trace.Wrap(err)
}

// GetSessionTracker returns the current state of a session tracker for an active session.
func (s *sessionTracker) GetSessionTracker(ctx context.Context, sessionID string) (types.SessionTracker, error) {
	session, err := s.loadSession(ctx, sessionID)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return session, nil
}

// GetActiveSessionTrackers returns a list of active session trackers.
func (s *sessionTracker) GetActiveSessionTrackers(ctx context.Context) ([]types.SessionTracker, error) {
	sessionList, err := s.getSessionList(ctx)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	sessions := make([]types.SessionTracker, len(sessionList))
	for i, sessionID := range sessionList {
		session, err := s.loadSession(ctx, sessionID)
		if err != nil {
			return nil, trace.Wrap(err)
		}

		sessions[i] = session
	}

	return sessions, nil
}

// CreateSessionTracker creates a tracker resource for an active session.
func (s *sessionTracker) CreateSessionTracker(ctx context.Context, req *proto.CreateSessionTrackerRequest) (types.SessionTracker, error) {
	now := time.Now().UTC()

	spec := types.SessionTrackerSpecV1{
		SessionID:         req.ID,
		Kind:              req.Type,
		State:             types.SessionState_SessionStatePending,
		Created:           now,
		Reason:            req.Reason,
		Invited:           req.Invited,
		Hostname:          req.Hostname,
		Address:           req.Address,
		ClusterName:       req.ClusterName,
		Login:             req.Login,
		Participants:      []types.Participant{*req.Initiator},
		Expires:           req.Expires,
		KubernetesCluster: req.KubernetesCluster,
		HostUser:          req.HostUser,
	}

	session, err := types.NewSessionTracker(spec)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	json, err := marshalSession(session)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	err = s.addSessionToList(ctx, session.GetSessionID())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	item := backend.Item{Key: backend.Key(sessionPrefix, session.GetSessionID()), Value: json}
	_, err = s.bk.Create(ctx, item)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return session, nil
}

// UpdateSessionTracker updates a tracker resource for an active session.
func (s *sessionTracker) UpdateSessionTracker(ctx context.Context, req *proto.UpdateSessionTrackerRequest) error {
	sessionItem, err := s.bk.Get(ctx, backend.Key(sessionPrefix, req.SessionID))
	if err != nil {
		return trace.Wrap(err)
	}

	session, err := unmarshalSession(sessionItem.Value)
	if err != nil {
		return trace.Wrap(err)
	}

	switch session := session.(type) {
	case *types.SessionTrackerV1:
		switch update := req.Update.(type) {
		case *proto.UpdateSessionTrackerRequest_UpdateState:
			session.SetState(update.UpdateState.State)
		case *proto.UpdateSessionTrackerRequest_AddParticipant:
			session.AddParticipant(*update.AddParticipant.Participant)
		case *proto.UpdateSessionTrackerRequest_RemoveParticipant:
			session.RemoveParticipant(update.RemoveParticipant.ParticipantID)
		}
	default:
		return trace.BadParameter("unrecognized session version %T", session)
	}

	sessionJSON, err := marshalSession(session)
	if err != nil {
		return trace.Wrap(err)
	}

	item := backend.Item{Key: backend.Key(sessionPrefix, req.SessionID), Value: sessionJSON}
	_, err = s.bk.CompareAndSwap(ctx, *sessionItem, item)
	if trace.IsCompareFailed(err) {
		time.Sleep(retryDelay)
		return s.UpdateSessionTracker(ctx, req)
	}

	return trace.Wrap(err)
}

// RemoveSessionTracker removes a tracker resource for an active session.
func (s *sessionTracker) RemoveSessionTracker(ctx context.Context, sessionID string) error {
	err := s.removeSessionFromList(ctx, sessionID)
	if err != nil {
		return trace.Wrap(err)
	}

	return trace.Wrap(s.bk.Delete(ctx, backend.Key(sessionPrefix, sessionID)))
}

func (s *sessionTracker) addSessionToList(ctx context.Context, sessionID string) error {
	listItem, err := s.bk.Get(ctx, backend.Key(sessionPrefix, sessionList))
	if err != nil {
		return trace.Wrap(err)
	}

	var list []string
	err = utils.FastUnmarshal(listItem.Value, &list)
	if err != nil {
		return trace.Wrap(err)
	}

	list = append(list, sessionID)
	listJSON, err := utils.FastMarshal(list)
	if err != nil {
		return trace.Wrap(err)
	}

	newListItem := backend.Item{Key: backend.Key(sessionPrefix, sessionList), Value: listJSON}
	_, err = s.bk.CompareAndSwap(ctx, *listItem, newListItem)
	return trace.Wrap(err)
}

func (s *sessionTracker) removeSessionFromList(ctx context.Context, sessionID string) error {
	listItem, err := s.bk.Get(ctx, backend.Key(sessionPrefix, sessionList))
	if err != nil {
		return trace.Wrap(err)
	}

	var list []string
	err = utils.FastUnmarshal(listItem.Value, &list)
	if err != nil {
		return trace.Wrap(err)
	}

	found := false
	for i, id := range list {
		session, err := s.loadSession(ctx, id)
		if err != nil {
			return trace.Wrap(err)
		}

		doGC := session.GetCreated().Add(gcDelay).Before(time.Now().UTC()) && session.GetState() == types.SessionState_SessionStateTerminated
		isStale := session.GetExpires().Before(time.Now().UTC())
		if id == sessionID || doGC || isStale {
			list = append(list[:i], list[i+1:]...)
			found = true
			break
		}

		if doGC {
			err := s.RemoveSessionTracker(ctx, id)
			if err != nil {
				return trace.Wrap(err)
			}
		}
	}

	if !found {
		return trace.NotFound("session %v not found in list", sessionID)
	}

	listJSON, err := utils.FastMarshal(list)
	if err != nil {
		return trace.Wrap(err)
	}

	newListItem := backend.Item{Key: backend.Key(sessionPrefix, sessionList), Value: listJSON}
	_, err = s.bk.CompareAndSwap(ctx, *listItem, newListItem)
	if trace.IsCompareFailed(err) {
		time.Sleep(retryDelay)
		return s.removeSessionFromList(ctx, sessionID)
	}

	return trace.Wrap(err)
}

func (s *sessionTracker) getSessionList(ctx context.Context) ([]string, error) {
	listItem, err := s.bk.Get(ctx, backend.Key(sessionPrefix, sessionList))
	if err != nil {
		return nil, trace.Wrap(err)
	}

	var list []string
	err = utils.FastUnmarshal(listItem.Value, &list)
	return list, trace.Wrap(err)
}

// unmarshalSession unmarshals the Session resource from JSON.
func unmarshalSession(bytes []byte, opts ...services.MarshalOption) (types.SessionTracker, error) {
	var session types.SessionTrackerV1

	if len(bytes) == 0 {
		return nil, trace.BadParameter("missing resource data")
	}

	cfg, err := services.CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if err := utils.FastUnmarshal(bytes, &session); err != nil {
		return nil, trace.BadParameter(err.Error())
	}

	if err := session.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	if cfg.ID != 0 {
		session.SetResourceID(cfg.ID)
	}

	if !cfg.Expires.IsZero() {
		session.SetExpiry(cfg.Expires)
	}

	return &session, nil
}

// marshalSession marshals the Session resource to JSON.
func marshalSession(session types.SessionTracker, opts ...services.MarshalOption) ([]byte, error) {
	if err := session.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}

	cfg, err := services.CollectOptions(opts)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	switch session := session.(type) {
	case *types.SessionTrackerV1:
		if !cfg.PreserveResourceID {
			copy := *session
			copy.SetResourceID(0)
			session = &copy
		}
		return utils.FastMarshal(session)
	default:
		return nil, trace.BadParameter("unrecognized session version %T", session)
	}
}
