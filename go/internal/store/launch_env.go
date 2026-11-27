// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// launchEnvJSON is the column value for a Build's LaunchEnv: NULL when there is nothing to add, else a JSON object (keys sorted by the encoder, so
// the stored text is deterministic).
func launchEnvJSON(env map[string]string) any {
	if len(env) == 0 {
		return nil
	}
	b, err := json.Marshal(env)
	if err != nil { // a map[string]string cannot fail to marshal; refuse to write something unreadable rather than a silent NULL
		panic(fmt.Sprintf("store: launch_env marshal: %v", err))
	}
	return string(b)
}

// parseLaunchEnv reads the launch_env column. NULL and "" are no environment; anything that is not a JSON object of strings is an error (a failed
// parse is never read as "no environment": the launch would silently lose the variables the engine needs).
func parseLaunchEnv(raw sql.NullString) (map[string]string, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(raw.String), &env); err != nil {
		return nil, fmt.Errorf("launch_env is not a JSON object of strings: %w", err)
	}
	if len(env) == 0 {
		return nil, nil
	}
	return env, nil
}

// nullIfEmpty maps "" to SQL NULL for optional text columns.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
