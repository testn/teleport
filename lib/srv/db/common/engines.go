/*
Copyright 2022 Gravitational, Inc.

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

package common

import (
	"context"
	"sync"

	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/trace"

	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
)

var (
	// engines is a global database engines registry.
	engines map[string]EngineFn
	// enginesMu protects access to the global engines registry map.
	enginesMu sync.RWMutex
)

// EngineFn defines a database engine constructor function.
type EngineFn func(EngineConfig) (Engine, error)

// RegisterEngine registers a new engine constructor.
func RegisterEngine(name string, fn EngineFn) {
	enginesMu.Lock()
	defer enginesMu.Unlock()
	if engines == nil {
		engines = make(map[string]EngineFn)
	}
	engines[name] = fn
}

// GetEngine returns a new engine for the provided configuration.
func GetEngine(name string, conf EngineConfig) (Engine, error) {
	enginesMu.RLock()
	engineFn := engines[name]
	enginesMu.RUnlock()
	if engineFn == nil {
		return nil, trace.NotFound("database engine %q is not registered", name)
	}
	return engineFn(conf)
}

// EngineConfig is the common configuration every database engine uses.
type EngineConfig struct {
	// Auth handles database access authentication.
	Auth Auth
	// Audit emits database access audit events.
	Audit Audit
	// AuthClient is the cluster auth server client.
	AuthClient *auth.Client
	// CloudClients provides access to cloud API clients.
	CloudClients CloudClients
	// Context is the database server close context.
	Context context.Context
	// Clock is the clock interface.
	Clock clockwork.Clock
	// Log is used for logging.
	Log logrus.FieldLogger
}
