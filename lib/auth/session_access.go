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

package auth

import (
	"regexp"
	"strings"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
	"github.com/vulcand/predicate"
)

// SessionAccessEvaluator takes a set of policies
// and uses rules to evaluate them to determine when a session may start
// and if a user can join a session.
//
// The current implementation is very simple and uses a brute-force algorithm.
// More efficient implementations that run in non O(n^2)-ish time a possible but require complex
// and non intuitive code.
// In the real world, the number of roles and session are small enough that this doesn't have a meaningful impact.
type SessionAccessEvaluator struct {
	kind     types.SessionKind
	requires []*types.SessionRequirePolicy
	roles    []types.Role
}

// NewSessionAccessEvaluator creates a new session access evaluator for a given session kind
// and a set of roles attached to the host user.
func NewSessionAccessEvaluator(roles []types.Role, kind types.SessionKind) SessionAccessEvaluator {
	requires := getRequirePolicies(roles)

	return SessionAccessEvaluator{
		kind,
		requires,
		roles,
	}
}

func getRequirePolicies(participant []types.Role) []*types.SessionRequirePolicy {
	var policies []*types.SessionRequirePolicy

	for _, role := range participant {
		policiesFromRole := role.GetSessionRequirePolicies()
		if len(policiesFromRole) == 0 {
			continue
		}

		policies = append(policies, policiesFromRole...)
	}

	return policies
}

func getAllowPolicies(participant SessionAccessContext) []*types.SessionJoinPolicy {
	var policies []*types.SessionJoinPolicy

	for _, role := range participant.Roles {
		policies = append(policies, role.GetSessionJoinPolicies()...)
	}

	return policies
}

func contains(s []string, e types.SessionKind) bool {
	for _, a := range s {
		if types.SessionKind(a) == e {
			return true
		}
	}

	return false
}

// SessionAccessContext is the context that must be provided per participant in the session.
type SessionAccessContext struct {
	Username string
	Roles    []types.Role
}

// GetIdentifier is used by the `predicate` library to evaluate variable expressions when
// evaluating policy filters. It deals with evaluating strings like `participant.name` to the appropriate value.
func (ctx *SessionAccessContext) GetIdentifier(fields []string) (interface{}, error) {
	if fields[0] == "participant" {
		if len(fields) == 2 || len(fields) == 3 {
			switch fields[1] {
			case "name":
				return ctx.Username, nil
			case "roles":
				var roles []string
				for _, role := range ctx.Roles {
					roles = append(roles, role.GetName())
				}

				return roles, nil
			}
		}
	}

	return nil, trace.NotFound("%v is not defined", strings.Join(fields, "."))
}

func (ctx *SessionAccessContext) GetResource() (types.Resource, error) {
	return nil, trace.BadParameter("resource unsupported")
}

func (e *SessionAccessEvaluator) matchesPredicate(ctx *SessionAccessContext, require *types.SessionRequirePolicy, allow *types.SessionJoinPolicy) (bool, error) {
	if !e.matchesKind(require.Kinds) || !e.matchesKind(allow.Kinds) {
		return false, nil
	}

	parser, err := services.NewWhereParser(ctx)
	if err != nil {
		return false, trace.Wrap(err)
	}

	ifn, err := parser.Parse(require.Filter)
	if err != nil {
		return false, trace.Wrap(err)
	}

	fn, ok := ifn.(predicate.BoolPredicate)
	if !ok {
		return false, trace.BadParameter("unsupported type: %T", ifn)
	}

	return fn(), nil
}

func (e *SessionAccessEvaluator) matchesJoin(allow *types.SessionJoinPolicy) bool {
	if !e.matchesKind(allow.Kinds) {
		return false
	}

	for _, requireRole := range e.roles {
		for _, allowRole := range allow.Roles {
			expr := utils.GlobToRegexp(requireRole.GetName())
			// GlobToRegexp makes sure this is always a valid regexp.
			matched, _ := regexp.MatchString(expr, allowRole)

			if matched {
				return true
			}
		}
	}

	return false
}

func (e *SessionAccessEvaluator) matchesKind(allow []string) bool {
	if contains(allow, e.kind) || contains(allow, "*") {
		return true
	}

	return false
}

// CanJoin returns the modes a user has access to join a session with.
// If the list is empty, the user doesn't have access to join the session at all.
func (e *SessionAccessEvaluator) CanJoin(user SessionAccessContext) ([]types.SessionParticipantMode, error) {
	supported, err := e.supportsSessionAccessControls()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if !supported {
		return preAccessControlsModes(e.kind), nil
	}

	var modes []types.SessionParticipantMode

	for _, allowPolicy := range getAllowPolicies(user) {
		if e.matchesJoin(allowPolicy) {
			for _, modeString := range allowPolicy.Modes {
				mode := types.SessionParticipantMode(modeString)
				if !SliceContainsMode(modes, mode) {
					modes = append(modes, mode)
				}
			}
		}
	}

	return modes, nil
}

func SliceContainsMode(s []types.SessionParticipantMode, e types.SessionParticipantMode) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

// PolicyOptions is a set of settings for the session determined by the matched require policy.
type PolicyOptions struct {
	TerminateOnLeave bool
}

// FulfilledFor checks if a given session may run with a list of participants.
func (e *SessionAccessEvaluator) FulfilledFor(participants []SessionAccessContext) (bool, PolicyOptions, error) {
	supported, err := e.supportsSessionAccessControls()
	if err != nil {
		return false, PolicyOptions{}, trace.Wrap(err)
	}

	if len(e.requires) == 0 || !supported {
		return true, PolicyOptions{}, nil
	}

	for _, requirePolicy := range e.requires {
		left := requirePolicy.Count

		for _, participant := range participants {
			allowPolicies := getAllowPolicies(participant)
			for _, allowPolicy := range allowPolicies {
				matchesPredicate, err := e.matchesPredicate(&participant, requirePolicy, allowPolicy)
				if err != nil {
					return false, PolicyOptions{}, trace.Wrap(err)
				}

				if matchesPredicate && e.matchesJoin(allowPolicy) {
					left--
					break
				}
			}

			if left <= 0 {
				options := PolicyOptions{}

				switch requirePolicy.OnLeave {
				case types.OnSessionLeaveTerminate:
					options.TerminateOnLeave = true
				case types.OnSessionLeavePause:
					options.TerminateOnLeave = false
				default:
					return false, PolicyOptions{}, trace.BadParameter("unsupported on_leave policy: %v", requirePolicy.OnLeave)
				}

				return true, options, nil
			}
		}
	}

	return false, PolicyOptions{}, nil
}

// supportsSessionAccessControls checks if moderated sessions-style access controls can be applied to the session.
// If a set only has v4 or earlier roles, we don't want to apply the access checks to SSH sessions.
//
// This only applies to SSH sessions since they previously had no access control for joining sessions.
// We don't need this fallback behaviour for multiparty kubernetes since it's a new feature.
func (e *SessionAccessEvaluator) supportsSessionAccessControls() (bool, error) {
	if e.kind == types.SSHSessionKind {
		for _, role := range e.roles {
			switch role.GetVersion() {
			case types.V1, types.V2, types.V3, types.V4:
				return false, nil
			case types.V5:
				return true, nil
			default:
				return false, trace.BadParameter("unsupported role version: %v", role.GetVersion())
			}
		}
	}

	return false, nil
}

func preAccessControlsModes(kind types.SessionKind) []types.SessionParticipantMode {
	switch kind {
	case types.SSHSessionKind:
		return []types.SessionParticipantMode{types.SessionPeerMode}
	default:
		return nil
	}
}
