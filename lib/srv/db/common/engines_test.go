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
	"testing"

	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"
)

// TestRegisterEngine verifies database engine registration.
func TestRegisterEngine(t *testing.T) {
	ec := EngineConfig{
		Context: context.Background(),
	}

	// No engine is registered initially.
	engine, err := GetEngine("test", ec)
	require.Nil(t, engine)
	require.IsType(t, trace.NotFound(""), err)

	// Register a "test" engine.
	RegisterEngine("test", func(ec EngineConfig) (Engine, error) {
		return &testEngine{ec: ec}, nil
	})

	// Create the registered engine instance.
	engine, err = GetEngine("test", ec)
	require.NoError(t, err)
	require.NotNil(t, engine)

	// Verify it's the one we registered.
	engineInst, ok := engine.(*testEngine)
	require.True(t, ok)
	require.Equal(t, ec, engineInst.ec)
}

type testEngine struct {
	Engine
	ec EngineConfig
}
